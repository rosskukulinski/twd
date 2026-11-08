package mgmt

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// Clock is injectable for hermetic tests.
type Clock interface {
	Now() time.Time
	// After is like time.After.
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Throttle is a per-tenant registration token bucket (RFC §4.9, ADR-017):
// `perMinute` tokens per minute, burst = max(1, perMinute/10). On a 429/5xx
// from the coordination server, Backoff halves the effective rate (down to
// 1/min) and pauses minting until the server's Retry-After (or a jittered
// exponential delay) passes; every success adds back 1/min of rate (AIMD).
type Throttle struct {
	mu        sync.Mutex
	clock     Clock
	limit     float64 // configured tokens/minute
	rate      float64 // effective tokens/minute (≤ limit)
	burst     float64
	tokens    float64
	last      time.Time
	pauseTill time.Time
	failures  int
	jitter    func() float64 // [0,1)
}

// NewThrottle returns a full bucket.
func NewThrottle(perMinute int, clock Clock) *Throttle {
	if perMinute <= 0 {
		perMinute = DefaultRegistrationsPerMinute
	}
	if clock == nil {
		clock = realClock{}
	}
	b := float64(perMinute) / 10
	if b < 1 {
		b = 1
	}
	return &Throttle{clock: clock, limit: float64(perMinute), rate: float64(perMinute), burst: b, tokens: b,
		last: clock.Now(), jitter: rand.Float64}
}

func (t *Throttle) refill(now time.Time) {
	if el := now.Sub(t.last); el > 0 {
		t.tokens += el.Minutes() * t.rate
		if t.tokens > t.burst {
			t.tokens = t.burst
		}
	}
	t.last = now
}

// Reserve takes a token if one is available and returns 0, or returns how
// long to wait before trying again.
func (t *Throttle) Reserve() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock.Now()
	if now.Before(t.pauseTill) {
		return t.pauseTill.Sub(now)
	}
	t.refill(now)
	if t.tokens >= 1 {
		t.tokens--
		return 0
	}
	need := (1 - t.tokens) / t.rate // minutes
	return time.Duration(need * float64(time.Minute))
}

// Wait blocks until a token is available or ctx is done.
func (t *Throttle) Wait(ctx context.Context) error {
	for {
		d := t.Reserve()
		if d == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.clock.After(d):
		}
	}
}

// Backoff records a rate-limit / server error. retryAfter (0 if absent)
// is the server's hint.
func (t *Throttle) Backoff(retryAfter time.Duration) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failures++
	t.rate /= 2
	if t.rate < 1 {
		t.rate = 1
	}
	d := retryAfter
	if d <= 0 {
		exp := min(time.Second<<min(t.failures-1, 6), time.Minute) // 1s, 2s, … 60s
		d = exp/2 + time.Duration(t.jitter()*float64(exp/2))
	}
	t.pauseTill = t.clock.Now().Add(d)
	t.tokens = 0
	return d
}

// Success records a successful registration (additive increase).
func (t *Throttle) Success() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failures = 0
	t.rate = min(t.limit, t.rate+1)
}

// Rate returns the effective tokens/minute.
func (t *Throttle) Rate() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rate
}

// SetLimit changes the configured rate (tenant update).
func (t *Throttle) SetLimit(perMinute int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.limit = float64(perMinute)
	t.rate = min(t.rate, t.limit)
	t.burst = max(1, t.limit/10)
}
