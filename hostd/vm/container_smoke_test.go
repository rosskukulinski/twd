//go:build linux || darwin

package vm

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/image"
	"github.com/rosskukulinski/twd/hostd/sandbox"
)

type execResult struct {
	out, err string
	code     int
}

func runExec(t *testing.T, c sandbox.Container, es sandbox.ExecSpec, stdin string) execResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	es.Stdin = stdin != ""
	e, err := c.Exec(ctx, es)
	if err != nil {
		t.Fatalf("exec %q: %v", es.Argv, err)
	}
	if es.Stdin {
		go func() {
			io.WriteString(e.Stdin(), stdin)
			e.Stdin().Close()
		}()
	}
	var ob, eb bytes.Buffer
	var wg sync.WaitGroup
	for _, p := range []struct {
		r io.Reader
		b *bytes.Buffer
	}{{e.Stdout(), &ob}, {e.Stderr(), &eb}} {
		if p.r != nil {
			wg.Add(1)
			go func() { defer wg.Done(); io.Copy(p.b, p.r) }()
		}
	}
	code, err := e.Wait(ctx)
	wg.Wait()
	if err != nil {
		t.Fatalf("exec %q wait: %v", es.Argv, err)
	}
	return execResult{ob.String(), eb.String(), code}
}

// checkCPUShare: the busy loop's CPU share must show the fractional cap
// where the guest's own clock can see it (vz: cpu.max on the guest
// cgroup). Under Firecracker the cap throttles the VMM, which the guest
// may count as its own CPU time, so there it is only logged.
func checkCPUShare(t *testing.T, s string) {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("busy loop: %q", s)
	}
	if guestSeesCPUCap && f > 0.5 {
		t.Fatalf("a basic (1/4 vCPU) container used %.2f of a CPU", f)
	}
}

