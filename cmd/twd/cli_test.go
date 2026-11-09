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
	"github.com/rosskukulinski/twd/mgmt"
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
	// OAuth (M12): the one client this fake knows, and its tokens.
	oauthID, oauthSecret string
	oauthTokens          map[string]bool
	oauthExchanges       int
	bearers              map[string]int // bearer → requests (which credential minted)
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
	return &fakeTS{policy: p, keys: map[string]bool{}, devDel: map[string]bool{}, oauthTokens: map[string]bool{}, bearers: map[string]int{}}
}

func (f *fakeTS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	if p == "/api/v2/oauth/token" {
		r.ParseForm()
		if f.oauthSecret == "" || r.Form.Get("client_id") != f.oauthID || r.Form.Get("client_secret") != f.oauthSecret {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		f.oauthExchanges++
		tok := fmt.Sprintf("oauth-access-%d", f.oauthExchanges)
		f.oauthTokens[tok] = true
		json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "expires_in": 3600})
		return
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.bearers[bearer]++
	if strings.HasPrefix(bearer, "oauth-access-") && !f.oauthTokens[bearer] {
		w.WriteHeader(401)
		return
	}
	switch {
	case r.Method == "GET" && strings.HasSuffix(p, "/devices"):
		w.WriteHeader(403) // the OAuth client has no devices:core read: twd falls back to the netmap / --tailnet
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

// TestCLILink is M10 hermetically: `twd up` in apikey mode with mgmt's own
// tsnet node on testcontrol (unattended join), then a second state dir on
// the same machine acts as a developer box: `twd link` through its own
// tsnet node (the test transport), remote deploy / ls / kill / audit with
// deployedBy = the WhoIs principal, `logs` refused, a non-admin refused
// with the grant snippet, and --unlink.
func TestCLILink(t *testing.T) {
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
	// Every peer holds the admin cap toward every other (global caps): the
	// linked CLI is an admin. The non-admin path is covered below with a
	// fake WhoIs, since testcontrol can't grant per peer.
	ctl.Server.SetGlobalAppCaps(tailcfg.PeerCapMap{
		nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue},
		cliflow.MCPCap:  []tailcfg.RawMessage{`{"tools":["lookup_order"]}`},
		mgmt.AdminCap:   []tailcfg.RawMessage{`{}`},
	})
	fts := newFakeTS(t)
	api := httptest.NewServer(fts)
	defer api.Close()
	bin := cliflow.BuildBinaries(t)
	state, _ := hostdtest.ShortDir("twd-lk-")
	work := t.TempDir()
	keyFile := filepath.Join(work, "api.env")
	os.WriteFile(keyFile, []byte("TS_API_KEY=fake-not-a-key\n"), 0o600)
	authFile := filepath.Join(work, "auth.env")
	os.WriteFile(authFile, []byte("TS_AUTHKEY="+nettest.DummyKey+"\n"), 0o600)
	host := &cliflow.Env{T: t, BinDir: bin, StateDir: state, WorkDir: work, Tailnet: "ttest.ts.net", APIKeyFile: keyFile,
		ControlURL: ctl.URL, TSAPIURL: api.URL, Prefix: "twd-l-", Res: cliflow.NewResults("hermetic-link", "ttest.ts.net")}
	devState := filepath.Join(work, "dev-state")
	dev := &cliflow.Env{T: t, BinDir: bin, StateDir: devState, WorkDir: work, Res: host.Res}
	t.Cleanup(func() {
		if t.Failed() {
			for _, c := range []string{"supervisor", "mgmt", "hostd"} {
				b, _ := os.ReadFile(filepath.Join(state, "logs", c+".log"))
				t.Logf("%s log (tail):\n%s", c, hostdtest.Trim(string(b[max(0, len(b)-5000):]), 5000))
			}
		}
		host.Twd("down", "--purge")
		os.RemoveAll(state)
	})
	var up struct {
		MgmtNode, MgmtIP, MintMode string
		Timings                    map[string]float64 `json:"timingsMs"`
	}
	host.MustJSON(&up, "up", "--tailnet", "ttest.ts.net", "--api-key-file", keyFile, "--yes", "--ephemeral", "--control-url", ctl.URL,
		"--tsapi-url", api.URL, "--hostname-prefix", "twd-l-", "--mgmt-hostname", "twd-mgmt", "--mgmt-authkey-file", authFile)
	if up.MgmtIP == "" || up.MgmtNode == "" || up.MintMode != "apikey" {
		t.Fatalf("twd up with a mgmt node: %+v", up)
	}
	if _, se, _, err := host.Twd("up", "--wif", "--yes", "--control-url", ctl.URL); err == nil || !strings.Contains(se, "state_conflict") {
		t.Fatalf("keyless twd up on an apikey state dir should be state_conflict: %v %s", err, se)
	}
	// A second twd up (supervisor running) must not touch the node.
	host.MustJSON(nil, "up", "--tailnet", "ttest.ts.net", "--api-key-file", keyFile, "--yes", "--ephemeral", "--control-url", ctl.URL,
		"--tsapi-url", api.URL, "--hostname-prefix", "twd-l-", "--mgmt-hostname", "twd-mgmt")

	// The developer box: linked through its own tsnet node (test transport;
	// the product path is the machine's Tailscale). No HTTPS on testcontrol.
	ep := "http://" + up.MgmtIP
	// Auto-discovery resolves `twd-mgmt` through this machine's MagicDNS;
	// a developer Mac may run a real twd-mgmt there, so point it at a name
	// that can't resolve (the test never talks to the machine's tailnet).
	t.Setenv("TWD_LINK_DISCOVER_NAME", "twd-mgmt-hermetic.invalid")
	if _, se, _, err := dev.Twd("link"); err == nil || !strings.Contains(se, "mgmt_not_found") {
		t.Fatalf("link auto-discovery without MagicDNS should fail mgmt_not_found: %v %s", err, se)
	}
	var lk struct {
		Endpoint, Login, Actor string
		Admin                  bool
		Tenants                []struct{ TenantID, MintMode string }
	}
	d := dev.MustJSON(&lk, "link", ep, "--tsnet", "--tsnet-dir", filepath.Join(work, "dev-node"), "--tsnet-hostname", "ttest-dev", "--control-url", ctl.URL, "--tsnet-authkey-file", authFile)
	if !lk.Admin || lk.Endpoint != ep || len(lk.Tenants) != 1 || lk.Tenants[0].MintMode != "apikey" || lk.Actor == "" {
		t.Fatalf("twd link: %+v", lk)
	}
	host.Res.Numbers["link_ms"] = float64(d.Milliseconds())
	if _, se, _, err := dev.Twd("logs", "--component", "mgmt"); err == nil || !strings.Contains(se, "remote_unsupported") {
		t.Fatalf("remote logs: %v %s", err, se)
	}
	if _, se, _, err := dev.Twd("tenant", "add", "x.ts.net", "--api-key-file", keyFile); err == nil || !strings.Contains(se, "remote_unsupported") {
		t.Fatalf("remote tenant add: %v %s", err, se)
	}
	// Remote deploy (bundled locally, uploaded), ls with deployedBy, kill, audit.
	agentDir := filepath.Join(work, "agent")
	dev.MustJSON(nil, "init", agentDir, "--template", "agent")
	var dep struct {
		DeploymentID string
		Endpoint     struct{ Node, MagicDNS string }
	}
	d = dev.MustJSON(&dep, "deploy", agentDir)
	host.Res.Numbers["remote_deploy_ms"] = float64(d.Milliseconds())
	if dep.Endpoint.Node == "" {
		t.Fatalf("remote deploy: %+v", dep)
	}
	var ls struct {
		Deployments []struct{ Workload, DeployedBy string }
		Objects     []struct{ ObjectID, Status string }
	}
	dev.MustJSON(&ls, "ls")
	if len(ls.Deployments) != 1 || ls.Deployments[0].DeployedBy != lk.Actor || !strings.Contains(ls.Deployments[0].DeployedBy, "ttest-dev") && !strings.Contains(ls.Deployments[0].DeployedBy, "@") {
		t.Fatalf("remote ls deployedBy: %+v (actor %q)", ls.Deployments, lk.Actor)
	}
	var kr struct {
		Result struct{ Killed []string }
	}
	dev.MustJSON(&kr, "kill", dep.Endpoint.Node, "--cascade", "--reason", "m10 remote kill")
	if len(kr.Result.Killed) != 1 {
		t.Fatalf("remote kill: %+v", kr)
	}
	var aud struct {
		Verified bool
		Records  []struct{ Kind, ActingFor string }
	}
	dev.MustJSON(&aud, "audit", "--all")
	var sawDeploy, sawKill bool
	for _, r := range aud.Records {
		if r.Kind == "deploy" && r.ActingFor == lk.Actor {
			sawDeploy = true
		}
		if r.Kind == "kill" {
			sawKill = true
		}
	}
	if !aud.Verified || !sawDeploy || !sawKill {
		t.Fatalf("remote audit: verified=%v deploy=%v kill=%v (%d records)", aud.Verified, sawDeploy, sawKill, len(aud.Records))
	}
	// The host's own CLI still works locally (unix socket + token) and sees the remote deploy.
	host.MustJSON(&ls, "ls", "--all")
	if len(ls.Deployments) != 1 {
		t.Fatalf("local ls after remote deploy: %+v", ls.Deployments)
	}
	// Non-admin: a fake mgmt whose WhoIs grants nothing → not_admin + snippet.
	srv := httptest.NewServer((&mgmt.Server{M: nil}).TailnetHandler(func(ctx context.Context, addr string) (mgmt.WhoIsInfo, error) {
		return mgmt.WhoIsInfo{NodeID: "nEVE", Login: "eve@example.com"}, nil
	}, http.NotFoundHandler()))
	defer srv.Close()
	noadmin := &cliflow.Env{T: t, BinDir: bin, StateDir: filepath.Join(work, "noadmin-state"), WorkDir: work, Res: host.Res}
	_, se, _, err := noadmin.Twd("link", srv.URL)
	if err == nil || !strings.Contains(se, "not_admin") || !strings.Contains(se, mgmt.AdminCap) || !strings.Contains(se, `"dst": ["`+mgmt.MgmtTag+`"]`) || !strings.Contains(se, "eve@example.com") {
		t.Fatalf("non-admin link should print not_admin with the grant snippet: %v\n%s", err, se)
	}
	if _, err := os.Stat(filepath.Join(work, "noadmin-state", "link.json")); !os.IsNotExist(err) {
		t.Fatal("a refused link must not be saved")
	}
	// Unlink.
	dev.MustJSON(nil, "link", "--unlink")
	if _, se, _, err := dev.Twd("ls"); err == nil || !strings.Contains(se, "not_up") {
		t.Fatalf("after unlink, ls should be not_up: %v %s", err, se)
	}
	host.Down()
	b, _ := json.MarshalIndent(host.Res, "", "  ")
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
		"state_dir_writable": "ok", "mint_mode": "skip", "transport": "skip", "api_key": "skip", "sandbox": "skip", "host_sockets": "skip", "node_egress": "skip", "system_tailscaled_not_used": "ok"}
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
		// Linux host checks: present, never failing on a supported host
		// (kvm/firecracker may skip, systemd/landlock may warn). No microVM
		// guest sits next to this test build, so microvm_tier is warn (the
		// tier is off; L3's helper status is what it reports).
		for _, n := range []string{"glibc", "kernel", "landlock", "user_namespaces", "kvm", "firecracker", "cgroups_v2", "systemd", "microvm_tier"} {
			st, ok := got[n]
			if !ok || st == "fail" || (n == "microvm_tier" && st != "warn") {
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

// TestCLIOAuth is M12 hermetically: `twd up --oauth-client-file` (mint mode
// oauth: no browser, no Funnel, no API key anywhere) against testcontrol
// and a fake Tailscale API with an OAuth token endpoint. Refusals first
// (no mode → the three options; a group-readable file; a policy without
// tag:twd as owner of tag:twd-mgmt/tag:twd-host → policy_missing_tag_owners
// before anything is minted), then the whole E2E-MAC scenario (a)–(f) in
// oauth mode, with every key minted by OAuth access tokens.
func TestCLIOAuth(t *testing.T) {
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
	fts.oauthID, fts.oauthSecret = "kTestOAuthCNTRL", "tskey-"+"client-kTestOAuthCNTRL-"+strings.Repeat("S", 24)
	api := httptest.NewServer(fts)
	defer api.Close()
	bin := cliflow.BuildBinaries(t)
	state, _ := hostdtest.ShortDir("twd-oa-")
	work := t.TempDir()
	cred := filepath.Join(work, "oauth.env")
	os.WriteFile(cred, []byte("TS_OAUTH_CLIENT_ID="+fts.oauthID+"\nTS_OAUTH_CLIENT_SECRET="+fts.oauthSecret+"\n"), 0o644)
	os.Chmod(cred, 0o644)
	res := cliflow.NewResults("hermetic-oauth", "ttest.ts.net")
	var nodes []*nettest.Node
	e := &cliflow.Env{T: t, BinDir: bin, StateDir: state, WorkDir: work, Tailnet: "ttest.ts.net", OAuthClientFile: cred,
		ControlURL: ctl.URL, TSAPIURL: api.URL, Prefix: "twd-o-", Res: res,
		NewNode: func(role string) *nettest.Node {
			n, err := nettest.NewNode(context.Background(), ctl.URL, nettest.DummyKey, "ttest-"+role, filepath.Join(work, role))
			if err != nil {
				t.Fatal(err)
			}
			nodes = append(nodes, n)
			return n
		}}
	t.Cleanup(func() {
		if t.Failed() {
			for _, c := range []string{"supervisor", "mgmt", "hostd"} {
				b, _ := os.ReadFile(filepath.Join(state, "logs", c+".log"))
				t.Logf("%s log (tail):\n%s", c, hostdtest.Trim(string(b[max(0, len(b)-5000):]), 5000))
			}
		}
		e.Twd("down", "--purge")
		for _, n := range nodes {
			n.Close()
		}
		os.RemoveAll(state)
	})
	up := []string{"up", "--oauth-client-file", cred, "--tailnet", "ttest.ts.net", "--yes", "--control-url", ctl.URL, "--tsapi-url", api.URL,
		"--hostname-prefix", "twd-o-", "--mgmt-hostname", "twd-o-mgmt", "--host-id", "twd-o", "--ephemeral", "--allow-sibling-egress"}
	// 1. A group/world-readable client file is refused before anything happens.
	if _, se, _, err := e.Twd(up...); err == nil || !strings.Contains(se, "credential_file_insecure") || !strings.Contains(se, "chmod 600") {
		t.Fatalf("0644 client file: %v %s", err, se)
	}
	os.Chmod(cred, 0o600)
	// 2. Policy lacks tag:twd as an owner of the infra tags: typed error with
	// the snippet, BEFORE any key is minted or node joined.
	fts.mu.Lock()
	good := fts.policy
	fts.policy = []byte(`{"tagOwners": {"tag:twd": ["autogroup:admin"], "tag:twd-mgmt": ["autogroup:admin"]}}`)
	minted := fts.n
	fts.mu.Unlock()
	_, se, _, err := e.Twd(up...)
	if err == nil || !strings.Contains(se, "policy_missing_tag_owners") || !strings.Contains(se, `"tag:twd-mgmt": ["autogroup:admin","tag:twd"]`) {
		t.Fatalf("policy without tag:twd owners: %v %s", err, se)
	}
	fts.mu.Lock()
	if fts.n != minted {
		t.Fatalf("%d keys minted before the policy was fixed", fts.n-minted)
	}
	fts.policy = good
	fts.mu.Unlock()
	if _, err := os.Stat(filepath.Join(state, "mgmt", "tsnet", "tailscaled.state")); err == nil {
		t.Fatal("mgmt's node joined before the policy was fixed")
	}
	res.Pass("oauth_refusals", "0644 → credential_file_insecure; no tag:twd owner → policy_missing_tag_owners, nothing minted")

	// 3. The scenario (a)–(f) in oauth mode.
	e.Run()
	var cfg struct {
		MintMode, APIKeyFile, OAuthClientFile, OAuthClientID, IssuerURL string
	}
	b, _ := os.ReadFile(filepath.Join(state, "twd.json"))
	json.Unmarshal(b, &cfg)
	if cfg.MintMode != "oauth" || cfg.APIKeyFile != "" || cfg.OAuthClientFile != cred || cfg.OAuthClientID != fts.oauthID || cfg.IssuerURL != "" ||
		strings.Contains(string(b), "tskey-") {
		t.Fatalf("twd.json in oauth mode: %+v", cfg)
	}
	var doc struct {
		Checks []struct{ Name, Status, Detail string }
	}
	e.MustJSON(&doc, "doctor")
	for _, c := range doc.Checks {
		if c.Name == "mint_mode" && (c.Status != "ok" || !strings.HasPrefix(c.Detail, "oauth: OAuth client "+fts.oauthID)) {
			t.Fatalf("doctor mint_mode: %s %s", c.Status, c.Detail)
		}
		if c.Name == "api_key" && c.Status != "skip" {
			t.Fatalf("doctor api_key in oauth mode: %s %s", c.Status, c.Detail)
		}
	}
	os.Chmod(cred, 0o640)
	e.MustJSON(&doc, "doctor")
	for _, c := range doc.Checks {
		if c.Name == "mint_mode" && (c.Status != "warn" || !strings.Contains(c.Detail, "0640")) {
			t.Fatalf("doctor mint_mode with a loose file: %s %s", c.Status, c.Detail)
		}
	}
	os.Chmod(cred, 0o600)
	e.Down()
	fts.mu.Lock()
	defer fts.mu.Unlock()
	if fts.oauthExchanges == 0 || fts.bearers["fake-not-a-key"] != 0 {
		t.Fatalf("oauth exchanges %d, API-key requests %d", fts.oauthExchanges, fts.bearers["fake-not-a-key"])
	}
	for _, c := range []string{"mgmt", "hostd", "supervisor"} {
		lb, _ := os.ReadFile(filepath.Join(state, "logs", c+".log"))
		if strings.Contains(string(lb), "SSSSSSSS") || strings.Contains(string(lb), "oauth-access-") {
			t.Fatalf("%s log carries the OAuth secret or an access token", c)
		}
	}
	res.Numbers["oauth_exchanges"] = float64(fts.oauthExchanges)
	out, _ := json.MarshalIndent(res, "", "  ")
	t.Logf("results:\n%s", out)
}
