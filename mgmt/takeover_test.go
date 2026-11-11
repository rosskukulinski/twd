package mgmt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMM_F4_GuardedTakeover (ADR-028 §3): mgmt must not move a stateful
// workload off a host that stopped heartbeating (it may still be serving,
// and DO state is on its disk) unless the host was removed or the deploy
// confirms the takeover. Stateless workloads and `"fence": "never"` classes
// move freely. Moving away from a live holder stays refused (lease_held).
func TestMM_F4_GuardedTakeover(t *testing.T) {
	const (
		stateful  = `{ "tailnet": { "objects": { "SupportAgent": { "tags": ["tag:twd-agent-support"], "agent": true } } } }`
		stateless = `{ "tailnet": { "tags": ["tag:twd-web"] } }`
		fenceNo   = `{ "tailnet": { "objects": { "Cache": { "tags": ["tag:twd-cache"], "fence": "never" } } } }`
		// A DO class declared only in wrangler.jsonc is stateful too.
		wranglerOnly = `{ "durable_objects": { "bindings": [ { "name": "ROOMS", "class_name": "Room" } ] } }`
	)
	type step struct {
		name     string
		jsonc    string
		wrangler string
		removeH1 bool
		force    bool
		h1Live   bool
		wantErr  string // "" = must move to h2
	}
	for _, s := range []step{
		{name: "stateful, h1 silent", jsonc: stateful, wantErr: "takeover_unconfirmed"},
		{name: "stateful via wrangler only, h1 silent", jsonc: stateless, wrangler: wranglerOnly, wantErr: "takeover_unconfirmed"},
		{name: "stateful, forced", jsonc: stateful, force: true},
		{name: "stateful, h1 removed", jsonc: stateful, removeH1: true},
		{name: "stateful, h1 still live (pinned move)", jsonc: stateful, h1Live: true}, // refused as before F4: lease_held
		{name: "stateless", jsonc: stateless},
		{name: "fence never", jsonc: fenceNo},
	} {
		t.Run(s.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			st, err := OpenStore(ctx, filepath.Join(dir, "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			clk := &stepClock{now: time.Unix(1_800_000_000, 0)}
			st.SetClock(clk)
			cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
			m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			kf := filepath.Join(dir, "api.env")
			os.WriteFile(kf, []byte("TS_API_KEY=x\n"), 0o600)
			tn, _ := m.CreateTenant(ctx, TenantRequest{Tailnet: "t.ts.net", MintMode: "apikey", APIKeyFile: kf})
			for _, h := range []string{"h1", "h2"} {
				m.RegisterHost(ctx, RegisterRequest{HostID: h, CapacityNodes: 10, Tiers: []string{"process"}}, "", "")
			}
			files := map[string]string{"twd.jsonc": s.jsonc, "dist/index.js": "export default {}"}
			if s.wrangler != "" {
				files["wrangler.jsonc"] = s.wrangler
			}
			deploy := func(host string, force bool) (DeployResponse, error) {
				return m.Deploy(ctx, DeployRequest{TenantID: tn.ID, Workload: "w", Host: host, ForceTakeover: force, Bundle: makeBundle(t, files)})
			}
			if _, err := deploy("h1", false); err != nil {
				t.Fatal(err)
			}
			if s.h1Live {
				clk.advance(2 * time.Second)
				m.Heartbeat(ctx, "h1", 0, nil)
				m.Heartbeat(ctx, "h2", 0, nil)
				// Pre-existing and safe: a live holder keeps its lease, so a
				// pinned move is refused with lease_held, and the message says
				// how to move it (drain), not "wait for the lease".
				_, err := deploy("h2", false)
				var e *Error
				if !errors.As(err, &e) || e.Code != "lease_held" || !strings.Contains(e.Message, "live") || !strings.Contains(e.Message, "twd host rm h1 --drain") {
					t.Fatalf("pinned move away from a live holder: want lease_held explaining the drain, got %v", err)
				}
				return
			}
			clk.advance(NamespaceLeaseTTL + 5*time.Second) // h1 silent past its lease (and its fence); h2 alive
			m.Heartbeat(ctx, "h2", 0, nil)
			if s.removeH1 {
				if _, err := m.DeleteHost(ctx, "h1", true, "test"); err != nil {
					t.Fatal(err)
				}
			}
			r, err := deploy("", s.force)
			if s.wantErr != "" {
				var e *Error
				if !errors.As(err, &e) || e.Code != s.wantErr || e.Status != 409 {
					t.Fatalf("want 409 %s, got %+v %v", s.wantErr, r, err)
				}
				for _, w := range []string{"h1", "--force-takeover", "twd host rm h1"} {
					if !strings.Contains(e.Message, w) {
						t.Errorf("message should mention %q: %s", w, e.Message)
					}
				}
				return
			}
			if err != nil || r.Host != "h2" {
				t.Fatalf("want a move to h2, got %+v %v", r, err)
			}
			if s.force {
				recs, err := st.ListAudit(ctx, AuditFilter{TenantID: tn.ID, Kinds: []string{"deploy"}})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, a := range recs {
					if d := string(a.Detail); a.Kind == "deploy" && strings.Contains(d, `"takeoverFrom":"h1"`) && strings.Contains(d, `"forced":true`) {
						found = true
					}
				}
				if !found {
					t.Error("a forced takeover must be audited with takeoverFrom and forced")
				}
			}
		})
	}
}

func TestBundleStateful(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"workers only", map[string]string{"twd.jsonc": `{"tailnet":{"tags":["tag:w"]}}`}, false},
		{"twd objects", map[string]string{"twd.jsonc": `{"tailnet":{"objects":{"A":{"tags":["tag:a"]}}}}`}, true},
		{"all fence never", map[string]string{"twd.jsonc": `{"tailnet":{"objects":{"A":{"tags":["tag:a"],"fence":"never"},"B":{"node":false,"fence":"never"}}}}`}, false},
		{"one class not exempt", map[string]string{"twd.jsonc": `{"tailnet":{"objects":{"A":{"tags":["tag:a"],"fence":"never"},"B":{"node":false}}}}`}, true},
		{"wrangler-only DO", map[string]string{"twd.jsonc": `{"tailnet":{"tags":["tag:w"]}}`, "wrangler.json": `{"durable_objects":{"bindings":[{"name":"X","class_name":"X"}]}}`}, true},
		{"wrangler DO exempted in twd.jsonc", map[string]string{"twd.jsonc": `{"tailnet":{"objects":{"X":{"node":false,"fence":"never"}}}}`, "wrangler.jsonc": `{"durable_objects":{"bindings":[{"name":"X","class_name":"X"}]}}`}, false},
		{"wrangler.toml: assume stateful", map[string]string{"twd.jsonc": `{"tailnet":{"tags":["tag:w"]}}`, "wrangler.toml": "name = \"x\""}, true},
		{"deps don't count", map[string]string{"twd.jsonc": `{"tailnet":{"tags":["tag:w"]}}`, "node_modules/pkg/wrangler.toml": "x"}, false},
	} {
		if got, why := BundleStateful(makeBundle(t, tc.files)); got != tc.want {
			t.Errorf("%s: stateful=%v (%s), want %v", tc.name, got, why, tc.want)
		}
	}
}

