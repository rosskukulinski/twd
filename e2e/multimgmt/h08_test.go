//go:build e2emultimgmt

package multimgmt

import (
	"encoding/json"
	"testing"
	"time"
)

// TestMM_H08_DeploymentStatusPerReplica probes H08: the host-reported
// deployment status (heartbeat "deployments") is kept in memory on the
// replica that received the heartbeat (m.depStatus), so other replicas
// report "assigned" for an active deployment. `twd deploy` waits for
// "active", so behind a load-balanced mgmt it can wait on the wrong replica.
// Invariant I10: every replica reports the same status.
func TestMM_H08_DeploymentStatusPerReplica(t *testing.T) {
	r := newRun(t, "H08")
	c := newCluster(t, r, 2)
	for i := range 2 {
		c.start(i)
		if _, err := c.waitReady(i, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	var tn struct{ TenantID string }
	keyFile := r.dir + "/api.env"
	writeAPIKey(t, keyFile)
	c.must(0, "", "POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "apikey", "apiKeyFile": keyFile}, &tn)
	c.must(0, "h1", "PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 100, "tiers": []string{"process"}}, nil)
	var dep struct {
		DeploymentID string
		Epoch        int64
	}
	c.must(0, "", "POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "w",
		"bundle": makeBundle(map[string]string{"twd.jsonc": twdJSONC, "dist/index.js": "export default {}"})}, &dep)
	// The host reports the deployment active, to r0 only.
	c.must(0, "h1", "PUT", "/v1/hosts/h1/status", map[string]any{"liveNodes": 1,
		"leases":      []map[string]any{{"namespace": "w", "epoch": dep.Epoch}},
		"deployments": []map[string]any{{"deploymentID": dep.DeploymentID, "status": "active"}}}, nil)
	got := make([]string, 2)
	for i := range 2 {
		_, b, _ := c.call(i, "GET", "/v1/deployments/"+dep.DeploymentID, nil)
		var v struct{ Status string }
		json.Unmarshal(b, &v)
		got[i] = v.Status
	}
	t.Logf("deployment status: r0 (got the heartbeat) %q, r1 %q", got[0], got[1])
	if got[0] != got[1] {
		t.Errorf("I10 violated: replicas disagree on an active deployment's status (r0 %q, r1 %q)", got[0], got[1])
	}
}
