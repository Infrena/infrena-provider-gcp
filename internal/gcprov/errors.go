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
	// RetryAfter is honoured when the response carries a Retry-After header.
	// Whether GCP's own APIs ever send one is Task 18's question to measure
	// against a live project -- this only reads the header when present and
	// makes no claim about how often that is.
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
	} `json:"error"`
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
// The switch tries Code first and Status second, per the task-11 brief: a
// Code this function does not recognise (e.g. "INTERNAL") still gets a
// verdict from Status. errors.As, not a type assertion: a caller wrapping the
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
