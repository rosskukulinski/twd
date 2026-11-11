//go:build e2emultimgmt

package multimgmt

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMM_H04_SpawnBudgetRace probes H04: spawn budgets (maxLive live
// descendants per root) are checked with CountLive under a per-replica
// mutex (mgmt/service.go tenantLock, checkSpawn), so concurrent spawns
// through different replicas can each see room and all insert.
// Invariant I5: live descendants of a root never exceed maxLive.
func TestMM_H04_SpawnBudgetRace(t *testing.T) {
	const (
		replicas = 3
		maxLive  = 5
		spawns   = 40
	)
	r := newRun(t, "H04")
	c := newCluster(t, r, replicas)
	c.jsonc = strings.Replace(twdJSONC, `"maxLive": 64`, fmt.Sprintf(`"maxLive": %d`, maxLive), 1)
	db := c.db()
	for i := range replicas {
		c.start(i)
		if _, err := c.waitReady(i, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	s := c.setupTenant(0, 6000)
	stop := make(chan struct{})
	defer close(stop)
	c.heartbeat(s, stop)
	// Realistic database latency widens the window between CountLive and
	// the insert (on loopback Postgres it is too narrow to hit).
	for i := range replicas {
		c.proxy(i).set("delay", pgDelay())
	}
	var root struct{ ObjectID string }
	c.must(0, "h1", "POST", "/v1/objects", map[string]any{"tenantID": s.tenantID, "namespace": "SupportAgent", "objectName": "root"}, &root)

	var mu sync.Mutex
	codes := map[string]int{}
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for k := range spawns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			st, b, err := c.callAs(k%replicas, "h1", "POST", "/v1/objects", map[string]any{"tenantID": s.tenantID,
				"namespace": "LookupAgent", "objectName": fmt.Sprintf("c%02d", k), "parent": map[string]any{"objectID": root.ObjectID}})
			var e struct{ Code string }
			json.Unmarshal(b, &e)
			key := fmt.Sprintf("%d %s", st, e.Code)
			if err != nil {
				key = "err " + err.Error()
			}
			mu.Lock()
			codes[key]++
			mu.Unlock()
		}()
	}
	close(gate)
	wg.Wait()
	var live int
	db.QueryRow("SELECT COUNT(*) FROM objects WHERE root_id=$1 AND id<>$1 AND status NOT IN ('killed','failed')", root.ObjectID).Scan(&live)
	t.Logf("pg delay %v; responses: %v; live descendants of root = %d (maxLive %d)", pgDelay(), codes, live, maxLive)
	if live > maxLive {
		t.Errorf("I5 violated: root has %d live descendants, maxLive is %d (%d concurrent spawns over %d replicas)", live, maxLive, spawns, replicas)
	}
}

// pgDelay is the per-read Postgres latency for H04 (TWD_MM_PG_DELAY, default 5ms).
func pgDelay() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("TWD_MM_PG_DELAY")); err == nil {
		return d
	}
	return 5 * time.Millisecond
}
