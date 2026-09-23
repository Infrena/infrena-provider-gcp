package gcprov

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
	"github.com/infrena/infrena/pkg/provider"
)

// staticTokenValue is staticToken with a caller-chosen token value, for the
// one test that needs to assert on the exact bearer token attached.
func staticTokenValue(token string) oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token, TokenType: "Bearer"})
}

func TestDoSendsTheBodyAndDecodesTheJSONResponse(t *testing.T) {
	gcptest.Isolate(t)
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("server: decoding the request body: %v", err)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name": "widgets/one", "sizeGb": 10}`))
	}))
	defer srv.Close()

	c := NewClient(staticToken(), srv.URL+"/", ClientOptions{})
	got, err := c.Do(context.Background(), http.MethodPost, srv.URL+"/v1/widgets", map[string]any{"sizeGb": float64(10)})
	if err != nil {
		t.Fatal(err)
	}
	if got["name"] != "widgets/one" {
		t.Errorf("got %v", got)
	}
	if gotBody["sizeGb"] != float64(10) {
		t.Errorf("the server received %v, want the request body", gotBody)
	}
}

func TestDoSendsNoBodyWhenNil(t *testing.T) {
	gcptest.Isolate(t)
	var gotContentLength int64 = -1
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentLength = r.ContentLength
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(staticToken(), srv.URL+"/", ClientOptions{})
	if _, err := c.Do(context.Background(), http.MethodGet, srv.URL+"/v1/widgets/one", nil); err != nil {
		t.Fatal(err)
	}
	if gotContentLength > 0 {
		t.Errorf("Content-Length = %d, want no body sent", gotContentLength)
	}
	if gotContentType != "" {
		t.Errorf("Content-Type = %q, want unset when there is no body", gotContentType)
	}
}

// TestDoJoinsARelativeURLWithBase. Every other Do test passes an absolute
// srv.URL-prefixed url; this is the untested branch -- a caller (or a test)
// that passes just the path, relying on the base NewClient was given, e.g.
// how a Client built with base "" and always-absolute catalog urls would
// never exercise this, but NewClient's own doc says base exists precisely so
// a relative url works.
func TestDoJoinsARelativeURLWithBase(t *testing.T) {
	gcptest.Isolate(t)
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer srv.Close()

	c := NewClient(staticToken(), srv.URL+"/", ClientOptions{})
	got, err := c.Do(context.Background(), http.MethodGet, "v1/widgets/one", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["ok"] != true {
		t.Errorf("got %v", got)
	}
	if want := "/v1/widgets/one"; gotPath != want {
		t.Errorf("the server received path %q, want %q: base was not joined with the relative url", gotPath, want)
	}
}

func TestDoSetsTheQuotaProjectHeaderWhenConfigured(t *testing.T) {
	gcptest.Isolate(t)
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Goog-User-Project")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(staticToken(), srv.URL+"/", ClientOptions{QuotaProject: "billing-project"})
	if _, err := c.Do(context.Background(), http.MethodGet, srv.URL+"/v1/widgets/one", nil); err != nil {
		t.Fatal(err)
	}
	if got != "billing-project" {
		t.Errorf("X-Goog-User-Project = %q, want %q", got, "billing-project")
	}
}

func TestDoLeavesTheQuotaProjectHeaderUnsetWhenNotConfigured(t *testing.T) {
	gcptest.Isolate(t)
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		present = r.Header.Get("X-Goog-User-Project") != ""
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(staticToken(), srv.URL+"/", ClientOptions{})
	if _, err := c.Do(context.Background(), http.MethodGet, srv.URL+"/v1/widgets/one", nil); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Error("X-Goog-User-Project was sent although no quota project was configured")
	}
}

