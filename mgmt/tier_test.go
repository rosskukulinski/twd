package mgmt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPlacementTier: placement sends a workload only to hosts that offer its
// tier (twd.jsonc "tier", else the tenant's default). Before this, pickHost
// ignored Host.Tiers, so a "microvm" workload could land on a process-only
// host, where hostd fell back to the process tier with only a log warning.
func TestPlacementTier(t *testing.T) {
	const (
		microvm = `{ "tier": "microvm", "tailnet": { "tags": ["tag:twd-web"] } }`
		process = `{ "tier": "process", "tailnet": { "tags": ["tag:twd-web"] } }`
		unset   = `{ "tailnet": { "tags": ["tag:twd-web"] } }`
		node    = `{ "runtime": "node", "tailnet": { "tags": ["tag:twd-web"] } }`
	)
	type host struct {
		id    string
		cap   int
		tiers []string
	}
	procBig := host{"hp", 1000, []string{"process"}}     // most free capacity
	vm := host{"hv", 10, []string{"process", "microvm"}} // the only microvm host
	legacy := host{"hl", 5000, nil}                      // lists no tiers: process only
	for _, tc := range []struct {
		name       string
		hosts      []host
		jsonc      string
		tenantTier string
		want       string // host ID, or "" with wantErr
		pin        string // --host
		wantErr    string
	}{
		{name: "microvm skips the roomier process host", hosts: []host{procBig, vm}, jsonc: microvm, want: "hv"},
		{name: "process goes by capacity", hosts: []host{procBig, vm}, jsonc: process, want: "hp"},
		{name: "unset goes by capacity", hosts: []host{procBig, vm}, jsonc: unset, want: "hp"},
		{name: "tenant default microvm applies", hosts: []host{procBig, vm}, jsonc: unset, tenantTier: "microvm", want: "hv"},
		{name: "bundle process overrides tenant microvm", hosts: []host{procBig, vm}, jsonc: process, tenantTier: "microvm", want: "hp"},
		{name: "node runtime is process-tier", hosts: []host{procBig, vm}, jsonc: node, tenantTier: "microvm", want: "hp"},
		{name: "host listing no tiers is process only", hosts: []host{legacy, vm}, jsonc: microvm, want: "hv"},
		{name: "no microvm host", hosts: []host{procBig, legacy}, jsonc: microvm, wantErr: "no_host"},
		{name: "pinned to a process host", hosts: []host{procBig, vm}, jsonc: microvm, pin: "hp", wantErr: "tier_unavailable"},
		{name: "pinned to the microvm host", hosts: []host{procBig, vm}, jsonc: microvm, pin: "hv", want: "hv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, tn := tierTestMgmt(t)
			ctx := context.Background()
			for _, h := range tc.hosts {
				if _, _, err := m.RegisterHost(ctx, RegisterRequest{HostID: h.id, CapacityNodes: h.cap, Tiers: h.tiers}, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			if tc.tenantTier != "" {
				if _, err := m.UpdateTenant(ctx, tn.ID, TenantPatch{Tier: &tc.tenantTier}); err != nil {
					t.Fatal(err)
				}
			}
			files := map[string]string{"twd.jsonc": tc.jsonc, "dist/index.js": "export default {}"}
			if tc.jsonc == node {
				files = map[string]string{"twd.jsonc": tc.jsonc, "package.json": `{"name":"w"}`, "index.js": ""}
			}
			dep, err := m.Deploy(ctx, DeployRequest{TenantID: tn.ID, Workload: "w", Host: tc.pin, Bundle: makeBundle(t, files)})
			if tc.wantErr != "" {
				var e *Error
				if !errors.As(err, &e) || e.Code != tc.wantErr {
					t.Fatalf("err = %v, want %s", err, tc.wantErr)
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

// A workload whose live host lacks the tier it now asks for is refused with
// a fix, not left on the wrong tier (stickiness would otherwise keep it).
func TestPlacementTierStickyHolder(t *testing.T) {
	m, tn := tierTestMgmt(t)
	ctx := context.Background()
	m.RegisterHost(ctx, RegisterRequest{HostID: "hp", CapacityNodes: 1000, Tiers: []string{"process"}}, "", "")
	m.RegisterHost(ctx, RegisterRequest{HostID: "hv", CapacityNodes: 10, Tiers: []string{"process", "microvm"}}, "", "")
	deploy := func(jsonc string) (DeployResponse, error) {
		return m.Deploy(ctx, DeployRequest{TenantID: tn.ID, Workload: "w",
			Bundle: makeBundle(t, map[string]string{"twd.jsonc": jsonc, "dist/index.js": "export default {}"})})
	}
	dep, err := deploy(`{ "tailnet": { "tags": ["tag:twd-web"] } }`)
	if err != nil || dep.Host != "hp" {
		t.Fatalf("first deploy: %+v %v", dep, err)
	}
	_, err = deploy(`{ "tier": "microvm", "tailnet": { "tags": ["tag:twd-web"] } }`)
	var e *Error
	if !errors.As(err, &e) || e.Code != "tier_unavailable" {
		t.Fatalf("redeploy as microvm on a process holder: err = %v, want tier_unavailable", err)
	}
}

func tierTestMgmt(t *testing.T) (*Mgmt, Tenant) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := OpenStore(ctx, filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	kf := filepath.Join(dir, "api.env")
	os.WriteFile(kf, []byte("TS_API_KEY=x\n"), 0o600)
	tn, err := m.CreateTenant(ctx, TenantRequest{Tailnet: "t.ts.net", MintMode: "apikey", APIKeyFile: kf})
	if err != nil {
		t.Fatal(err)
	}
	return m, tn
}
