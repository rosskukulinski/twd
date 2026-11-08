//go:build linux

package procsb_test

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/internal/procsb"
)

func needLandlock(t *testing.T) int {
	t.Helper()
	abi, err := procsb.ABI()
	if err != nil || abi < 1 {
		t.Skipf("no Landlock on this kernel: %v", err)
	}
	return abi
}

func self(t *testing.T) string {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// probe runs op under prof and returns "ok" or the errno name.
func probe(t *testing.T, prof, op string, args ...string) string {
	t.Helper()
	cmd, err := procsb.ProbeCommand(prof, op, args...)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	res := strings.TrimSpace(string(out))
	if err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
			t.Fatalf("probe %s %v: %v %s %s", op, args, err, res, ee.Stderr)
		}
	}
	return res
}

func writeProfile(t *testing.T, dir string, p procsb.Profile) string {
	t.Helper()
	p.Launcher = self(t)
	path := filepath.Join(dir, "sandbox.json")
	if err := p.Write(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSandboxProbes(t *testing.T) {
	abi := needLandlock(t)
	root := t.TempDir()
	own, deny, prof := filepath.Join(root, "own"), filepath.Join(root, "deny"), filepath.Join(root, "prof")
	for _, d := range []string{own, deny, prof} {
		os.MkdirAll(d, 0o700)
	}
	canary := filepath.Join(deny, "canary")
	os.WriteFile(canary, []byte("secret"), 0o600)
	os.WriteFile(filepath.Join(own, "mine"), []byte("mine"), 0o600)

	// A loopback listener outside the sandbox (another tenant's app port).
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	go func() {
		for {
			c, err := other.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	allowed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer allowed.Close()
	go http.Serve(allowed, http.NotFoundHandler())
	aport := uint16(allowed.Addr().(*net.TCPAddr).Port)
	bport := freePort(t)

	p := writeProfile(t, prof, procsb.Profile{Own: []string{own}, ConnectTCP: []uint16{aport}, BindTCP: []uint16{bport}, Deny: []string{deny}})

	// A guarded unix socket (mgmt's / hostd's) and an unguarded one.
	gsock, usock := filepath.Join(deny, "g.sock"), filepath.Join(deny, "u.sock")
	gl, _ := net.Listen("unix", gsock)
	defer gl.Close()
	go http.Serve(procsb.Guard(gl, nil), http.NotFoundHandler())
	ul, _ := net.Listen("unix", usock)
	defer ul.Close()
	go http.Serve(ul, http.NotFoundHandler())

	sib := exec.Command("sleep", "60")
	if err := sib.Start(); err != nil {
		t.Fatal(err)
	}
	defer sib.Process.Kill()
	sibPID := strconv.Itoa(sib.Process.Pid)
	home, _ := os.UserHomeDir()

	type c struct {
		op   string
		args []string
		want string // "ok" or "denied"
	}
	cases := []c{
		{procsb.ProbeRead, []string{filepath.Join(own, "mine")}, "ok"},
		{procsb.ProbeWrite, []string{filepath.Join(own, "new")}, "ok"},
		{procsb.ProbeList, []string{own}, "ok"},
		{procsb.ProbeRead, []string{"/etc/hostname"}, "ok"},
		{procsb.ProbeRead, []string{canary}, "denied"},
		{procsb.ProbeList, []string{deny}, "denied"},
		{procsb.ProbeWrite, []string{filepath.Join(deny, "evil")}, "denied"},
		{procsb.ProbeList, []string{home}, "denied"},
		{procsb.ProbeList, []string{"/tmp"}, "denied"},
		{procsb.ProbeList, []string{"/proc"}, "denied"},
		{procsb.ProbeRead, []string{"/proc/1/cmdline"}, "denied"},
		{procsb.ProbeRead, []string{"/proc/self/net/unix"}, "denied"},
		{procsb.ProbeRead, []string{"/proc/" + strconv.Itoa(os.Getpid()) + "/environ"}, "denied"},
		{procsb.ProbeConnectTCP, []string{"1.1.1.1:80"}, "denied"},
		{procsb.ProbeConnectTCP, []string{other.Addr().String()}, "denied"},
		{procsb.ProbeConnectTCP, []string{allowed.Addr().String()}, "ok"},
		{procsb.ProbeBindTCP, []string{"127.0.0.1:0"}, "denied"},
		{procsb.ProbeBindTCP, []string{"127.0.0.1:" + strconv.Itoa(int(bport))}, "ok"},
		{procsb.ProbeUDP, []string{"1.1.1.1:53"}, "denied"},
		{procsb.ProbePtrace, []string{sibPID}, "denied"},
		{procsb.ProbeVMRead, []string{sibPID}, "denied"},
		{procsb.ProbeConnectUnix, []string{gsock}, "denied"},
	}
	if abi < 4 {
		t.Logf("Landlock ABI %d < 4: TCP is not restricted", abi)
	}
	for _, k := range cases {
		got := probe(t, p, k.op, k.args...)
		if abi < 4 && (k.op == procsb.ProbeConnectTCP || k.op == procsb.ProbeBindTCP) {
			continue
		}
		if (k.want == "ok") != (got == "ok") {
			t.Errorf("%s %v: got %q, want %s", k.op, k.args, got, k.want)
		} else {
			t.Logf("%s %v: %s", k.op, k.args, got)
		}
	}
	// Pathname unix sockets outside the own dirs: Landlock refuses the
	// connect from ABI 9; below that only servers that Guard their
	// listener refuse it (all of twd's do).
	got := probe(t, p, procsb.ProbeConnectUnix, usock)
	t.Logf("unguarded unix socket outside own dirs (ABI %d): %s", abi, got)
	if abi >= 9 && got == "ok" {
		t.Errorf("ABI %d: connect to a unix socket outside the own dirs allowed", abi)
	}
}

func freePort(t *testing.T) uint16 {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return uint16(l.Addr().(*net.TCPAddr).Port)
}

func repoWorkerd(t *testing.T) string {
	p, _ := filepath.Abs("../../node_modules/.bin/workerd")
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Skip("workerd not installed (make deps)")
	}
	return r
}

// workerd serves under the profile (unix-socket listener in its own dir).
func TestWorkerdUnderSandbox(t *testing.T) {
	needLandlock(t)
	wd := repoWorkerd(t)
	dir := t.TempDir()
	own := filepath.Join(dir, "inst")
	os.MkdirAll(own, 0o700)
	sock := filepath.Join(own, "in.sock")
	os.WriteFile(filepath.Join(own, "w.js"), []byte(`export default { fetch() { return new Response("hi from sandbox") } }`), 0o600)
	os.WriteFile(filepath.Join(own, "c.capnp"), []byte(`using Workerd = import "/workerd/workerd.capnp";
const config :Workerd.Config = ( services = [ (name = "main", worker = .w) ], sockets = [ ( name = "http", address = "unix:`+sock+`", http = (), service = "main" ) ] );
const w :Workerd.Worker = ( modules = [ (name = "w.js", esModule = embed "w.js") ], compatibilityDate = "2024-01-01" );
`), 0o600)
	p := writeProfile(t, dir, procsb.Profile{Exec: []string{wd}, Own: []string{own}})
	cmd := procsb.Command(self(t), p, wd, "serve", filepath.Join(own, "c.capnp"), "--experimental")
	cmd.Dir = own
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	hc := &http.Client{Transport: &http.Transport{Dial: func(_, _ string) (net.Conn, error) { return net.Dial("unix", sock) }}}
	deadline := time.Now().Add(15 * time.Second)
	for {
		r, err := hc.Get("http://x/")
		if err == nil {
			r.Body.Close()
			if r.StatusCode != 200 {
				t.Fatalf("status %d", r.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workerd under sandbox never served: %v\n%s", err, out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	st, _ := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/status")
	if !strings.Contains(string(st), "NoNewPrivs:\t1") || !procsb.Sandboxed(cmd.Process.Pid) {
		t.Fatalf("workerd not marked sandboxed:\n%s", st)
	}
}

// node runs a server and egresses only to the allowed port.
func TestNodeUnderSandbox(t *testing.T) {
	needLandlock(t)
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	nodeBin, _ = filepath.EvalSymlinks(nodeBin)
	dir := t.TempDir()
	own := filepath.Join(dir, "inst")
	os.MkdirAll(own, 0o700)
	port := freePort(t)
	os.WriteFile(filepath.Join(own, "s.mjs"), []byte(`import http from "node:http"; import net from "node:net"; import os from "node:os";
const port = Number(process.argv[2]);
const tryConnect = (host, p) => new Promise((res) => { const s = net.connect({ host, port: p }); s.on("connect", () => { s.destroy(); res("ok"); }); s.on("error", (e) => res(e.code)); });
http.createServer(async (q, r) => {
  const u = new URL(q.url, "http://x");
  if (u.pathname === "/net") return r.end(await tryConnect(u.searchParams.get("host"), Number(u.searchParams.get("port"))));
  r.end("cpus " + os.cpus().length);
}).listen(port, "127.0.0.1");
`), 0o600)
	p := writeProfile(t, dir, procsb.Profile{Exec: []string{nodeBin}, Own: []string{own}, BindTCP: []uint16{port}, ConnectTCP: []uint16{port}})
	cmd := procsb.Command(self(t), p, nodeBin, filepath.Join(own, "s.mjs"), strconv.Itoa(int(port)))
	cmd.Dir = own
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + own}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	base := "http://127.0.0.1:" + strconv.Itoa(int(port))
	get := func(path string) string {
		r, err := http.Get(base + path)
		if err != nil {
			return "ERR " + err.Error()
		}
		defer r.Body.Close()
		var b strings.Builder
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		b.Write(buf[:n])
		return b.String()
	}
	deadline := time.Now().Add(15 * time.Second)
	for !strings.HasPrefix(get("/"), "cpus") {
		if time.Now().After(deadline) {
			t.Fatalf("node under sandbox never served:\n%s", out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	other, _ := net.Listen("tcp", "127.0.0.1:0")
	defer other.Close()
	op := strconv.Itoa(other.Addr().(*net.TCPAddr).Port)
	for _, k := range []struct{ host, port, want string }{
		{"1.1.1.1", "80", "!ok"},
		{"127.0.0.1", op, "!ok"},
		{"127.0.0.1", strconv.Itoa(int(port)), "ok"},
	} {
		got := get("/net?host=" + k.host + "&port=" + k.port)
		if (k.want == "ok") != (got == "ok") {
			t.Errorf("node net.connect %s:%s = %q, want %s", k.host, k.port, got, k.want)
		} else {
			t.Logf("node net.connect %s:%s = %s", k.host, k.port, got)
		}
	}
}
