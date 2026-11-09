package mgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	v1 "github.com/rosskukulinski/twd/mgmt/api/v1"
)

// /v1 conventions (api/openapi.yaml): cursor pagination, JSON Merge Patch,
// ETag / If-Match, DELETE semantics, the role gate per route, host join
// keys. Every exchange here also passes the spec validator (newTestEnv).

func v1Tenant(t *testing.T, e *testEnv, tailnet string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"tailnet": tailnet, "mintMode": "apikey", "apiKeyFile": e.keyFile}
	for k, v := range extra {
		body[k] = v
	}
	var tn struct{ TenantID string }
	if st := e.c.okStatus("POST", "/v1/tenants", body, &tn); st != 201 {
		t.Fatalf("create tenant: %d", st)
	}
	return tn.TenantID
}

func v1Host(t *testing.T, e *testEnv, id string) *apiClient {
	t.Helper()
	h := e.asHost(id)
	h.ok("PUT", "/v1/hosts/"+id, map[string]any{"capacityNodes": 100, "tiers": []string{"process"}}, nil)
	return h
}

func TestV1Pagination(t *testing.T) {
	e := newTestEnv(t, nil)
	c := e.c
	var ids []string
	for i := range 5 {
		ids = append(ids, v1Tenant(t, e, fmt.Sprintf("t%d.ts.net", i), nil))
	}
	// limit above the cap is 400 (the spec's schema caps it at 500, so a
	// conformant client never sends more; checked directly); the default is 50.
	if st, pg := direct(t, e, "", "GET", "/v1/tenants?limit=501", nil); st != 400 || pg["code"] != "bad_request" {
		t.Fatalf("limit=501: %d %v", st, pg)
	}
	var tns []Tenant
	if next := c.items("/v1/tenants", &tns); len(tns) != 5 || next != nil {
		t.Fatalf("default limit: %d items, next %v", len(tns), next)
	}
	// Out-of-range limits and a bad cursor are typed 400s (the generated
	// binder parses; the spec's minimum is enforced by the handler).
	directFail(t, e, "", "GET", "/v1/tenants?limit=0", 400, "bad_request")
	directFail(t, e, "", "GET", "/v1/tenants?limit=-3", 400, "bad_request")
	c.fail("GET", "/v1/tenants?cursor=not-a-cursor", nil, 400, "bad_cursor")
	// Walk pages of 2 over 5 rows: every row exactly once, null at the end.
	var seen []string
	path := "/v1/tenants?limit=2"
	for pages := 0; ; pages++ {
		var pg []Tenant
		next := c.items(path, &pg)
		for _, tn := range pg {
			seen = append(seen, tn.ID)
		}
		if pages == 0 {
			// A row inserted mid-walk lands after the cursor (later
			// createdAt) and never shifts the earlier pages.
			ids = append(ids, v1Tenant(t, e, "late.ts.net", nil))
		}
		if next == nil {
			break
		}
		if len(pg) != 2 {
			t.Fatalf("page %d has %d rows with a next cursor", pages, len(pg))
		}
		path = "/v1/tenants?limit=2&cursor=" + *next
		if pages > 5 {
			t.Fatal("cursor never ended")
		}
	}
	slices.Sort(seen)
	want := append([]string(nil), ids...)
	slices.Sort(want)
	if !slices.Equal(seen, want) || len(seen) != 6 {
		t.Fatalf("walked %v, want %v", seen, want)
	}

	// Objects and audit page the same way.
	tn := ids[0]
	h1 := v1Host(t, e, "h1")
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "w", "bundle": noConfigBundle(t),
		"namespaces": []ClassConfig{{Class: "A", Tags: []string{"tag:ttest"}}}}, nil)
	for i := range 5 {
		h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn, "workload": "w", "namespace": "A", "objectName": fmt.Sprintf("o%d", i)}, nil)
	}
	var objs, all []Object
	path = "/v1/objects?tenant=" + tn + "&limit=2"
	for {
		next := c.items(path, &objs)
		all = append(all, objs...)
		if next == nil {
			break
		}
		path = "/v1/objects?tenant=" + tn + "&limit=2&cursor=" + *next
	}
	if len(all) != 5 {
		t.Fatalf("objects walked: %d", len(all))
	}
	var recs, allRecs []AuditRecord
	path = "/v1/audit?tenant=" + tn + "&limit=3"
	for {
		next := c.items(path, &recs)
		allRecs = append(allRecs, recs...)
		if next == nil {
			break
		}
		path = "/v1/audit?tenant=" + tn + "&limit=3&cursor=" + *next
	}
	var once []AuditRecord
	c.items("/v1/audit?tenant="+tn, &once)
	if len(allRecs) != len(once) || len(once) < 6 {
		t.Fatalf("audit walked %d, unpaged %d", len(allRecs), len(once))
	}
	keys := map[string]bool{}
	for _, r := range allRecs {
		k := fmt.Sprintf("%s/%d", r.Chain, r.Seq)
		if keys[k] {
			t.Fatalf("audit record %s paged twice", k)
		}
		keys[k] = true
	}
}

