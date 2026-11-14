package hostd_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/netd/nettest"
)

// TestMicroVMRefusedWithoutBackend (ADR-032 D5b): a "tier": "microvm"
// deployment reaching a host with no microVM backend is refused with
// tier_unavailable: nothing of it runs (no workerd, no workload node), and
// mgmt sees the typed failure on the deployment. Before, hostd ran it in the
// process tier with only a log warning.
func TestMicroVMRefusedWithoutBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	ctl, err := nettest.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctl.Close)
	e := hostdtest.Start(t, hostdtest.Options{Fake: fakemgmt.Config{StaticKey: nettest.DummyKey}, Ephemeral: true,
		Tenant: mgmtapi.Tenant{Tailnet: "a.ts.net", ControlURL: ctl.URL, MintMode: "static"}})
	files, err := hostdtest.Build(hostdtest.Template("agent"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := bundle.UnmarshalJSONC(files["twd.jsonc"], &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["tier"] = "microvm"
	files["twd.jsonc"], _ = json.Marshal(cfg)
	dep := e.DeployFiles(files, "agent")
	deadline := time.Now().Add(30 * time.Second)
	for {
		var ws *hostd.WorkloadStatus
		for _, w := range e.Host.Status().Workloads {
			if w.Workload == "agent" {
				ws = &w
			}
		}
		if ws != nil && ws.Status == "active" {
			t.Fatalf("a microvm deployment runs on a host without a microVM backend (tier %q)", ws.Tier)
		}
		if ws != nil && ws.Status == "failed" {
			if !strings.HasPrefix(ws.Error, "tier_unavailable:") {
				t.Fatalf("failed for another reason: %s", ws.Error)
			}
			if ws.PID != 0 || ws.InstanceDir != "" || ws.Tier != "" {
				t.Fatalf("something of the refused deployment runs: %+v", ws)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the microvm deployment neither failed nor ran")
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, o := range e.Host.Status().Objects {
		if o.Workload == "agent" {
			t.Fatalf("the refused workload got a node: %+v", o)
		}
	}
	// mgmt sees it on the deployment (heartbeat report), typed.
	deadline = time.Now().Add(10 * time.Second)
	for {
		var d mgmtapi.DeployResponse
		if err := e.Mgmt.Do(context.Background(), "GET", "/v1/deployments/"+dep, nil, &d); err != nil {
			t.Fatal(err)
		}
		if d.Status == "failed" && strings.HasPrefix(d.Error, "tier_unavailable:") {
			t.Logf("mgmt: %s %s", d.Status, d.Error)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("mgmt never saw the refusal: %+v", d)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
