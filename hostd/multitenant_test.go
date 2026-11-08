package hostd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/tailcfg"
)

// TestMultiTenant: one hostd serves tenant A (control plane A) and tenant
// B (control plane B: another "tailnet") with the same app, workload and
// object names. Identities, state dirs and workerd processes are distinct;
// tenant A's agent can't reach tenant B's nodes by name or IP; tenant A's
// workerd can't read tenant B's state (Seatbelt, on macOS); killing A's
// object leaves B's untouched.
func TestMultiTenant(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	ctl := map[string]*nettest.Control{}
	for _, k := range []string{"a", "b"} {
		c, err := nettest.StartControl()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		c.Server.SetGlobalAppCaps(tailcfg.PeerCapMap{nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue}})
		ctl[k] = c
	}
	ea := hostdtest.Start(t, hostdtest.Options{Fake: fakemgmt.Config{StaticKey: nettest.DummyKey}, Ephemeral: true,
		KillTombstone: 3 * time.Second, Sandbox: hostd.SandboxAuto,
		Tenant: mgmtapi.Tenant{Tailnet: "a.ts.net", ControlURL: ctl["a"].URL, MintMode: "static"}})
	eb := ea.AddTenant(mgmtapi.Tenant{Tailnet: "b.ts.net", ControlURL: ctl["b"].URL, MintMode: "static"})
	if ea.TenantID == eb.TenantID {
		t.Fatal("tenant IDs collide")
	}
	ctx := context.Background()
	node := func(k, role string) *nettest.Node {
		n, err := nettest.NewNode(ctx, ctl[k].URL, nettest.DummyKey, "ttest-"+k+"-"+role, filepath.Join(ea.Dir, k+"-"+role))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { n.Close() })
		return n
	}
	envs := map[string]*hostdtest.Env{"a": ea, "b": eb}
	clients := map[string]*nettest.Node{"a": node("a", "client"), "b": node("b", "client")}
	echoA := node("a", "echo")
	if err := echoA.ServeEcho(); err != nil {
		t.Fatal(err)
	}

	// The same app, deployed under the same workload name to both tenants.
	wl := map[string]hostd.ObjectStatus{}
	for _, k := range []string{"a", "b"} {
		dep := envs[k].Deploy(hostdtest.Template("agent"), "agent", nil)
		wl[k] = envs[k].WaitActive("agent", dep, 60*time.Second)
	}
	get := func(k, ip, q string) (int, map[string]any) {
		t.Helper()
		hc := clients[k].HTTPClient(30 * time.Second)
		resp, err := hc.Get("http://" + ip + "/" + q)
		if err != nil {
			t.Fatalf("%s GET %s: %v", k, q, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}
	objs := map[string]hostd.ObjectStatus{}
	for _, k := range []string{"a", "b"} {
		waitPeerOf(t, clients[k], wl[k].IP)
		if code, m := get(k, wl[k].IP, "?agent=a1"); code != 200 || m["agent"] != "a1" {
			t.Fatalf("tenant %s agent: %d %v", k, code, m)
		}
		objs[k] = envs[k].WaitObject("agent", "SupportAgent", "a1", 60*time.Second)
	}
	// (b) Same tenant-local names, distinct identities. (fakemgmt doesn't
	// name nodes, so hostd's <workload>-<class>-<name> hostname is the same
	// in both tailnets, which is harmless; real mgmt names nodes by the
	// tenant-scoped object ID, see e2e/multi.)
	if objs["a"].ID == objs["b"].ID || objs["a"].NetdID == objs["b"].NetdID || wl["a"].ID == wl["b"].ID {
		t.Fatalf("identities collide across tenants: %+v / %+v", objs["a"], objs["b"])
	}
	t.Logf("same names, distinct identities: A %s (%s) · B %s (%s)", objs["a"].ID, objs["a"].Hostname, objs["b"].ID, objs["b"].Hostname)

	// Distinct workerd processes, per-tenant dirs, 0700.
	st := ea.Host.Status()
	ws := map[string]hostd.WorkloadStatus{}
	for _, w := range st.Workloads {
		for k, e := range envs {
			if w.TenantID == e.TenantID && w.Workload == "agent" {
				ws[k] = w
			}
		}
	}
	if ws["a"].PID == 0 || ws["a"].PID == ws["b"].PID || ws["a"].InstanceDir == ws["b"].InstanceDir || ws["a"].DOStorage == ws["b"].DOStorage {
		t.Fatalf("workerd shared across tenants: %+v / %+v", ws["a"], ws["b"])
	}
	data := filepath.Join(ea.Dir, "data")
	for k, e := range envs {
		td := filepath.Join(data, "tenants", e.TenantID)
		for _, d := range []string{filepath.Dir(td), td, filepath.Join(td, "nodes"), filepath.Join(td, "workloads"),
			filepath.Join(td, "nodes", objs[k].NetdID), ws[k].DOStorage, ws[k].InstanceDir} {
			fi, err := os.Stat(d)
			if err != nil || fi.Mode().Perm() != 0o700 {
				t.Fatalf("tenant %s dir %s: %v %v", k, d, fi.Mode(), err)
			}
		}
		if !strings.HasPrefix(ws[k].DOStorage, td+string(filepath.Separator)) {
			t.Fatalf("tenant %s DO storage %s is outside its tenant dir %s", k, ws[k].DOStorage, td)
		}
		other := envs[map[string]string{"a": "b", "b": "a"}[k]]
		if _, err := os.Stat(filepath.Join(data, "tenants", other.TenantID, "nodes", objs[k].NetdID)); err == nil {
			t.Fatalf("tenant %s's node state is in tenant %s's dir", k, other.TenantID)
		}
	}

	// (d) Tenant A's workerd sandbox can't read tenant B's state.
	if sb, _ := hostd.ResolveSandbox(hostd.SandboxAuto); sb == hostd.SandboxSeatbelt {
		if ws["a"].Sandbox != hostd.SandboxSeatbelt || ws["a"].SandboxProfile == "" {
			t.Fatalf("tenant A workerd not sandboxed: %+v", ws["a"])
		}
		bState := filepath.Join(data, "tenants", eb.TenantID)
		probe := func(args ...string) error {
			return exec.Command("/usr/bin/sandbox-exec", append([]string{"-f", ws["a"].SandboxProfile}, args...)...).Run()
		}
		for _, p := range []string{bState, filepath.Join(bState, "nodes", objs["b"].NetdID), ws["b"].DOStorage} {
			if err := probe("/bin/ls", p); err == nil {
				t.Fatalf("tenant A's sandbox can list tenant B's %s", p)
			}
		}
		if err := probe("/bin/cat", filepath.Join(data, "audit.key")); err == nil {
			t.Fatal("tenant A's sandbox can read hostd's audit key")
		}
		if home, _ := os.UserHomeDir(); home != "" {
			if err := probe("/bin/ls", home); err == nil {
				t.Fatal("tenant A's sandbox can list the home dir (API key files live there)")
			}
		}
		if err := probe("/bin/ls", ws["a"].DOStorage); err != nil {
			t.Fatalf("tenant A's sandbox can't list its own DO storage: %v", err)
		}
		t.Logf("seatbelt: tenant A's workerd profile denies tenant B's state dir, node state and DO storage")
	} else {
		t.Log("no Seatbelt on this OS: sandbox checks skipped (dirs are 0700)")
	}

	// (c) Isolation: tenant A's agent can't reach tenant B's echo by name
	// or IP. Pad control B so B's echo IP is not one tenant A uses
	// (testcontrol numbers nodes, and so IPs, sequentially per control).
	for i := range 6 {
		node("b", fmt.Sprintf("pad%d", i))
	}
	echoB := node("b", "echo")
	if err := echoB.ServeEcho(); err != nil {
		t.Fatal(err)
	}
	ebst, _ := echoB.LC.StatusWithoutPeers(ctx)
	echoBIP := ebst.Self.TailscaleIPs[0].String()
	for _, o := range ea.Host.Status().Objects {
		if o.TenantID == ea.TenantID && o.IP == echoBIP {
			t.Fatalf("precondition: B's echo IP %s is also tenant A's %s", echoBIP, o.Hostname)
		}
	}
	for _, n := range []*nettest.Node{clients["a"], echoA} {
		if s, _ := n.LC.StatusWithoutPeers(ctx); s.Self.TailscaleIPs[0].String() == echoBIP {
			t.Fatalf("precondition: B's echo IP %s is a tenant A test node", echoBIP)
		}
	}
	// Sanity: A's agent reaches A's echo; B's agent reaches B's echo.
	upstream := func(k, target string) map[string]any {
		t.Helper()
		_, m := get(k, wl[k].IP, "?agent=a1&upstream="+url.QueryEscape(target))
		u, _ := m["upstream"].(map[string]any)
		return u
	}
	if u := upstream("a", "http://ttest-a-echo/"); fmt.Sprint(u["status"]) != "200" {
		t.Fatalf("A → A echo: %v", u)
	}
	if u := upstream("b", "http://ttest-b-echo/"); fmt.Sprint(u["status"]) != "200" {
		t.Fatalf("B → B echo: %v", u)
	}
	// (B's agent by name/IP is checked by e2e/multi: here its hostname and
	// IP are tenant A's own, since both control planes number alike.)
	for _, target := range []string{"http://ttest-b-echo/", "http://ttest-b-echo.ttest.ts.net/", "http://" + echoBIP + "/"} {
		u := upstream("a", target)
		if c := fmt.Sprint(u["error"]); c != "peer_not_visible" && c != "forbidden_destination" {
			t.Fatalf("A → %s: want a typed refusal, got %v", target, u)
		}
		t.Logf("A → %s: %v", target, u["error"])
	}

	// (e) Kill tenant A's agent: tenant B's agent is unaffected.
	if _, err := ea.FM.Kill(ctx, objs["a"].ID, true, "test kill tenant A"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if code, _ := get("a", wl["a"].IP, "?agent=a1"); code == 410 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tenant A's agent still answers after the kill")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code, m := get("b", wl["b"].IP, "?agent=a1"); code != 200 || m["agent"] != "a1" {
		t.Fatalf("tenant B's agent after killing tenant A's: %d %v", code, m)
	}
	if o, ok := eb.Host.Object(eb.TenantID, "agent", "SupportAgent", "a1"); !ok || o.Status != "ready" {
		t.Fatalf("tenant B's object after the kill: %+v", o)
	}
}

func waitPeerOf(t *testing.T, n *nettest.Node, ip string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := n.WaitPeers(ctx, ip); err != nil {
		t.Fatalf("peer %s: %v", ip, err)
	}
}