// TestMM_F9_AssignmentFence: deployments record whether the host must fence
// them (BundleStateful) and assignments carry it (ADR-028 §3).
func TestMM_F9_AssignmentFence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := OpenStore(ctx, filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	kf := filepath.Join(dir, "api.env")
	os.WriteFile(kf, []byte("TS_API_KEY=x\n"), 0o600)
	tn, _ := m.CreateTenant(ctx, TenantRequest{Tailnet: "t.ts.net", MintMode: "apikey", APIKeyFile: kf})
	m.RegisterHost(ctx, RegisterRequest{HostID: "h1", CapacityNodes: 10, Tiers: []string{"process"}}, "", "")
	for _, tc := range []struct {
		workload, jsonc string
		fence           bool
	}{
		{"web", `{"tailnet":{"tags":["tag:w"]}}`, false},
		{"agents", `{"tailnet":{"objects":{"A":{"tags":["tag:a"]}}}}`, true},
		{"cache", `{"tailnet":{"objects":{"C":{"tags":["tag:c"],"fence":"never"}}}}`, false},
	} {
		if _, err := m.Deploy(ctx, DeployRequest{TenantID: tn.ID, Workload: tc.workload,
			Bundle: makeBundle(t, map[string]string{"twd.jsonc": tc.jsonc, "dist/index.js": "export default {}"})}); err != nil {
			t.Fatal(err)
		}
	}
	as, err := m.Assignments(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"web": false, "agents": true, "cache": false}
	for _, a := range as {
		if a.Fence != want[a.Workload] {
			t.Errorf("%s: assignment fence = %v, want %v", a.Workload, a.Fence, want[a.Workload])
		}
		delete(want, a.Workload)
	}
	if len(want) != 0 {
		t.Errorf("missing assignments: %v", want)
	}
}

