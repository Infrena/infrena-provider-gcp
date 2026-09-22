package gcprov

import (
	"context"
	"testing"
	"time"

	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
)

// TestTheLimiterSurvivesBetweenRequests. A limiter created per request
// measures nothing: the bucket's whole purpose is to carry rate information
// between calls, which is why the client cache exists.
func TestTheLimiterSurvivesBetweenRequests(t *testing.T) {
	gcptest.Isolate(t)
	c := NewClient(staticToken(), "https://example.invalid/", ClientOptions{QPS: 2})
	first := c.limiterFor("p", "compute")
	second := c.limiterFor("p", "compute")
	if first != second {
		t.Error("a new limiter per call, so the rate is never actually limited")
	}
	if c.limiterFor("p", "storage") == first {
		t.Error("one limiter across APIs; GCP quota is per API per project")
	}
	if c.limiterFor("q", "compute") == first {
		t.Error("one limiter across projects; GCP quota is per API per project")
	}
}

// TestBackoffDelayIsBoundedAndJittered. A fixed attempt number must never
// exceed the ceiling exponential backoff would set for it, and repeated
// calls must not all return the same value -- a backoff with no jitter at
// all defeats the point of adding it.
func TestBackoffDelayIsBoundedAndJittered(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		d := backoffDelay(1, 0) // ceiling: backoffBase*2 = 500ms
		if d < 0 || d > 2*backoffBase {
			t.Fatalf("backoffDelay(1, 0) = %v, out of bounds", d)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Error("backoffDelay(1, 0) returned the same value every time; no jitter")
	}
}

// TestBackoffDelayIsCapped. A long run of failures must never wait longer
// than backoffCap between tries, however large the attempt number grows.
func TestBackoffDelayIsCapped(t *testing.T) {
	if d := backoffDelay(20, 0); d > backoffCap {
		t.Errorf("backoffDelay(20, 0) = %v, exceeds the cap %v", d, backoffCap)
	}
}

// TestBackoffDelayHonoursRetryAfter. GCP naming a wait is more informed than
// this client's own guess, and must simply be used, not blended with jitter
// or the exponential schedule.
func TestBackoffDelayHonoursRetryAfter(t *testing.T) {
	if d := backoffDelay(5, 7*time.Second); d != 7*time.Second {
		t.Errorf("backoffDelay(5, 7s) = %v, want exactly 7s", d)
	}
}

// TestBackoffDelayScheduleIsDeterministicUnderAFixedJitterSource. jitter is
// swappable specifically so the schedule can be asserted exactly, rather
// than only "it returned something in range" -- this is that assertion.
func TestBackoffDelayScheduleIsDeterministicUnderAFixedJitterSource(t *testing.T) {
	old := jitter
	defer func() { jitter = old }()
	jitter = func(n int64) int64 { return n - 1 } // always the ceiling itself

	if d := backoffDelay(0, 0); d != backoffBase {
		t.Errorf("backoffDelay(0, 0) = %v, want the full ceiling %v under a fixed jitter source", d, backoffBase)
	}
	if d := backoffDelay(1, 0); d != 2*backoffBase {
		t.Errorf("backoffDelay(1, 0) = %v, want %v", d, 2*backoffBase)
	}
}

// TestLimiterWaitConsumesTokensAtTheConfiguredRate uses a fake clock so the
// test asserts on the limiter's own accounting rather than on real elapsed
// time, which would make the test slow or flaky.
func TestLimiterWaitConsumesTokensAtTheConfiguredRate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	l := newLimiter(1, clock) // 1 qps: starts with one token available

	ctx := context.Background()
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("first Wait (bucket starts full): %v", err)
	}

	wait, ok := l.take()
	if ok {
		t.Fatal("a second immediate call found a token; the bucket should be empty")
	}
	if wait <= 0 || wait > time.Second {
		t.Errorf("wait = %v, want a positive duration up to 1s", wait)
	}

	now = now.Add(time.Second)
	if _, ok := l.take(); !ok {
		t.Error("no token available a full second later at 1 qps")
	}
}

// TestLimiterWaitReturnsWhenContextEnds. A caller must never be stuck behind
// a rate limiter forever once its context is done -- cancellation always
// wins over waiting for a token.
func TestLimiterWaitReturnsWhenContextEnds(t *testing.T) {
	l := newLimiter(1, nil)
	// Drain the initial token so the next Wait actually has to block.
	if _, ok := l.take(); !ok {
		t.Fatal("the bucket did not start with a token")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Wait(ctx); err == nil {
		t.Error("Wait did not report the context ending")
	}
}

// TestUnlimitedQPSNeverWaits. QPS 0 means unlimited, relying on GCP's own
// 429s plus retry/backoff rather than pre-emptive shaping.
func TestUnlimitedQPSNeverWaits(t *testing.T) {
	l := newLimiter(0, nil)
	for i := 0; i < 1000; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}
