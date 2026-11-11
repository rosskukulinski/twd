package hostd

import (
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/mgmt"
)

// TestMM_F9_FenceStatefulOnly (ADR-028 §3): heartbeat loss stops only
// workloads whose assignment says fence; a missing field (older mgmt) fences.
func TestMM_F9_FenceStatefulOnly(t *testing.T) {
	h := &Host{workloads: map[string]*workload{}}
	add := func(name string, fence *bool) {
		w := &workload{h: h, name: name}
		w.wantDeploy(mgmtapi.Assignment{DeploymentID: "d-" + name, Workload: name, Fence: fence})
		h.workloads["t/"+name] = w
	}
	yes, no := true, false
	add("stateful", &yes)
	add("stateless", &no)
	add("older-mgmt", nil)
	got := map[string]bool{}
	for _, w := range h.fenceTargets() {
		got[w.name] = true
	}
	if !got["stateful"] || !got["older-mgmt"] || got["stateless"] || len(got) != 2 {
		t.Fatalf("fence targets = %v, want stateful and older-mgmt only", got)
	}
}

// The host must stop serving before mgmt can hand its lease to another host.
func TestFenceBelowLease(t *testing.T) {
	const margin = 5 * time.Second
	if MaxFenceAfter+margin > mgmt.NamespaceLeaseTTL {
		t.Fatalf("MaxFenceAfter %v + margin %v must be ≤ mgmt.NamespaceLeaseTTL %v", MaxFenceAfter, margin, mgmt.NamespaceLeaseTTL)
	}
	if DefaultFenceAfter > MaxFenceAfter {
		t.Fatalf("DefaultFenceAfter %v > MaxFenceAfter %v", DefaultFenceAfter, MaxFenceAfter)
	}
	base := Config{HostID: "h", Mgmt: &mgmtapi.Client{}, DataDir: t.TempDir(), WorkerdBin: "/bin/true"}
	h, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	if h.cfg.FenceAfter != DefaultFenceAfter {
		t.Fatalf("default FenceAfter = %v, want %v", h.cfg.FenceAfter, DefaultFenceAfter)
	}
	base.FenceAfter = 50 * time.Second
	if _, err := New(base); err == nil || !strings.Contains(err.Error(), "FenceAfter") {
		t.Fatalf("FenceAfter above the max must be refused, got %v", err)
	}
}
