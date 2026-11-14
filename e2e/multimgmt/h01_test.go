//go:build e2emultimgmt

package multimgmt

import (
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMM_H01_StartupAdvisoryLock probes H01: the migration advisory lock is
// taken and released through a pooled *sql.DB (mgmt/sqlstore.go:153), so it
// could leak on an idle pooled connection and block every later replica's
// migrate. Invariant I8: a replica (re)starting while others run reaches
// ready in < 10 s, and no session holds lock 7464 once everyone is ready.
func TestMM_H01_StartupAdvisoryLock(t *testing.T) {
	r := newRun(t, "H01")
	c := newCluster(t, r, 3)
	db := c.db()
	const budget = 10 * time.Second

	check := func(step string) {
		t.Helper()
		h, err := advisoryHolders(db)
		if err != nil {
			t.Fatalf("%s: pg_locks: %v", step, err)
		}
		if len(h) > 0 {
			t.Errorf("%s: advisory lock 7464 still held after every replica is ready: %v", step, h)
		} else {
			t.Logf("%s: lock 7464 free", step)
		}
	}
	ready := func(step string, i int) {
		t.Helper()
		d, err := c.waitReady(i, budget)
		if err != nil {
			h, _ := advisoryHolders(db)
			t.Fatalf("%s: %v (lock 7464 holders: %v)", step, err, h)
		}
		t.Logf("%s: r%d ready in %v", step, i, d.Round(time.Millisecond))
	}

	// 1. Cold start of all three at once on an empty database (S7a).
	var wg sync.WaitGroup
	for i := range 3 {
		c.start(i)
	}
	errs := make([]error, 3)
	durs := make([]time.Duration, 3)
	for i := range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); durs[i], errs[i] = c.waitReady(i, budget) }()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			h, _ := advisoryHolders(db)
			t.Fatalf("cold start: %v (lock 7464 holders: %v)", err, h)
		}
		t.Logf("cold start: r%d ready in %v", i, durs[i].Round(time.Millisecond))
	}
	check("after cold start")

	// 2. Graceful restart of r0 while r1 and r2 serve.
	c.stop(0, syscall.SIGTERM)
	c.start(0)
	ready("restart r0 (SIGTERM)", 0)
	check("after r0 restart")

	// 3. SIGKILL r1, restart it.
	c.stop(1, syscall.SIGKILL)
	c.start(1)
	ready("restart r1 (SIGKILL)", 1)
	check("after r1 restart")

	// 4. Ten quick restarts of r2 (each runs migrate against a live cluster).
	for k := range 10 {
		c.stop(2, syscall.SIGKILL)
		c.start(2)
		ready(fmt.Sprintf("restart r2 #%d", k), 2)
	}
	check("after 10 r2 restarts")

	// 5. All still serve.
	for i := range 3 {
		if st, b, err := c.call(i, "GET", "/v1/status", nil); err != nil || st != 200 {
			t.Errorf("r%d status: %d %v %.200s", i, st, err, b)
		}
	}
}
