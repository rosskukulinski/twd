package mgmt

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/internal/publicspec"
)

// ADR-029 public ingress, mgmt side: the tenant gate, the "_public"
// namespace and node, `twd expose`, Assignment.Public and the preflight.

const publicTwdJSONC = `{
  "public": { "routes": ["/webhooks/*", "/oauth/callback"], "limits": { "rps": 5 } },
  "tailnet": { "tags": ["tag:twd-wl-support"], "objects": {
    "Room": { "tags": ["tag:twd-do-room"], "public": { "create": true, "maxObjects": 10 } } } } }`

const tailnetOnlyTwdJSONC = `{ "tailnet": { "tags": ["tag:twd-wl-support"], "objects": { "Room": { "tags": ["tag:twd-do-room"] } } } }`

func publicBundle(t *testing.T, twd string) []byte {
	return makeBundle(t, map[string]string{"twd.jsonc": twd, "dist/index.js": "export default {}"})
}

// pushRecorder is a host push listener that counts pushes per path.
type pushRecorder struct {
	mu   sync.Mutex
	hits map[string]int
}

func (p *pushRecorder) count(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits[path]
}

// publicHost registers host h1 with a push listener.
func publicHost(t *testing.T, e *testEnv) *pushRecorder {
	t.Helper()
	rec := &pushRecorder{hits: map[string]int{}}
	sock := filepath.Join(os.TempDir(), "twd-pub-"+filepath.Base(e.sock)+"-"+time.Now().Format("150405.000000")+".sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.hits[r.URL.Path]++
		rec.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); os.Remove(sock) })
	if _, _, err := e.m.RegisterHost(context.Background(), RegisterRequest{HostID: "h1", CapacityNodes: 100, Tiers: []string{"process"}, PushURL: "unix://" + sock}, "", ""); err != nil {
		t.Fatal(err)
	}
	return rec
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// rawAssignments fetches h1's assignments as raw JSON items.
func rawAssignments(t *testing.T, e *testEnv) []json.RawMessage {
	t.Helper()
	var items []json.RawMessage
	e.asHost("h1").items("/v1/hosts/h1/assignments", &items)
	return items
}

func hostAssignment(t *testing.T, e *testEnv, workload string) mgmtapi.Assignment {
	t.Helper()
	for _, raw := range rawAssignments(t, e) {
		var a mgmtapi.Assignment
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatal(err)
		}
		if a.Workload == workload {
			return a
		}
	}
	t.Fatalf("no assignment for %s", workload)
	return mgmtapi.Assignment{}
}

