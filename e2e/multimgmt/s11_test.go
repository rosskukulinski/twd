//go:build e2emultimgmt

package multimgmt

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestMM_S11_HostFailover walks the host-failover protocol across replicas
// (ADR-028 §3, F4, F5, F9, I1/I2) with fake hosts: h1 dies; an unforced
// redeploy is refused (takeover_unconfirmed); a forced one is refused while
// h1's lease is valid (lease_held + Retry-After); after the lease expires a
// forced redeploy through another replica lands on h2 at epoch+1 and is
// audited; h1 coming back is revoked and fenced (lease_lost); h2 can join
// objects at the new epoch. (A real twd-hostd is not used: see LEDGER.)
func TestMM_S11_HostFailover(t *testing.T) {
	r := newRun(t, "S11")
	c := newCluster(t, r, 3)
	db := c.db()
	for i := range 3 {
		c.start(i)
		if _, err := c.waitReady(i, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	keyFile := r.dir + "/api.env"
	writeAPIKey(t, keyFile)
	var tn struct{ TenantID string }
	c.must(0, "", "POST", "/v1/tenants", map[string]any{"tailnet": "f.ts.net", "mintMode": "apikey", "apiKeyFile": keyFile}, &tn)
	for _, h := range []string{"h1", "h2"} {
		c.must(0, h, "PUT", "/v1/hosts/"+h, map[string]any{"capacityNodes": 100, "tiers": []string{"process"}}, nil)
	}
	bundle := makeBundle(map[string]string{"twd.jsonc": twdJSONC, "dist/index.js": "export default {}"})
	var d1 struct {
		DeploymentID, Host string
		Epoch              int64
	}
	c.must(0, "", "POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "w", "bundle": bundle, "host": "h1"}, &d1)
	hb := func(rep int, host string, epoch int64) []string {
		_, b, _ := c.callAs(rep, host, "PUT", "/v1/hosts/"+host+"/status", map[string]any{"liveNodes": 0,
			"leases": []map[string]any{{"namespace": "w", "epoch": epoch}}})
		var resp struct{ Revoke []string }
		json.Unmarshal(b, &resp)
		return resp.Revoke
	}
	// Some objects, joined by h1.
	var ids []string
	for k := range 4 {
		var o struct{ ObjectID string }
		c.must(0, "h1", "POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "namespace": "SupportAgent", "objectName": fmt.Sprintf("o%d", k)}, &o)
		ids = append(ids, o.ObjectID)
	}
	for _, id := range ids[:2] {
		c.must(0, "h1", "POST", fmt.Sprintf("/v1/objects/%s/credentials?epoch=%d&wait=10", id, d1.Epoch), nil, nil)
		c.must(0, "h1", "PATCH", "/v1/objects/"+id, map[string]any{"status": "ready", "nodeID": "1", "stableNodeID": "n-" + id, "magicDNS": id + ".f.ts.net"}, nil)
	}
	// h2 stays alive (through r1); h1 heartbeats (through r0) then dies.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			hb(1, "h2", 0)
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	hb(0, "h1", d1.Epoch)
	died := time.Now()
	deploy := func(rep int, force bool) (int, string, []byte) {
		body := map[string]any{"tenantID": tn.TenantID, "workload": "w", "bundle": bundle}
		if force {
			body["forceTakeover"] = true
		}
		st, b, _ := c.call(rep, "POST", "/v1/deployments", body)
		var e struct{ Code string }
		json.Unmarshal(b, &e)
		return st, e.Code, b
	}
	time.Sleep(12 * time.Second) // dead to placement (10 s), lease (45 s) still valid
	if st, code, b := deploy(2, false); st != 409 || code != "takeover_unconfirmed" {
		t.Errorf("unforced redeploy off a silent host: want 409 takeover_unconfirmed, got %d %s %.200s", st, code, b)
	}
	if st, code, b := deploy(2, true); st != 409 || code != "lease_held" {
		t.Errorf("forced redeploy while the lease is valid: want 409 lease_held, got %d %s %.200s", st, code, b)
	}
	// Wait out the lease (45 s from h1's last heartbeat).
	time.Sleep(time.Until(died.Add(47 * time.Second)))
	st, code, b := deploy(1, true)
	var d2 struct {
		DeploymentID, Host string
		Epoch              int64
	}
	json.Unmarshal(b, &d2)
	if st/100 != 2 || d2.Host != "h2" || d2.Epoch != d1.Epoch+1 {
		t.Fatalf("forced redeploy after lease expiry: want 2xx on h2 at epoch %d, got %d %s host=%s epoch=%d", d1.Epoch+1, st, code, d2.Host, d2.Epoch)
	}
	var detail string
	db.QueryRow(`SELECT raw FROM audit WHERE kind='deploy' AND raw LIKE '%takeoverFrom%' ORDER BY ts DESC LIMIT 1`).Scan(&detail)
	if !strings.Contains(detail, `"takeoverFrom":"h1"`) || !strings.Contains(detail, `"forced":true`) {
		t.Errorf("the forced takeover must be audited with takeoverFrom h1, forced: %.300s", detail)
	}
	// h1 comes back: its old-epoch heartbeat is revoked; its credential
	// requests at the old epoch are fenced (I2), through any replica.
	if rev := hb(2, "h1", d1.Epoch); len(rev) == 0 {
		t.Errorf("I1: h1 returned at the old epoch and was not revoked")
	}
	for rep := range 3 {
		st, b, _ := c.callAs(rep, "h1", "POST", fmt.Sprintf("/v1/objects/%s/credentials?epoch=%d", ids[2], d1.Epoch), nil)
		var e struct{ Code string }
		json.Unmarshal(b, &e)
		if st != 409 || e.Code != "lease_lost" {
			t.Errorf("I2: h1 (stale epoch) got a credential via r%d: %d %s", rep, st, e.Code)
		}
	}
	// h2 at the new epoch can join an object.
	if st, b, _ := c.callAs(0, "h2", "POST", fmt.Sprintf("/v1/objects/%s/credentials?epoch=%d&wait=10", ids[3], d2.Epoch), nil); st/100 != 2 {
		t.Errorf("h2 at the new epoch must get a credential: %d %.200s", st, b)
	}
	t.Logf("failover: h1 silent at %v; redeploy to %s at epoch %d after %v", died.Format("15:04:05"), d2.Host, d2.Epoch, time.Since(died).Round(time.Second))
}
