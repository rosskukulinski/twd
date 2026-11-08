//go:build nodetier

// make test-node: the opt-in Node process tier (runtime "node") and Vercel
// Workflow on it, hermetic (testcontrol + local DERP, fakemgmt, real hostd,
// real node). Two hosts: A runs the Node apps, B runs the twd MCP server the
// workflow's agent calls over the tailnet.
package node_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/internal/procsb"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/tailcfg"
)

const mcpCap = "example.com/cap/mcp"

type env struct {
	t        *testing.T
	ctl      *nettest.Control
	a, b     *hostdtest.Env
	client   *nettest.Node
	echo     *nettest.Node
	hc       *http.Client
	clientID string
	results  map[string]any
	mu       sync.Mutex
}

func (e *env) record(k string, v any) {
	e.mu.Lock()
	e.results[k] = v
	e.mu.Unlock()
	e.t.Logf("measure %s = %v", k, v)
}

func start(t *testing.T) *env {
	if _, err := os.Stat(hostdtest.NodeDeps()); err != nil {
		t.Skip("node tier deps not installed (make test-node runs npm ci in e2e/node)")
	}
	// A canary in hostd's environment must never reach a Node app.
	t.Setenv("TWD_TEST_CANARY_SECRET", "canary")
	ctl, err := nettest.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctl.Close)
	ctl.Server.SetGlobalAppCaps(tailcfg.PeerCapMap{
		nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue},
		mcpCap:          []tailcfg.RawMessage{`{"tools":["lookup_order"]}`},
	})
	opts := func(prefix string) hostdtest.Options {
		// Sandbox "auto" is what twd-hostd runs by default (Seatbelt on macOS).
		return hostdtest.Options{ControlURL: ctl.URL, Ephemeral: true, HostnamePrefix: prefix, Sandbox: hostd.SandboxAuto,
			Tenant: mgmtapi.Tenant{Tailnet: "ttest.ts.net", ControlURL: ctl.URL, MintMode: "static"}}
	}
	oa, ob := opts("a-"), opts("b-")
	oa.Fake.StaticKey, ob.Fake.StaticKey = nettest.DummyKey, nettest.DummyKey
	e := &env{t: t, ctl: ctl, results: map[string]any{}}
	e.a = hostdtest.Start(t, oa)
	e.b = hostdtest.Start(t, ob)
	ctx := context.Background()
	var wg sync.WaitGroup
	var cerr, eerr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		e.client, cerr = nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-client", filepath.Join(e.a.Dir, "client"))
	}()
	go func() {
		defer wg.Done()
		e.echo, eerr = nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-echo", filepath.Join(e.a.Dir, "echo"))
	}()
	wg.Wait()
	if cerr != nil || eerr != nil {
		t.Fatal(cerr, eerr)
	}
	t.Cleanup(func() { e.client.Close(); e.echo.Close() })
	if err := e.echo.ServeEcho(); err != nil {
		t.Fatal(err)
	}
	st, _ := e.client.LC.StatusWithoutPeers(ctx)
	e.clientID = string(st.Self.ID)
	e.hc = e.client.HTTPClient(30 * time.Second)
	t.Cleanup(func() {
		b, _ := json.MarshalIndent(map[string]any{"when": time.Now().UTC().Format(time.RFC3339), "goos": runtime.GOOS, "goarch": runtime.GOARCH,
			"node": nodeVersion(), "results": e.results}, "", "  ")
		os.MkdirAll("results", 0o755)
		os.WriteFile(filepath.Join("results", fmt.Sprintf("m9-%s-%s-%s.json", t.Name(), runtime.GOOS, time.Now().UTC().Format("20060102T150405Z"))), b, 0o644)
	})
	return e
}

func nodeVersion() string {
	b, _ := exec.Command("node", "-v").Output()
	return strings.TrimSpace(string(b))
}

