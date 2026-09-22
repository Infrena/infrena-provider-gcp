package gcprov

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/infrena/infrena/pkg/provider"
)

// APIError is one GCP REST API error response:
//
//	{"error": {"code": 409, "message": "...", "status": "ABORTED"}}
//
// Status is the HTTP status GCP actually answered with (usually, but not
// always, identical to the numeric "code" GCP repeats inside the body).
// Code is GCP's own error.status enum string (e.g. "ABORTED",
// "ALREADY_EXISTS") -- the thing that actually distinguishes two errors
// sharing one HTTP status, which the status code alone cannot.
type APIError struct {
	Status  int
	Code    string
	Message string
	// Reason is google.rpc.ErrorInfo's own `reason` -- "RATE_LIMIT_EXCEEDED",
	// "SERVICE_DISABLED", "IAM_PERMISSION_DENIED" -- or, when the response is
	// in the older Discovery shape that carries no ErrorInfo at all, the
	// legacy error.errors[].reason ("rateLimitExceeded", "forbidden").
	//
	// IT IS SEPARATE FROM Code BECAUSE IT IS A DIFFERENT FIELD, not a
	// fallback for one. Measured against live compute on 2026-09-22: a quota
	// throttle answers 403 with NO error.status at all, so Code is empty and
	// the HTTP status is indistinguishable from a real permission denial.
	// The reason is the only thing in the response that tells them apart.
	Reason string
	// RetryAfter is honoured when the response carries a Retry-After header.
	//
	// MEASURED AGAINST LIVE GCP, 2026-09-22 (live/README.md): across roughly
	// 65,000 throttled responses in two independent rounds, cloudresourcemanager
	// (429 RESOURCE_EXHAUSTED) and compute (403 rateLimitExceeded) sent it
	// zero times, and storage could not be throttled at all inside a minute.
	// Two of the twenty-five APIs this provider calls is not "never", which
	// is why this stays: it reads the header when present and makes no claim
	// about how often that is. See live/README.md for what would close the
	// question.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("gcp: %s (%d %s)", e.Message, e.Status, e.Code)
	}
	return fmt.Sprintf("gcp: %s (%d)", e.Message, e.Status)
}

