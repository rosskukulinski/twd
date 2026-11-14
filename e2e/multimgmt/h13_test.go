//go:build e2emultimgmt

package multimgmt

import (
	"encoding/json"
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMM_H13_OBORootKeyDiverges probes H13: a tenant's OBO root key is
// created on first use with Get → (not found) → Put → re-Get, and cached per
// replica. Two replicas doing that concurrently can each cache a different
// key (the last Put wins in custody, but the first replica keeps its own).
// Invariant: every replica reports (and signs with) the same OBO key.
// (a) normal create: the create response makes the key on the creating
// replica first. (b) upgrade-like: the key is absent and replicas with
// cold caches hit the tenant concurrently.
func TestMM_H13_OBORootKeyDiverges(t *testing.T) {
	const replicas, rounds = 3, 8
	r := newRun(t, "H13")
	c := newCluster(t, r, replicas)
	for i := range replicas {
		c.start(i)
		if _, err := c.waitReady(i, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	keyFile := r.dir + "/api.env"
	writeAPIKey(t, keyFile)
	pubOn := func(i int, tid string) string {
		var v struct{ OBOPublicKey string }
		_, b, _ := c.call(i, "GET", "/v1/tenants/"+tid, nil)
		json.Unmarshal(b, &v)
		return v.OBOPublicKey
	}
	diverged := func(tid string) (bool, []string) {
		keys := make([]string, replicas)
		for i := range replicas {
			keys[i] = pubOn(i, tid)
		}
		for i := 1; i < replicas; i++ {
			if keys[i] != keys[0] {
				return true, keys
			}
		}
		return false, keys
	}
	// (a)
	var tids []string
	badA := 0
	for k := range rounds {
		var tn struct{ TenantID string }
		c.must(k%replicas, "", "POST", "/v1/tenants", map[string]any{"tailnet": fmt.Sprintf("a%d.ts.net", k), "mintMode": "apikey", "apiKeyFile": keyFile}, &tn)
		tids = append(tids, tn.TenantID)
		if d, keys := diverged(tn.TenantID); d {
			badA++
			t.Logf("(a) %s: replicas disagree: %v", tn.TenantID, keys)
		}
	}
	// (b) drop the keys from custody, restart every replica (cold caches),
	// then hit each tenant on all replicas at once.
	// Truly cold: no choice row and no custody entry (legacy or keyed).
	db := c.db()
	for _, tid := range tids {
		db.Exec("DELETE FROM secret_choice WHERE name=$1", "obo/"+tid)
		setecDeletePrefix("twd/" + tid + "/obo-root")
	}
	for i := range replicas {
		c.stop(i, syscall.SIGTERM)
		c.start(i)
		c.waitReady(i, 10*time.Second)
	}
	badB := 0
	for _, tid := range tids {
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for i := range replicas {
			wg.Add(1)
			go func() { defer wg.Done(); <-gate; pubOn(i, tid) }()
		}
		close(gate)
		wg.Wait()
		if d, keys := diverged(tid); d {
			badB++
			t.Logf("(b) %s: replicas disagree: %v", tid, keys)
		}
	}
	t.Logf("diverged: (a) normal create %d/%d, (b) cold concurrent first use %d/%d", badA, rounds, badB, rounds)
	if badA+badB > 0 {
		t.Errorf("OBO root keys diverged across replicas: (a) %d/%d, (b) %d/%d tenants", badA, rounds, badB, rounds)
	}
}
