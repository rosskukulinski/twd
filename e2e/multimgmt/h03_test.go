//go:build e2emultimgmt

package multimgmt

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// TestMM_H03_ThrottlePerReplica probes H03: the per-tenant registration
// throttle (ADR-017; burst = max(1, rpm/10), then rpm/60 per second) is an
// in-memory token bucket per replica, so N replicas may mint N× the limit.
// Invariant I4: across all replicas, keys created for a tenant in any 10 s
// window ≤ burst + 10 s × rate (+10% tolerance).
func TestMM_H03_ThrottlePerReplica(t *testing.T) {
	const (
		replicas = 3
		rpm      = 60 // 1/s, burst 6
		nObjects = 45
	)
	r := newRun(t, "H03")
	c := newCluster(t, r, replicas)
	db := c.db()
	for i := range replicas {
		c.start(i)
		if _, err := c.waitReady(i, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	s := c.setupTenant(0, rpm)
	stop := make(chan struct{})
	defer close(stop)
	c.heartbeat(s, stop)

	// Create every object at once, spread over the replicas.
	var wg sync.WaitGroup
	for k := range nObjects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.must(k%replicas, "h1", "POST", "/v1/objects", map[string]any{"tenantID": s.tenantID, "namespace": "SupportAgent",
				"objectName": fmt.Sprintf("o%03d", k)}, nil)
		}()
	}
	wg.Wait()
	start := time.Now()
	deadline := start.Add(90 * time.Second)
	for {
		st, _ := objectStatuses(db)
		left := 0
		for _, v := range st {
			if v == "pending" {
				left++
			}
		}
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d still pending after 90 s", left)
		}
		time.Sleep(200 * time.Millisecond)
	}
	elapsed := time.Since(start)

	c.ts.mu.Lock()
	times := append([]time.Time(nil), c.ts.createTimes...)
	c.ts.mu.Unlock()
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	maxWin := 0
	for i := range times {
		n := sort.Search(len(times), func(j int) bool { return times[j].After(times[i].Add(10 * time.Second)) }) - i
		maxWin = max(maxWin, n)
	}
	limit := 6 + 10*rpm/60
	rate := float64(len(times)) / times[len(times)-1].Sub(times[0]).Seconds()
	t.Logf("%d keys in %v (%.2f/s overall); max in any 10 s window = %d (limit %d); single-replica expectation ≈ %v",
		len(times), elapsed.Round(time.Millisecond), rate, maxWin, limit, time.Duration(nObjects-6)*time.Second)
	if float64(maxWin) > float64(limit)*1.1 {
		t.Errorf("I4 violated: %d registrations in 10 s for a tenant limited to %d/min (burst 6): ≈%.1f× the limit across %d replicas",
			maxWin, rpm, float64(maxWin)/float64(limit), replicas)
	}
}