// errorEnvelope is the wire shape decodeAPIError parses: GCP nests every
// REST error under "error", regardless of API.
type errorEnvelope struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
		// Details is google.rpc's own status detail list. Only ErrorInfo is
		// read, and only its reason: that is the machine-readable cause
		// Google documents as stable, where the message is prose.
		Details []struct {
			Type   string `json:"@type"`
			Reason string `json:"reason"`
		} `json:"details"`
		// Errors is the OLDER Discovery error shape, which compute, storage
		// and the other pre-google.rpc APIs still answer with. It carries no
		// status and no ErrorInfo, so without reading it a compute throttle
		// has nothing in it to recognise at all.
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"error"`
}

// errorInfoType is the google.rpc.ErrorInfo detail's own @type. Matched
// exactly rather than by suffix: a detail list carries several types
// (RetryInfo, QuotaFailure, Help) and only ErrorInfo's `reason` is the
// documented, stable cause.
const errorInfoType = "type.googleapis.com/google.rpc.ErrorInfo"

// reasonOf reads the machine-readable cause out of one error envelope:
// google.rpc.ErrorInfo's reason when the response carries one, and the legacy
// Discovery errors[].reason otherwise. Empty when neither is present.
func reasonOf(env errorEnvelope) string {
	for _, d := range env.Error.Details {
		if d.Type == errorInfoType && d.Reason != "" {
			return d.Reason
		}
	}
	for _, e := range env.Error.Errors {
		if e.Reason != "" {
			return e.Reason
		}
	}
	return ""
}

// decodeAPIError builds an APIError from one HTTP response and its
// already-read body. The body is parsed on a best-effort basis: a response
// that is not GCP's own JSON error shape (a proxy's plain-text 502, an empty
// or truncated body) still produces a usable APIError carrying the status and
// the raw body as its message, rather than a decode failure masking the real
// one or, worse, becoming a nil error.
func decodeAPIError(resp *http.Response, body []byte) *APIError {
	ae := &APIError{Status: resp.StatusCode, Message: string(body)}
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && (env.Error.Status != "" || env.Error.Message != "") {
		if env.Error.Message != "" {
			ae.Message = env.Error.Message
		}
		ae.Code = env.Error.Status
		ae.Reason = reasonOf(env)
	}
	ae.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	return ae
}

// parseRetryAfter reads a Retry-After header in either form the HTTP spec
// allows: a delay in seconds, or an HTTP-date. An empty or unparseable
// header yields zero, which callers treat as "not present".
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// retryableCodes and notRetryableCodes are GCP's own error.status enum,
// split by spec §5.6 / the measured facts in the task-11 brief. A 409 is
// either ABORTED (a concurrency conflict worth retrying) or ALREADY_EXISTS
// (the thing is already there, and retrying fails identically forever) --
// both share the HTTP status, so the split must read Code, never Status
// alone.
var retryableCodes = map[string]bool{
	"RESOURCE_EXHAUSTED": true,
	"UNAVAILABLE":        true,
	"ABORTED":            true,
}

var notRetryableCodes = map[string]bool{
	"INVALID_ARGUMENT":    true,
	"PERMISSION_DENIED":   true,
	"FAILED_PRECONDITION": true,
	"ALREADY_EXISTS":      true,
}

// retryableReasons are the google.rpc.ErrorInfo (and legacy Discovery)
// reasons that mean "you asked too fast", whatever HTTP status they arrive
// with.
//
// THIS EXISTS BECAUSE COMPUTE DOES NOT SEND 429. Measured against the live
// project on 2026-09-22 by driving a real throttle: cloudresourcemanager
// answers a quota throttle with 429 and status RESOURCE_EXHAUSTED, which the
// code and status tables below already catch -- but compute answers the same
// thing with
//
//	403 {"error":{"code":403,"message":"Quota exceeded for quota metric
//	'Read requests' ...","errors":[{"domain":"usageLimits",
//	"reason":"rateLimitExceeded"}],"details":[{"@type":".../ErrorInfo",
//	"reason":"RATE_LIMIT_EXCEEDED"}]}}
//
// and NO error.status at all. So Code was empty, 403 is not in
// retryableStatus, and every compute throttle was classified NotSafeToRetry
// -- a transient quota blip reported to the user as a permanent failure, on
// the API this provider calls more than any other. 3,219 such responses were
// produced in sixty seconds, so it is not a corner.
//
// Both spellings, because both are live: the screaming-snake one is
// google.rpc.ErrorInfo's, the camel one is the older Discovery shape's, and
// which a service uses is a fact about the service's age.
//
// A 403 that is a REAL permission denial carries a different reason
// ("forbidden", "IAM_PERMISSION_DENIED", "SERVICE_DISABLED") and still falls
// through to NotSafeToRetry, which is what stops this turning every
// permission problem into a retry storm.
var retryableReasons = map[string]bool{
	"RATE_LIMIT_EXCEEDED": true,
	"rateLimitExceeded":   true,
}

var retryableStatus = map[int]bool{
	http.StatusTooManyRequests:     true, // 429
	http.StatusInternalServerError: true, // 500
	http.StatusServiceUnavailable:  true, // 503
}

// ClassifyError says whether the operation that produced err may be safely
// retried. It is a PURE FUNCTION OF THE ERROR: the host may ask any
// configured instance to classify an error, not necessarily the instance
// whose call actually failed (provider.Provider.ClassifyError), so nothing
// here may read Client or Provider state.
//
// The switch tries Reason first, Code second and Status third. Reason leads
// because it is the only field that survives an API answering a throttle
// with a status code that means something else -- see retryableReasons, and
// the live measurement that found it. A Code this function does not
// recognise (e.g. "INTERNAL") still gets a verdict from Status.
//
// errors.As, not a type assertion: a caller wrapping the
// failure with fmt.Errorf("...: %w", err) -- normal, expected practice
// everywhere else in this codebase -- must still classify correctly. A bare
// type assertion would silently degrade every wrapped *APIError to
// NotSafeToRetry.
//
// A transport-level failure -- no *APIError anywhere in the chain, but a
// net.Error, a *url.Error (net/http.Client.Do wraps every RoundTrip failure
// in one), or io.ErrUnexpectedEOF -- is ConditionallyRetryable, not
// NotSafeToRetry. This is exactly what the three-valued Retryability exists
// for: a connection reset or a dropped TLS session may have happened before
// or after GCP actually acted on the request, and there is no way to tell
// from the error alone. Classifying it NotSafeToRetry fails an apply a
// single retry would have completed; classifying it SafeToRetry risks a
// duplicate create. ConditionallyRetryable is the honest answer, and the
// core already knows what to do with it: retry reads and updates, never
// retry creates or deletes.
//
// Anything still unrecognised after both checks -- not an *APIError, not a
// transport failure -- is NotSafeToRetry, the least safe kind and the
// correct default for something nobody has classified.
func ClassifyError(err error) provider.Retryability {
	var ae *APIError
	if errors.As(err, &ae) {
		// FIRST, because it is the most specific thing in the response and
		// the only thing that distinguishes compute's 403 throttle from
		// compute's 403 permission denial. A body that names a rate limit is
		// a rate limit whatever status it arrived with.
		if retryableReasons[ae.Reason] {
			return provider.SafeToRetry
		}
		if retryableCodes[ae.Code] {
			return provider.SafeToRetry
		}
		if notRetryableCodes[ae.Code] {
			return provider.NotSafeToRetry
		}
		if retryableStatus[ae.Status] {
			return provider.SafeToRetry
		}
		return provider.NotSafeToRetry
	}
	if isTransportError(err) {
		return provider.ConditionallyRetryable
	}
	return provider.NotSafeToRetry
}

// isTransportError reports whether err is (or wraps) a failure below the
// HTTP-response level: the request never got a response to classify by
// status or code at all.
func isTransportError(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF)
}