func TestV1TenantPatchAndETag(t *testing.T) {
	e := newTestEnv(t, nil)
	c := e.c
	tn := v1Tenant(t, e, "t.ts.net", map[string]any{"maxObjects": 10})
	// GET carries an ETag; If-None-Match answers 304.
	var before Tenant
	st, _, hdr := c.doRaw("GET", "/v1/tenants/"+tn, nil, &before)
	et := hdr.Get("ETag")
	if st != 200 || et == "" {
		t.Fatalf("GET: %d etag %q", st, et)
	}
	c.hdr = map[string]string{"If-None-Match": et}
	if st, _, _ := c.doRaw("GET", "/v1/tenants/"+tn, nil, nil); st != 304 {
		t.Fatalf("If-None-Match: %d", st)
	}
	c.hdr = nil
	// Merge patch: absent fields untouched, null clears, immutables refused.
	var after Tenant
	c.ok("PATCH", "/v1/tenants/"+tn, map[string]any{"registrationsPerMinute": 7, "wifLifecycleClientID": "life"}, &after)
	if after.RegistrationsPerMinute != 7 || after.MaxObjects != 10 || after.WIFLifecycleClientID != "life" || after.MintMode != MintAPIKey {
		t.Fatalf("after patch: %+v", after)
	}
	var cleared Tenant // a fresh value: omitted fields must not linger from a previous decode
	c.ok("PATCH", "/v1/tenants/"+tn, map[string]any{"wifLifecycleClientID": nil}, &cleared)
	if cleared.WIFLifecycleClientID != "" || cleared.RegistrationsPerMinute != 7 || cleared.MaxObjects != 10 {
		t.Fatalf("null did not clear: %+v", cleared)
	}
	// Inputs the spec itself forbids (so the validator would flag the
	// client) still get the server's typed refusal: checked directly.
	if st, eb := direct(t, e, "", "PATCH", "/v1/tenants/"+tn, map[string]any{"tailnet": "other.ts.net"}); st != 400 || eb["code"] != "immutable_field" || eb["missing"].(map[string]any)["field"] != "tailnet" {
		t.Fatalf("immutable: %d %v", st, eb)
	}
	for _, bad := range []map[string]any{{"bogus": 1}, {"maxObjects": 0}, {"mintMode": "magic"}} {
		if st, eb := direct(t, e, "", "PATCH", "/v1/tenants/"+tn, bad); st != 400 || eb["code"] != "bad_request" {
			t.Fatalf("patch %v: %d %v", bad, st, eb)
		}
	}
	// If-Match: stale → 412, current → 200.
	c.hdr = map[string]string{"If-Match": et} // the ETag from before the patches
	if eb := c.fail("PATCH", "/v1/tenants/"+tn, map[string]any{"maxObjects": 11}, 412, "precondition_failed"); eb == nil {
		t.Fatal("no 412 body")
	}
	_, _, hdr = c.doRaw("GET", "/v1/tenants/"+tn, nil, &after)
	c.hdr = map[string]string{"If-Match": hdr.Get("ETag")}
	c.ok("PATCH", "/v1/tenants/"+tn, map[string]any{"maxObjects": 11}, &after)
	c.hdr = nil
	if after.MaxObjects != 11 {
		t.Fatalf("patch with current If-Match: %+v", after)
	}
	// The audit trail has the update with the actor.
	var recs []AuditRecord
	c.items("/v1/audit?tenant="+tn+"&kind=tenant_update", &recs)
	if len(recs) != 3 {
		t.Fatalf("tenant_update records: %d", len(recs))
	}
}

