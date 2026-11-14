package hostd_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/sandbox/sandboxtest"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/tailcfg"
)

// TestContainerPolyfill drives Cloudflare's ctx.container API from real
// workerd (the generated platform wrapper's polyfill) through hostd's
// container manager, on the fake process backend (sandboxtest: no
// isolation; the microVM backend is e2e/container in Lima). Facet and
// plain-DO classes both.
func TestContainerPolyfill(t *testing.T) {
	procs := &sandboxtest.Procs{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "port server: %s %s?%s as=%q probe=%q", r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("x-twd-as"), r.Header.Get("x-probe"))
	}))
	defer srv.Close()
	procs.Ports = map[uint16]string{8080: srv.Listener.Addr().String()}
	st, err := sandboxtest.ImageStore(t.TempDir(), []string{"sleep", "3600"}, "twd.test/box:1")
	if err != nil {
		t.Fatal(err)
	}
	s := startSuiteCapsOpts(t, fakemgmt.Config{}, tailcfg.PeerCapMap{nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue}}, func(o *hostdtest.Options) {
		o.Containers, o.Images, o.ContainerGrace = procs, st, 2*time.Second
	})
	e := s.e
	dep := e.Deploy(hostdtest.Fixture("ctr"), "ctr", nil)
	wl := e.WaitActive("ctr", dep, 60*time.Second)
	s.waitPeer(wl.IP)
	base := "http://" + wl.IP
	call := func(path string, q ...string) map[string]any {
		t.Helper()
		v := url.Values{}
		for i := 0; i+1 < len(q); i += 2 {
			v.Set(q[i], q[i+1])
		}
		u := base + path
		if len(v) > 0 {
			u += "?" + v.Encode()
		}
		code, m := s.getJSON(u)
		if code == 0 {
			t.Fatalf("%s: %v", path, m)
		}
		return m
	}
	ctrOf := func(class, name string) hostd.ContainerStatus {
		for _, c := range e.Host.Containers() {
			if c.Class == class && c.Object == name {
				return c
			}
		}
		return hostd.ContainerStatus{}
	}
	waitState := func(class, name, state string) hostd.ContainerStatus {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			c := ctrOf(class, name)
			if c.State == state || time.Now().After(deadline) {
				return c
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	t.Run("PresentOnlyOnContainerClasses", func(t *testing.T) {
		if r := call("/f/a/has"); r["has"] != true {
			t.Fatalf("facet class: %v", r)
		}
		if r := call("/p/a/has"); r["has"] != true {
			t.Fatalf("plain class: %v", r)
		}
		if r := call("/n/a/has"); r["has"] != false {
			t.Fatalf("class without containers: %v", r)
		}
	})

	t.Run("StartExecOutput", func(t *testing.T) {
		if r := call("/f/a/running"); r["running"] != false {
			t.Fatal(r)
		}
		if r := call("/f/a/start"); r["running"] != true {
			t.Fatalf("running must be true right after start(): %v", r)
		}
		// exec waits for the start; no shell; only PATH is inherited (the
		// start() env GREETING and the image env are not).
		r := call("/f/a/exec", "argv", `["sh","-c","echo [$GREETING][$IMAGE_ENV]; echo out; echo err >&2; exit 3"]`)
		if r["exitCode"] != float64(3) || r["stdout"] != "[][]\nout\n" || r["stderr"] != "err\n" {
			t.Fatalf("exec: %v", r)
		}
		r = call("/f/a/exec", "argv", `["sh","-c","echo $X; echo e >&2"]`, "env", `{"X":"y"}`, "stderr", "combined")
		if r["exitCode"] != float64(0) || !strings.Contains(r["stdout"].(string), "y\n") || !strings.Contains(r["stdout"].(string), "e\n") || r["hasStderr"] != false {
			t.Fatalf("combined/env: %v", r)
		}
		if r := call("/f/a/exec", "argv", `["no-such-binary-twd"]`); r["error"] == nil {
			t.Fatalf("a missing binary must fail: %v", r)
		}
		if c := ctrOf("Box", "a"); c.State != "running" || c.Image != "twd.test/box:1" {
			t.Fatalf("hostd view: %+v", c)
		}
	})

	t.Run("StreamingStdinKill", func(t *testing.T) {
		r := call("/f/a/stream")
		if r["first"] != "first\n" || r["rest"] != "second\n" || r["code"] != float64(7) {
			t.Fatalf("stream: %v", r)
		}
		if r["tFirst"].(float64) > 800 || r["tTotal"].(float64) < 950 {
			t.Fatalf("not streamed: first after %vms, total %vms", r["tFirst"], r["tTotal"])
		}
		body := strings.Repeat("abcdefghij", 20000)
		code, _, b := s.do("POST", base+"/f/a/stdin", []byte(body))
		if code != 200 || !strings.Contains(string(b), `"exitCode":0`) || !strings.Contains(string(b), body[:1000]) || len(b) < len(body) {
			t.Fatalf("stdin: %d %.200s", code, b)
		}
		if r := call("/f/a/kill"); r["code"] != float64(137) {
			t.Fatalf("kill(9): %v", r)
		}
		if r := call("/f/a/abort"); r["code"] != float64(137) {
			t.Fatalf("AbortSignal: %v", r)
		}
	})

	t.Run("OnlyItsOwnContainer", func(t *testing.T) {
		// b has no container: it sees none, and its exec doesn't reach a's.
		if r := call("/f/b/running"); r["running"] != false {
			t.Fatalf("b sees a's container: %v", r)
		}
		if r := call("/f/b/exec", "argv", `["true"]`); r["error"] == nil {
			t.Fatalf("b exec ran: %v", r)
		}
		// The facet keeps no platform binding in env, and a plain-DO object
		// (the workload's trust domain) can't claim a facet object.
		if r := call("/f/a/envtwd"); r["hasTWD"] != false {
			t.Fatalf("facet env leaks __TWD: %v", r)
		}
		for _, op := range []string{"destroy", "status", "signal"} {
			r := call("/p/z/forge", "target", "a", "op", op)
			if r["ok"] != false {
				t.Fatalf("plain object drove facet a's container (%s): %v", op, r)
			}
		}
		if c := ctrOf("Box", "a"); c.State != "running" {
			t.Fatalf("a after forgery attempts: %+v", c)
		}
	})

	t.Run("GetTcpPort", func(t *testing.T) {
		r := call("/f/a/port", "p", "8080")
		if r["status"] != float64(200) || !strings.Contains(r["body"].(string), "GET /hello?x=1") || !strings.Contains(r["body"].(string), `as=""`) ||
			!strings.Contains(r["body"].(string), `probe="1"`) {
			t.Fatalf("port fetch: %v", r)
		}
		r = call("/f/a/port", "p", "8081")
		if !strings.Contains(fmt.Sprint(r["error"]), "the container is not listening") {
			t.Fatalf("closed port: %v", r)
		}
		if r := call("/f/a/inspect"); fmt.Sprint(get(r, "info", "image")) != "twd.test/box:1" {
			t.Fatalf("inspect: %v", r)
		}
		if r := call("/f/a/intercept"); r["ok"] != true {
			t.Fatalf("intercept: %v", r)
		}
	})

	t.Run("MonitorAndRestart", func(t *testing.T) {
		call("/f/m/start", "entrypoint", `["sh","-c","sleep 0.5; exit 5"]`)
		r := call("/f/m/monitor")
		if !strings.Contains(fmt.Sprint(r["rejected"]), "unexpected exit code: 5") || r["exitCode"] != float64(5) || r["running"] != false {
			t.Fatalf("monitor on exit 5: %v", r)
		}
		call("/f/m/start", "entrypoint", `["sh","-c","sleep 0.3"]`)
		if r := call("/f/m/monitor"); r["resolved"] != true {
			t.Fatalf("monitor on exit 0: %v", r)
		}
		if c := ctrOf("Box", "m"); c.Gen != 2 || c.State != "stopped" {
			t.Fatalf("%+v", c)
		}
	})

	t.Run("DestroyThenStart", func(t *testing.T) {
		live := procs.Live()
		if r := call("/f/a/destroy"); r["running"] != false {
			t.Fatal(r)
		}
		if c := ctrOf("Box", "a"); c.State != "stopped" || procs.Live() != live-1 {
			t.Fatalf("after destroy: %+v live %d", c, procs.Live())
		}
		if r := call("/f/a/exec", "argv", `["true"]`); r["error"] == nil {
			t.Fatalf("exec after destroy: %v", r)
		}
		call("/f/a/start")
		if r := call("/f/a/exec", "argv", `["echo","again"]`); r["stdout"] != "again\n" {
			t.Fatalf("after restart: %v", r)
		}
		if r := call("/f/t/startTwice"); !strings.Contains(fmt.Sprint(r["second"]), "already running") {
			t.Fatalf("start() twice: %v", r)
		}
	})

	t.Run("PlainMode", func(t *testing.T) {
		call("/p/q/start")
		if r := call("/p/q/exec", "argv", `["echo","plain"]`); r["stdout"] != "plain\n" {
			t.Fatalf("plain-DO exec: %v", r)
		}
		if c := ctrOf("PBox", "q"); c.State != "running" || c.Class != "PBox" {
			t.Fatalf("%+v", c)
		}
		if sp := procs.Specs[len(procs.Specs)-1]; sp.MemMiB != 1024 || sp.CPUMax != 0.25 {
			t.Fatalf("basic instance shape: %+v", sp)
		}
	})

	t.Run("InactivityTimeout", func(t *testing.T) {
		call("/f/i/start")
		call("/f/i/idle", "ms", "800")
		c := waitState("Box", "i", "stopped")
		if c.State != "stopped" || c.ExitReason != "inactivity timeout" {
			t.Fatalf("idle container: %+v", c)
		}
		if r := call("/f/i/running"); r["running"] != false {
			t.Fatalf("running after the inactivity stop: %v", r)
		}
	})

	t.Run("KillCascades", func(t *testing.T) {
		call("/f/k/start")
		if c := waitState("Box", "k", "running"); c.State != "running" {
			t.Fatal(c)
		}
		e.WaitObject("ctr", "Box", "k", 60*time.Second)
		st := time.Now()
		call("/f/k/selfkill")
		c := waitState("Box", "k", "stopped")
		if c.State != "stopped" || !strings.HasPrefix(c.ExitReason, "object killed") {
			t.Fatalf("container survived its object's kill: %+v", c)
		}
		t.Logf("kill → container stopped in %v", time.Since(st))
	})

	t.Run("SurvivesRedeploy", func(t *testing.T) {
		call("/f/r/start")
		gen := ctrOf("Box", "r").Gen
		dep2 := e.Deploy(hostdtest.Fixture("ctr"), "ctr", nil)
		e.WaitActive("ctr", dep2, 60*time.Second)
		// The new code sees the same container (Cloudflare's durable_object
		// policy: the new version decides).
		if r := call("/f/r/running"); r["running"] != true {
			t.Fatalf("after redeploy: %v", r)
		}
		if r := call("/f/r/exec", "argv", `["echo","still"]`); r["stdout"] != "still\n" || ctrOf("Box", "r").Gen != gen {
			t.Fatalf("after redeploy exec: %v", r)
		}
	})
}
