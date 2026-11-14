package mgmt

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestContainerPlacement (ADR-032 D9): a bundle that declares containers is
// placed only on a live host offering "containers" for its images' arch,
// even when the parent is process-tier; otherwise 503 no_host, or 409
// tier_unavailable for a pinned host or a sticky lease holder.
func TestContainerPlacement(t *testing.T) {
	const tags = `"tailnet": { "tags": ["tag:twd-web"] }`
	wrangler := func(arch string) string {
		a := ""
		if arch != "" {
			a = `, "arch": "` + arch + `"`
		}
		return `{ "name": "w", "main": "index.js",
			"containers": [{ "class_name": "Sandbox", "image": "./Dockerfile", "max_instances": 2` + a + ` }],
			"durable_objects": { "bindings": [{ "name": "SANDBOX", "class_name": "Sandbox" }] } }`
	}
	type host struct {
		id    string
		cap   int
		tiers []string
		arch  string
	}
	proc := host{"hp", 1000, []string{"process"}, "amd64"}                          // roomiest, no VMs
	vm := host{"hv", 500, []string{"process", "microvm"}, "amd64"}                  // microVM DOs, no containers
	ctrArm := host{"ha", 50, []string{"process", "microvm", "containers"}, "arm64"} // containers, arm64
	ctrAmd := host{"hx", 10, []string{"process", "microvm", "containers"}, "amd64"} // containers, amd64
	for _, tc := range []struct {
		name     string
		hosts    []host
		twd      string
		wrangler string
		pin      string
		want     string
		wantErr  string
		mention  string
	}{
		{name: "no containers: by capacity", hosts: []host{proc, ctrArm}, twd: `{` + tags + `}`, want: "hp"},
		{name: "process-tier parent still needs containers", hosts: []host{proc, vm, ctrArm, ctrAmd}, twd: `{` + tags + `}`, wrangler: wrangler(""), want: "ha"},
		{name: "amd64 images", hosts: []host{proc, ctrArm, ctrAmd}, twd: `{` + tags + `}`, wrangler: wrangler("amd64"), want: "hx"},
		{name: "arm64 via twd.jsonc", hosts: []host{proc, ctrArm, ctrAmd}, twd: `{"containers": [{"class": "Sandbox", "arch": "arm64"}], ` + tags + `}`, wrangler: wrangler("amd64"), want: "ha"},
		{name: "microvm parent with containers", hosts: []host{vm, ctrAmd}, twd: `{"tier": "microvm", ` + tags + `}`, wrangler: wrangler(""), want: "hx"},
		{name: "no containers host", hosts: []host{proc, vm}, twd: `{` + tags + `}`, wrangler: wrangler(""), wantErr: "no_host", mention: "containers"},
		{name: "no host for the arch", hosts: []host{proc, ctrArm}, twd: `{` + tags + `}`, wrangler: wrangler("amd64"), wantErr: "no_host", mention: "amd64"},
		{name: "pinned to a host without containers", hosts: []host{vm, ctrAmd}, twd: `{` + tags + `}`, wrangler: wrangler(""), pin: "hv", wantErr: "tier_unavailable"},
		{name: "pinned to the containers host", hosts: []host{vm, ctrAmd}, twd: `{` + tags + `}`, wrangler: wrangler(""), pin: "hx", want: "hx"},
		{name: "mixed arches", hosts: []host{ctrArm, ctrAmd}, twd: `{"containers": [{"class": "Other", "image": "x", "arch": "arm64"}], ` + tags + `}`, wrangler: wrangler("amd64"), wantErr: "bad_bundle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, tn := tierTestMgmt(t)
			ctx := context.Background()
			for _, h := range tc.hosts {
				if _, _, err := m.RegisterHost(ctx, RegisterRequest{HostID: h.id, CapacityNodes: h.cap, Tiers: h.tiers, Arch: h.arch}, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			files := map[string]string{"twd.jsonc": tc.twd, "index.js": "export default {}"}
			if tc.wrangler != "" {
				files["wrangler.jsonc"] = tc.wrangler
			}
			dep, err := m.Deploy(ctx, DeployRequest{TenantID: tn.ID, Workload: "w", Host: tc.pin, Bundle: makeBundle(t, files)})
			if tc.wantErr != "" {
				var e *Error
				if !errors.As(err, &e) || e.Code != tc.wantErr || !strings.Contains(e.Message, tc.mention) {
					t.Fatalf("err = %v, want %s mentioning %q", err, tc.wantErr, tc.mention)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if dep.Host != tc.want {
				t.Fatalf("placed on %s, want %s", dep.Host, tc.want)
			}
		})
	}
}

// A workload on a host without containers that now declares them is
// refused (stickiness never keeps it where its containers can't run).
func TestContainerPlacementStickyHolder(t *testing.T) {
	m, tn := tierTestMgmt(t)
	ctx := context.Background()
	m.RegisterHost(ctx, RegisterRequest{HostID: "hp", CapacityNodes: 1000, Tiers: []string{"process"}, Arch: "amd64"}, "", "")
	m.RegisterHost(ctx, RegisterRequest{HostID: "hx", CapacityNodes: 10, Tiers: []string{"process", "microvm", "containers"}, Arch: "amd64"}, "", "")
	files := map[string]string{"twd.jsonc": `{ "tailnet": { "tags": ["tag:twd-web"] } }`, "index.js": "export default {}"}
	if dep, err := m.Deploy(ctx, DeployRequest{TenantID: tn.ID, Workload: "w", Bundle: makeBundle(t, files)}); err != nil || dep.Host != "hp" {
		t.Fatalf("first deploy: %+v %v", dep, err)
	}
	files["wrangler.jsonc"] = `{ "name": "w", "main": "index.js", "containers": [{ "class_name": "S", "image": "i" }],
		"durable_objects": { "bindings": [{ "name": "S", "class_name": "S" }] } }`
	_, err := m.Deploy(ctx, DeployRequest{TenantID: tn.ID, Workload: "w", Bundle: makeBundle(t, files)})
	var e *Error
	if !errors.As(err, &e) || e.Code != "tier_unavailable" || !strings.Contains(e.Message, "containers") {
		t.Fatalf("redeploy with containers on a host without them: %v", err)
	}
}

// TestV1ContainerReservations: the reservation API end to end over the
// spec-validated handler (and Postgres under make test-pg): host capacity
// and arch at registration and usage in heartbeats; only the lease holder
// reserves; quotas (tenant and max_instances) refuse with a typed 429;
// re-reserving is idempotent; release is the holder's; heartbeats renew,
// release and adopt on the store clock; every step is audited; sandboxes
// don't use the object quota.
func TestV1ContainerReservations(t *testing.T) {
	e := newTestEnv(t, nil)
	clk := &stepClock{now: time.Now()}
	e.store.SetClock(clk)
	c, h1, h2 := e.c, e.asHost("h1"), e.asHost("h2")
	var tn struct {
		TenantID           string  `json:"tenantID"`
		MaxContainers      int     `json:"maxContainers"`
		MaxContainerMemMiB int     `json:"maxContainerMemMiB"`
		MaxContainerVCPU   float64 `json:"maxContainerVCPU"`
		MaxObjects         int     `json:"maxObjects"`
	}
	c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile, "maxObjects": 1}, &tn)
	if tn.MaxContainers != DefaultMaxContainers || tn.MaxContainerMemMiB != DefaultMaxContainerMemMiB || tn.MaxContainerVCPU != DefaultMaxContainerVCPU {
		t.Fatalf("default container quotas: %+v", tn)
	}
	c.ok("PATCH", "/v1/tenants/"+tn.TenantID, map[string]any{"maxContainers": 3, "maxContainerVCPU": 0.5}, &tn)
	if tn.MaxContainers != 3 || tn.MaxContainerVCPU != 0.5 {
		t.Fatalf("patched quotas: %+v", tn)
	}
	if st, eb := direct(t, e, "", "PATCH", "/v1/tenants/"+tn.TenantID, map[string]any{"maxContainers": 0}); st != 400 || eb["code"] != "bad_request" {
		t.Fatalf("maxContainers 0: %d %v", st, eb) // the spec forbids it; the server refuses it too
	}

	capVec := map[string]any{"nodes": 0, "vms": 8, "vmMemMiB": 8192, "hugeMemMiB": 4096, "vcpus": 4, "vcpuOvercommit": 2, "scratchMiB": 100000}
	h1.ok("PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 100, "tiers": []string{"process", "microvm", "containers"}, "arch": "amd64", "capacity": capVec}, nil)
	h2.ok("PUT", "/v1/hosts/h2", map[string]any{"capacityNodes": 100, "tiers": []string{"process"}, "arch": "arm64"}, nil)
	bundle := makeBundle(t, map[string]string{"twd.jsonc": `{ "tailnet": { "tags": ["tag:twd-web"] } }`, "index.js": "export default {}",
		"wrangler.jsonc": `{ "name": "w", "main": "index.js", "containers": [{ "class_name": "Sandbox", "image": "./Dockerfile", "max_instances": 2 }],
			"durable_objects": { "bindings": [{ "name": "SANDBOX", "class_name": "Sandbox" }] } }`})
	var dep DeployResponse
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "sb", "bundle": bundle}, &dep)
	if dep.Host != "h1" {
		t.Fatalf("placed on %s, want the containers host h1", dep.Host)
	}
	var hv struct {
		Arch     string         `json:"arch"`
		Capacity map[string]any `json:"capacity"`
		Usage    map[string]any `json:"usage"`
	}
	c.ok("GET", "/v1/hosts/h1", nil, &hv)
	if hv.Arch != "amd64" || hv.Capacity["vms"] != float64(8) || hv.Capacity["nodes"] != float64(100) {
		t.Fatalf("host h1 capacity: %+v", hv)
	}

	lite := map[string]any{"vcpu": 0.0625, "memMiB": 256, "diskMiB": 2000}
	reserve := func(hc *apiClient, obj string, epoch int64, res map[string]any) (int, ContainerReservation, map[string]any) {
		var out ContainerReservation
		st, eb := hc.do("POST", "/v1/container-reservations", map[string]any{"tenantID": tn.TenantID, "workload": "sb", "class": "Sandbox",
			"object": obj, "epoch": epoch, "resources": res}, &out)
		return st, out, eb
	}
	st, a, _ := reserve(h1, "a", dep.Epoch, lite)
	if st != 201 || a.ID == "" || a.HostID != "h1" {
		t.Fatalf("reserve a: %d %+v", st, a)
	}
	// Only the workload's lease holder, at its epoch.
	if st, _, eb := reserve(h2, "a2", dep.Epoch, lite); st != 409 || eb["code"] != "lease_lost" {
		t.Fatalf("reserve by a non-holder: %d %v", st, eb)
	}
	if st, _, eb := reserve(h1, "a2", dep.Epoch+1, lite); st != 409 || eb["code"] != "lease_lost" {
		t.Fatalf("reserve at a wrong epoch: %d %v", st, eb)
	}
	// Idempotent: the same key is the same reservation (200).
	if st, again, _ := reserve(h1, "a", dep.Epoch, lite); st != 200 || again.ID != a.ID {
		t.Fatalf("re-reserve: %d %+v", st, again)
	}
	// Class max_instances 2.
	if st, _, _ := reserve(h1, "b", dep.Epoch, lite); st != 201 {
		t.Fatalf("reserve b: %d", st)
	}
	st, _, eb := reserve(h1, "c", dep.Epoch, lite)
	if st != 429 || eb["code"] != "container_quota_exceeded" {
		t.Fatalf("over max_instances: %d %v", st, eb)
	}
	if miss, _ := eb["missing"].(map[string]any); miss["quota"] != "max_instances" || miss["limit"] != float64(2) {
		t.Fatalf("missing: %v", eb["missing"])
	}
	// Tenant vCPU quota: 0.125 used, 0.5 allowed; a standard-1 needs 0.5.
	st, _, eb = reserve(h1, "a", dep.Epoch, map[string]any{"vcpu": 0.5, "memMiB": 4096, "diskMiB": 8000})
	if miss, _ := eb["missing"].(map[string]any); st != 429 || miss["quota"] != "maxContainerVCPU" {
		t.Fatalf("grown restart over vCPU: %d %v", st, eb)
	}
	// Sandboxes are not objects: maxObjects 1 doesn't stop them, and they
	// create none.
	var objs []Object
	c.items("/v1/objects?tenant="+tn.TenantID, &objs)
	for _, o := range objs {
		if o.Namespace == "Sandbox" {
			t.Fatalf("a container reservation created an object: %+v", o)
		}
	}

	// Release: the holder only; then it's gone.
	if st, eb := h2.do("DELETE", "/v1/container-reservations/"+a.ID, nil, nil); st != 403 || eb["code"] != "forbidden" {
		t.Fatalf("release by another host: %d %v", st, eb)
	}
	h1.ok("DELETE", "/v1/container-reservations/"+a.ID, nil, nil)
	h1.fail("DELETE", "/v1/container-reservations/"+a.ID, nil, 404, "not_found")

	// Heartbeat reconciliation: h1 reports usage, its running b, and x
	// started while mgmt was away (adopted). After 31 s without a report, b
	// is released; reports keep x.
	var mine []ContainerReservation
	hb := func(running ...string) {
		cs := []map[string]any{}
		for _, o := range running {
			cs = append(cs, map[string]any{"tenantID": tn.TenantID, "workload": "sb", "class": "Sandbox", "object": o, "resources": lite})
		}
		h1.ok("PUT", "/v1/hosts/h1/status", map[string]any{"liveNodes": 0, "leases": []map[string]any{{"namespace": LeaseName(tn.TenantID, "sb"), "epoch": dep.Epoch}},
			"usage":      map[string]any{"nodes": 0, "vms": len(running), "vmMemMiB": 256 * len(running), "hugeMemMiB": 0, "vcpus": 0.0625 * float64(len(running)), "scratchMiB": 0, "containers": len(running)},
			"containers": cs}, nil)
	}
	hb("b", "x")
	h1.items("/v1/container-reservations", &mine)
	if objsOf(mine) != "b,x" {
		t.Fatalf("after adopt: %s", objsOf(mine))
	}
	c.ok("GET", "/v1/hosts/h1", nil, &hv)
	if hv.Usage["containers"] != float64(2) {
		t.Fatalf("usage: %+v", hv.Usage)
	}
	clk.advance(10 * time.Second)
	hb("x") // b unreported for 10 s: kept (a start the host hasn't reported yet)
	h1.items("/v1/container-reservations", &mine)
	if objsOf(mine) != "b,x" {
		t.Fatalf("released within the grace: %s", objsOf(mine))
	}
	clk.advance(21 * time.Second)
	hb("x")
	h1.items("/v1/container-reservations", &mine)
	if objsOf(mine) != "x" {
		t.Fatalf("after b went unreported for 31 s: %s", objsOf(mine))
	}
	// A host lists only its own reservations.
	h2.items("/v1/container-reservations?host=h1", &mine)
	if len(mine) != 0 {
		t.Fatalf("h2 sees h1's reservations: %+v", mine)
	}
	// Audited: reserve, adopt, release (explicit and reconciled), denials.
	var page struct {
		Items []AuditRecord `json:"items"`
	}
	c.ok("GET", "/v1/audit?limit=500&tenant="+tn.TenantID, nil, &page)
	kinds := map[string]int{}
	adopted, reconciled := false, false
	for _, r := range page.Items {
		k := r.Kind
		if r.Kind == "deny" {
			k += ":" + r.Code
		}
		kinds[k]++
		d := string(r.Detail)
		if r.Kind == "container_reserve" && strings.Contains(d, `"adopted":true`) {
			adopted = true
		}
		if r.Kind == "container_release" && strings.Contains(d, "not reported") {
			reconciled = true
		}
	}
	if kinds["container_reserve"] < 4 || kinds["container_release"] < 2 || kinds["deny:lease_lost"] < 2 || kinds["deny:container_quota_exceeded"] < 2 || !adopted || !reconciled {
		t.Fatalf("audit: %v adopted=%v reconciled=%v", kinds, adopted, reconciled)
	}
}

func objsOf(rs []ContainerReservation) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Object)
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}