// TestDoRetriesASafeToRetryFailureThenSucceeds. A 503 UNAVAILABLE must be
// retried, not returned to the caller on the first failure.
func TestDoRetriesASafeToRetryFailureThenSucceeds(t *testing.T) {
	gcptest.Isolate(t)
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error": {"code": 503, "status": "UNAVAILABLE", "message": "try again"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer srv.Close()

	c := NewClient(staticToken(), srv.URL+"/", ClientOptions{})
	got, err := c.Do(context.Background(), http.MethodGet, srv.URL+"/v1/widgets/one", nil)
	if err != nil {
		t.Fatalf("a retryable failure was not retried: %v", err)
	}
	if got["ok"] != true {
		t.Errorf("got %v", got)
	}
	if n := atomic.LoadInt32(&attempts); n != 2 {
		t.Errorf("%d attempts, want 2 (one failure, one retry)", n)
	}
}

// TestDoDoesNotRetryANotSafeToRetryFailure. Retrying an INVALID_ARGUMENT
// wastes an attempt on a call that will fail identically every time.
func TestDoDoesNotRetryANotSafeToRetryFailure(t *testing.T) {
	gcptest.Isolate(t)
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": {"code": 400, "status": "INVALID_ARGUMENT", "message": "bad request"}}`))
	}))
	defer srv.Close()

	c := NewClient(staticToken(), srv.URL+"/", ClientOptions{})
	_, err := c.Do(context.Background(), http.MethodGet, srv.URL+"/v1/widgets/one", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Errorf("%d attempts, want 1: a not-safe-to-retry failure must not be retried", n)
	}
	if !contains(err.Error(), "bad request") {
		t.Errorf("error = %v, want the server's message", err)
	}
}

// TestDoGivesUpAfterMaxAttempts. A persistently failing but retryable call
// must not retry forever.
func TestDoGivesUpAfterMaxAttempts(t *testing.T) {
	gcptest.Isolate(t)
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error": {"status": "UNAVAILABLE", "message": "down"}}`))
	}))
	defer srv.Close()

	c := NewClient(staticToken(), srv.URL+"/", ClientOptions{MaxAttempts: 3})
	_, err := c.Do(context.Background(), http.MethodGet, srv.URL+"/v1/widgets/one", nil)
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if n := atomic.LoadInt32(&attempts); n != 3 {
		t.Errorf("%d attempts, want exactly MaxAttempts (3)", n)
	}
}

// TestDoErrorNeverMentionsTheToken. Client.Do must never log or return the
// Authorization header or the bearer token in an error, even when the
// request itself fails.
func TestDoErrorNeverMentionsTheToken(t *testing.T) {
	gcptest.Isolate(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-test-token" {
			t.Errorf("Authorization = %q, want the token attached by oauth2.Transport", got)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": {"status": "INVALID_ARGUMENT", "message": "nope"}}`))
	}))
	defer srv.Close()

	ts := staticTokenValue("secret-test-token")
	c := NewClient(ts, srv.URL+"/", ClientOptions{})
	_, err := c.Do(context.Background(), http.MethodGet, srv.URL+"/v1/widgets/one", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if contains(err.Error(), "secret-test-token") {
		t.Errorf("the error leaked the bearer token: %v", err)
	}
	if contains(err.Error(), "Authorization") {
		t.Errorf("the error mentions the Authorization header: %v", err)
	}
}

func TestNewClientDefaultsTheTimeoutWhenHTTPClientIsUnset(t *testing.T) {
	gcptest.Isolate(t)
	c := NewClient(staticToken(), "https://example.invalid/", ClientOptions{})
	if c.httpClient.Timeout != defaultTimeout {
		t.Errorf("Timeout = %v, want defaultTimeout (%v): a client with no timeout at all wedges the plugin on a hung endpoint", c.httpClient.Timeout, defaultTimeout)
	}
}

