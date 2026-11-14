//go:build e2eplacement

// Package placement is the hermetic smart-placement e2e (ADR-030): real
// twd-mgmt and two real twd-hostd processes on testcontrol + a local DERP,
// a "database" peer tagged tag:db, and one host forced onto DERP
// (TS_DEBUG_ALWAYS_USE_DERP=1 in that hostd's environment, so its nodes
// never use direct UDP). Nothing talks to the internet; no real keys.
package placement

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/e2e/cliflow"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/mgmt"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/tsnet"
)

const fakeToken = "fake-api-token"

// fakeTS is the smallest Tailscale API mgmt needs: it mints keys
// testcontrol accepts (any string) and deletes them.
func fakeTS(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != fakeToken {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/keys"):
			mu.Lock()
			n++
			id := fmt.Sprintf("k%06d", n)
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"id": id, "key": "fake-authkey-" + id})
		case r.Method == "DELETE":
			w.WriteHeader(200)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/acl"):
			w.Header().Set("ETag", `"p1"`)
			w.Write([]byte(`{"tagOwners": {"tag:ttest": ["autogroup:admin"]}}`))
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/devices"):
			json.NewEncoder(w).Encode(map[string]any{"devices": []any{}})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type proc struct {
	name string
	cmd  *exec.Cmd
	log  string
	done chan struct{}
}

type env struct {
	t        *testing.T
	bin, dir string
	ctl      *nettest.Control
	sock     string
	token    string
	hc       *http.Client
	mu       sync.Mutex
	procs    map[string]*proc
	workerd  string
}

func (e *env) start(name string, extraEnv []string, args ...string) *proc {
	e.t.Helper()
	logf := filepath.Join(e.dir, name+".log")
	lf, err := os.OpenFile(logf, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		e.t.Fatal(err)
	}
	bin := "twd-hostd"
	if name == "mgmt" {
		bin = "twd-mgmt"
	}
	cmd := exec.Command(filepath.Join(e.bin, bin), args...)
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	p := &proc{name: name, cmd: cmd, log: logf, done: make(chan struct{})}
	go func() { cmd.Wait(); lf.Close(); close(p.done) }()
	e.mu.Lock()
	e.procs[name] = p
	e.mu.Unlock()
	return p
}

// stop terminates a process this test started (and its process group:
// workerd children), waiting for it to exit.
func (e *env) stop(name string) {
	e.mu.Lock()
	p := e.procs[name]
	delete(e.procs, name)
	e.mu.Unlock()
	if p == nil {
		return
	}
	syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
}

func (e *env) startHost(id string, forceDERP bool) {
	var envv []string
	if forceDERP {
		envv = append(envv, "TS_DEBUG_ALWAYS_USE_DERP=1")
	}
	hd := filepath.Join(e.dir, id)
	e.start(id, envv, "-local-dev", "-mgmt", "unix://"+e.sock, "-mgmt-token-file", filepath.Join(e.dir, "token"),
		"-host-id", id, "-data-dir", filepath.Join(hd, "data"), "-run-dir", filepath.Join(hd, "run"),
		"-control-url", e.ctl.URL, "-ephemeral", "-heartbeat", "500ms", "-path-every", "2s",
		"-sandbox", "none", "-workerd", e.workerd, "-hostname-prefix", id+"-", "-log-level", "debug")
}

func (e *env) do(method, path string, body, out any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	rq, _ := http.NewRequest(method, "http://mgmt"+path, rd)
	rq.Header.Set(mgmt.HostTokenHeader, e.token)
	rq.Header.Set("content-type", "application/json")
	resp, err := e.hc.Do(rq)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode/100 == 2 {
		json.Unmarshal(b, out)
	}
	return resp.StatusCode, b
}

func (e *env) must(method, path string, body, out any) {
	e.t.Helper()
	if st, b := e.do(method, path, body, out); st/100 != 2 {
		e.t.Fatalf("%s %s: %d %s", method, path, st, b)
	}
}

func (e *env) dumpLogs() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, name := range []string{"mgmt", "h1", "h2"} {
		b, _ := os.ReadFile(filepath.Join(e.dir, name+".log"))
		lines := strings.Split(string(b), "\n")
		if len(lines) > 60 {
			lines = lines[len(lines)-60:]
		}
		e.t.Logf("---- %s.log (tail)\n%s", name, strings.Join(lines, "\n"))
	}
}

type deployment struct {
	DeploymentID, Host, Status, Error string
	Epoch                             int64
	Placement                         mgmt.PlacementDecision
	Endpoint                          struct{ Node, MagicDNS, Status string }
}

