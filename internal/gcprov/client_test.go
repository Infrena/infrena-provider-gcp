package gcprov

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
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
