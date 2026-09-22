package gcprov

import (
	"errors"
	"net/http"
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

func TestDecodeAPIErrorParsesTheEnvelope(t *testing.T) {
	body := []byte(`{"error": {"code": 409, "message": "already there", "status": "ALREADY_EXISTS"}}`)
	ae := decodeAPIError(http.StatusConflict, body, http.Header{})
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
	ae := decodeAPIError(http.StatusBadGateway, []byte("<html>502 Bad Gateway</html>"), http.Header{})
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

func TestDecodeAPIErrorCapturesRetryAfterInSeconds(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "30")
	ae := decodeAPIError(http.StatusServiceUnavailable, []byte(`{}`), h)
	if ae.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", ae.RetryAfter)
	}
}

func TestDecodeAPIErrorWithNoRetryAfterHeaderLeavesItZero(t *testing.T) {
	ae := decodeAPIError(http.StatusServiceUnavailable, []byte(`{}`), http.Header{})
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