func auditKinds(t *testing.T, e *testEnv, tenant string, kinds ...string) []AuditRecord {
	t.Helper()
	recs, err := e.store.ListAudit(context.Background(), AuditFilter{TenantID: tenant, Kinds: kinds})
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestPublicGateAndAssignment(t *testing.T) {
	e := newTestEnv(t, nil)
	c := e.c
	push := publicHost(t, e)
	tn := v1Tenant(t, e, "t.ts.net", nil)
	bundle := publicBundle(t, publicTwdJSONC)
	deploy := map[string]any{"tenantID": tn, "workload": "support", "bundle": bundle}

	// D1: the gate is deny by default.
	eb := c.fail("POST", "/v1/deployments", deploy, 403, "public_ingress_not_allowed")
	if !strings.Contains(eb["message"].(string), "twd tenant set "+tn+" --public-ingress allow") {
		t.Fatalf("refusal names no fix: %v", eb)
	}
	c.fail("POST", "/v1/deployments?dryRun=true", deploy, 403, "public_ingress_not_allowed")
	// A tailnet-only deploy is unaffected by the gate.
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "other", "bundle": publicBundle(t, tailnetOnlyTwdJSONC)}, nil)

	// Bad values and bundles.
	c.fail("PATCH", "/v1/tenants/"+tn, map[string]any{"publicIngress": "maybe"}, 400, "bad_request")
	var tv Tenant
	c.ok("PATCH", "/v1/tenants/"+tn, map[string]any{"publicIngress": "allow"}, &tv)
	if tv.PublicIngress != PublicAllow {
		t.Fatalf("tenant %+v", tv)
	}
	if recs := auditKinds(t, e, tn, "tenant_update"); len(recs) == 0 || !strings.Contains(string(recs[len(recs)-1].Detail), `"publicIngress":"allow"`) {
		t.Fatalf("tenant_update audit: %+v", recs)
	}
	c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "twd-mgmt", "bundle": bundle}, 400, "bad_request")
	c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "twd-host-x", "bundle": bundle}, 400, "bad_request")
	c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "bad",
		"bundle": publicBundle(t, `{"public": {"routes": ["/_twd/*"]}, "tailnet": {"tags": ["tag:twd-wl-x"]}}`)}, 400, "bad_bundle")
	c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "bad",
		"bundle": publicBundle(t, `{"runtime": "node", "public": {"routes": ["/*"]}, "tailnet": {"tags": ["tag:twd-wl-x"]}}`)}, 400, "bad_bundle")
	c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "bad",
		"bundle": publicBundle(t, `{"tailnet": {"tags": ["tag:twd-wl-x"], "objects": {"R": {"tags": ["tag:twd-do-r"], "public": {"create": true}}}}}`)}, 400, "bad_bundle")

	// Allowed: deploy ok, "_public" namespace recorded, Assignment.Public emitted.
	var dep DeployResponse
	c.ok("POST", "/v1/deployments", deploy, &dep)
	d, err := e.store.GetDeployment(context.Background(), dep.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(d.Namespaces, func(cc ClassConfig) bool {
		return cc.Class == PublicNamespace && slices.Equal(cc.Tags, []string{publicspec.Tag})
	}) || d.Public == nil || d.Public.Classes["Room"].MaxObjects != 10 {
		t.Fatalf("deployment %+v public %+v", d.Namespaces, d.Public)
	}
	waitFor(t, "assignments push", func() bool { return push.count("/v1/push/assignments") > 0 })
	a := hostAssignment(t, e, "support")
	want := publicspec.Limits{RPS: 5}
	if a.Public == nil || !slices.Equal(a.Public.Routes, []string{"/oauth/callback", "/webhooks/*"}) ||
		a.Public.Limits != want.Effective() || !slices.Equal(a.Public.Tags, []string{"tag:twd-public"}) || !a.Public.Exposed {
		t.Fatalf("Assignment.Public %+v", a.Public)
	}
	for _, ns := range a.Namespaces {
		if ns.Class == PublicNamespace {
			t.Fatal("the _public namespace leaked into Assignment.Namespaces")
		}
	}
	if o := hostAssignment(t, e, "other"); o.Public != nil {
		t.Fatalf("tailnet-only workload has Public %+v", o.Public)
	}
	var dv struct {
		Public *DeploymentPublicView `json:"public"`
	}
	c.ok("GET", "/v1/deployments/"+dep.DeploymentID, nil, &dv)
	if dv.Public == nil || dv.Public.URL != "https://support.t.ts.net/" || !dv.Public.Exposed || !dv.Public.Allowed {
		t.Fatalf("deployment view public %+v", dv.Public)
	}

	// Gate back to deny: Public disappears (the host closes its listener), and hosts are pushed.
	before := push.count("/v1/push/assignments")
	c.ok("PATCH", "/v1/tenants/"+tn, map[string]any{"publicIngress": "deny"}, nil)
	waitFor(t, "gate push", func() bool { return push.count("/v1/push/assignments") > before })
	if a := hostAssignment(t, e, "support"); a.Public != nil {
		t.Fatalf("deny still assigns Public %+v", a.Public)
	}
	// ...and no public node can be minted while denied.
	e.asHost("h1").fail("POST", "/v1/objects", map[string]any{"tenantID": tn, "workload": "support", "namespace": PublicNamespace, "objectName": "support"}, 403, "public_ingress_not_allowed")
	tv = Tenant{}
	c.ok("PATCH", "/v1/tenants/"+tn, map[string]any{"publicIngress": nil}, &tv)
	if tv.PublicIngress != "" || tv.PublicAllowed() {
		t.Fatalf("null did not clear the gate: %+v", tv)
	}
}