func (e *env) waitPeer(ip string) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.client.WaitPeers(ctx, ip); err != nil {
		e.t.Fatal(err)
	}
}

func do(c *http.Client, method, u string, body any, hdr ...string) (int, http.Header, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, u, rd)
	req.Header.Set("content-type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func obj(b []byte) map[string]any {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return map[string]any{"raw": string(b)}
	}
	return m
}

func get(m any, path ...string) any {
	for _, p := range path {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[p]
	}
	return m
}

func (e *env) nodeStatus(h *hostdtest.Env, workload string) hostd.WorkloadStatus {
	for _, w := range h.Host.Status().Workloads {
		if w.Workload == workload {
			return w
		}
	}
	e.t.Fatalf("no workload %s", workload)
	return hostd.WorkloadStatus{}
}

func rssMiB(pid int) float64 {
	b, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return -1
	}
	kb, _ := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return kb / 1024
}

// hostIP is a non-loopback address of this machine (SSRF target).
func hostIP() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !strings.HasPrefix(n.IP.String(), "100.") {
			return n.IP.String()
		}
	}
	return "192.168.1.1"
}

func TestNodeTier(t *testing.T) {
	e := start(t)
	depStart := time.Now()
	dep := e.a.DeployNode(hostdtest.Fixture("nodeprobe"), "nodeprobe", map[string]string{"PROBE_GREETING": "hello-var"})
	wl := e.a.WaitActive("nodeprobe", dep, 60*time.Second)
	e.waitPeer(wl.IP)
	base := "http://" + wl.IP
	code, _, b := do(e.hc, "GET", base+"/", nil)
	if code != 200 {
		t.Fatalf("first request: %d %s", code, b)
	}
	ws := e.nodeStatus(e.a, "nodeprobe")
	if ws.Runtime != "node" || ws.Node == nil || ws.Node.PID == 0 {
		t.Fatalf("status: %+v", ws)
	}
	e.record("nodeprobe.deploy_to_first_200_ms", time.Since(depStart).Milliseconds())
	e.record("nodeprobe.cold_start_spawn_to_healthy_ms", ws.Node.ReadyMs)

	t.Run("VerifiedCaller", func(t *testing.T) {
		code, _, b := do(e.hc, "GET", base+"/whoami", nil)
		m := obj(b)
		if code != 200 || get(m, "claims", "caller", "nodeId") != e.clientID {
			t.Fatalf("whoami: %d %s", code, b)
		}
		if get(m, "claims", "workload") != "nodeprobe" || get(m, "greeting") != "hello-var" {
			t.Fatalf("claims/vars: %s", b)
		}
		if caps, _ := get(m, "claims", "caller", "capMap").(map[string]any); caps[nettest.TestCap] == nil {
			t.Fatalf("capMap missing the test cap: %s", b)
		}
		// A forged header from a tailnet client is stripped by netd: the app
		// still sees the real caller.
		forged := "v1." + base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"caller":{"nodeId":"nEVIL"},"iat":`+strconv.FormatInt(time.Now().UnixMilli(), 10)+`}`)) + ".AAAA"
		code, _, b = do(e.hc, "GET", base+"/whoami", nil, "x-twd-caller", forged, "x-twd-cf", `{"tailscale":{"caller":{"nodeId":"nEVIL"}}}`)
		if code != 200 || get(obj(b), "claims", "caller", "nodeId") != e.clientID {
			t.Fatalf("forged via tailnet: %d %s", code, b)
		}
	})

	t.Run("ForgedHeaderRejected", func(t *testing.T) {
		// Bypass netd entirely (another local process dialling the app's
		// port): no header, a forged one, and one signed with the wrong key
		// are all 401.
		ws := e.nodeStatus(e.a, "nodeprobe")
		direct := &http.Client{Timeout: 5 * time.Second}
		wrongKey := make([]byte, 32)
		rand.Read(wrongKey)
		wrong := signLike(wrongKey, map[string]any{"v": 1, "caller": map[string]any{"nodeId": "nEVIL"}, "iat": time.Now().UnixMilli()})
		for name, h := range map[string]string{"none": "", "forged": "v1.eyJ2IjoxfQ.AAAA", "wrong-key": wrong} {
			var hdr []string
			if h != "" {
				hdr = []string{"x-twd-caller", h}
			}
			code, _, b := do(direct, "GET", "http://"+ws.Node.Addr+"/whoami", nil, hdr...)
			if code != 401 || get(obj(b), "code") != "no_identity" {
				t.Fatalf("%s: want 401 no_identity, got %d %s", name, code, b)
			}
		}
		// The optional-caller endpoint sees null.
		code, _, b := do(direct, "GET", "http://"+ws.Node.Addr+"/maybe", nil, "x-twd-caller", wrong)
		if code != 200 || get(obj(b), "caller") != nil {
			t.Fatalf("maybe: %d %s", code, b)
		}
	})

	t.Run("EnvHygiene", func(t *testing.T) {
		_, _, b := do(e.hc, "GET", base+"/proc", nil)
		keys := fmt.Sprint(get(obj(b), "env"))
		for _, want := range []string{"TWD_CALLER_KEY", "TWD_EGRESS_PROXY", "HTTP_PROXY", "HTTPS_PROXY", "TWD_DATA_DIR"} {
			if !strings.Contains(keys, want) {
				t.Fatalf("env %s missing: %s", want, keys)
			}
		}
		if strings.Contains(keys, "CANARY") {
			t.Fatalf("hostd's environment leaked into the app: %s", keys)
		}
	})

	t.Run("Sandbox", func(t *testing.T) {
		// Node apps are tenant code too: on macOS they run under the same
		// Seatbelt profile shape as workerd (own instance + data dir only).
		// On Linux: Landlock (+ seccomp), the same allow list plus TCP ports.
		ws := e.nodeStatus(e.a, "nodeprobe")
		sb, _ := hostd.ResolveSandbox(hostd.SandboxAuto)
		if sb == hostd.SandboxNone {
			if ws.Sandbox != hostd.SandboxNone {
				t.Fatalf("sandbox %q on a host without one", ws.Sandbox)
			}
			t.Skip("no process sandbox on this OS (Node tier dirs are 0700; the microVM tier is the isolation answer)")
		}
		if ws.Sandbox != sb || ws.SandboxProfile == "" {
			t.Fatalf("node app not sandboxed: %q %q (want %s)", ws.Sandbox, ws.SandboxProfile, sb)
		}
		fsop := func(op, path string) map[string]any {
			code, _, b := do(e.hc, "GET", base+"/fs?op="+op+"&path="+url.QueryEscape(path), nil)
			if code != 200 {
				t.Fatalf("fs %s %s: %d %s", op, path, code, b)
			}
			return obj(b)
		}
		// Own data dir: writable and readable.
		own := filepath.Join(ws.Node.DataDir, "probe.txt")
		if m := fsop("write", own); m["ok"] != true {
			t.Fatalf("write own data dir: %v", m)
		}
		if m := fsop("read", own); m["ok"] != true {
			t.Fatalf("read own data dir: %v", m)
		}
		// hostd's data dir (node keys, other tenants' state), a canary in it,
		// hostd's other run dirs and the home dir: denied.
		canary := filepath.Join(e.a.Dir, "data", "canary.txt")
		if err := os.WriteFile(canary, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		home, _ := os.UserHomeDir()
		denied := map[string][2]string{
			"hostd_data_canary": {"read", canary},
			"hostd_data_list":   {"list", filepath.Join(e.a.Dir, "data")},
			"hostd_data_write":  {"write", filepath.Join(e.a.Dir, "data", "evil.txt")},
			"tenants_list":      {"list", filepath.Join(e.a.Dir, "data", "tenants")},
			"home_list":         {"list", home},
		}
		for name, c := range denied {
			// Seatbelt: EPERM; Landlock: EACCES.
			if m := fsop(c[0], c[1]); m["ok"] != false || (m["code"] != "EPERM" && m["code"] != "EACCES") {
				t.Fatalf("%s (%s %s) not denied: %v", name, c[0], c[1], m)
			}
		}
		rec := map[string]any{"sandbox": ws.Sandbox, "own_rw": true, "denied": len(denied)}
		if sb == hostd.SandboxLandlock {
			rec["landlock"] = e.landlockNodeChecks(t, ws, base)
		}
		e.record("nodeprobe.sandbox", rec)
	})

	t.Run("EgressAttributed", func(t *testing.T) {
		echoURL := "http://" + e.echoIP() + "/"
		for _, mode := range []string{"sdk", "global", "http", "undici"} {
			st := time.Now()
			code, _, b := do(e.hc, "GET", base+"/egress?mode="+mode+"&url="+url.QueryEscape(echoURL), nil)
			m := obj(b)
			if code != 200 || get(m, "status") != float64(200) {
				t.Fatalf("%s: %d %s", mode, code, b)
			}
			var rep nettest.EchoReply
			json.Unmarshal([]byte(get(m, "body").(string)), &rep)
			if rep.Caller.NodeID != wl.NodeID {
				t.Fatalf("%s: egress left as %q, want the deployment's node %q", mode, rep.Caller.NodeID, wl.NodeID)
			}
			e.record("egress."+mode+"_ms", time.Since(st).Milliseconds())
		}
	})

	t.Run("SSRFBlocked", func(t *testing.T) {
		ws := e.nodeStatus(e.a, "nodeprobe")
		_, proxyPort, _ := net.SplitHostPort(ws.Node.ProxyAddr)
		cases := map[string]string{
			"loopback":       "http://127.0.0.1:" + proxyPort + "/",
			"loopback-v6":    "http://[::1]/",
			"metadata":       "http://169.254.169.254/latest/meta-data/",
			"host":           "http://" + hostIP() + "/",
			"self":           "http://" + wl.IP + "/",
			"internet":       "http://1.1.1.1/",
			"localhost-name": "http://localhost/",
		}
		for name, u := range cases {
			// sdk: absolute form; connect: a CONNECT tunnel (env proxies); global:
			// fetch() via the global dispatcher (env proxy or installGlobalDispatcher).
			for _, mode := range []string{"sdk", "connect", "global"} {
				code, _, b := do(e.hc, "GET", base+"/egress?mode="+mode+"&url="+url.QueryEscape(u), nil)
				m := obj(b)
				te, _ := get(m, "twdError").(string)
				typed := code == 200 && get(m, "status") == float64(403) && (te == "forbidden_destination" || te == "peer_not_visible")
				if !typed && !(mode == "global" && code == 500) { // a refused CONNECT makes fetch() throw
					t.Fatalf("%s/%s: want a typed 403 forbidden_destination|peer_not_visible, got %d %s", name, mode, code, b)
				}
			}
		}
		// The proxy itself needs the per-process credentials.
		pu, _ := url.Parse("http://" + ws.Node.ProxyAddr)
		noauth := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
		code, _, _ := do(noauth, "GET", "http://"+e.echoIP()+"/", nil)
		if code != http.StatusProxyAuthRequired {
			t.Fatalf("proxy without credentials: want 407, got %d", code)
		}
		bad, _ := url.Parse("http://twd:wrong@" + ws.Node.ProxyAddr)
		badc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(bad)}}
		if code, _, _ := do(badc, "GET", "http://"+e.echoIP()+"/", nil); code != http.StatusProxyAuthRequired {
			t.Fatalf("proxy with a wrong token: want 407, got %d", code)
		}
	})

	t.Run("SupervisionAndLogs", func(t *testing.T) {
		_, _, b := do(e.hc, "GET", base+"/proc", nil)
		pid1 := int(get(obj(b), "pid").(float64))
		e.record("nodeprobe.rss_mib", rssMiB(pid1))
		do(e.hc, "GET", base+"/log?m=m9-marker", nil)
		killAt := time.Now()
		do(e.hc, "GET", base+"/crash", nil)
		var pid2 int
		for time.Since(killAt) < 20*time.Second {
			code, _, b := do(e.hc, "GET", base+"/proc", nil)
			if code == 200 {
				if p := int(get(obj(b), "pid").(float64)); p != pid1 {
					pid2 = p
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		if pid2 == 0 {
			t.Fatal("app was not restarted")
		}
		e.record("nodeprobe.crash_to_serving_ms", time.Since(killAt).Milliseconds())
		ws := e.nodeStatus(e.a, "nodeprobe")
		if ws.Node.Starts < 2 || ws.Status != "active" {
			t.Fatalf("status after restart: %+v %+v", ws, ws.Node)
		}
		// The caller key rotated with the process: whoami still verifies.
		if code, _, b := do(e.hc, "GET", base+"/whoami", nil); code != 200 {
			t.Fatalf("whoami after restart: %d %s", code, b)
		}
		lb, _ := os.ReadFile(ws.LogFile)
		if !strings.Contains(string(lb), "probe-log-line m9-marker") {
			t.Fatalf("log %s lacks the app's output: %q", ws.LogFile, hostdtest.Trim(string(lb), 400))
		}
		if strings.Contains(string(lb), "tskey-") {
			t.Fatal("a key in the app log")
		}
	})
}

func syscallKill(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }

func (e *env) echoIP() string {
	return e.echo.IP4
}

// signLike signs claims the way hostd does, under an arbitrary key.
func signLike(key []byte, claims any) string {
	return hostd.SignCaller(key, claims)
}

func TestNodeWorkflow(t *testing.T) {
	e := start(t)
	dMCP := e.b.Deploy(hostdtest.Template("mcp"), "mcp", nil)
	depStart := time.Now()
	dWF := e.a.DeployNode(hostdtest.Template("node-workflow"), "node-workflow", nil)
	e.record("workflow.build_and_deploy_ms", time.Since(depStart).Milliseconds())
	mcp := e.b.WaitActive("mcp", dMCP, 60*time.Second)
	wf := e.a.WaitActive("node-workflow", dWF, 60*time.Second)
	e.waitPeer(mcp.IP)
	e.waitPeer(wf.IP)
	ws := e.nodeStatus(e.a, "node-workflow")
	e.record("workflow.cold_start_spawn_to_healthy_ms", ws.Node.ReadyMs)
	if sb, _ := hostd.ResolveSandbox(hostd.SandboxAuto); ws.Sandbox != sb {
		t.Fatalf("workflow sandbox %q, want %q", ws.Sandbox, sb)
	}
	e.record("workflow.sandbox", ws.Sandbox)
	base := "http://" + wf.IP

	// The API needs a verified caller.
	if code, _, b := do(&http.Client{Timeout: 5 * time.Second}, "POST", "http://"+ws.Node.Addr+"/refunds", map[string]any{"order": "o1"}); code != 401 {
		t.Fatalf("direct, no caller: want 401, got %d %s", code, b)
	}
	runStart := time.Now()
	code, _, b := do(e.hc, "POST", base+"/refunds", map[string]any{"order": "o1", "usd": 10, "mcp": "http://" + mcp.IP + "/mcp", "sleep": "2s"})
	if code != 202 {
		t.Fatalf("start: %d %s", code, b)
	}
	runID, _ := get(obj(b), "runId").(string)
	token, _ := get(obj(b), "approveToken").(string)
	e.record("workflow.start_ms", time.Since(runStart).Milliseconds())
	pid1 := ws.Node.PID
	e.record("workflow.rss_mib", rssMiB(pid1))

	// Kill the node process mid-workflow (during the durable sleep, after the
	// agent's tool calls), let hostd restart it, and wait for the run to
	// reach the approval hook.
	time.Sleep(900 * time.Millisecond)
	killAt := time.Now()
	if err := syscallKill(pid1); err != nil {
		t.Fatal(err)
	}
	var readyAt time.Time
	for time.Since(killAt) < 30*time.Second {
		if s := e.nodeStatus(e.a, "node-workflow"); s.Node != nil && s.Node.PID != 0 && s.Node.PID != pid1 {
			if code, _, _ := do(&http.Client{Timeout: 2 * time.Second}, "GET", "http://"+s.Node.Addr+"/healthz", nil); code == 200 {
				readyAt = time.Now()
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if readyAt.IsZero() {
		t.Fatal("node-workflow not restarted after kill")
	}
	e.record("workflow.kill_to_restarted_ms", readyAt.Sub(killAt).Milliseconds())
	for {
		code, _, _ := do(e.hc, "GET", base+"/hooks/"+url.PathEscape(token), nil)
		if code == 200 {
			break
		}
		if time.Since(killAt) > 60*time.Second {
			t.Fatalf("run never reached the hook after restart (last %d)", code)
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.record("workflow.kill_to_hook_ms", time.Since(killAt).Milliseconds())

	// Kill again while the run waits on the hook; resume it on the new process.
	pid2 := e.nodeStatus(e.a, "node-workflow").Node.PID
	kill2 := time.Now()
	syscallKill(pid2)
	for time.Since(kill2) < 30*time.Second {
		s := e.nodeStatus(e.a, "node-workflow")
		if s.Node != nil && s.Node.PID != 0 && s.Node.PID != pid2 {
			if code, _, _ := do(e.hc, "GET", base+"/healthz", nil); code == 200 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	resumeAt := time.Now()
	if code, _, b := do(e.hc, "POST", base+"/hooks/"+url.PathEscape(token), nil); code != 200 {
		t.Fatalf("resume hook: %d %s", code, b)
	}
	var res map[string]any
	for time.Since(resumeAt) < 60*time.Second {
		_, _, b := do(e.hc, "GET", base+"/runs/"+runID, nil)
		res = obj(b)
		if s := get(res, "status"); s == "completed" || s == "failed" || s == "cancelled" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if get(res, "status") != "completed" {
		t.Fatalf("run: %v", res)
	}
	e.record("workflow.hook_resume_to_completed_ms", time.Since(resumeAt).Milliseconds())
	e.record("workflow.total_ms", time.Since(runStart).Milliseconds())
	r := get(res, "result").(map[string]any)
	calls, _ := r["calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("calls: %v", r)
	}
	c0, c1 := calls[0].(map[string]any), calls[1].(map[string]any)
	// Authorized by the caller's caps: the workflow's node holds
	// {"tools":["lookup_order"]}, so lookup succeeds and refund is cap_missing.
	if c0["tool"] != "lookup_order" || c0["isError"] != false || !strings.Contains(c0["text"].(string), "shipped") {
		t.Fatalf("lookup_order: %v", c0)
	}
	if c1["tool"] != "refund" || c1["isError"] != true || c1["code"] != "cap_missing" {
		t.Fatalf("refund should be cap_missing: %v", c1)
	}
	// The MCP server authorized the deployment's own node (its denial names
	// the caller chain), not the host or another identity.
	if !strings.Contains(c1["text"].(string), wf.NodeID) {
		t.Fatalf("MCP caller is not the workflow's node %s: %v", wf.NodeID, c1["text"])
	}
	if !strings.Contains(fmt.Sprint(get(r, "approval", "by")), "ttest-client") || r["triage"] != "triaged o1" {
		t.Fatalf("result: %v", r)
	}
	marks, _ := r["marks"].([]any)
	pids := map[float64]bool{}
	for _, m := range marks {
		pids[m.(map[string]any)["pid"].(float64)] = true
	}
	if len(marks) != 3 || len(pids) != 3 {
		t.Fatalf("want 3 marks from 3 processes (killed twice): %v", marks)
	}
	// Resume latency: the first step after the sleep ran at "slept"; it could
	// not run before both the new process was up and the 2 s sleep was due.
	triaged := int64(marks[0].(map[string]any)["at"].(float64))
	slept := int64(marks[1].(map[string]any)["at"].(float64))
	e.record("workflow.resume_after_restart_ms", slept-max(readyAt.UnixMilli(), triaged+2000))
	e.record("workflow.mcp_caller_node", wf.NodeID)
}

// RT-08: the Node tier's egress proxy is an enforcement boundary only under
// a sandbox that confines the process's TCP (Landlock); elsewhere status says
// `Egress: "unenforced"`. On a host with NO process sandbox at all, a
// Node-tier deployment for a second tenant is refused (egress_unenforced)
// unless the operator passes -insecure-shared-host.
func TestNodeTierSharedHostRefused(t *testing.T) {
	e := start(t)
	dep := e.a.DeployNode(hostdtest.Fixture("nodeprobe"), "nodeprobe", nil)
	e.a.WaitActive("nodeprobe", dep, 60*time.Second)
	ws := e.nodeStatus(e.a, "nodeprobe")
	if ws.Egress != hostd.EgressEnforcement(ws.Sandbox) || (ws.Sandbox == hostd.SandboxLandlock) != (ws.Egress == hostd.EgressEnforced) {
		t.Fatalf("status Egress=%q Sandbox=%q", ws.Egress, ws.Sandbox)
	}
	// A host without any process sandbox, serving a second tenant.
	o := hostdtest.Options{ControlURL: e.ctl.URL, Ephemeral: true, HostnamePrefix: "n-", Sandbox: hostd.SandboxNone,
		Tenant: mgmtapi.Tenant{Tailnet: "ttest.ts.net", ControlURL: e.ctl.URL, MintMode: "static"}}
	o.Fake.StaticKey = nettest.DummyKey
	c := hostdtest.Start(t, o)
	c.Deploy(hostdtest.Fixture("probe"), "probe", nil) // tenant A's workload on the host
	b := c.AddTenant(mgmtapi.Tenant{Tailnet: "tenant-b.ts.net", ControlURL: e.ctl.URL, MintMode: "static"})
	b.DeployNode(hostdtest.Fixture("nodeprobe"), "nodeprobe", nil)
	deadline := time.Now().Add(60 * time.Second)
	for {
		st := b.Host.Status()
		var found *hostd.WorkloadStatus
		for i := range st.Workloads {
			if st.Workloads[i].TenantID == b.TenantID && st.Workloads[i].Workload == "nodeprobe" {
				found = &st.Workloads[i]
			}
		}
		if found != nil && found.Status == "failed" && strings.Contains(found.Error, "egress_unenforced") {
			t.Logf("refused: %s", found.Error)
			break
		}
		if found != nil && found.Status == "failed" {
			t.Fatalf("refused for the wrong reason: %s", found.Error)
		}
		if found != nil && found.Status == "active" {
			t.Fatal("tenant B's Node-tier deployment ran on an unsandboxed shared host without -insecure-shared-host")
		}
		if time.Now().After(deadline) {
			t.Fatalf("tenant B's deployment neither failed nor ran: %+v", found)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The operator's override lets it run (and status still says unenforced).
	o2 := o
	o2.HostnamePrefix, o2.InsecureSharedHost = "i-", true
	c2 := hostdtest.Start(t, o2)
	c2.Deploy(hostdtest.Fixture("probe"), "probe", nil)
	d := c2.AddTenant(mgmtapi.Tenant{Tailnet: "tenant-d.ts.net", ControlURL: e.ctl.URL, MintMode: "static"})
	depD := d.DeployNode(hostdtest.Fixture("nodeprobe"), "nodeprobe", nil)
	d.WaitActive("nodeprobe", depD, 60*time.Second)
	if ws := e.nodeStatus(d, "nodeprobe"); ws.Egress != hostd.EgressUnenforced || !c2.Host.Status().InsecureSharedHost {
		t.Fatalf("override: %+v", ws)
	}
}

// landlockNodeChecks: from inside the sandboxed Node app (Linux), raw TCP
// that bypasses the egress proxy fails (the internet, a host-loopback
// listener), unix sockets of hostd fail, its own app port works; under the
// app's profile, ptrace of a sibling fails. Egress through netd is checked
// by EgressAttributed, which runs under the same sandbox.
func (e *env) landlockNodeChecks(t *testing.T, ws hostd.WorkloadStatus, base string) map[string]string {
	t.Helper()
	netop := func(q string) map[string]any {
		code, _, b := do(e.hc, "GET", base+"/net?"+q, nil)
		if code != 200 {
			t.Fatalf("net %s: %d %s", q, code, b)
		}
		return obj(b)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	lport := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	got := map[string]string{}
	for name, q := range map[string]string{
		"tcp_1.1.1.1:80":    "host=1.1.1.1&port=80",
		"tcp_1.1.1.1:443":   "host=1.1.1.1&port=443",
		"tcp_host_loopback": "host=127.0.0.1&port=" + lport,
		"unix_hostd_push_a": "path=" + url.QueryEscape(e.a.Host.PushSocket()),
		"unix_hostd_push_b": "path=" + url.QueryEscape(e.b.Host.PushSocket()),
		"unix_tailscaled":   "path=" + url.QueryEscape("/var/run/tailscale/tailscaled.sock"),
		"unix_docker":       "path=" + url.QueryEscape("/var/run/docker.sock"),
	} {
		m := netop(q)
		// TCP mode: the app has no AF_UNIX socket() at all, so no unix
		// socket on the host (tailscaled's, docker's, hostd's) is reachable.
		if m["ok"] != false {
			t.Fatalf("node app (Landlock) %s: connected (%v)", name, m)
		}
		got[name] = fmt.Sprint(m["code"])
	}
	// The app's own port is allowed (self-calls).
	if m := netop("host=127.0.0.1&port=" + strings.TrimPrefix(ws.Node.Addr, "127.0.0.1:")); m["ok"] != true {
		t.Fatalf("node app can't connect to its own port %s: %v", ws.Node.Addr, m)
	}
	// Guarded sockets really refuse it: an HTTP request through hostd's push
	// socket from the app's profile gets no answer.
	for _, sock := range []string{e.a.Host.PushSocket(), e.b.Host.PushSocket()} {
		cmd, err := procsb.ProbeCommand(ws.SandboxProfile, procsb.ProbeConnectUnix, sock)
		if err != nil {
			t.Fatal(err)
		}
		if out, _ := cmd.Output(); strings.TrimSpace(string(out)) == "ok" {
			t.Fatalf("hostd push socket %s answers a sandboxed peer", sock)
		}
	}
	sib := exec.Command("sleep", "30")
	if err := sib.Start(); err != nil {
		t.Fatal(err)
	}
	defer sib.Process.Kill()
	cmd, _ := procsb.ProbeCommand(ws.SandboxProfile, procsb.ProbePtrace, strconv.Itoa(sib.Process.Pid))
	if out, _ := cmd.Output(); strings.TrimSpace(string(out)) == "ok" {
		t.Fatal("ptrace of a sibling allowed under the node app's profile")
	} else {
		got["ptrace_sibling"] = strings.TrimSpace(string(out))
	}
	if !procsb.Sandboxed(ws.Node.PID) {
		t.Fatalf("node pid %d carries no sandbox marker", ws.Node.PID)
	}
	t.Logf("landlock node app: %v", got)
	return got
}
