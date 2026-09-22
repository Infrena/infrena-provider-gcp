package gcprov

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/infrena/infrena/pkg/provider"
)

func TestErrorsAreClassifiedFromStatusAndCode(t *testing.T) {
	cases := []struct {
		err  error
		want provider.Retryability
	}{
		{&APIError{Status: 429, Code: "RESOURCE_EXHAUSTED"}, provider.SafeToRetry},
		{&APIError{Status: 503, Code: "UNAVAILABLE"}, provider.SafeToRetry},
		{&APIError{Status: 500, Code: "INTERNAL"}, provider.SafeToRetry},
		{&APIError{Status: 409, Code: "ABORTED"}, provider.SafeToRetry},
		{&APIError{Status: 400, Code: "INVALID_ARGUMENT"}, provider.NotSafeToRetry},
		{&APIError{Status: 403, Code: "PERMISSION_DENIED"}, provider.NotSafeToRetry},
		{&APIError{Status: 409, Code: "ALREADY_EXISTS"}, provider.NotSafeToRetry},
		{&APIError{Status: 412, Code: "FAILED_PRECONDITION"}, provider.NotSafeToRetry},
		{errors.New("something nobody classified"), provider.NotSafeToRetry},
	}
	for _, c := range cases {
		if got := ClassifyError(c.err); got != c.want {
			t.Errorf("ClassifyError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestClassificationDoesNotDependOnAnInstance. The host asks ANY configured
// instance to classify, not the one whose call failed, so classification
// must not read instance state -- there is no instance state to read here at
// all, and calling ClassifyError twice on the same error must agree.
func TestClassificationDoesNotDependOnAnInstance(t *testing.T) {
	err := &APIError{Status: 429, Code: "RESOURCE_EXHAUSTED"}
	a := ClassifyError(err)
	b := ClassifyError(err)
	if a != b || a != provider.SafeToRetry {
		t.Errorf("classification is not a pure function of the error: %v then %v", a, b)
	}
}

// TestStatusAloneIsNotEnoughFor409. Both ABORTED and ALREADY_EXISTS are 409,
// and they mean opposite things: one is a concurrency conflict worth
// retrying, the other says the thing is already there and retrying will
// fail identically forever.
func TestStatusAloneIsNotEnoughFor409(t *testing.T) {
	if ClassifyError(&APIError{Status: 409, Code: "ABORTED"}) ==
		ClassifyError(&APIError{Status: 409, Code: "ALREADY_EXISTS"}) {
		t.Error("409 is being classified by status alone")
	}
}

// testResponse builds a minimal *http.Response for decodeAPIError, which
// reads only Status/StatusCode and Header -- the body is passed separately,
// matching how Client.doOnce already has to read it (to bound its size)
// before it knows whether the response was an error at all.
func testResponse(status int, h http.Header) *http.Response {
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: h}
}

func TestDecodeAPIErrorParsesTheEnvelope(t *testing.T) {
	body := []byte(`{"error": {"code": 409, "message": "already there", "status": "ALREADY_EXISTS"}}`)
	ae := decodeAPIError(testResponse(http.StatusConflict, nil), body)
	if ae.Status != http.StatusConflict {
		t.Errorf("Status = %d, want %d", ae.Status, http.StatusConflict)
	}
	if ae.Code != "ALREADY_EXISTS" {
		t.Errorf("Code = %q", ae.Code)
	}
	if ae.Message != "already there" {
		t.Errorf("Message = %q", ae.Message)
	}
}

// TestDecodeAPIErrorSurvivesANonGCPBody. A proxy or load balancer in front of
// an API can answer with plain text, not GCP's JSON error envelope. That
// must still produce an APIError -- the status code alone is still useful --
// rather than losing the failure to a JSON decode error.
func TestDecodeAPIErrorSurvivesANonGCPBody(t *testing.T) {
	ae := decodeAPIError(testResponse(http.StatusBadGateway, nil), []byte("<html>502 Bad Gateway</html>"))
	if ae.Status != http.StatusBadGateway {
		t.Errorf("Status = %d", ae.Status)
	}
	if ae.Code != "" {
		t.Errorf("Code = %q, want empty: no GCP status enum was present", ae.Code)
	}
	if !contains(ae.Message, "502") {
		t.Errorf("Message = %q, want the raw body preserved", ae.Message)
	}
}

// TestDecodeAPIErrorSurvivesAnEmptyBody. A truncated or bodiless error
// response (a dropped connection, a HEAD-like answer) must still produce a
// usable APIError carrying the status, never a nil error masking a real
// failure.
func TestDecodeAPIErrorSurvivesAnEmptyBody(t *testing.T) {
	ae := decodeAPIError(testResponse(http.StatusServiceUnavailable, nil), nil)
	if ae == nil {
		t.Fatal("decodeAPIError returned nil for an empty body")
	}
	if ae.Status != http.StatusServiceUnavailable {
		t.Errorf("Status = %d", ae.Status)
	}
}

func TestDecodeAPIErrorCapturesRetryAfterInSeconds(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "30")
	ae := decodeAPIError(testResponse(http.StatusServiceUnavailable, h), []byte(`{}`))
	if ae.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", ae.RetryAfter)
	}
}

func TestDecodeAPIErrorWithNoRetryAfterHeaderLeavesItZero(t *testing.T) {
	ae := decodeAPIError(testResponse(http.StatusServiceUnavailable, nil), []byte(`{}`))
	if ae.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, want 0: no header was present", ae.RetryAfter)
	}
}

