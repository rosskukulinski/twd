package mgmt

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock advances only when told; After fires when the clock passes it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{c.now.Add(d), ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
}

func TestThrottleRate(t *testing.T) {
	c := newFakeClock()
	th := NewThrottle(200, c)
	// Burst is 20; then 200/min = one token per 300 ms.
	got := 0
	for th.Reserve() == 0 {
		got++
	}
	if got != 20 {
		t.Fatalf("burst %d, want 20", got)
	}
	if d := th.Reserve(); d <= 0 || d > 300*time.Millisecond {
		t.Fatalf("wait %v", d)
	}
	// Over one simulated minute exactly 200 more tokens are granted.
	granted := 0
	for range 600 { // 100 ms steps
		c.Advance(100 * time.Millisecond)
		for th.Reserve() == 0 {
			granted++
		}
	}
	if granted < 199 || granted > 201 {
		t.Fatalf("granted %d in a minute, want ~200", granted)
	}
}

func TestThrottleDefaultAndBackoff(t *testing.T) {
	c := newFakeClock()
	th := NewThrottle(0, c)
	if th.Rate() != DefaultRegistrationsPerMinute {
		t.Fatal(th.Rate())
	}
	th.jitter = func() float64 { return 0 }
	// A 429 with Retry-After pauses minting for exactly that long and halves the rate.
	if d := th.Backoff(7 * time.Second); d != 7*time.Second {
		t.Fatal(d)
	}
	if th.Rate() != 100 {
		t.Fatalf("rate after backoff %v", th.Rate())
	}
	if d := th.Reserve(); d != 7*time.Second {
		t.Fatalf("paused: %v", d)
	}
	c.Advance(7 * time.Second)
	if d := th.Reserve(); d != 0 {
		t.Fatalf("after pause: %v", d)
	}
	// Without Retry-After: exponential (jitter 0 → half of 1s, 2s, 4s…).
	if d := th.Backoff(0); d != time.Second {
		t.Fatalf("second failure %v", d)
	}
	if d := th.Backoff(0); d != 2*time.Second {
		t.Fatalf("third failure %v", d)
	}
	for range 20 {
		th.Backoff(0)
	}
	if th.Rate() != 1 {
		t.Fatalf("rate floor %v", th.Rate())
	}
	// Additive increase on success, capped at the limit.
	for range 500 {
		th.Success()
	}
	if th.Rate() != DefaultRegistrationsPerMinute {
		t.Fatal(th.Rate())
	}
}

func TestThrottleWait(t *testing.T) {
	c := newFakeClock()
	th := NewThrottle(60, c) // burst 6, then 1/s
	for range 6 {
		if err := th.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- th.Wait(context.Background()) }()
	select {
	case <-done:
		t.Fatal("Wait returned without a token")
	case <-time.After(20 * time.Millisecond):
	}
	for i := 0; i < 50; i++ {
		c.Advance(100 * time.Millisecond)
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatal("Wait never returned")
}

func TestThrottleWaitCancel(t *testing.T) {
	th := NewThrottle(1, newFakeClock())
	th.Reserve()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := th.Wait(ctx); err == nil {
		t.Fatal("cancelled wait succeeded")
	}
}
