package gcpplugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// TestImpersonationTimesOutRatherThanHanging is F1: a hung
// iamcredentials.googleapis.com must not wedge the plugin process. The
// server below accepts the connection and then never responds, and the
// token source is given a short client timeout (independent of the
// context's own, much longer deadline -- context.Background() carries none
// at all) so a correct implementation returns an error quickly. The test
// itself is bounded well above that by an outer 5s deadline so a regression
// that genuinely hangs fails the test suite fast rather than stalling it.
func TestImpersonationTimesOutRatherThanHanging(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang // never respond
	}))
	// close(hang) must run BEFORE srv.Close(): Close() waits for the
	// in-flight handler goroutine to return, which only happens once hang is
	// closed -- deferred in this order (LIFO) so that happens first rather
	// than the two deadlocking each other.
	defer srv.Close()
	defer close(hang)

	orig := iamCredentialsBaseURL
	iamCredentialsBaseURL = srv.URL
	defer func() { iamCredentialsBaseURL = orig }()

	ts := &impersonatedTokenSource{
		ctx:    context.Background(), // deliberately no deadline: proves the bound does not come from the caller
		base:   oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "base-token"}),
		target: "test@example.iam.gserviceaccount.com",
		client: &http.Client{Timeout: 200 * time.Millisecond},
	}

	done := make(chan error, 1)
	go func() {
		_, err := ts.Token()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a timeout error from a server that never responds")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Token() did not return within 5s; the timeout is not being enforced")
	}
}
