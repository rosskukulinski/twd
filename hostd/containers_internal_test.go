package hostd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/sandbox/sandboxtest"
)

func ctrHost(t *testing.T, mod func(*Config)) (*Host, *sandboxtest.Procs) {
	t.Helper()
	procs := &sandboxtest.Procs{}
	st, err := sandboxtest.ImageStore(t.TempDir(), []string{"sleep", "3600"}, "twd.test/box:1", "twd.test/other:1")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{HostID: "h", Mgmt: &mgmtapi.Client{}, DataDir: t.TempDir(), WorkerdBin: "/bin/true", Containers: procs, Images: st}
	if mod != nil {
		mod(&cfg)
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.ctr.destroyWhere(func(ctrKey) bool { return true }, "test end", 0) })
	return h, procs
}

func ctrInst(h *Host, tenant, wl string) *instance {
	w := &workload{h: h, tenantID: tenant, name: wl}
	h.mu.Lock()
	h.workloads[tenant+"/"+wl] = w
	h.mu.Unlock()
	app := &bundle.App{Classes: map[string]*bundle.Class{"Box": {Name: "Box", Node: true}, "NoCtr": {Name: "NoCtr", Node: true}},
		Containers: map[string]*bundle.ContainerClass{"Box": {Class: "Box", Image: "twd.test/box:1", InstanceType: "lite", Images: map[string]string{}}}}
	return &instance{h: h, w: w, app: app}
}