func TestAPIErrorMessageNamesTheCodeAndStatus(t *testing.T) {
	err := &APIError{Status: 409, Code: "ABORTED", Message: "conflict"}
	if !contains(err.Error(), "409") || !contains(err.Error(), "ABORTED") || !contains(err.Error(), "conflict") {
		t.Errorf("Error() = %q, missing status, code or message", err.Error())
	}
}

// TestClassifyErrorSeesThroughAWrappedError. Wrapping with fmt.Errorf("%w")
// is normal practice everywhere else in this codebase and must not silently
// degrade a retryable *APIError to NotSafeToRetry.
func TestClassifyErrorSeesThroughAWrappedError(t *testing.T) {
	wrapped := fmt.Errorf("gcp: creating the widget: %w", &APIError{Status: 503, Code: "UNAVAILABLE"})
	if got := ClassifyError(wrapped); got != provider.SafeToRetry {
		t.Errorf("ClassifyError(wrapped) = %v, want SafeToRetry", got)
	}
}

// TestClassifyErrorTreatsANetErrorAsConditionallyRetryable. A connection
// reset or timeout means the request may or may not have reached GCP and
// taken effect -- neither SafeToRetry (risks a duplicate create) nor
// NotSafeToRetry (fails an apply a retry would have completed) is honest.
// context.DeadlineExceeded implements net.Error (Timeout() == true), which
// makes it a convenient net.Error to construct directly in a test.
func TestClassifyErrorTreatsANetErrorAsConditionallyRetryable(t *testing.T) {
	if got := ClassifyError(context.DeadlineExceeded); got != provider.ConditionallyRetryable {
		t.Errorf("ClassifyError(context.DeadlineExceeded) = %v, want ConditionallyRetryable", got)
	}
}

// TestClassifyErrorTreatsAWrappedURLErrorAsConditionallyRetryable.
// net/http.Client.Do wraps every RoundTrip failure (a dial refusal, a reset
// connection) in a *url.Error, and Client.doOnce wraps THAT again with
// fmt.Errorf("%w") -- both layers must still classify correctly.
func TestClassifyErrorTreatsAWrappedURLErrorAsConditionallyRetryable(t *testing.T) {
	transportErr := &url.Error{Op: "Get", URL: "https://compute.googleapis.com/", Err: errors.New("connection reset by peer")}
	wrapped := fmt.Errorf("gcprov: GET https://compute.googleapis.com/: %w", transportErr)
	if got := ClassifyError(wrapped); got != provider.ConditionallyRetryable {
		t.Errorf("ClassifyError(wrapped url.Error) = %v, want ConditionallyRetryable", got)
	}
}

