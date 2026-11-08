package main_test

// Hermetic end-to-end test of the twd CLI: the real binaries (twd,
// twd-mgmt, twd-hostd) against testcontrol + local DERP and a fake
// Tailscale API. It runs the same scenario as E2E-MAC (e2e/cliflow).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/e2e/cliflow"
	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/internal/policy"
	"github.com/rosskukulinski/twd/internal/procsb"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/tailcfg"
)

// fakeTS is the subset of the Tailscale API twd uses.
type fakeTS struct {
	mu        sync.Mutex
	policy    []byte
	etag      int
	keys      map[string]bool // id → revoked
	devDel    map[string]bool
	n         int
	setPolicy int
}

func newFakeTS(t *testing.T) *fakeTS {
	base := `{
  "tagOwners": {"tag:twd": ["autogroup:admin"], "tag:ttest": ["autogroup:admin"]},
  "grants": [{"src": ["tag:ttest"], "dst": ["tag:ttest"], "ip": ["*"], "app": {"example.com/cap/ttest": [{"role": "tester"}]}}],
}`
	p, err := policy.MergeWith([]byte(base), cliflow.Policy(), policy.Options{AllowTags: []string{cliflow.TestTag}})
	if err != nil {
		t.Fatal(err)
	}
	return &fakeTS{policy: p, keys: map[string]bool{}, devDel: map[string]bool{}}
}