func TestV1DeleteSemantics(t *testing.T) {
	e := newTestEnv(t, nil)
	c := e.c
	tn := v1Tenant(t, e, "t.ts.net", nil)
	h1 := v1Host(t, e, "h1")
	var dep1, dep2 DeployResponse
	body := map[string]any{"tenantID": tn, "workload": "w", "bundle": noConfigBundle(t), "namespaces": []ClassConfig{{Class: "A", Tags: []string{"tag:ttest"}}}}
	if st := c.okStatus("POST", "/v1/deployments", body, &dep1); st != 202 {
		t.Fatalf("deploy: %d", st)
	}
	var root, kid CreateObjectResponse
	if st := h1.okStatus("POST", "/v1/objects", map[string]any{"tenantID": tn, "workload": "w", "namespace": "A", "objectName": "root"}, &root); st != 202 {
		t.Fatalf("create: %d", st)
	}
	if st := h1.okStatus("POST", "/v1/objects", map[string]any{"tenantID": tn, "workload": "w", "namespace": "A", "objectName": "root"}, &root); st != 200 {
		t.Fatalf("idempotent create: %d", st)
	}
	h1.ok("POST", "/v1/objects/"+root.ObjectID+"/credentials?epoch=1", nil, &Credential{})
	h1.ok("PATCH", "/v1/objects/"+root.ObjectID, map[string]any{"status": "ready", "nodeID": "1", "stableNodeID": "nR", "magicDNS": root.ObjectID + ".t.ts.net", "epoch": 1}, nil)
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn, "workload": "w", "namespace": "A", "objectName": "kid", "parent": map[string]string{"objectID": root.ObjectID}}, &kid)

	// Tenant delete refuses while objects live; host delete refuses while leases are held.
	if eb := c.fail("DELETE", "/v1/tenants/"+tn, nil, 409, "tenant_has_live_objects"); eb["missing"].(map[string]any)["liveObjects"] != float64(2) {
		t.Fatalf("live objects: %v", eb["missing"])
	}
	if eb := c.fail("DELETE", "/v1/hosts/h1", nil, 409, "host_holds_leases"); fmt.Sprint(eb["missing"].(map[string]any)["leases"]) != "["+tn+"/w]" {
		t.Fatalf("leases: %v", eb["missing"])
	}

	// Kill: 202, then 410 with the kill record (by, cascade, root).
	c.hdr = map[string]string{"Twd-Actor": "ross@example.com"}
	var kr KillResult
	if st := c.okStatus("DELETE", "/v1/objects/"+root.ObjectID+killQuery(true, "test kill"), nil, &kr); st != 202 || len(kr.Killed) != 2 {
		t.Fatalf("kill: %d %+v", st, kr)
	}
	c.hdr = nil
	for _, id := range []string{root.ObjectID, kid.ObjectID} {
		st, eb, hdr := c.doRaw("GET", "/v1/objects/"+id, nil, nil)
		if st != 410 || eb["code"] != "killed" || hdr.Get("Twd-Error") != "killed" {
			t.Fatalf("GET killed %s: %d %v", id, st, eb)
		}
		ts := eb["tombstone"].(map[string]any)
		if ts["by"] != "ross@example.com" || ts["root"] != root.ObjectID || ts["reason"] != "test kill" || ts["cascade"] != (id == kid.ObjectID) || ts["objectID"] != id {
			t.Fatalf("tombstone %s: %v", id, ts)
		}
	}
	// Killed objects answer 410 to a second kill and to a credential.
	c.fail("DELETE", "/v1/objects/"+root.ObjectID, nil, 410, "killed")
	h1.fail("POST", "/v1/objects/"+kid.ObjectID+"/credentials?epoch=1", nil, 410, "killed")
	// A tombstone is still listed, and ?state= filters it.
	var objs []Object
	c.items("/v1/objects?tenant="+tn+"&state=killed", &objs)
	if len(objs) != 2 || objs[0].Status != StatusKilled {
		t.Fatalf("killed list: %+v", objs)
	}
	c.items("/v1/objects?tenant="+tn+"&state=pending&state=ready", &objs)
	if len(objs) != 0 {
		t.Fatalf("live list after kill: %+v", objs)
	}

	// Undeploy: a superseded deployment can't be undeployed; the latest can.
	c.ok("POST", "/v1/deployments", body, &dep2)
	c.fail("DELETE", "/v1/deployments/"+dep1.DeploymentID, nil, 409, "superseded")
	var live CreateObjectResponse
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn, "workload": "w", "namespace": "A", "objectName": "live"}, &live)
	if st := c.okStatus("DELETE", "/v1/deployments/"+dep2.DeploymentID+"?reason=undeploy", nil, &kr); st != 202 || !slices.Contains(kr.Killed, live.ObjectID) {
		t.Fatalf("undeploy: %d %+v", st, kr)
	}
	var dv DeploymentView
	c.ok("GET", "/v1/deployments/"+dep2.DeploymentID, nil, &dv)
	if dv.Status != "stopped" {
		t.Fatalf("after undeploy: %+v", dv)
	}
	if _, err := e.m.store.GetLease(context.Background(), LeaseName(tn, "w")); err == nil {
		if l, _ := e.m.store.GetLease(context.Background(), LeaseName(tn, "w")); l.Holder != "" {
			t.Fatalf("lease still held after undeploy: %+v", l)
		}
	}
	// Host delete: nothing held any more → 204; with a lease, drain=true → 202.
	if st := c.okStatus("DELETE", "/v1/hosts/h1", nil, nil); st != 204 {
		t.Fatalf("delete idle host: %d", st)
	}
	c.fail("GET", "/v1/hosts/h1", nil, 404, "not_found")
	h2 := v1Host(t, e, "h2")
	c.ok("POST", "/v1/deployments", body, &dep2)
	h2.ok("POST", "/v1/objects", map[string]any{"tenantID": tn, "workload": "w", "namespace": "A", "objectName": "drained"}, &live)
	var hd HostDeleteResult
	if st := c.okStatus("DELETE", "/v1/hosts/h2?drain=true", nil, &hd); st != 202 || len(hd.Killed) != 1 || len(hd.LeasesReleased) != 1 {
		t.Fatalf("drain: %d %+v", st, hd)
	}
	// Tenant delete with force: 202, then gone.
	h3 := v1Host(t, e, "h3")
	c.ok("POST", "/v1/deployments", body, &dep2)
	h3.ok("POST", "/v1/objects", map[string]any{"tenantID": tn, "workload": "w", "namespace": "A", "objectName": "last"}, &live)
	var td TenantDeleteResult
	if st := c.okStatus("DELETE", "/v1/tenants/"+tn+"?force=true", nil, &td); st != 202 || len(td.Killed) != 1 {
		t.Fatalf("force delete: %d %+v", st, td)
	}
	c.fail("GET", "/v1/tenants/"+tn, nil, 404, "not_found")
	var tns []Tenant
	c.items("/v1/tenants", &tns)
	if len(tns) != 0 {
		t.Fatalf("tenants after delete: %+v", tns)
	}
	// An empty tenant deletes with 204.
	tn2 := v1Tenant(t, e, "u.ts.net", nil)
	if st := c.okStatus("DELETE", "/v1/tenants/"+tn2, nil, nil); st != 204 {
		t.Fatalf("delete empty tenant: %d", st)
	}
}

