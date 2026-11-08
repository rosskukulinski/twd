//go:build e2e

// Package local runs the twd-netd binary as a real process against a local
// control plane (testcontrol + DERP + STUN) and a stock workerd.
package local

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
)

type proc struct {
	cmd *exec.Cmd
	log string
}

func startNetd(t *testing.T, bin string, args ...string) *proc {
	t.Helper()
	lf, err := os.CreateTemp("", "twd-netd-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return &proc{cmd: cmd, log: lf.Name()}
}

func (p *proc) stop(t *testing.T) time.Duration {
	st := time.Now()
	p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		p.cmd.Process.Kill()
		t.Error("twd-netd did not exit on SIGTERM")
	}
	return time.Since(st)
}

func ctlClient(sock string) *http.Client {
	return &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
}

func ctlDo(t *testing.T, c *http.Client, method, path string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, "http://ctl"+path, nil)
	var resp *http.Response
	var err error
	for i := 0; i < 100; i++ { // the socket appears shortly after start
		if resp, err = c.Do(req); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
		}
	}
	return resp.StatusCode
}

func TestNetdBinary(t *testing.T) {
	root, err := nettest.FindRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	wdBin, err := nettest.WorkerdBinary()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "twd-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	os.Chmod(dir, 0o700)
	bin := filepath.Join(dir, "twd-netd")
	if out, err := exec.Command("go", "build", "-o", bin, filepath.Join(root, "cmd", "twd-netd")).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	ctl, err := nettest.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()

	runDir := filepath.Join(dir, "run")
	os.MkdirAll(runDir, 0o700)
	wd, err := nettest.StartWorkerd(wdBin, filepath.Join(root, "netd", "testdata", "workerd", "config.capnp"), runDir, filepath.Join(runDir, "egress.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer wd.Stop()
	keyFile := filepath.Join(dir, "authkey.env")
	os.WriteFile(keyFile, []byte("TS_AUTHKEY="+nettest.DummyKey+"\n"), 0o600)
	args := []string{"-control-url", ctl.URL, "-state-dir", filepath.Join(dir, "state"), "-run-dir", runDir,
		"-upstream", wd.IngressSock, "-authkey-file", keyFile, "-objects", "a,b"}
	p := startNetd(t, bin, args...)
	defer func() {
		if t.Failed() {
			b, _ := os.ReadFile(p.log)
			t.Logf("twd-netd log:\n%s", b)
		}
	}()
	cc := ctlClient(filepath.Join(runDir, "ctl.sock"))
	var info netd.ObjectInfo
	for _, id := range []string{"a", "b"} {
		if code := ctlDo(t, cc, "POST", "/v1/objects?id="+id+"&wait=1", &info); code != 202 || info.State != netd.StateReady {
			t.Fatalf("create %s: %d %+v", id, code, info)
		}
	}
	var objs []netd.ObjectInfo
	ctlDo(t, cc, "GET", "/v1/objects", &objs)
	ips := map[string]string{}
	nodes := map[string]string{}
	for _, o := range objs {
		ips[o.ID], nodes[o.ID] = o.IPs[0], o.NodeID
	}

	ctx := context.Background()
	client, err := nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-client", filepath.Join(dir, "client"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	echo, err := nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-echo", filepath.Join(dir, "echo"))
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	echo.ServeEcho()
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.WaitPeers(wctx, ips["a"], ips["b"], echo.IP4); err != nil {
		t.Fatal(err)
	}
	hc := client.HTTPClient(20 * time.Second)
	get := func(u string) (int, []byte) {
		resp, err := hc.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	// identity
	code, b := get("http://" + ips["a"] + "/")
	var r struct {
		Caller   netd.Caller `json:"caller"`
		ObjectID string      `json:"objectId"`
	}
	json.Unmarshal(b, &r)
	if code != 200 || r.ObjectID != "a" || r.Caller.Hostname != "ttest-client" {
		t.Fatalf("identity: %d %s", code, b)
	}
	// egress attribution
	for _, id := range []string{"a", "b"} {
		code, b := get("http://" + ips[id] + "/egress?url=http://ttest-echo/")
		var er nettest.EchoReply
		json.Unmarshal(b, &er)
		if code != 200 || !strings.HasPrefix(er.Caller.Node, "twd-"+id+".") {
			t.Fatalf("egress %s: %d %s", id, code, b)
		}
	}
	// SSRF + peer_not_visible through the binary
	for u, want := range map[string]string{"http://" + ips["b"] + "/": netd.CodeForbiddenDestination,
		"http://127.0.0.1/": netd.CodeForbiddenDestination, "http://example.com/": netd.CodePeerNotVisible} {
		_, b := get("http://" + ips["a"] + "/egress?url=" + u)
		var e netd.Error
		json.Unmarshal(b, &e)
		if e.Code != want {
			t.Errorf("%s: %s", u, b)
		}
	}

	// restart: SIGTERM closes all nodes in parallel and keeps state; a new
	// process rejoins from it with the same node IDs, using no auth key.
	stopDur := p.stop(t)
	t.Logf("SIGTERM -> exit (parallel Close) %v", stopDur.Round(time.Millisecond))
	args2 := []string{"-control-url", ctl.URL, "-state-dir", filepath.Join(dir, "state"), "-run-dir", runDir,
		"-upstream", wd.IngressSock}
	p = startNetd(t, bin, args2...)
	defer p.stop(t)
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctlDo(t, cc, "GET", "/v1/objects", &objs)
		ready := 0
		for _, o := range objs {
			if o.State == netd.StateReady {
				ready++
			}
		}
		if ready == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rejoin: %+v", objs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, o := range objs {
		if !o.Rejoined || o.NodeID != nodes[o.ID] || o.IPs[0] != ips[o.ID] {
			t.Errorf("rejoin %s: %+v (want node %s)", o.ID, o, nodes[o.ID])
		}
		t.Logf("rejoined %s as %s in %.0f ms", o.ID, o.NodeID, o.JoinMS)
	}
	if code, _ := get("http://" + ips["a"] + "/"); code != 200 {
		t.Fatalf("after rejoin: %d", code)
	}

	// kill via the control socket
	var rep netd.KillReport
	if code := ctlDo(t, cc, "POST", "/v1/objects/a/kill?reason=e2e", &rep); code != 200 || rep.PrevState != netd.StateReady {
		t.Fatalf("kill: %d %+v", code, rep)
	}
	t.Logf("kill report: %+v", rep)
	if code := ctlDo(t, cc, "GET", "/v1/objects/a", &info); code != 200 || info.State != netd.StateKilled {
		t.Fatalf("after kill: %+v", info)
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "a")); !os.IsNotExist(err) {
		t.Errorf("state for killed object kept")
	}
}