func TestPublicNodeObject(t *testing.T) {
	e := newTestEnv(t, nil)
	c, h1 := e.c, e.asHost("h1")
	publicHost(t, e)
	tn := v1Tenant(t, e, "t.ts.net", map[string]any{"publicIngress": "allow"})
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "support", "bundle": publicBundle(t, publicTwdJSONC)}, nil)

	// The public node: hostname = the workload (stable public URL), tag:twd-public.
	h1.fail("POST", "/v1/objects", map[string]any{"tenantID": tn, "workload": "support", "namespace": PublicNamespace, "objectName": "x"}, 400, "bad_request")
	var pn CreateObjectResponse
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn, "workload": "support", "namespace": PublicNamespace, "objectName": "support"}, &pn)
	if pn.Hostname != "support" || !slices.Equal(pn.Tags, []string{"tag:twd-public"}) || pn.Status != StatusPending {
		t.Fatalf("public node %+v", pn)
	}
	var cred Credential
	h1.ok("POST", "/v1/objects/"+pn.ObjectID+"/credentials?epoch=1", nil, &cred)
	if cred.Hostname != "support" {
		t.Fatalf("credential hostname %q", cred.Hostname)
	}
	if kb := e.ts.createdKey(1); !strings.Contains(string(mustJSON(kb)), `"tag:twd-public"`) {
		t.Fatalf("minted key tags %v", kb)
	}

	// An object a public request created is audited with origin public (D5).
	var room CreateObjectResponse
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn, "workload": "support", "namespace": "Room", "objectName": "lobby", "actingFor": ActingForPublic}, &room)
	recs := auditKinds(t, e, tn, "create")
	if !slices.ContainsFunc(recs, func(r AuditRecord) bool {
		return r.ObjectID == room.ObjectID && strings.Contains(string(r.Detail), `"origin":"public"`)
	}) {
		t.Fatalf("no origin:public create record: %+v", recs)
	}

	// Public routes removed by a redeploy: the public node is killed (device deleted).
	time.Sleep(3 * time.Millisecond) // a distinct createdAt: "latest deployment" ties break by random ID
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "support", "bundle": publicBundle(t, tailnetOnlyTwdJSONC)}, nil)
	o, err := e.store.GetObject(context.Background(), pn.ObjectID)
	if err != nil || o.Live() || o.KillReason != "public routes removed" {
		t.Fatalf("public node after removal: %+v %v", o, err)
	}
	if a := hostAssignment(t, e, "support"); a.Public != nil {
		t.Fatalf("Public after removal %+v", a.Public)
	}
	// The host gets the tombstone (410), never a fresh public node.
	h1.fail("POST", "/v1/objects", map[string]any{"tenantID": tn, "workload": "support", "namespace": PublicNamespace, "objectName": "support"}, 410, "killed")

	// Declared again: the dead public node is purged and can be recreated (same ID, same name).
	time.Sleep(3 * time.Millisecond)
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "support", "bundle": publicBundle(t, publicTwdJSONC)}, nil)
	var pn2 CreateObjectResponse
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn, "workload": "support", "namespace": PublicNamespace, "objectName": "support"}, &pn2)
	if pn2.ObjectID != pn.ObjectID || pn2.Status != StatusPending || pn2.Hostname != "support" {
		t.Fatalf("recreated public node %+v (was %+v)", pn2, pn)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestPublicExpose(t *testing.T) {
	e := newTestEnv(t, nil)
	c := e.c
	push := publicHost(t, e)
	tn := v1Tenant(t, e, "t.ts.net", map[string]any{"publicIngress": "allow"})
	path := "/v1/tenants/" + tn + "/workloads/support/public"
	var never WorkloadPublic
	c.ok("GET", path, nil, &never) // never toggled (no deployment yet): exposed
	if !never.Exposed {
		t.Fatalf("untoggled: %+v", never)
	}
	c.fail("PATCH", path, map[string]any{"exposed": false}, 404, "unknown_workload")
	bundle := publicBundle(t, publicTwdJSONC)
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "support", "bundle": bundle}, nil)

	var wp WorkloadPublic
	_, _, hdr := c.doRaw("GET", path, nil, &wp)
	if !wp.Exposed {
		t.Fatalf("default not exposed: %+v", wp)
	}
	// If-Match on a stale ETag is refused.
	stale := *c
	stale.hdr = map[string]string{"If-Match": `"stale"`}
	stale.fail("PATCH", path, map[string]any{"exposed": false}, 412, "precondition_failed")
	if st, eb := direct(t, e, "", "PATCH", path, map[string]any{"exposed": "no"}); st != 400 || eb["code"] != "bad_request" {
		t.Fatalf("non-boolean exposed: %d %v", st, eb)
	}

	before := push.count("/v1/push/assignments")
	cur := *c
	cur.hdr = map[string]string{"If-Match": hdr.Get("ETag")}
	cur.ok("PATCH", path, map[string]any{"exposed": false}, &wp)
	if wp.Exposed || wp.UpdatedBy == "" {
		t.Fatalf("expose off: %+v", wp)
	}
	waitFor(t, "expose push", func() bool { return push.count("/v1/push/assignments") > before })
	if a := hostAssignment(t, e, "support"); a.Public == nil || a.Public.Exposed {
		t.Fatalf("expose off not assigned: %+v", a.Public)
	}
	// Survives a redeploy.
	time.Sleep(3 * time.Millisecond)
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "support", "bundle": bundle}, nil)
	if a := hostAssignment(t, e, "support"); a.Public == nil || a.Public.Exposed {
		t.Fatalf("expose off lost on redeploy: %+v", a.Public)
	}
	c.ok("PATCH", path, map[string]any{"exposed": true}, &wp)
	if a := hostAssignment(t, e, "support"); !a.Public.Exposed {
		t.Fatal("expose on not assigned")
	}
	var kinds []string
	for _, r := range auditKinds(t, e, tn, "expose_on", "expose_off") {
		kinds = append(kinds, r.Kind)
		if r.ActingFor == "" {
			t.Fatalf("expose record without actor: %+v", r)
		}
	}
	if !slices.Equal(kinds, []string{"expose_off", "expose_on"}) {
		t.Fatalf("expose audit kinds %v", kinds)
	}
}