func ctrStart(t *testing.T, h *Host, inst *instance, name string, r startReq) error {
	t.Helper()
	k, cc, err := h.ctrKeyFor(inst, &ctrAs{Class: "Box", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.ctr.start(context.Background(), inst, k, cc, r)
	return err
}

func codeOf(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// No container backend: start() is refused (ADR-032 D5, no fallback),
// never run some other way.
func TestContainerNoBackendRefuses(t *testing.T) {
	h, _ := ctrHost(t, func(c *Config) { c.Containers = nil })
	err := ctrStart(t, h, ctrInst(h, "t", "w"), "a", startReq{})
	if codeOf(err) != "tier_unavailable" {
		t.Fatalf("want tier_unavailable, got %v", err)
	}
}

// Only the class's own (pinned) images start, and only container classes
// have a container.
func TestContainerImageAndClassScope(t *testing.T) {
	h, procs := ctrHost(t, nil)
	inst := ctrInst(h, "t", "w")
	if err := ctrStart(t, h, inst, "a", startReq{Image: "twd.test/other:1"}); codeOf(err) != "bad_request" || !strings.Contains(err.Error(), "not one of class Box's images") {
		t.Fatalf("undeclared image: %v", err)
	}
	if _, _, err := h.ctrKeyFor(inst, &ctrAs{Class: "NoCtr", Name: "x"}); codeOf(err) != "bad_request" {
		t.Fatalf("class without a container: %v", err)
	}
	if _, _, err := h.ctrKeyFor(inst, nil); codeOf(err) != "bad_request" {
		t.Fatalf("no caller: %v", err)
	}
	if procs.Boots.Load() != 0 {
		t.Fatal("a refused start booted a VM")
	}
	if err := ctrStart(t, h, inst, "a", startReq{}); err != nil {
		t.Fatal(err)
	}
	if err := ctrStart(t, h, inst, "a", startReq{}); codeOf(err) != "container_running" {
		t.Fatalf("second start: %v", err)
	}
	s := procs.Specs[0]
	if s.VCPUs != 1 || s.CPUMax != 1.0/16 || s.MemMiB != 256 || s.Argv[0] != "sleep" {
		t.Fatalf("lite shape / argv: %+v", s)
	}
}

// Host capacity and the mgmt quota both gate start(); a quota error fails
// closed (nothing boots).
func TestContainerCapacityAndQuota(t *testing.T) {
	h, procs := ctrHost(t, func(c *Config) { c.ContainerCapacity = ContainerCapacity{Max: 1} })
	inst := ctrInst(h, "t", "w")
	if err := ctrStart(t, h, inst, "a", startReq{}); err != nil {
		t.Fatal(err)
	}
	if err := ctrStart(t, h, inst, "b", startReq{}); codeOf(err) != "capacity_exhausted" {
		t.Fatalf("over capacity: %v", err)
	}
	h.ctr.destroy(ctrKey{"t", "w", "Box", "a"}, "test", 0)
	if err := ctrStart(t, h, inst, "b", startReq{}); err != nil {
		t.Fatalf("after a stopped: %v", err)
	}
	if procs.Live() != 1 {
		t.Fatalf("live %d", procs.Live())
	}

	var got []ContainerReservation
	released := 0
	h2, procs2 := ctrHost(t, func(c *Config) {
		c.ContainerQuota = quotaFunc(func(r ContainerReservation) (func(), error) {
			got = append(got, r)
			if r.Object == "denied" {
				return nil, errors.New("mgmt unreachable")
			}
			return func() { released++ }, nil
		})
	})
	inst2 := ctrInst(h2, "t", "w")
	if err := ctrStart(t, h2, inst2, "denied", startReq{}); codeOf(err) != "quota_exceeded" || procs2.Boots.Load() != 0 {
		t.Fatalf("quota failure must fail closed: %v (boots %d)", err, procs2.Boots.Load())
	}
	if err := ctrStart(t, h2, inst2, "ok", startReq{}); err != nil {
		t.Fatal(err)
	}
	h2.ctr.destroy(ctrKey{"t", "w", "Box", "ok"}, "test", 0)
	if released != 1 || len(got) != 2 || got[1].MemMiB != 256 || got[1].Class != "Box" {
		t.Fatalf("reservations %+v released %d", got, released)
	}
}

type quotaFunc func(ContainerReservation) (func(), error)

func (f quotaFunc) Reserve(_ context.Context, r ContainerReservation) (func(), error) { return f(r) }

// ADR-028: a container inherits its parent's fence. Heartbeat loss stops
// the stateful workload's containers (two live copies under one identity
// must not happen) and leaves a stateless workload's running.
func TestContainerFencedWithParent(t *testing.T) {
	h, procs := ctrHost(t, nil)
	stateful := ctrInst(h, "t", "w")
	stateless := ctrInst(h, "t", "w2")
	stateless.w.noFence = true
	for _, in := range []*instance{stateful, stateless} {
		if err := ctrStart(t, h, in, "a", startReq{}); err != nil {
			t.Fatal(err)
		}
	}
	h.log = h.cfg.Logger
	st := time.Now()
	h.fenceStateful("heartbeat lost")
	d := time.Since(st)
	states := map[string]string{}
	for _, c := range h.Containers() {
		states[c.Workload] = c.State
	}
	if states["w"] != "stopped" || states["w2"] != "running" || procs.Live() != 1 {
		t.Fatalf("after fence: %v (live %d)", states, procs.Live())
	}
	if d > time.Second {
		t.Fatalf("fencing the containers took %v", d)
	}
}

// The operator's hard max lifetime and the inactivity timeout stop a
// container no matter what the object does.
func TestContainerTimers(t *testing.T) {
	h, procs := ctrHost(t, func(c *Config) { c.ContainerMaxLife = 700 * time.Millisecond })
	inst := ctrInst(h, "t", "w")
	if err := ctrStart(t, h, inst, "old", startReq{}); err != nil {
		t.Fatal(err)
	}
	k := ctrKey{"t", "w", "Box", "idle"}
	if err := ctrStart(t, h, inst, "idle", startReq{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ctr.op(context.Background(), inst, k, inst.app.Containers["Box"], &ctrReq{Op: "setInactivityTimeout", MS: "300"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ctr.op(context.Background(), inst, k, inst.app.Containers["Box"], &ctrReq{Op: "setInactivityTimeout", MS: "21600001"}); codeOf(err) != "bad_request" {
		t.Fatalf("over 6 h: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && procs.Live() > 0 {
		time.Sleep(50 * time.Millisecond)
	}
	reasons := map[string]string{}
	for _, c := range h.Containers() {
		reasons[c.Object] = c.State + " " + c.ExitReason
	}
	if reasons["old"] != "stopped max lifetime" || reasons["idle"] != "stopped inactivity timeout" {
		t.Fatalf("%v", reasons)
	}
}

// An exec ID is only good for the container it was started in.
func TestContainerExecIDsAreScoped(t *testing.T) {
	h, _ := ctrHost(t, nil)
	inst := ctrInst(h, "t", "w")
	a, b := ctrKey{"t", "w", "Box", "a"}, ctrKey{"t", "w", "Box", "b"}
	h.ctr.execs["e1"] = &ctrExec{key: a}
	if _, err := h.ctr.op(context.Background(), inst, b, inst.app.Containers["Box"], &ctrReq{Op: "exec-signal", Exec: "e1", Signal: 9}); codeOf(err) != "unknown_object" {
		t.Fatalf("another object's exec: %v", err)
	}
}