// TestContainerSmoke boots a container guest from a real image on this
// OS's backend (Linux: root, KVM, TWD_VM_CONTAINER_KERNEL (erofs),
// TWD_VM_ROOTFS; macOS: vz, see ctrSmokeBackend) and TWD_CTR_IMAGES (a dir
// from guest/images/build-sandbox-images.sh for the arch the backend runs),
// and checks what a hostile tenant inside it can and can't do.
func TestContainerSmoke(t *testing.T) {
	imgs := os.Getenv("TWD_CTR_IMAGES")
	if imgs == "" {
		t.Skip("TWD_CTR_IMAGES not set")
	}
	b, emulated := ctrSmokeBackend(t)
	st := &image.Store{Dir: filepath.Join(t.TempDir(), "cache"), Sources: []image.Source{image.DirSource{Dir: imgs}}, Emulated: emulated}
	ctx := context.Background()
	t0 := time.Now()
	img, err := st.Get(ctx, "smoke", "twd.local/debian-trixie-python:1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("image %s %s (linux/%s) %d MiB, convert %v", img.Ref, img.Digest, img.Config.Architecture, img.SizeB>>20, time.Since(t0))
	argv, env, err := sandbox.ContainerProcess(img.Config.Config.Entrypoint, img.Config.Config.Cmd, img.Config.Config.Env, nil, map[string]string{"TWD_TEST": "1"})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := sandbox.ResolveInstance("basic", nil, "")
	vcpus, cpuMax, mem, scratch := res.VMShape()
	t1 := time.Now()
	c, err := b.BootContainer(ctx, sandbox.ContainerSpec{ID: "ctrsmoke", Image: img.Path, ScratchMiB: scratch, VCPUs: vcpus, CPUMax: cpuMax, MemMiB: mem,
		Pids: 64, Argv: argv, Env: env, Cwd: img.Config.Config.WorkingDir, User: img.Config.Config.User})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop(0)
	t.Logf("boot → ready %v; info %+v; rss %d kB", time.Since(t1), c.Info(), c.RSSKB())
	checkCtrConfinement(t, c.Info(), "25000 ") // basic: 1/4 vCPU

	// The fractional vCPU holds: a busy loop gets about a quarter of one
	// CPU (no more than half) over a second of wall time.
	r := runExec(t, c, sandbox.ExecSpec{Argv: []string{"python3", "-c", `
import time
w=time.monotonic(); c=time.process_time()
while time.monotonic()-w < 1.5: pass
print(round((time.process_time()-c)/(time.monotonic()-w), 3))
`}}, "")
	t.Logf("busy loop CPU share (basic, cpu.max 1/4): %s", strings.TrimSpace(r.out))
	checkCPUShare(t, strings.TrimSpace(r.out))

	// The guest image's arch, as the container sees it (x86_64 under Rosetta).
	r = runExec(t, c, sandbox.ExecSpec{Argv: []string{"sh", "-c", "uname -m; python3 -c 'import platform; print(platform.machine())'"}}, "")
	t.Logf("uname -m / python platform.machine() in the container: %q", r.out)

	r = runExec(t, c, sandbox.ExecSpec{Argv: []string{"python3", "-c", "print(2 + 2)"}}, "")
	if r.out != "4\n" || r.code != 0 {
		t.Fatalf("python: %+v", r)
	}
	r = runExec(t, c, sandbox.ExecSpec{Argv: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}}, "")
	if r.out != "out\n" || r.err != "err\n" || r.code != 3 {
		t.Fatalf("exit code / stderr: %+v", r)
	}
	r = runExec(t, c, sandbox.ExecSpec{Argv: []string{"sh", "-c", "echo out; echo err >&2"}, Stderr: "combined"}, "")
	if !strings.Contains(r.out, "out\n") || !strings.Contains(r.out, "err\n") || r.err != "" {
		t.Fatalf("combined: %+v", r)
	}
	big := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB through stdin and back
	r = runExec(t, c, sandbox.ExecSpec{Argv: []string{"cat"}}, big)
	if r.out != big || r.code != 0 {
		t.Fatalf("stdin round trip: %d bytes back, code %d", len(r.out), r.code)
	}
	r = runExec(t, c, sandbox.ExecSpec{Argv: []string{"sh", "-c", "echo hello > /workspace/hello.txt && cat /workspace/hello.txt && pwd && id -u && echo x${TWD_TEST}x $X"},
		Cwd: "/workspace", Env: []string{"X=y"}}, "")
	if r.out != "hello\n/workspace\n0\nxx y\n" {
		t.Fatalf("file/cwd/env: %q", r.out)
	}
	r = runExec(t, c, sandbox.ExecSpec{Argv: []string{"id", "-u"}, User: "65534:65534"}, "")
	if r.out != "65534\n" {
		t.Fatalf("user: %+v", r)
	}
	if r := runExec(t, c, sandbox.ExecSpec{Argv: []string{"tty"}, PTY: true}, ""); !strings.Contains(r.out, "/dev/pts/") {
		t.Fatalf("pty: %+v", r)
	}
	// Nothing of the guest's plumbing is visible: no guestd, no config
	// drive contents, no block devices.
	r = runExec(t, c, sandbox.ExecSpec{Argv: []string{"sh", "-c", "ls /run/twd /sbin/twd-guestd 2>&1; ls /dev; cat /proc/mounts | grep -c vd"}}, "")
	if strings.Contains(r.out, "guest.json") || !strings.Contains(r.out, "No such file") || strings.Contains(r.out, "vda") || strings.Contains(r.out, "vdc") {
		t.Fatalf("guest internals visible: %s", r.out)
	}
	// No egress: the gateway's platform ports, the host, the internet.
	r = runExec(t, c, sandbox.ExecSpec{Argv: []string{"python3", "-c", `
import socket
for a in [("192.168.72.1",3128),("192.168.72.1",3130),("192.168.72.1",22),("10.0.2.2",22),("1.1.1.1",443)]:
    s=socket.socket(); s.settimeout(2)
    try:
        s.connect(a); print("CONNECTED", a)
    except Exception as e:
        print("refused", a, type(e).__name__)
`}}, "")
	if strings.Contains(r.out, "CONNECTED") {
		t.Fatalf("egress allowed:\n%s", r.out)
	}
	t.Logf("egress probes:\n%s", r.out)
	// Nor the host itself: a canary listening on every host address gets
	// no connection from the guest, by IP or by name (there is no DNS).
	canary, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer canary.Close()
	var hits atomic.Int32
	go func() {
		for {
			cn, err := canary.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			cn.Close()
		}
	}()
	port := canary.Addr().(*net.TCPAddr).Port
	targets := []string{fmt.Sprintf(`("127.0.0.1",%d)`, port), fmt.Sprintf(`("localhost",%d)`, port), `("example.com",443)`}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil && !ip.IP.IsLoopback() {
			targets = append(targets, fmt.Sprintf(`(%q,%d)`, ip.IP.String(), port))
		}
	}
	r = runExec(t, c, sandbox.ExecSpec{Argv: []string{"python3", "-c", `
import socket
for a in [` + strings.Join(targets, ",") + `]:
    s=socket.socket(); s.settimeout(2)
    try:
        s.connect(a); print("CONNECTED", a)
    except Exception as e:
        print("refused", a, type(e).__name__)
`}}, "")
	t.Logf("host canary probes (%d host addresses):\n%s", len(targets)-3, r.out)
	if strings.Contains(r.out, "CONNECTED") || hits.Load() != 0 {
		t.Fatalf("the guest reached the host (canary hits %d):\n%s", hits.Load(), r.out)
	}
	// pids.max: a process storm stops at the cgroup limit.
	r = runExec(t, c, sandbox.ExecSpec{Argv: []string{"sh", "-c", "for i in $(seq 1 200); do sleep 3 & done; echo all-200-started"}}, "")
	t.Logf("pids storm (pids.max 64): %q %q code %d", r.out, r.err, r.code)
	if strings.Contains(r.out, "all-200-started") || r.code == 0 {
		t.Fatalf("pids.max did not stop the storm: %+v", r)
	}
	time.Sleep(4 * time.Second) // the storm's sleeps end

	// A long-running exec, killed by signal; and a server reached via DialPort.
	e, err := c.Exec(ctx, sandbox.ExecSpec{Argv: []string{"python3", "-m", "http.server", "8000", "--bind", "0.0.0.0"}, Cwd: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, e.Stdout())
	go io.Copy(io.Discard, e.Stderr())
	var body string
	for i := 0; i < 100; i++ {
		conn, err := c.DialPort(ctx, 8000)
		if err == nil {
			conn.Write([]byte("GET /hello.txt HTTP/1.0\r\n\r\n"))
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err == nil {
				bb, _ := io.ReadAll(resp.Body)
				body = string(bb)
			}
			conn.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if body != "hello\n" {
		t.Fatalf("getTcpPort: %q", body)
	}
	if _, err := c.DialPort(ctx, 8001); err == nil {
		t.Fatal("dial to a closed port succeeded")
	}
	if err := e.Signal(9); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.Wait(ctx); code != 137 {
		t.Fatalf("killed exec code %d", code)
	}
	// Exec latency.
	var lat []time.Duration
	for i := 0; i < 20; i++ {
		s := time.Now()
		runExec(t, c, sandbox.ExecSpec{Argv: []string{"true"}}, "")
		lat = append(lat, time.Since(s))
	}
	t.Logf("exec true latency: %v", lat)
	// destroy: SIGTERM to the entrypoint (sleep dies of it), VM gone.
	s := time.Now()
	c.Stop(5 * time.Second)
	t.Logf("stop → done %v; exit code %d", time.Since(s), c.ExitCode())
	select {
	case <-c.Done():
	default:
		t.Fatal("VM not done after Stop")
	}
	if c.ExitCode() != 143 {
		t.Fatalf("entrypoint exit code %d, want 143 (SIGTERM)", c.ExitCode())
	}
}