func (f *fakeTS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case r.Method == "GET" && strings.HasSuffix(p, "/acl"):
		w.Header().Set("ETag", fmt.Sprintf(`"e%d"`, f.etag))
		w.Write(f.policy)
	case r.Method == "POST" && strings.HasSuffix(p, "/acl/validate"):
		w.Write([]byte(`{}`))
	case r.Method == "POST" && strings.HasSuffix(p, "/acl"):
		f.setPolicy++
		w.WriteHeader(500)
	case r.Method == "POST" && strings.HasSuffix(p, "/keys"):
		f.n++
		id := fmt.Sprintf("k%d", f.n)
		f.keys[id] = false
		json.NewEncoder(w).Encode(map[string]string{"id": id, "key": nettest.DummyKey})
	case strings.Contains(p, "/keys/"):
		id := p[strings.LastIndex(p, "/")+1:]
		revoked, ok := f.keys[id]
		switch {
		case !ok || (r.Method == "GET" && revoked):
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"not found"}`))
		case r.Method == "DELETE":
			f.keys[id] = true
		default:
			// single-use keys are consumed by the join
			json.NewEncoder(w).Encode(map[string]any{"id": id, "invalid": true})
		}
	case strings.HasPrefix(p, "/api/v2/device/"):
		id := strings.TrimPrefix(p, "/api/v2/device/")
		if r.Method == "DELETE" {
			f.devDel[id] = true
			return
		}
		if f.devDel[id] {
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"not found"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id": id, "nodeId": id})
	default:
		w.WriteHeader(404)
	}
}

func TestCLIEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	wd, err := nettest.WorkerdBinary()
	if err != nil || !hostdtest.HasNode() {
		t.Skip("needs workerd and node (make deps)")
	}
	t.Setenv("TWD_WORKERD", wd)
	ctl, err := nettest.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	ctl.Server.SetGlobalAppCaps(tailcfg.PeerCapMap{
		nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue},
		cliflow.MCPCap:  []tailcfg.RawMessage{`{"tools":["lookup_order"]}`},
	})
	fts := newFakeTS(t)
	api := httptest.NewServer(fts)
	defer api.Close()

	bin := cliflow.BuildBinaries(t)
	state, _ := hostdtest.ShortDir("twd-cli-")
	work := t.TempDir()
	keyFile := filepath.Join(work, "api.env")
	os.WriteFile(keyFile, []byte("TS_API_KEY=fake-not-a-key\n"), 0o600)
	res := cliflow.NewResults("hermetic", "ttest.ts.net")
	var nodes []*nettest.Node
	e := &cliflow.Env{T: t, BinDir: bin, StateDir: state, WorkDir: work, Tailnet: "ttest.ts.net", APIKeyFile: keyFile,
		ControlURL: ctl.URL, TSAPIURL: api.URL, Prefix: "twd-t-", Res: res,
		NewNode: func(role string) *nettest.Node {
			n, err := nettest.NewNode(context.Background(), ctl.URL, nettest.DummyKey, "ttest-"+role, filepath.Join(work, role))
			if err != nil {
				t.Fatal(err)
			}
			nodes = append(nodes, n)
			return n
		}}
	t.Cleanup(func() {
		if os.Getenv("TWD_KEEP") != "" {
			t.Logf("TWD_KEEP: state left running in %s", state)
			return
		}
		if _, _, _, err := e.Twd("status"); err == nil || t.Failed() {
			for _, c := range []string{"supervisor", "mgmt", "hostd"} {
				if t.Failed() {
					b, _ := os.ReadFile(filepath.Join(state, "logs", c+".log"))
					t.Logf("%s log (tail):\n%s", c, hostdtest.Trim(string(b[max(0, len(b)-5000):]), 5000))
				}
			}
			e.Twd("down")
		}
		for _, n := range nodes {
			n.Close()
		}
		os.RemoveAll(state)
	})
	e.Run()
	e.Down()
	// `twd down --purge && twd up && twd deploy` on the same state dir.
	e.RedeployAfterPurge()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	devs, keys := cliflow.Leftovers(ctx, &tsapi.Client{Tailnet: "ttest.ts.net", APIKey: "fake", BaseURL: api.URL}, res)
	if len(res.NodeIDs) < 5 || len(devs) > 0 || len(keys) > 0 {
		t.Fatalf("after twd down: %d node IDs tracked, leftover devices %v, keys %v", len(res.NodeIDs), devs, keys)
	}
	if fts.setPolicy != 0 {
		t.Fatalf("twd wrote the policy without --policy-apply (%d times)", fts.setPolicy)
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	t.Logf("results:\n%s", b)
}

func TestDevOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	wd, err := nettest.WorkerdBinary()
	if err != nil || !hostdtest.HasNode() {
		t.Skip("needs workerd and node (make deps)")
	}
	t.Setenv("TWD_WORKERD", wd)
	bin := cliflow.BuildBinaries(t)
	work := t.TempDir()
	e := &cliflow.Env{T: t, BinDir: bin, StateDir: filepath.Join(work, "state"), WorkDir: work}
	e.MustJSON(nil, "init", filepath.Join(work, "hello"), "--template", "hello")
	var r struct {
		Smoke struct {
			Status float64 `json:"status"`
			Caller struct {
				NodeID string `json:"nodeId"`
			} `json:"caller"`
		} `json:"smoke"`
	}
	e.MustJSON(&r, "dev", filepath.Join(work, "hello"), "--once", "--port", "0")
	if r.Smoke.Status != 200 || r.Smoke.Caller.NodeID == "" {
		t.Fatalf("twd dev --once: %+v", r)
	}
}

// TestCLIMultiTenant is E2E-MULTI hermetically: one host (twd up) serves
// tenant A (testcontrol A) and tenant B (`twd tenant add`, testcontrol B)
// with the same agent app; e2e/cliflow MultiEnv checks (a)–(f).
func TestCLIMultiTenant(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	wd, err := nettest.WorkerdBinary()
	if err != nil || !hostdtest.HasNode() {
		t.Skip("needs workerd and node (make deps)")
	}
	t.Setenv("TWD_WORKERD", wd)
	ctls := map[string]*nettest.Control{}
	for _, k := range []string{"a", "b"} {
		c, err := nettest.StartControl()
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Server.SetGlobalAppCaps(tailcfg.PeerCapMap{
			nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue},
			cliflow.MCPCap:  []tailcfg.RawMessage{`{"tools":["lookup_order"]}`}, // check (d): the narrowed child keeps lookup_order
		})
		ctls[k] = c
	}
	fts := newFakeTS(t)
	api := httptest.NewServer(fts)
	defer api.Close()
	bin := cliflow.BuildBinaries(t)
	state, _ := hostdtest.ShortDir("twd-mt-")
	work := t.TempDir()
	var nodes []*nettest.Node
	tenant := func(k string) *cliflow.Tenant {
		kf := filepath.Join(work, k+"-api.env")
		os.WriteFile(kf, []byte("TS_API_KEY=fake-not-a-key-"+k+"\n"), 0o600)
		return &cliflow.Tenant{Tailnet: k + ".ttest.ts.net", APIKeyFile: kf, ControlURL: ctls[k].URL,
			Res: cliflow.NewResults("hermetic-"+k, k+".ttest.ts.net"),
			NewNode: func(role string) *nettest.Node {
				n, err := nettest.NewNode(context.Background(), ctls[k].URL, nettest.DummyKey, "ttest-"+role, filepath.Join(work, role))
				if err != nil {
					t.Fatal(err)
				}
				nodes = append(nodes, n)
				return n
			}}
	}
	m := &cliflow.MultiEnv{Env: &cliflow.Env{T: t, BinDir: bin, StateDir: state, WorkDir: work, TSAPIURL: api.URL, Prefix: "twd-m-",
		Res: cliflow.NewResults("hermetic-multi", "a+b")}, A: tenant("a"), B: tenant("b"), RequireSandbox: sandboxAvailable()}
	t.Cleanup(func() {
		if os.Getenv("TWD_KEEP") != "" {
			t.Logf("TWD_KEEP: state left running in %s", state)
			return
		}
		if t.Failed() {
			for _, c := range []string{"supervisor", "mgmt", "hostd"} {
				b, _ := os.ReadFile(filepath.Join(state, "logs", c+".log"))
				t.Logf("%s log (tail):\n%s", c, hostdtest.Trim(string(b[max(0, len(b)-5000):]), 5000))
			}
		}
		m.Twd("down")
		for _, n := range nodes {
			n.Close()
		}
		os.RemoveAll(state)
	})
	m.Run()
	m.Down()
	if fts.setPolicy != 0 {
		t.Fatalf("twd wrote a policy without --policy-apply (%d times)", fts.setPolicy)
	}
	b, _ := json.MarshalIndent(m.Res, "", "  ")
	t.Logf("results:\n%s", b)
}

func TestDoctor(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	wd, err := nettest.WorkerdBinary()
	if err != nil {
		t.Skip("needs workerd (make deps)")
	}
	t.Setenv("TWD_WORKERD", wd)
	bin := cliflow.BuildBinaries(t)
	work := t.TempDir()
	e := &cliflow.Env{T: t, BinDir: bin, StateDir: filepath.Join(work, "state"), WorkDir: work}
	var r struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name, Status, Detail string
		} `json:"checks"`
	}
	e.MustJSON(&r, "doctor")
	got := map[string]string{}
	for _, c := range r.Checks {
		got[c.Name] = c.Status
	}
	want := map[string]string{"binaries": "ok", "workerd": "ok", "bundler": "ok", "twd_home_writable": "ok",
		"state_dir_writable": "ok", "api_key": "skip", "sandbox": "skip", "host_sockets": "skip", "node_egress": "skip", "system_tailscaled_not_used": "ok"}
	if runtime.GOOS == "linux" {
		want["host_sockets"] = "ok"
		if hs := procsb.ScanHostSockets(); len(hs.Hard) > 0 {
			want["host_sockets"] = "warn"
		} else if hs.Tailscaled != "" {
			want["host_sockets"] = "info" // blocked for workloads by seccomp (twd is not up here)
			if !sandboxAvailable() {
				want["host_sockets"] = "warn"
			}
		}
	}
	if sandboxAvailable() {
		want["sandbox"] = "ok" // the Seatbelt / Landlock probe ran and enforced a deny
	} else if runtime.GOOS == "linux" {
		want["sandbox"] = "warn" // Linux without Landlock: loud warning
	}
	if runtime.GOOS == "linux" {
		// No microVM guest next to this test build: the tier is off (warn).
		want["microvm"] = "warn"
	}
	if runtime.GOOS == "linux" {
		// Linux host checks: present, never failing on a supported host
		// (kvm/firecracker/microvm_tier may skip, systemd/landlock may warn).
		for _, n := range []string{"glibc", "kernel", "landlock", "user_namespaces", "kvm", "firecracker", "cgroups_v2", "systemd", "microvm_tier"} {
			st, ok := got[n]
			if !ok || st == "fail" {
				t.Fatalf("doctor on linux: %s = %q: %v", n, st, r.Checks)
			}
			delete(got, n)
		}
		if st := r.Checks[1]; st.Name != "glibc" || st.Status != "ok" {
			t.Fatalf("doctor on linux: glibc must run before workerd and be ok: %+v", st)
		}
	}
	if !r.OK || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("doctor: ok=%v %v", r.OK, r.Checks)
	}
	if _, err := os.Stat(filepath.Join(work, "state")); !os.IsNotExist(err) {
		t.Fatalf("doctor created the state dir")
	}
	// A key file without TS_API_KEY fails the api_key check (and the command).
	bad := filepath.Join(work, "bad.env")
	os.WriteFile(bad, []byte("NOPE=1\n"), 0o600)
	if _, se, _, err := e.Twd("doctor", "--tailnet", "example.ts.net", "--api-key-file", bad); err == nil || !strings.Contains(se, "doctor_failed") {
		t.Fatalf("doctor with a bad key file: %v %s", err, se)
	}
}

// sandboxAvailable: this host runs process-tier workloads sandboxed
// (Seatbelt on macOS, Landlock on Linux).
func sandboxAvailable() bool {
	sb, _ := hostd.ResolveSandbox(hostd.SandboxAuto)
	return sb == hostd.SandboxSeatbelt || sb == hostd.SandboxLandlock
}