func TestClassifyErrorTreatsUnexpectedEOFAsConditionallyRetryable(t *testing.T) {
	if got := ClassifyError(io.ErrUnexpectedEOF); got != provider.ConditionallyRetryable {
		t.Errorf("ClassifyError(io.ErrUnexpectedEOF) = %v, want ConditionallyRetryable", got)
	}
	wrapped := fmt.Errorf("reading the response: %w", io.ErrUnexpectedEOF)
	if got := ClassifyError(wrapped); got != provider.ConditionallyRetryable {
		t.Errorf("ClassifyError(wrapped io.ErrUnexpectedEOF) = %v, want ConditionallyRetryable", got)
	}
}

// TestClassifyErrorStillDefaultsUnrecognisedToNotSafeToRetry. An error that
// is neither an *APIError nor any recognised transport failure shape stays
// NotSafeToRetry -- adding the transport-error branch must not turn
// ClassifyError into "retry anything we don't understand".
func TestClassifyErrorStillDefaultsUnrecognisedToNotSafeToRetry(t *testing.T) {
	if got := ClassifyError(errors.New("something nobody classified")); got != provider.NotSafeToRetry {
		t.Errorf("ClassifyError(plain error) = %v, want NotSafeToRetry", got)
	}
}

// The bodies below are VERBATIM from the live suite's throttle measurement
// against project example-project-1234 on 2026-09-22 (live/README.md),
// trimmed only of the Help link. They are fixtures copied from Google, not
// fixtures invented here, which is the difference between testing what the
// API does and testing what somebody assumed it does.
const (
	// cloudresourcemanager, and the shape the tables already handled: 429
	// with a google.rpc status.
	liveCRMThrottle = `{"error":{"code":429,"message":"Quota exceeded for quota metric ` +
		`'Project V3 get requests' and limit 'Project V3 get requests per minute' of service ` +
		`'cloudresourcemanager.googleapis.com' for consumer 'project_number:123456789012'.",` +
		`"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo",` +
		`"reason":"RATE_LIMIT_EXCEEDED","domain":"googleapis.com"}]}}`

	// compute, and the shape that was classified wrong: 403, no status at
	// all, and the cause only in the legacy errors[] and in ErrorInfo.
	liveComputeThrottle = `{"error":{"code":403,"message":"Quota exceeded for quota metric ` +
		`'Read requests' and limit 'Read requests per minute per region' of service ` +
		`'compute.googleapis.com' for consumer 'project_number:123456789012'.",` +
		`"errors":[{"message":"Quota exceeded for quota metric 'Read requests'",` +
		`"domain":"usageLimits","reason":"rateLimitExceeded"}],` +
		`"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo",` +
		`"reason":"RATE_LIMIT_EXCEEDED","domain":"googleapis.com"}]}}`

	// A REAL 403, for the other half of the claim. Also verbatim: this is
	// what the live service account got from storage before its quota
	// project was removed.
	liveRealPermissionDenied = `{"error":{"code":403,"message":"infrena-live@example-project-1234` +
		`.iam.gserviceaccount.com does not have serviceusage.services.use access to the Google ` +
		`Cloud project.","status":"PERMISSION_DENIED","details":[{"@type":` +
		`"type.googleapis.com/google.rpc.ErrorInfo","reason":"USER_PROJECT_DENIED",` +
		`"domain":"googleapis.com"}]}}`
)