func (e *env) waitActive(id string, timeout time.Duration) deployment {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var d deployment
		e.must("GET", "/v1/deployments/"+id, nil, &d)
		// The entry node must be the one on the deployment's host (after a
		// move the object briefly still names the old host's node).
		if d.Status == "active" && d.Endpoint.Status == "ready" && strings.HasPrefix(d.Endpoint.MagicDNS, d.Host+"-") {
			return d
		}
		if d.Status == "failed" || time.Now().After(deadline) {
			e.dumpLogs()
			e.t.Fatalf("deployment %s: status %s endpoint %s: %s", id, d.Status, d.Endpoint.Status, d.Error)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// hostStatus reads a hostd's local status (its push socket).
func (e *env) hostWorkloads(id string) map[string]string {
	sock := filepath.Join(e.dir, id, "run", "hostd.sock")
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := hc.Get("http://hostd/v1/status")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var st struct {
		Workloads []struct{ Workload, Status string }
	}
	json.NewDecoder(resp.Body).Decode(&st)
	out := map[string]string{}
	for _, w := range st.Workloads {
		out[w.Workload] = w.Status
	}
	return out
}

func TestSmartPlacementHermetic(t *testing.T) {
	wd, err := nettest.WorkerdBinary()
	if err != nil {
		t.Skip(err)
	}
	if !hostdtest.HasNode() {
		t.Skip("node not available (needed to bundle the app)")
	}
	dir, err := hostdtest.ShortDir("twd-sp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !t.Failed() || os.Getenv("TWD_KEEP") == "" {
			os.RemoveAll(dir)
		}
	})
	ctl, err := nettest.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctl.Close)
	ts := fakeTS(t)
	e := &env{t: t, bin: cliflow.BuildBinaries(t), dir: dir, ctl: ctl, sock: filepath.Join(dir, "mgmt.sock"), procs: map[string]*proc{}, workerd: wd}
	t.Cleanup(func() {
		for _, n := range []string{"h1", "h2", "mgmt"} {
			e.stop(n)
		}
	})
	e.hc = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", e.sock)
	}}}

	// The "database": a tsnet node tagged tag:db serving HTTP.
	db := &tsnet.Server{Hostname: "pg-1", ControlURL: ctl.URL, AuthKey: nettest.DummyKey, Ephemeral: true, Store: new(mem.Store),
		Dir: filepath.Join(dir, "db"), AdvertiseTags: []string{"tag:db"}, Logf: func(string, ...any) {}, UserLogf: func(string, ...any) {}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := db.Up(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dbLC, _ := db.LocalClient()
	dbLC.OmitAuth = true

	// mgmt (SQLite) and two hosts: h1 forced onto DERP, h2 normal.
	keyFile := filepath.Join(dir, "api.env")
	os.WriteFile(keyFile, []byte("TS_API_KEY="+fakeToken+"\n"), 0o600)
	e.start("mgmt", nil, "-local-dev", "-socket", e.sock, "-host-token-file", filepath.Join(dir, "token"), "-rotate-host-token",
		"-state-dir", filepath.Join(dir, "mgmt"), "-tsapi-url", ts.URL, "-reap-interval", "1h")
	for i := 0; ; i++ {
		b, err := os.ReadFile(filepath.Join(dir, "token"))
		if _, serr := os.Stat(e.sock); err == nil && len(bytes.TrimSpace(b)) == 64 && serr == nil {
			e.token = string(bytes.TrimSpace(b))
			break
		}
		if i > 100 {
			e.dumpLogs()
			t.Fatal("mgmt did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	var tn struct{ TenantID string }
	e.must("POST", "/v1/tenants", map[string]any{"tailnet": nettest.MagicDNSDomain, "controlURL": ctl.URL, "mintMode": "apikey", "apiKeyFile": keyFile}, &tn)
	e.startHost("h1", true)
	e.startHost("h2", false)
	waitHosts := func(ids ...string) {
		t.Helper()
		for i := 0; ; i++ {
			var pg struct {
				Items []struct{ HostID, Status string }
			}
			e.must("GET", "/v1/hosts", nil, &pg)
			live := 0
			for _, h := range pg.Items {
				for _, id := range ids {
					if h.HostID == id && h.Status == "live" {
						live++
					}
				}
			}
			if live == len(ids) {
				return
			}
			if i > 200 {
				e.dumpLogs()
				t.Fatalf("hosts %v not live: %+v", ids, pg.Items)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitHosts("h1", "h2")

	// The app: a Worker that declares it connects to tag:db.
	app := filepath.Join(dir, "app")
	os.MkdirAll(filepath.Join(app, "src"), 0o755)
	os.WriteFile(filepath.Join(app, "twd.jsonc"), []byte(`{"name": "pgapp", "tailnet": {"tags": ["tag:ttest"], "connects": ["tag:db"]}}`), 0o644)
	os.WriteFile(filepath.Join(app, "wrangler.jsonc"), []byte(`{"name": "pgapp", "main": "src/index.js", "compatibility_date": "2026-04-01"}`), 0o644)
	os.WriteFile(filepath.Join(app, "src", "index.js"), []byte(`export default { async fetch(req) { return new Response("pgapp ok\n"); } };`), 0o644)
	files, err := hostdtest.Build(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	bundle := base64.StdEncoding.EncodeToString(hostdtest.MustTar(files))
	deploy := func(body map[string]any) (deployment, int, []byte) {
		t.Helper()
		body["tenantID"], body["bundle"] = tn.TenantID, bundle
		var d deployment
		st, b := e.do("POST", "/v1/deployments", body, &d)
		return d, st, b
	}
	mustDeploy := func(body map[string]any) deployment {
		t.Helper()
		d, st, b := deploy(body)
		if st != 202 {
			e.dumpLogs()
			t.Fatalf("deploy %v: %d %s", body["workload"], st, b)
		}
		return d
	}

	// Tenant presence on both hosts: a seed workload pinned to each (their
	// nodes are the vantage points; nothing is minted to measure).
	s1 := mustDeploy(map[string]any{"workload": "seed1", "host": "h1"})
	s2 := mustDeploy(map[string]any{"workload": "seed2", "host": "h2"})
	e.waitActive(s1.DeploymentID, 90*time.Second)
	e.waitActive(s2.DeploymentID, 90*time.Second)

	// Read the shared store the way any replica would.
	st, err := mgmt.OpenStore(context.Background(), filepath.Join(dir, "mgmt", "mgmt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	waitPaths := func(want map[string]string, since time.Time, timeout time.Duration) map[string]mgmt.PathMeasurement {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			ms, _ := st.HostPaths(context.Background(), tn.TenantID, []string{"tag:db"})
			got := map[string]mgmt.PathMeasurement{}
			ok := true
			for _, m := range ms {
				got[m.HostID] = m
			}
			for h, p := range want {
				m, has := got[h]
				ok = ok && has && m.Path == p && time.UnixMilli(m.MeasuredAt).After(since)
			}
			if ok {
				return got
			}
			if time.Now().After(deadline) {
				e.dumpLogs()
				t.Fatalf("paths %+v, want %v (measured after %v)", got, want, since.Format(time.RFC3339))
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	p1 := waitPaths(map[string]string{"h1": "derp", "h2": "direct"}, time.Time{}, 60*time.Second)
	t.Logf("MEASURED phase 1: h1 %s %.3f ms (DERP %q, peer %s) · h2 %s %.3f ms (peer %s)",
		p1["h1"].Path, float64(p1["h1"].RTTMicros)/1000, p1["h1"].DERPRegion, p1["h1"].Peer, p1["h2"].Path, float64(p1["h2"].RTTMicros)/1000, p1["h2"].Peer)

	// 1. First deploy of the app lands on the direct host, h2.
	a1 := mustDeploy(map[string]any{"workload": "pgapp"})
	t.Logf("DEPLOY 1: ✓ placed     %s", a1.Placement.Summary)
	if a1.Host != "h2" || a1.Placement.Reason != "smart" {
		t.Fatalf("first deploy on %s (%s), want h2 smart", a1.Host, a1.Placement.Summary)
	}
	if !strings.Contains(a1.Placement.Summary, "tag:db direct") || !strings.Contains(a1.Placement.Summary, "h1: relayed") {
		t.Fatalf("explanation: %q", a1.Placement.Summary)
	}
	a1 = e.waitActive(a1.DeploymentID, 90*time.Second)
	hc := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DialContext: db.Dial}}
	get := func(host string) string {
		t.Helper()
		for i := 0; ; i++ {
			resp, err := hc.Get("http://" + host + "/")
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return string(b)
				}
			}
			if i > 30 {
				e.dumpLogs()
				t.Fatalf("GET %s from the db node: %v", host, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if b := get(a1.Endpoint.MagicDNS); !strings.Contains(b, "pgapp ok") {
		t.Fatalf("app on h2: %q", b)
	}

	// 2. The other host improves (h1 restarts without the DERP knob) and
	// h2 degrades (restarts with it): h1 is now clearly better. A plain
	// redeploy still keeps the app on h2 (sticky).
	e.stop("h1")
	e.stop("h2")
	flip := time.Now()
	e.startHost("h1", false)
	e.startHost("h2", true)
	waitHosts("h1", "h2")
	for i := 0; ; i++ { // h2 resumes its assignment (same epoch)
		if e.hostWorkloads("h2")["pgapp"] == "active" && e.hostWorkloads("h1")["seed1"] == "active" {
			break
		}
		if i > 300 {
			e.dumpLogs()
			t.Fatalf("workloads after restart: h1 %v h2 %v", e.hostWorkloads("h1"), e.hostWorkloads("h2"))
		}
		time.Sleep(200 * time.Millisecond)
	}
	p2 := waitPaths(map[string]string{"h1": "direct", "h2": "derp"}, flip, 60*time.Second)
	t.Logf("MEASURED phase 2: h1 %s %.3f ms · h2 %s %.3f ms (DERP %q)",
		p2["h1"].Path, float64(p2["h1"].RTTMicros)/1000, p2["h2"].Path, float64(p2["h2"].RTTMicros)/1000, p2["h2"].DERPRegion)
	a2 := mustDeploy(map[string]any{"workload": "pgapp"})
	t.Logf("DEPLOY 2 (plain): ✓ placed     %s", a2.Placement.Summary)
	if a2.Host != "h2" || a2.Placement.Reason != "sticky" || a2.Epoch != a1.Epoch+1 {
		t.Fatalf("plain redeploy moved: %s %s epoch %d (was %d)", a2.Host, a2.Placement.Summary, a2.Epoch, a1.Epoch)
	}
	a2 = e.waitActive(a2.DeploymentID, 90*time.Second)
	if e.hostWorkloads("h1")["pgapp"] == "active" {
		t.Fatal("pgapp runs on h1 without --replace")
	}

	// 3. The explicit re-place moves it: the lease goes to h1 at epoch+1,
	// h1 runs its normal deploy, h2 is revoked and stops.
	moveAt := time.Now()
	a3 := mustDeploy(map[string]any{"workload": "pgapp", "replace": true})
	t.Logf("DEPLOY 3 (--replace): ✓ placed     %s", a3.Placement.Summary)
	if a3.Host != "h1" || a3.Placement.Reason != "smart" || a3.Epoch != a2.Epoch+1 {
		t.Fatalf("replace: %s %s epoch %d", a3.Host, a3.Placement.Summary, a3.Epoch)
	}
	a3 = e.waitActive(a3.DeploymentID, 90*time.Second)
	activeAfter := time.Since(moveAt)
	if b := get(a3.Endpoint.MagicDNS); !strings.Contains(b, "pgapp ok") {
		t.Fatalf("app on h1: %q", b)
	}
	var stoppedAfter time.Duration
	for i := 0; ; i++ {
		if s := e.hostWorkloads("h2")["pgapp"]; s != "active" && s != "starting" {
			stoppedAfter = time.Since(moveAt)
			break
		}
		if i > 100 {
			t.Fatalf("h2 still serves pgapp after the move: %v", e.hostWorkloads("h2"))
		}
		time.Sleep(100 * time.Millisecond)
	}
	l, err := st.GetLease(context.Background(), mgmt.LeaseName(tn.TenantID, "pgapp"))
	if err != nil || l.Holder != "h1" || l.Epoch != a3.Epoch {
		t.Fatalf("lease after move: %+v %v", l, err)
	}
	var old deployment
	e.must("GET", "/v1/deployments/"+a2.DeploymentID, nil, &old)
	if old.Status != "superseded" {
		t.Fatalf("old deployment status %s", old.Status)
	}
	// The move is audited with its reason.
	var audit struct {
		Items []struct {
			Kind   string
			Detail map[string]any
		}
	}
	e.must("GET", "/v1/audit?limit=500&kind=deploy", nil, &audit)
	found := false
	for _, r := range audit.Items {
		if r.Detail["deploymentID"] == a3.DeploymentID && r.Detail["movedFrom"] == "h2" && r.Detail["replace"] == true {
			found = true
		}
	}
	if !found {
		t.Fatal("no deploy audit record with movedFrom h2")
	}
	t.Logf("MOVE: new deployment active+ready on h1 %.2fs after --replace; h2 stopped serving %.2fs after", activeAfter.Seconds(), stoppedAfter.Seconds())
	sc := bufio.NewScanner(strings.NewReader(a3.Placement.Summary))
	for sc.Scan() {
		t.Log("  ", sc.Text())
	}
}
