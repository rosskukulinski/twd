//go:build e2emultimgmt

package multimgmt

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestMM_H14_ConcurrentSameNameCreate probes H14: CreateObject is
// idempotent by name (an existing live object is returned), but the
// lookup-then-insert is serialised only per replica (tenantLock) and each
// replica derives object IDs from its own id.secret. Concurrent creates of
// one name through different replicas: one inserts, the others hit the
// UNIQUE (tenant, workload, namespace, name) constraint.
// Invariant: every caller gets 2xx with the same object ID.
func TestMM_H14_ConcurrentSameNameCreate(t *testing.T) {
	const replicas, names = 3, 10
	r := newRun(t, "H14")
	c := newCluster(t, r, replicas)
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
	splits := 0
	for n := range names {
		ids := make([]string, replicas)
		sts := make([]int, replicas)
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for i := range replicas {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				st, b, _ := c.callAs(i, "h1", "POST", "/v1/objects", map[string]any{"tenantID": s.tenantID, "namespace": "SupportAgent",
					"objectName": fmt.Sprintf("same%02d", n)})
				var o struct{ ObjectID, Code string }
				json.Unmarshal(b, &o)
				ids[i], sts[i] = o.ObjectID, st
				if st/100 != 2 {
					ids[i] = o.Code
				}
			}()
		}
		close(gate)
		wg.Wait()
		for i := 1; i < replicas; i++ {
			if ids[i] != ids[0] || sts[i]/100 != 2 || sts[0]/100 != 2 {
				splits++
				t.Logf("same%02d: statuses %v ids %v", n, sts, ids)
				break
			}
		}
	}
	t.Logf("%d/%d names got a non-2xx or differing IDs across concurrent creates", splits, names)
	if splits > 0 {
		t.Errorf("idempotent create broken across replicas for %d/%d names", splits, names)
	}
}
