package mgmt

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestStoreSharedThrottle (mm-loop F7, H03): two replicas' throttles for one
// tenant share the bucket through the store, so together they get one burst
// and one rate; a backoff seen by one pauses the other. On SQLite and (with
// TWD_TEST_POSTGRES_DSN, via make test-pg) Postgres.
func TestStoreSharedThrottle(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		clk := newFakeClock()
		backend := func(ctx context.Context, fn func(*ThrottleState)) error {
			return s.UpdateThrottle(ctx, "tn-"+t.Name(), fn)
		}
		a := NewSharedThrottle(60, clk, backend) // burst 6, 1/s
		b := NewSharedThrottle(60, clk, backend)
		a.jitter = func() float64 { return 0 }

		// Frozen clock: 20 concurrent reservations from each replica get
		// exactly one burst between them.
		var mu sync.Mutex
		granted := 0
		var wg sync.WaitGroup
		for i := range 40 {
			th := a
			if i%2 == 1 {
				th = b
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if th.Reserve() == 0 {
					mu.Lock()
					granted++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if granted != 6 {
			t.Fatalf("frozen clock: %d tokens granted across two replicas, want the burst (6)", granted)
		}
		// One second later: one more token, for whichever replica asks first.
		clk.Advance(time.Second)
		if a.Reserve() != 0 {
			t.Fatal("after 1 s replica A should get a token")
		}
		if d := b.Reserve(); d == 0 {
			t.Fatal("replica B must not get a second token in the same second")
		}
		// A backoff on A (a 429) pauses B too, and halves the shared rate.
		clk.Advance(10 * time.Second)
		if d := a.Backoff(5 * time.Second); d != 5*time.Second {
			t.Fatalf("backoff = %v", d)
		}
		if d := b.Reserve(); d < 4*time.Second || d > 5*time.Second {
			t.Fatalf("replica B during A's backoff: wait %v, want ≈5 s", d)
		}
		if r := b.Rate(); r != 30 {
			t.Fatalf("shared rate after one backoff = %v/min, want 30", r)
		}
		clk.Advance(6 * time.Second)
		b.Success()
		if r := a.Rate(); r != 31 {
			t.Fatalf("A sees B's success: rate %v/min, want 31", r)
		}
	})
}
