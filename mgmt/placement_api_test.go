package mgmt

import (
	"context"
	"strings"
	"testing"
)

// TestAPIPlacement drives smart placement through /v1 (spec-validated):
// capacity at first deploy, path reports (only for a host's own tenants),
// smart placement next to the target, stickiness, an explicit re-place that
// moves the lease at epoch+1, the stateful-move guard, and tier
// eligibility.
func TestAPIPlacement(t *testing.T) {
	e := newTestEnv(t, nil)
	c := e.c
	var tn struct{ TenantID string }
	c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile}, &tn)
	hosts := map[string]*apiClient{}
	for _, h := range []struct {
		id    string
		cap   int
		tiers []string
	}{{"h1", 1000, []string{"process"}}, {"h2", 100, []string{"process"}}, {"h3", 10, []string{"process", "microvm"}}} {
		hosts[h.id] = e.asHost(h.id)
		hosts[h.id].ok("PUT", "/v1/hosts/"+h.id, map[string]any{"capacityNodes": h.cap, "tiers": h.tiers}, nil)
	}
	stateless := func(extra string) []byte {
		return makeBundle(t, map[string]string{"twd.jsonc": `{` + extra + ` "tailnet": {"tags": ["tag:w"], "connects": ["tag:db"]}}`, "dist/index.js": "export default {}"})
	}
	type dep struct {
		DeploymentID, Host string
		Epoch              int64
		Placement          PlacementDecision
	}
	deploy := func(body map[string]any) dep {
		t.Helper()
		var d dep
		body["tenantID"] = tn.TenantID
		c.ok("POST", "/v1/deployments", body, &d)
		return d
	}
	report := func(h, target, path string, rtt float64) {
		t.Helper()
		hosts[h].ok("PUT", "/v1/hosts/"+h+"/paths", map[string]any{"tenants": []map[string]any{{"tenantID": tn.TenantID,
			"targets": []map[string]any{{"target": target, "path": path, "rttMs": rtt, "ageMs": 500, "peer": "pg-1"}}}}}, nil)
	}

	// First deploy: nothing measured, so capacity (h1).
	w := deploy(map[string]any{"workload": "worker", "bundle": stateless("")})
	if w.Host != "h1" || w.Placement.Reason != "capacity" || !strings.Contains(w.Placement.Summary, "no measured path") {
		t.Fatalf("first deploy: %s %+v", w.Host, w.Placement)
	}
	// h2 runs nothing of this tenant: it can't report for it.
	hosts["h2"].fail("PUT", "/v1/hosts/h2/paths", map[string]any{"tenants": []map[string]any{{"tenantID": tn.TenantID,
		"targets": []map[string]any{{"target": "tag:db", "path": "direct", "rttMs": 1, "ageMs": 0}}}}}, 403, "forbidden")
	report("h1", "tag:db", "derp", 24)
	seed := deploy(map[string]any{"workload": "seed", "bundle": stateless(""), "host": "h2"})
	if seed.Host != "h2" || seed.Placement.Reason != "pinned" {
		t.Fatalf("pinned: %+v", seed)
	}
	report("h2", "tag:db", "direct", 1.8)

	// A new workload that talks to tag:db lands next to it.
	api := deploy(map[string]any{"workload": "api", "bundle": stateless("")})
	want := "placed on h2: tag:db direct 1.8 ms (h1: relayed 24 ms, h3: unmeasured)"
	if api.Host != "h2" || api.Placement.Reason != "smart" || api.Placement.Summary != want {
		t.Fatalf("smart: %s %q, want h2 %q", api.Host, api.Placement.Summary, want)
	}
	var view struct{ Placement PlacementDecision }
	c.ok("GET", "/v1/deployments/"+api.DeploymentID, nil, &view)
	if view.Placement.Summary != want || len(view.Placement.Hosts) != 3 {
		t.Fatalf("GET placement: %+v", view.Placement)
	}
	// Placement off: capacity, measurements ignored.
	if d := deploy(map[string]any{"workload": "offw", "bundle": stateless(`"placement": {"mode": "off"},`)}); d.Host != "h1" || d.Placement.Reason != "capacity" {
		t.Fatalf("mode off: %+v", d)
	}

	// The worker (on h1, relayed) stays there on a plain redeploy: sticky.
	hb := func(h string, ns string, epoch int64) []string {
		var r struct{ Revoke []string }
		hosts[h].ok("PUT", "/v1/hosts/"+h+"/status", map[string]any{"liveNodes": 0, "leases": []map[string]any{{"namespace": ns, "epoch": epoch}}}, &r)
		return r.Revoke
	}
	w2 := deploy(map[string]any{"workload": "worker", "bundle": stateless("")})
	if w2.Host != "h1" || w2.Placement.Reason != "sticky" || w2.Epoch != w.Epoch+1 {
		t.Fatalf("sticky: %+v (first epoch %d)", w2, w.Epoch)
	}
	// --replace moves it: the lease goes to h2 at epoch+1 in one write.
	w3 := deploy(map[string]any{"workload": "worker", "bundle": stateless(""), "replace": true})
	if w3.Host != "h2" || w3.Placement.Reason != "smart" || w3.Epoch != w2.Epoch+1 {
		t.Fatalf("replace: %+v", w3)
	}
	l, _ := e.store.GetLease(context.Background(), LeaseName(tn.TenantID, "worker"))
	if l.Holder != "h2" || l.Epoch != w3.Epoch {
		t.Fatalf("lease after move: %+v", l)
	}
	if rv := hb("h1", "worker", w2.Epoch); len(rv) != 1 {
		t.Fatalf("old holder not revoked: %v", rv)
	}
	if rv := hb("h2", "worker", w3.Epoch); len(rv) != 0 {
		t.Fatalf("new holder revoked: %v", rv)
	}
	// Re-place again: h2 is already best, it stays (no churn).
	if w4 := deploy(map[string]any{"workload": "worker", "bundle": stateless(""), "replace": true}); w4.Host != "h2" || w4.Placement.Reason != "sticky" {
		t.Fatalf("replace on the best host: %+v", w4)
	}
	var audit struct {
		Records []struct {
			Kind   string
			Detail map[string]any
		} `json:"items"`
	}
	c.ok("GET", "/v1/audit?limit=500", nil, &audit)
	moves := 0
	for _, r := range audit.Records {
		if r.Kind == "deploy" && r.Detail["movedFrom"] == "h1" && r.Detail["deploymentID"] == w3.DeploymentID {
			moves++
			if p, _ := r.Detail["placement"].(map[string]any); p["reason"] != "smart" || !strings.Contains(p["summary"].(string), "placed on h2") {
				t.Fatalf("audit placement detail: %v", r.Detail)
			}
		}
	}
	if moves != 1 {
		t.Fatalf("audited moves: %d", moves)
	}

	// A stateful workload: a move off its live host must be confirmed.
	sf := makeBundle(t, map[string]string{"twd.jsonc": testTwdJSONC, "dist/index.js": "export default {}"})
	s1 := deploy(map[string]any{"workload": "agent", "bundle": sf, "host": "h1"})
	c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "agent", "bundle": sf, "host": "h2"}, 409, "lease_held")
	c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "agent", "bundle": sf, "host": "h2", "replace": true}, 409, "move_unconfirmed")
	if s2 := deploy(map[string]any{"workload": "agent", "bundle": sf, "host": "h2", "replace": true, "forceTakeover": true}); s2.Host != "h2" || s2.Epoch != s1.Epoch+1 {
		t.Fatalf("confirmed stateful move: %+v", s2)
	}

	// Tier: a microvm workload only goes to a host that offers it.
	vm := deploy(map[string]any{"workload": "vmw", "bundle": stateless(`"tier": "microvm",`)})
	if vm.Host != "h3" {
		t.Fatalf("microvm placed on %s (%s)", vm.Host, vm.Placement.Summary)
	}
	if e := c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "vmw2", "bundle": stateless(`"tier": "microvm",`), "host": "h1"}, 409, "tier_unavailable"); !strings.Contains(e["message"].(string), "does not offer the \"microvm\" tier") {
		t.Fatalf("pinned microvm on h1: %v", e)
	}
	// Runtime node always runs in the process tier, whatever the tenant default.
	c.ok("PATCH", "/v1/tenants/"+tn.TenantID, map[string]any{"tier": "microvm"}, nil)
	if d := deploy(map[string]any{"workload": "nodew", "bundle": stateless(`"runtime": "node",`)}); d.Host == "" {
		t.Fatal("node runtime not placed")
	}
	if d := deploy(map[string]any{"workload": "tenantvm", "bundle": stateless("")}); d.Host != "h3" {
		t.Fatalf("tenant tier microvm placed on %s", d.Host)
	}
	// The worker (stateless, on process-only h2) now asks for microvm: a
	// plain redeploy is refused (stickiness never keeps the wrong tier);
	// --replace moves it to the microvm host.
	c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "worker", "bundle": stateless(`"tier": "microvm",`)}, 409, "tier_unavailable")
	if d := deploy(map[string]any{"workload": "worker", "bundle": stateless(`"tier": "microvm",`), "replace": true}); d.Host != "h3" {
		t.Fatalf("replace onto the microvm host: %+v", d)
	}
}
