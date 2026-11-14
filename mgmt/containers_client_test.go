package mgmt

import (
	"context"
	"testing"

	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/internal/container"
)

// TestV1ContainerHostClient: hostd's own client (hostd/mgmtapi) against the
// real mgmt handler, spec-validated: it registers arch and the capacity
// vector, reserves and releases a container (a release of a gone
// reservation is not an error), a quota refusal arrives as a typed 429, and
// the heartbeat carries usage and the running containers.
func TestV1ContainerHostClient(t *testing.T) {
	e := newTestEnv(t, nil)
	ctx := context.Background()
	var tn struct{ TenantID string }
	e.c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile, "maxContainers": 1}, &tn)
	hc, err := mgmtapi.NewClient("unix://" + e.sock)
	if err != nil {
		t.Fatal(err)
	}
	hc.Token, hc.HostID = e.token, "h1"
	capVec := mgmtapi.HostCapacity{Nodes: 100, VMs: 4, VMMemMiB: 4096, VCPUs: 2, VCPUOvercommit: 1, ScratchMiB: 50000}
	if err := hc.Register(ctx, mgmtapi.RegisterRequest{HostID: "h1", CapacityNodes: 100, Tiers: []string{"process", "microvm", "containers"},
		Arch: "arm64", Capacity: &capVec, PushURL: "unix:///nonexistent"}); err != nil {
		t.Fatal(err)
	}
	h, err := e.store.GetHost(ctx, "h1")
	if err != nil || h.Arch != "arm64" || h.Capacity == nil || h.Capacity.VMs != 4 || !h.offers(placementNeed{Containers: true, Arch: "arm64"}) {
		t.Fatalf("registered host: %+v %v", h, err)
	}
	var dep DeployResponse
	e.c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "sb", "host": "h1",
		"bundle": makeBundle(t, map[string]string{"twd.jsonc": `{ "tailnet": { "tags": ["tag:twd-web"] } }`, "index.js": ""})}, &dep)
	req := mgmtapi.ContainerReservationRequest{TenantID: tn.TenantID, Workload: "sb", Class: "Sandbox", Object: "a", Epoch: dep.Epoch,
		Resources: container.InstanceTypes["lite"]}
	r, err := hc.ReserveContainer(ctx, req)
	if err != nil || r.ReservationID == "" || r.Host != "h1" || r.Resources != container.InstanceTypes["lite"] {
		t.Fatalf("reserve: %+v %v", r, err)
	}
	req.Object = "b"
	_, err = hc.ReserveContainer(ctx, req)
	if me, ok := mgmtapi.AsError(err); !ok || me.Status != 429 || me.Code != "container_quota_exceeded" || len(me.Missing) == 0 {
		t.Fatalf("over quota: %v", err)
	}
	usage := mgmtapi.HostUsage{VMs: 1, VMMemMiB: 256, VCPUs: 0.0625, ScratchMiB: 2000, Containers: 1}
	if _, err := hc.Heartbeat(ctx, "h1", mgmtapi.HeartbeatRequest{Leases: []mgmtapi.Lease{{Namespace: LeaseName(tn.TenantID, "sb"), Epoch: dep.Epoch}},
		Usage: &usage, Containers: []mgmtapi.RunningContainer{{ReservationID: r.ReservationID, TenantID: tn.TenantID, Workload: "sb", Class: "Sandbox", Object: "a", Resources: r.Resources}}}); err != nil {
		t.Fatal(err)
	}
	if h, _ := e.store.GetHost(ctx, "h1"); h.Usage == nil || h.Usage.Containers != 1 || h.Usage.VCPUs != 0.0625 {
		t.Fatalf("usage: %+v", h.Usage)
	}
	if err := hc.ReleaseContainer(ctx, r.ReservationID); err != nil {
		t.Fatal(err)
	}
	if err := hc.ReleaseContainer(ctx, r.ReservationID); err != nil {
		t.Fatalf("releasing a gone reservation: %v", err)
	}
	if _, err := hc.Heartbeat(ctx, "h1", mgmtapi.HeartbeatRequest{Leases: []mgmtapi.Lease{}, Containers: []mgmtapi.RunningContainer{}}); err != nil {
		t.Fatalf("heartbeat with no containers: %v", err)
	}
}