// TestMM_F12_IdentityConflictStillRefused: with the ID secret shared, a
// by-ID hit for the same identity is a concurrent create (idempotent), but a
// different identity holding the ID is still refused.
func TestMM_F12_IdentityConflictStillRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := OpenStore(ctx, filepath.Join(dir, "m.db"))
	defer st.Close()
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	kf := filepath.Join(dir, "api.env")
	os.WriteFile(kf, []byte("TS_API_KEY=x\n"), 0o600)
	tn, _ := m.CreateTenant(ctx, TenantRequest{Tailnet: "t.ts.net", MintMode: "apikey", APIKeyFile: kf})
	m.RegisterHost(ctx, RegisterRequest{HostID: "h1", CapacityNodes: 10, Tiers: []string{"process"}}, "", "")
	if _, err := m.Deploy(ctx, DeployRequest{TenantID: tn.ID, Workload: "w",
		Bundle: makeBundle(t, map[string]string{"twd.jsonc": testTwdJSONC, "dist/index.js": "export default {}"})}); err != nil {
		t.Fatal(err)
	}
	id := m.objectID(tn.ID, "w", "SupportAgent", "alice")
	// Another identity already holds alice's ID (a MAC prefix collision).
	if err := st.CreateObject(ctx, Object{ID: id, TenantID: tn.ID, Workload: "w", Namespace: "SupportAgent", Name: "mallory",
		Hostname: id, Status: StatusPending}); err != nil {
		t.Fatal(err)
	}
	_, err = m.CreateObject(ctx, CreateObjectRequest{TenantID: tn.ID, Workload: "w", Namespace: "SupportAgent", ObjectName: "alice"})
	var e *Error
	if !errors.As(err, &e) || e.Code != "identity_conflict" {
		t.Fatalf("a different identity holding the ID must be refused with identity_conflict, got %v", err)
	}
}

// TestMM_F13_DeploymentStatusShared (H08): the host-reported status is
// stored, so every replica reports the latest one, including after the host
// alternated between replicas (a per-replica cache must not skip the write).
func TestMM_F13_DeploymentStatusShared(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := OpenStore(ctx, filepath.Join(dir, "m.db"))
	defer st.Close()
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	mk := func() *Mgmt {
		m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
		return m
	}
	a, b := mk(), mk()
	kf := filepath.Join(dir, "api.env")
	os.WriteFile(kf, []byte("TS_API_KEY=x\n"), 0o600)
	tn, _ := a.CreateTenant(ctx, TenantRequest{Tailnet: "t.ts.net", MintMode: "apikey", APIKeyFile: kf})
	a.RegisterHost(ctx, RegisterRequest{HostID: "h1", CapacityNodes: 10, Tiers: []string{"process"}}, "", "")
	d, err := a.Deploy(ctx, DeployRequest{TenantID: tn.ID, Workload: "w",
		Bundle: makeBundle(t, map[string]string{"twd.jsonc": testTwdJSONC, "dist/index.js": "export default {}"})})
	if err != nil {
		t.Fatal(err)
	}
	report := func(m *Mgmt, status string) {
		m.Heartbeat(ctx, "h1", 1, nil, DeploymentStatus{DeploymentID: d.DeploymentID, Status: status})
	}
	want := func(status string) {
		t.Helper()
		for i, m := range []*Mgmt{a, b} {
			v, err := m.GetDeployment(ctx, d.DeploymentID)
			if err != nil || v.Status != status {
				t.Fatalf("replica %d: status %q (%v), want %q", i, v.Status, err, status)
			}
		}
	}
	want("assigned")
	report(a, "active")
	want("active")
	report(b, "failed")
	want("failed")
	report(a, "active") // a last wrote "active" itself; it must still write
	want("active")
}