// TestNewClientWrapsACallerSuppliedHTTPClientRatherThanReplacingIt. A caller
// injecting its own HTTPClient (a shorter timeout for a test, a custom
// transport) must still get oauth2 auth attached, and its own Timeout must
// be kept rather than silently overridden.
func TestNewClientWrapsACallerSuppliedHTTPClientRatherThanReplacingIt(t *testing.T) {
	gcptest.Isolate(t)
	custom := &http.Client{Timeout: 3 * time.Second}
	c := NewClient(staticToken(), "https://example.invalid/", ClientOptions{HTTPClient: custom})
	if c.httpClient.Timeout != 3*time.Second {
		t.Errorf("Timeout = %v, want the caller's own 3s", c.httpClient.Timeout)
	}
	if _, ok := c.httpClient.Transport.(*oauth2.Transport); !ok {
		t.Errorf("Transport = %T, want it wrapped with oauth2.Transport so auth is still attached", c.httpClient.Transport)
	}
	// The caller's own *http.Client must not be mutated -- NewClient must copy
	// it, not reach into the value the caller still holds a reference to.
	if custom.Transport != nil {
		t.Error("NewClient mutated the caller's own http.Client in place")
	}
}

func TestParseProjectAndAPI(t *testing.T) {
	cases := []struct {
		url         string
		wantProject string
		wantAPI     string
	}{
		{"https://compute.googleapis.com/compute/v1/projects/p/zones/z/instances/i", "p", "compute"},
		{"https://storage.googleapis.com/storage/v1/b/my-bucket", "", "storage"},
		{"https://accesscontextmanager.googleapis.com/v1/accessPolicies/123", "", "accesscontextmanager"},
		{"https://cloudresourcemanager.googleapis.com/v3/projects/q/tagBindings", "q", "cloudresourcemanager"},
		{"not a url at all", "", ""},
	}
	for _, c := range cases {
		project, api := parseProjectAndAPI(c.url)
		if project != c.wantProject || api != c.wantAPI {
			t.Errorf("parseProjectAndAPI(%q) = (%q, %q), want (%q, %q)",
				c.url, project, api, c.wantProject, c.wantAPI)
		}
	}
}