// TestV1RoleMatrix drives every operation in the spec through the WhoIs
// gate as a host, an admin and a non-admin, so a route can't be added
// without its x-twd-role being enforced.
func TestV1RoleMatrix(t *testing.T) {
	e := newTestEnv(t, nil)
	srv := &Server{M: e.m, Self: SelfInfo{DNSName: "twd-mgmt.t.ts.net", Tailnet: "t.ts.net"}}
	whois := func(ctx context.Context, addr string) (WhoIsInfo, error) {
		switch addr {
		case "100.64.0.1:1":
			return WhoIsInfo{NodeID: "nHOST1", Tags: []string{HostTag}, Login: "tagged-devices"}, nil
		case "100.64.0.6:1":
			return WhoIsInfo{NodeID: "nADM", Login: "alice@example.com", Caps: []string{AdminCap}}, nil
		case "100.64.0.4:1":
			return WhoIsInfo{NodeID: "nEVE", Login: "eve@example.com"}, nil
		}
		return WhoIsInfo{}, fmt.Errorf("unknown")
	}
	h := srv.TailnetHandler(whois, srv.Handler())
	call := func(from, method, path, body string) (int, string) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = from
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out struct {
			Code string `json:"code"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Code
	}
	if c, _ := call("100.64.0.1:1", "PUT", "/v1/hosts/h1", `{"capacityNodes":1,"tiers":["process"]}`); c != 201 {
		t.Fatalf("register host: %d", c)
	}
	roles, err := v1.OperationRoles()
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) < 35 {
		t.Fatalf("only %d operations in the spec", len(roles))
	}
	fill := strings.NewReplacer("{id}", "h1", "{stableNodeID}", "nX", "{tenant}", "tn_x")
	for key, role := range roles {
		// DELETE /v1/hosts/{id} (as admin) removes h1: re-register it so
		// the host principal stays bound for every operation.
		if c, _ := call("100.64.0.1:1", "PUT", "/v1/hosts/h1", `{"capacityNodes":1,"tiers":["process"]}`); c/100 != 2 {
			t.Fatalf("re-register host: %d", c)
		}
		method, tmpl, _ := strings.Cut(key, " ")
		path := fill.Replace(tmpl)
		if strings.Contains(tmpl, "/volumes/") {
			path = strings.Replace(path, "/volumes/h1/", "/volumes/tn_x:w/", 1)
		}
		body := ""
		if method == "POST" || method == "PUT" || method == "PATCH" {
			body = "{}"
		}
		hostOK := role == "any" || role == "public" || strings.Contains(role, "host")
		adminOK := role == "any" || role == "public" || strings.Contains(role, "admin")
		c, code := call("100.64.0.1:1", method, path, body)
		if hostOK && c == 403 {
			t.Errorf("%s (%s): host refused %d %s", key, role, c, code)
		}
		if !hostOK && (c != 403 || code != "forbidden") {
			t.Errorf("%s (%s): host got %d %s, want 403 forbidden", key, role, c, code)
		}
		c, code = call("100.64.0.6:1", method, path, body)
		if adminOK && c == 403 {
			t.Errorf("%s (%s): admin refused %d %s", key, role, c, code)
		}
		if !adminOK && (c != 403 || code != "forbidden") {
			t.Errorf("%s (%s): admin got %d %s, want 403 forbidden", key, role, c, code)
		}
		c, code = call("100.64.0.4:1", method, path, body)
		if role != "public" && (c != 403 || code != "not_admin") {
			t.Errorf("%s (%s): non-admin got %d %s, want 403 not_admin", key, role, c, code)
		}
	}
	// A host may not read or write another host's record.
	call("100.64.0.1:1", "PUT", "/v1/hosts/h1", `{"capacityNodes":1,"tiers":["process"]}`)
	for _, m := range []string{"GET", "PUT"} {
		if c, code := call("100.64.0.1:1", m, "/v1/hosts/h2", `{"capacityNodes":1,"tiers":["process"]}`); c != 403 || code != "forbidden" {
			t.Errorf("%s other host: %d %s", m, c, code)
		}
	}
}

func TestV1HostJoinKey(t *testing.T) {
	e := newTestEnv(t, nil)
	c := e.c
	tn := v1Tenant(t, e, "op.ts.net", nil)
	var hj struct {
		Host struct {
			HostID string `json:"hostID"`
			Status string `json:"status"`
		} `json:"host"`
		AuthKey  string   `json:"authKey"`
		Tags     []string `json:"tags"`
		Hostname string   `json:"hostname"`
		Tailnet  string   `json:"tailnet"`
	}
	st, _, hdr := c.doRaw("POST", "/v1/hosts", map[string]any{"hostID": "h-mbp"}, &hj)
	if st != 201 || hdr.Get("Location") != "/v1/hosts/h-mbp" || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("create host: %d %v", st, hdr)
	}
	if !strings.HasPrefix(hj.AuthKey, "tskey-auth-") || fmt.Sprint(hj.Tags) != "[tag:twd-host]" || hj.Hostname != "twd-host-h-mbp" || hj.Host.Status != "pending" || hj.Tailnet != "op.ts.net" {
		t.Fatalf("join: %+v", hj)
	}
	kb := e.ts.createdKey(1)
	create := kb["capabilities"].(map[string]any)["devices"].(map[string]any)["create"].(map[string]any)
	if create["reusable"] != false || create["preauthorized"] != true || fmt.Sprint(create["tags"]) != "[tag:twd-host]" || kb["expirySeconds"].(float64) != 600 {
		t.Fatalf("minted host key options %v", kb)
	}
	// Pending again: a fresh key, 200.
	first := hj.AuthKey
	if st := c.okStatus("POST", "/v1/hosts", map[string]any{"hostID": "h-mbp", "tenantID": tn}, &hj); st != 200 || hj.AuthKey == first {
		t.Fatalf("re-mint: %d same key %v", st, hj.AuthKey == first)
	}
	// It is listed as pending; once it registers it is live and a further
	// POST is 409 exists.
	var hosts []Host
	c.items("/v1/hosts", &hosts)
	if len(hosts) != 1 || hosts[0].ID != "h-mbp" || !hosts[0].Pending() {
		t.Fatalf("hosts: %+v", hosts)
	}
	e.asHost("h-mbp").ok("PUT", "/v1/hosts/h-mbp", map[string]any{"capacityNodes": 5, "tiers": []string{"process"}}, nil)
	c.fail("POST", "/v1/hosts", map[string]any{"hostID": "h-mbp"}, 409, "exists")
	if st, eb := direct(t, e, "", "POST", "/v1/hosts", map[string]any{"hostID": "bad/id"}); st != 400 || eb["code"] != "bad_request" {
		t.Fatalf("bad host ID: %d %v", st, eb)
	}
	c.fail("POST", "/v1/hosts", map[string]any{"hostID": "h-2", "tenantID": "tn_nope"}, 404, "unknown_tenant")
	var recs []AuditRecord
	c.items("/v1/audit?kind=host_add", &recs)
	if len(recs) != 2 {
		t.Fatalf("host_add records: %d", len(recs))
	}
	if strings.Contains(e.logs.String(), "tskey-") {
		t.Fatal("a host join key reached the logs")
	}
}

// TestV1Status: the operational counters and the transport label.
func TestV1Status(t *testing.T) {
	e := newTestEnv(t, nil)
	v1Tenant(t, e, "t.ts.net", nil)
	v1Host(t, e, "h1")
	var st struct {
		OK        bool           `json:"ok"`
		Transport string         `json:"transport"`
		Hosts     map[string]int `json:"hosts"`
		Tenants   int            `json:"tenants"`
	}
	e.c.ok("GET", "/v1/status", nil, &st)
	if !st.OK || st.Transport != "local-dev" || st.Hosts["total"] != 1 || st.Hosts["live"] != 1 || st.Tenants != 1 {
		t.Fatalf("status %+v", st)
	}
	var who WhoAmI
	e.c.ok("GET", "/v1/whoami", nil, &who)
	if who.Kind != "local" || who.Mgmt.Transport != "local-dev" {
		t.Fatalf("whoami %+v", who)
	}
	// There is no /v0 any more.
	if st, eb := direct(t, e, "", "GET", "/v0/healthz", nil); st != 404 || eb["code"] != "not_found" {
		t.Fatalf("/v0: %d %v", st, eb)
	}
	_ = http.StatusOK
}