// TestAComputeQuotaThrottleIsRetryable is the defect the live suite found.
//
// compute answers a quota throttle with 403 and NO error.status, so Code is
// empty and 403 is not a retryable HTTP status -- every compute throttle was
// classified NotSafeToRetry, which reports a transient quota blip to the
// user as a permanent failure on the API this provider calls most. 3,219 of
// these arrived in sixty seconds during the measurement, so it is not a
// corner case.
func TestAComputeQuotaThrottleIsRetryable(t *testing.T) {
	ae := decodeAPIError(testResponse(http.StatusForbidden, nil), []byte(liveComputeThrottle))
	if ae.Code != "" {
		t.Errorf("Code = %q; this body really does carry no error.status, and a test that "+
			"passes because one was invented is testing the fixture", ae.Code)
	}
	if ae.Reason != "RATE_LIMIT_EXCEEDED" {
		t.Errorf("Reason = %q, want RATE_LIMIT_EXCEEDED from the ErrorInfo detail", ae.Reason)
	}
	if got := ClassifyError(ae); got != provider.SafeToRetry {
		t.Errorf("ClassifyError = %v, want SafeToRetry", got)
	}
}

// TestALegacyRateLimitReasonIsRetryableWithoutErrorInfo. The older Discovery
// APIs answer with errors[].reason and no details at all, so the modern
// signal alone would still miss them.
func TestALegacyRateLimitReasonIsRetryableWithoutErrorInfo(t *testing.T) {
	const legacyOnly = `{"error":{"code":403,"message":"Quota exceeded",` +
		`"errors":[{"domain":"usageLimits","reason":"rateLimitExceeded"}]}}`
	ae := decodeAPIError(testResponse(http.StatusForbidden, nil), []byte(legacyOnly))
	if ae.Reason != "rateLimitExceeded" {
		t.Errorf("Reason = %q, want the legacy errors[].reason", ae.Reason)
	}
	if got := ClassifyError(ae); got != provider.SafeToRetry {
		t.Errorf("ClassifyError = %v, want SafeToRetry", got)
	}
}

// TestARealPermissionDenialIsStillNotRetryable is the half that makes the
// one above safe. If a 403 were retried on its status, a misconfigured
// service account would produce a retry storm against an error that cannot
// improve -- so the reason has to be doing the work, not the status.
func TestARealPermissionDenialIsStillNotRetryable(t *testing.T) {
	ae := decodeAPIError(testResponse(http.StatusForbidden, nil), []byte(liveRealPermissionDenied))
	if ae.Reason != "USER_PROJECT_DENIED" {
		t.Errorf("Reason = %q", ae.Reason)
	}
	if got := ClassifyError(ae); got != provider.NotSafeToRetry {
		t.Errorf("ClassifyError = %v, want NotSafeToRetry: a permission denial cannot be retried "+
			"into succeeding", got)
	}
}

// TestACloudResourceManagerThrottleIsRetryable pins the shape that already
// worked, so a change to the reason path cannot quietly break the code path.
func TestACloudResourceManagerThrottleIsRetryable(t *testing.T) {
	ae := decodeAPIError(testResponse(http.StatusTooManyRequests, nil), []byte(liveCRMThrottle))
	if ae.Code != "RESOURCE_EXHAUSTED" {
		t.Errorf("Code = %q", ae.Code)
	}
	if got := ClassifyError(ae); got != provider.SafeToRetry {
		t.Errorf("ClassifyError = %v, want SafeToRetry", got)
	}
}

// TestNeitherLiveThrottleCarriedARetryAfterHeader records the measurement
// itself, so the decision to KEEP APIError.RetryAfter is testable rather
// than only written down.
//
// It is NOT a claim that GCP never sends the header: two of the twenty-five
// APIs this provider calls were throttled, and storage could not be
// throttled at all. See live/README.md for what would close the question.
func TestNeitherLiveThrottleCarriedARetryAfterHeader(t *testing.T) {
	for name, body := range map[string]string{
		"cloudresourcemanager": liveCRMThrottle,
		"compute":              liveComputeThrottle,
	} {
		ae := decodeAPIError(testResponse(http.StatusTooManyRequests, nil), []byte(body))
		if ae.RetryAfter != 0 {
			t.Errorf("%s: RetryAfter = %v; the live responses carried no such header", name, ae.RetryAfter)
		}
	}
}