// TestAQuotaProject403SaysItIsAboutTheQuotaProject.
//
// The body is verbatim from the live run on 2026-09-22: a service account
// holding roles/storage.admin, with quota_project set, told it lacks
// "serviceusage.services.use access". That reads as a broken storage grant
// and is nothing of the kind, and a user who believes the message goes and
// edits the wrong role.
func TestAQuotaProject403SaysItIsAboutTheQuotaProject(t *testing.T) {
	const body = `{"error":{"code":403,"message":"infrena-live@example-project-1234.iam.` +
		`gserviceaccount.com does not have serviceusage.services.use access to the Google Cloud ` +
		`project.","status":"PERMISSION_DENIED","details":[{"@type":` +
		`"type.googleapis.com/google.rpc.ErrorInfo","reason":"USER_PROJECT_DENIED"}]}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewClient(staticToken(), srv.URL, ClientOptions{QuotaProject: "some-project", MaxAttempts: 1})
	_, err := c.Do(context.Background(), http.MethodGet, srv.URL+"/v1/thing", nil)
	if err == nil {
		t.Fatal("a 403 produced no error")
	}
	if !contains(err.Error(), "quota_project") {
		t.Errorf("the error never mentions quota_project, so the reader is sent after the wrong "+
			"permission:\n%s", err)
	}
	if !contains(err.Error(), "some-project") {
		t.Errorf("the error does not name the configured quota project:\n%s", err)
	}
	// AND THE TAXONOMY SURVIVES. Wrapping must not hide the *APIError, or
	// ClassifyError silently degrades every one of these to "unrecognised".
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("the wrapped error no longer unwraps to an *APIError: %T", err)
	}
	if ae.Status != http.StatusForbidden {
		t.Errorf("Status = %d", ae.Status)
	}
	if got := ClassifyError(err); got != provider.NotSafeToRetry {
		t.Errorf("ClassifyError = %v, want NotSafeToRetry", got)
	}
}

// TestAnInstanceWithNoQuotaProjectIsNotToldAboutOne. The explanation must not
// fire for an instance that never configured the setting, or it sends a
// reader after something they do not have.
func TestAnInstanceWithNoQuotaProjectIsNotToldAboutOne(t *testing.T) {
	const body = `{"error":{"code":403,"message":"caller lacks storage.buckets.get",` +
		`"status":"PERMISSION_DENIED"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewClient(staticToken(), srv.URL, ClientOptions{MaxAttempts: 1})
	_, err := c.Do(context.Background(), http.MethodGet, srv.URL+"/v1/thing", nil)
	if err == nil {
		t.Fatal("a 403 produced no error")
	}
	if contains(err.Error(), "quota_project") {
		t.Errorf("an instance that sets no quota project was told its problem is one:\n%s", err)
	}
}

// TestClientOptionsTimeoutIsActuallyHonoured. The field was declared and
// documented from Task 11 and NEVER READ until 2026-09-22: NewClient went
// straight from "no HTTPClient" to defaultTimeout, so every Client in the
// process ran at 30s whatever a caller asked for. Nothing noticed because
// no test had ever set it -- TestNewClientDefaultsTheTimeoutWhenHTTPClientIsUnset
// asserts the default and TestNewClientWrapsACallerSuppliedHTTPClientRatherThanReplacingIt
// asserts the HTTPClient path, and between them they covered both sides of
// the option that works and neither side of the one that did not.
//
// Found because the compute-wait test needs a round-trip bound short enough
// to measure, and PASSED against the unfixed code.
func TestClientOptionsTimeoutIsActuallyHonoured(t *testing.T) {
	gcptest.Isolate(t)
	c := NewClient(staticToken(), "https://example.invalid/", ClientOptions{Timeout: 2 * time.Second})
	if c.httpClient.Timeout != 2*time.Second {
		t.Errorf("Timeout = %v, want the caller's own 2s: ClientOptions.Timeout is not being read", c.httpClient.Timeout)
	}
}

// TestDoLongPollOutlivesTheRoundTripBoundAndDoDoesNot pins both halves of
// the distinction against one server that answers slowly: the SAME call is
// cut off by Do and completes through DoLongPoll. Asserting only the second
// would pass just as well if the bound had simply been raised for
// everything, which is the fix this deliberately is not (see DoLongPoll).
func TestDoLongPollOutlivesTheRoundTripBoundAndDoDoesNot(t *testing.T) {
	gcptest.Isolate(t)
	const roundTrip = 50 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(6 * roundTrip)
		_, _ = w.Write([]byte(`{"status":"DONE"}`))
	}))
	defer srv.Close()

	c := NewClient(staticToken(), srv.URL+"/", ClientOptions{Timeout: roundTrip, MaxAttempts: 1})

	if _, err := c.Do(context.Background(), http.MethodPost, srv.URL+"/wait", nil); err == nil {
		t.Error("an ordinary call was not cut off at the round-trip bound")
	}

	got, err := c.DoLongPoll(context.Background(), http.MethodPost, srv.URL+"/wait", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("a long poll was cut off at the ordinary round-trip bound: %v", err)
	}
	if got["status"] != "DONE" {
		t.Errorf("the long poll's answer was lost: %v", got)
	}
}

// TestDoLongPollWithNoBoundIsAnOrdinaryCall. A bound of zero must not be
// read as "no limit": granting an unbounded request is exactly the wedged
// plugin defaultTimeout exists to prevent, and a caller passing zero has
// asked for nothing in particular.
func TestDoLongPollWithNoBoundIsAnOrdinaryCall(t *testing.T) {
	gcptest.Isolate(t)
	const roundTrip = 50 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(6 * roundTrip)
		_, _ = w.Write([]byte(`{"status":"DONE"}`))
	}))
	defer srv.Close()

	c := NewClient(staticToken(), srv.URL+"/", ClientOptions{Timeout: roundTrip, MaxAttempts: 1})
	if _, err := c.DoLongPoll(context.Background(), http.MethodPost, srv.URL+"/wait", nil, 0); err == nil {
		t.Error("a long poll with no bound was granted one anyway")
	}
}