func TestPublicPreflight(t *testing.T) {
	run := func(t *testing.T, policy string) PreflightResult {
		e := newTestEnv(t, nil)
		e.ts.policy = policy
		publicHost(t, e)
		tn := v1Tenant(t, e, "t.ts.net", map[string]any{"publicIngress": "allow"})
		var res PreflightResult
		e.c.ok("POST", "/v1/deployments?dryRun=true", map[string]any{"tenantID": tn, "workload": "support", "bundle": publicBundle(t, publicTwdJSONC)}, &res)
		return res
	}
	owned := `"tag:twd": ["autogroup:admin"], "tag:twd-wl-support": ["tag:twd"], "tag:twd-do-room": ["tag:twd"]`
	t.Run("missing", func(t *testing.T) {
		res := run(t, `{"tagOwners": {`+owned+`}, "nodeAttrs": [{"target": ["tag:twd-do-room"], "attr": ["funnel"]}]}`)
		if res.OK || !slices.Contains(res.MissingTagOwners, "tag:twd-public") || !slices.Equal(res.MissingNodeAttrs, []string{"funnel on tag:twd-public"}) ||
			!strings.Contains(res.NodeAttrsSnippet, `"target": ["tag:twd-public"], "attr": ["funnel"]`) || res.PublicURL != "https://support.t.ts.net/" {
			t.Fatalf("preflight %+v", res)
		}
	})
	t.Run("present", func(t *testing.T) {
		res := run(t, `{"tagOwners": {`+owned+`, "tag:twd-public": ["autogroup:admin", "tag:twd"]},
			"nodeAttrs": [{"target": ["tag:twd-public"], "attr": ["funnel"]}, {"target": ["tag:x"], "attr": [{"ipPool": ["100.64.0.0/24"]}]}]}`)
		if !res.OK || len(res.MissingNodeAttrs) != 0 {
			t.Fatalf("preflight %+v", res)
		}
	})
}

// TestAssignmentPublicWireShape: mgmt's PublicAssignment and hostd's
// mgmtapi.PublicAssignment are the same JSON.
func TestAssignmentPublicWireShape(t *testing.T) {
	p := PublicAssignment{Routes: []string{"/a/*"}, Upgrade: []string{"/a/*"}, Limits: publicspec.Defaults, Tags: []string{"tag:twd-public"}, Exposed: true}
	b, _ := json.Marshal(p)
	var h mgmtapi.PublicAssignment
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	hb, _ := json.Marshal(h)
	if string(hb) != string(b) {
		t.Fatalf("wire shapes differ:\n mgmt %s\nhostd %s", b, hb)
	}
}
