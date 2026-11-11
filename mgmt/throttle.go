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

func (realClock) Now() time.Time                         { return wallNowAt(time.Now()) }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// ThrottleState is a token bucket's mutable state. With a shared store it is
// one row per tenant that every mgmt replica reads and writes atomically
// (mm-loop F7, H03), so the tenant's registration rate holds across replicas.
type ThrottleState struct {
	Tokens    float64 // available tokens
	Rate      float64 // effective tokens/minute (AIMD, ≤ the limit)
	Last      int64   // unix ms of the last refill (0 = new bucket)
	PauseTill int64   // unix ms: no tokens before this (after a 429/5xx)
	Failures  int     // consecutive backoffs (exponential delay)
}

// ThrottleBackend applies fn to a tenant's shared throttle state atomically
// (read, fn, write in one transaction).
type ThrottleBackend func(ctx context.Context, fn func(*ThrottleState)) error

// Throttle is a per-tenant registration token bucket (RFC §4.9, ADR-017):
// `perMinute` tokens per minute, burst = max(1, perMinute/10). On a 429/5xx
// from the coordination server, Backoff halves the effective rate (down to
// 1/min) and pauses minting until the server's Retry-After (or a jittered
// exponential delay) passes; every success adds back 1/min of rate (AIMD).
//
// Without a backend the state is in memory (one replica). With one, the
// state is shared by every replica of the store, so N replicas together mint
// at most the tenant's rate.
type Throttle struct {
	mu      sync.Mutex
	clock   Clock
	limit   float64 // configured tokens/minute
	burst   float64
	st      ThrottleState   // in-memory state (no backend)
	backend ThrottleBackend // shared state, or nil
	jitter  func() float64  // [0,1)
}

// NewThrottle returns a full in-memory bucket.
func NewThrottle(perMinute int, clock Clock) *Throttle {
	if perMinute <= 0 {
		perMinute = DefaultRegistrationsPerMinute
	}
	if clock == nil {
		clock = realClock{}
	}
	t := &Throttle{clock: clock, limit: float64(perMinute), burst: max(1, float64(perMinute)/10), jitter: rand.Float64}
	t.st = ThrottleState{Tokens: t.burst, Rate: t.limit, Last: clock.Now().UnixMilli()}
	return t
}

// NewSharedThrottle returns a bucket whose state lives in backend.
func NewSharedThrottle(perMinute int, clock Clock, backend ThrottleBackend) *Throttle {
	t := NewThrottle(perMinute, clock)
	t.backend = backend
	return t
}

func (t *Throttle) cfg() (limit, burst float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.limit, t.burst
}

// sharedRetry is how long Reserve asks callers to wait when the shared
// state can't be read or written (fail closed: no token without the store).
const sharedRetry = time.Second

// apply runs fn on the bucket state: in memory, or atomically in the backend.
func (t *Throttle) apply(fn func(s *ThrottleState)) error {
	t.mu.Lock()
	limit, burst, backend := t.limit, t.burst, t.backend
	t.mu.Unlock()
	init := func(s *ThrottleState, now time.Time) {
		if s.Last == 0 { // a new shared bucket starts full
			*s = ThrottleState{Tokens: burst, Rate: limit, Last: now.UnixMilli()}
		}
		s.Rate = min(s.Rate, limit)
		if s.Rate <= 0 {
			s.Rate = limit
		}
		s.Tokens = min(s.Tokens, burst)
	}
	if backend == nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		init(&t.st, t.clock.Now())
		fn(&t.st)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return backend(ctx, func(s *ThrottleState) {
		init(s, t.clock.Now())
		fn(s)
	})
}

func refill(s *ThrottleState, now time.Time, burst float64) {
	if el := now.Sub(time.UnixMilli(s.Last)); el > 0 {
		s.Tokens = min(burst, s.Tokens+el.Minutes()*s.Rate)
	}
	s.Last = now.UnixMilli()
}

// Reserve takes a token if one is available and returns 0, or returns how
// long to wait before trying again.
func (t *Throttle) Reserve() time.Duration {
	var wait time.Duration
	_, burst := t.cfg()
	err := t.apply(func(s *ThrottleState) {
		now := t.clock.Now()
		if pt := time.UnixMilli(s.PauseTill); now.Before(pt) {
			wait = pt.Sub(now)
			return
		}
		refill(s, now, burst)
		if s.Tokens >= 1 {
			s.Tokens--
			wait = 0
			return
		}
		wait = time.Duration((1 - s.Tokens) / s.Rate * float64(time.Minute))
	})
	if err != nil {
		return sharedRetry
	}
	return wait
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
	var d time.Duration
	err := t.apply(func(s *ThrottleState) {
		s.Failures++
		s.Rate = max(1, s.Rate/2)
		d = retryAfter
		if d <= 0 {
			exp := min(time.Second<<min(s.Failures-1, 6), time.Minute) // 1s, 2s, … 60s
			d = exp/2 + time.Duration(t.jitter()*float64(exp/2))
		}
		s.PauseTill = t.clock.Now().Add(d).UnixMilli()
		s.Tokens = 0
	})
	if err != nil && d == 0 {
		d = sharedRetry
	}
	return d
}

// Success records a successful registration (additive increase).
func (t *Throttle) Success() {
	limit, _ := t.cfg()
	t.apply(func(s *ThrottleState) {
		s.Failures = 0
		s.Rate = min(limit, s.Rate+1)
	})
}

// Rate returns the effective tokens/minute.
func (t *Throttle) Rate() float64 {
	var r float64
	if err := t.apply(func(s *ThrottleState) { r = s.Rate }); err != nil {
		limit, _ := t.cfg()
		return limit
	}
	return r
}

// SetLimit changes the configured rate (tenant update).
func (t *Throttle) SetLimit(perMinute int) {
	t.mu.Lock()
	t.limit = float64(perMinute)
	t.burst = max(1, t.limit/10)
	t.mu.Unlock()
	t.apply(func(*ThrottleState) {}) // init clamps rate and tokens to the new limit
}
