//go:build e2emultimgmt

package multimgmt

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// TestMM_H16_TenantQuotaRace probes H16: the tenant's maxObjects quota is
// checked (CountLive) before the insert, serialised only per replica, so
// concurrent creates through different replicas can overshoot it.
// Invariant: live objects of a tenant ≤ maxObjects.
func TestMM_H16_TenantQuotaRace(t *testing.T) {
	const replicas, quota, burst = 3, 10, 30
	r := newRun(t, "H16")
	c := newCluster(t, r, replicas)
	db := c.db()
	for i := range replicas {
		c.start(i)
		if _, err := c.waitReady(i, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		c.proxy(i).set("delay", 5*time.Millisecond)
	}
	keyFile := r.dir + "/api.env"
	writeAPIKey(t, keyFile)
	var tn struct{ TenantID string }
	c.must(0, "", "POST", "/v1/tenants", map[string]any{"tailnet": "q.ts.net", "mintMode": "apikey", "apiKeyFile": keyFile,
		"registrationsPerMinute": 6000, "maxObjects": quota}, &tn)
	c.must(0, "h1", "PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 10000, "tiers": []string{"process"}}, nil)
	var dep struct{ Epoch int64 }
	c.must(0, "", "POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "w",
		"bundle": makeBundle(map[string]string{"twd.jsonc": twdJSONC, "dist/index.js": "export default {}"})}, &dep)
	stop := make(chan struct{})
	defer close(stop)
	c.heartbeat(setup{tenantID: tn.TenantID, workload: "w", epoch: dep.Epoch}, stop)
	// The deploy's workload entry object may count; fill up to quota-1 live.
	var live int
	count := func() int {
		db.QueryRow("SELECT COUNT(*) FROM objects WHERE tenant_id=$1 AND status NOT IN ('killed','failed')", tn.TenantID).Scan(&live)
		return live
	}
	for k := 0; count() < quota-1; k++ {
		c.must(0, "h1", "POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "namespace": "SupportAgent", "objectName": fmt.Sprintf("pre%02d", k)}, nil)
	}
	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for k := range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			st, _, _ := c.callAs(k%replicas, "h1", "POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "namespace": "SupportAgent",
				"objectName": fmt.Sprintf("b%02d", k)})
			mu.Lock()
			codes[st]++
			mu.Unlock()
		}()
	}
	close(gate)
	wg.Wait()
	got := count()
	summary := fmt.Sprintf("quota %d; responses %v; live objects %d", quota, codes, got)
	t.Log(summary)
	os.WriteFile(r.dir+"/report.txt", []byte(summary+"\n"), 0o600)
	if got > quota {
		t.Errorf("tenant quota exceeded: %d live objects, maxObjects %d", got, quota)
	}
}
