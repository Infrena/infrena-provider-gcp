package gcprov

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"time"
)

// backoffBase and backoffCap bound the exponential backoff schedule between
// retries: the delay before the first retry is uniformly random in
// [0, backoffBase], doubling the ceiling each further attempt, capped at
// backoffCap so a long run of failures never waits more than that between
// tries.
const (
	backoffBase = 250 * time.Millisecond
	backoffCap  = 30 * time.Second
)

// jitterMu guards jitterFn: it is swapped by plain assignment from test code
// (backoff_test.go's withJitterForTest), and without a lock that is
// unsynchronized global mutable state shared with every production
// backoffDelay call. Nothing in this package calls t.Parallel() today, so an
// unguarded swap is latent rather than active -- but the moment a parallel
// test lands, a swap racing a concurrent Do would surface under -race as
// something that looks like flakiness rather than like this.
var jitterMu sync.Mutex

// jitterFn is the randomness source backoffDelay draws from, swappable
// (under jitterMu) so a test can assert the resulting schedule
// deterministically rather than only "it returned something in range".
// Production leaves it as rand.Int63n, which since Go 1.20 draws from an
// automatically-seeded global source.
var jitterFn = rand.Int63n

// jitter reads jitterFn under lock and calls it. A function, not a direct
// read of jitterFn, so every call -- including a concurrent one -- observes
// either the old source or the new one, never a torn read of the func value.
func jitter(n int64) int64 {
	jitterMu.Lock()
	f := jitterFn
	jitterMu.Unlock()
	return f(n)
}

// backoffDelay returns how long to wait before retry attempt n (0-based: the
// wait before the FIRST retry is backoffDelay(0, 0)).
//
// ra, when non-zero, is honoured directly: it is the previous attempt's own
// APIError.RetryAfter, and GCP naming a wait is more informed than a guess
// this client makes on its own. Otherwise the wait is full jitter -- a
// uniformly random duration between 0 and min(backoffCap, backoffBase*2^n).
// Full jitter, not capped exponential alone, is what keeps many clients that
// all hit a shared failure (a regional blip, a deploy) from retrying in
// lockstep and re-creating the very spike that caused it.
func backoffDelay(n int, ra time.Duration) time.Duration {
	if ra > 0 {
		return ra
	}
	ceiling := float64(backoffBase) * math.Pow(2, float64(n))
	if ceiling > float64(backoffCap) {
		ceiling = float64(backoffCap)
	}
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(jitter(int64(ceiling) + 1))
}

// limiter is a hand-rolled token bucket: qps tokens accrue per second, up to
// a burst of one second's worth, and Wait blocks the caller until one is
// available or ctx ends. Hand-rolled rather than golang.org/x/time/rate
// because this package's dependency budget is net/http plus
// golang.org/x/oauth2 and nothing else (task-11 brief) -- and because this is
// the one piece of rate-limiting logic this provider owns rather than an
// SDK owning it, per spec §5.1/§5.6, it is code this repository must test
// directly.
type limiter struct {
	mu     sync.Mutex
	qps    float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

// newLimiter starts a bucket full (qps tokens available immediately), the
// usual token-bucket convention: the first burst of calls up to the rate
// limit is not artificially delayed just because the bucket was just built.
func newLimiter(qps float64, now func() time.Time) *limiter {
	if now == nil {
		now = time.Now
	}
	return &limiter{qps: qps, tokens: qps, last: now(), now: now}
}

// Wait blocks until one token is available, or ctx is done. qps <= 0 means
// unlimited: Wait returns immediately, relying on GCP's own 429s plus this
// client's retry/backoff instead of shaping traffic pre-emptively.
func (l *limiter) Wait(ctx context.Context) error {
	if l.qps <= 0 {
		return nil
	}
	for {
		wait, ok := l.take()
		if ok {
			return nil
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// take refills the bucket for the elapsed time and, if a token is available,
// consumes it and reports ok. Otherwise it reports how long the caller must
// wait for one to accrue.
func (l *limiter) take() (wait time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	elapsed := now.Sub(l.last).Seconds()
	l.last = now
	l.tokens += elapsed * l.qps
	if l.tokens > l.qps {
		l.tokens = l.qps
	}

	if l.tokens >= 1 {
		l.tokens--
		return 0, true
	}
	needed := 1 - l.tokens
	return time.Duration(needed / l.qps * float64(time.Second)), false
}
