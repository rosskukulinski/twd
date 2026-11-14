//go:build darwin

package vm

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm/helperapi"
)

const vzSmokeJS = `export default {
  async fetch(req, env) {
    const u = new URL(req.url);
    if (u.pathname === "/_twd/admin") return new Response("ok");
    if (u.pathname === "/egress") { const r = await env.GW.fetch("http://anything/x"); return new Response("via host: " + await r.text()); }
    return new Response("hello from the guest " + (req.cf ? JSON.stringify(req.cf) : "nocf"));
  }
};
`

func vzBackend(t *testing.T, rosetta bool) *Backend {
	t.Helper()
	kernel, rootfs := os.Getenv("TWD_VM_KERNEL"), os.Getenv("TWD_VM_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("TWD_VM_KERNEL / TWD_VM_ROOTFS not set (make e2e-vz)")
	}
	b, err := New(Config{VMM: os.Getenv("TWD_VZ_VMM"), Kernel: kernel, Rootfs: rootfs, VCPUs: envInt("TWD_VM_VCPUS"), Rosetta: rosetta})
	if err != nil {
		t.Fatalf("vz backend: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func vzApp(t *testing.T) string {
	app := t.TempDir()
	os.WriteFile(filepath.Join(app, "w.js"), []byte(vzSmokeJS), 0o600)
	os.WriteFile(filepath.Join(app, "config.capnp"), []byte(smokeCapnp), 0o600)
	return app
}

// TestVZSmoke boots the guest under Virtualization.framework: guestd
// reports over vsock, workerd is healthy, requests reach it through the
// host-side stack, its egress reaches only the gateway handler, raw TCP
// to anything else is refused, and a VMM kill ends the VM (and the
// framework's VM service) at once.
func TestVZSmoke(t *testing.T) {
	b := vzBackend(t, false)
	ctx := context.Background()
	m, err := b.Boot(ctx, sandbox.Spec{ID: "smoke", AppDir: vzApp(t), WorkerdArgs: []string{"serve", "config.capnp", "--experimental"},
		HealthCF: `{}`, Debug: true,
		Handlers: map[uint16]http.Handler{proto.EgressObj: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "host handler saw "+r.Method+" "+r.URL.Path)
		})}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	info := m.Info()
	t.Logf("boot: VMM spawn → guest init %v, → workerd ready %v, Boot() %v; VMM pid %d, VM service pid %d; rss %d kB",
		info.Boot, info.Ready, info.Total, info.PID, info.ServicePID, m.RSSKB())
	if info.ServicePID == 0 || info.ServicePID == info.PID || info.PID == os.Getpid() {
		t.Fatalf("want the guest in its own VMM process and VM service: %+v", info)
	}
	cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return m.DialGuest(ctx) }}}
	for _, p := range []string{"/", "/egress"} {
		resp, err := cl.Get("http://guest" + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Logf("%s → %d %s", p, resp.StatusCode, body)
		if resp.StatusCode != 200 || (p == "/egress" && !strings.Contains(string(body), "host handler saw GET /x")) {
			t.Fatal("unexpected")
		}
	}
	var lat []time.Duration
	for i := 0; i < 200; i++ {
		st := time.Now()
		resp, err := cl.Get("http://guest/")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		lat = append(lat, time.Since(st))
	}
	t.Logf("ingress latency (200 req, keep-alive): %s", pcts(lat))

	canary, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer canary.Close()
	hits := make(chan struct{}, 64)
	go func() {
		for {
			c, err := canary.Accept()
			if err != nil {
				return
			}
			hits <- struct{}{}
			c.Close()
		}
	}()
	cport := canary.Addr().(*net.TCPAddr).Port
	targets := []string{"192.168.72.1:22", "192.168.72.1:3129", fmt.Sprintf("192.168.72.1:%d", cport), fmt.Sprintf("127.0.0.1:%d", cport),
		"10.0.2.2:22", "169.254.169.254:80", "1.1.1.1:443", "192.168.72.3:80"}
	if addrs, _ := net.InterfaceAddrs(); addrs != nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
				targets = append(targets, fmt.Sprintf("%s:%d", ipn.IP, cport))
			}
		}
	}
	r, err := m.Op(ctx, proto.Msg{Op: "probe", Targets: targets})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range r.Results {
		if p.Result == "connected" {
			t.Errorf("guest reached %s", p.Target)
		}
	}
	if len(hits) != 0 {
		t.Errorf("host canary got %d connections from the guest", len(hits))
	}
	t.Logf("probes %+v; refused at the host stack: %v", r.Results, m.Refused())
	st, err := m.Op(ctx, proto.Msg{Op: "stats"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("guest stats %+v", st.Stats)

	svc := info.ServicePID
	m.Kill()
	k := time.Now()
	select {
	case <-m.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("VMM kill did not end the VM")
	}
	t.Logf("kill → done %v (err %v)", time.Since(k), m.Err())
	if _, err := cl.Get("http://guest/"); err == nil {
		t.Fatal("guest still answers after the kill")
	}
	for i := 0; ; i++ {
		if !pidAlive(svc) {
			t.Logf("VM service %d gone %v after the kill", svc, time.Since(k))
			break
		}
		if i > 100 {
			t.Fatalf("VM service %d survived its VMM", svc)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestVZVolume: the DO volume is an encrypted image on the host; the guest
// formats it on first use, data written in one VM is there in the next,
// the image holds no plaintext, a second attach is refused while the first
// VM runs, and an older epoch is fenced.
func TestVZVolume(t *testing.T) {
	b := vzBackend(t, false)
	ctx := context.Background()
	key := make([]byte, 32)
	rand.Read(key)
	dir := t.TempDir()
	vol := &sandbox.Volume{Path: filepath.Join(dir, "do.img"), Name: "t/w", Epoch: 2, Key: key, SizeMiB: 128}
	spec := sandbox.Spec{ID: "vol", AppDir: vzApp(t), WorkerdArgs: []string{"serve", "config.capnp", "--experimental"}, HealthCF: `{}`, Debug: true, Volume: vol}
	m, err := b.Boot(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	info := m.Info()
	if !info.VolumeNew {
		t.Fatal("first boot did not format the volume")
	}
	t.Logf("volume attach %.1f ms (new)", info.VolumeAttachMS)
	bench, err := m.Op(ctx, proto.Msg{Op: "fsbench", N: 100})
	if err != nil || bench.Bench == nil || !strings.HasPrefix(bench.Bench.FS, proto.VolumeDev+" ext4") {
		t.Fatalf("fsbench: %v %+v", err, bench.Bench)
	}
	t.Logf("guest fsbench on the encrypted volume: %+v", *bench.Bench)
	// fsbench wrote 0x5a-filled 4 KiB blocks: a plaintext image would show them.
	if _, err := b.Boot(ctx, sandbox.Spec{ID: "intruder", AppDir: vzApp(t), WorkerdArgs: []string{"--version"}, HealthCF: "{}", Volume: vol}); !helperapi.IsCode(err, helperapi.CodeVolumeBusy) {
		t.Fatalf("second VM on the volume: want volume_busy, got %v", err)
	}
	m.Stop()
	img := volumeImage(vol)
	if hit := grepFile(t, img, []byte(strings.Repeat("\x5a", 4096))); hit {
		t.Fatal("plaintext fsbench pattern found in the volume image: not encrypted")
	}
	m2, err := b.Boot(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Info().VolumeNew {
		t.Fatal("second boot reformatted the volume")
	}
	t.Logf("volume re-attach %.1f ms", m2.Info().VolumeAttachMS)
	m2.Stop()
	old := *vol
	old.Epoch = 1
	if _, err := b.Boot(ctx, sandbox.Spec{ID: "stale", AppDir: vzApp(t), WorkerdArgs: []string{"--version"}, HealthCF: "{}", Volume: &old}); !helperapi.IsCode(err, helperapi.CodeVolumeFenced) {
		t.Fatalf("older epoch: want volume_fenced, got %v", err)
	}
	wrong := *vol
	wrong.Key = make([]byte, 32)
	if _, err := b.Boot(ctx, sandbox.Spec{ID: "badkey", AppDir: vzApp(t), WorkerdArgs: []string{"--version"}, HealthCF: "{}", Volume: &wrong}); !helperapi.IsCode(err, helperapi.CodeVolumeKey) {
		t.Fatalf("wrong key: want volume_key, got %v", err)
	}
}

// TestVZRosetta: with Rosetta shared in, a static linux/amd64 binary runs
// inside the arm64 guest (ADR-032 D7 "to verify").
func TestVZRosetta(t *testing.T) {
	if os.Getenv("TWD_VM_ROSETTA") == "" {
		t.Skip("TWD_VM_ROSETTA not set (needs Rosetta for Linux and a virtiofs kernel)")
	}
	b := vzBackend(t, true)
	app := vzApp(t)
	src := filepath.Join(t.TempDir(), "main.go")
	os.WriteFile(src, []byte("package main\nimport (\"fmt\"; \"runtime\")\nfunc main() { fmt.Printf(\"hello from %s/%s\\n\", runtime.GOOS, runtime.GOARCH) }\n"), 0o600)
	cmd := exec.Command("go", "build", "-o", filepath.Join(app, "hello-amd64"), src)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build amd64 binary: %v %s", err, out)
	}
	os.Chmod(filepath.Join(app, "hello-amd64"), 0o755)
	m, err := b.Boot(context.Background(), sandbox.Spec{ID: "rosetta", AppDir: app, WorkerdArgs: []string{"serve", "config.capnp", "--experimental"}, HealthCF: `{}`, Debug: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	st := time.Now()
	r, err := m.Op(context.Background(), proto.Msg{Op: "run", Argv: []string{proto.AppDir + "/hello-amd64"}})
	t.Logf("amd64 binary in the arm64 guest: %q exit %d err %v (%v)", r.Text, r.ExitCode, err, time.Since(st))
	if err != nil || r.ExitCode != 0 || !strings.Contains(r.Text, "hello from linux/amd64") {
		b, _ := os.ReadFile(m.Info().Console)
		t.Fatalf("rosetta: console:\n%s", b)
	}
}

func pidAlive(pid int) bool {
	out, _ := exec.Command("/bin/ps", "-o", "pid=", "-p", fmt.Sprint(pid)).Output()
	return strings.TrimSpace(string(out)) != ""
}

func grepFile(t *testing.T, p string, needle []byte) bool {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(b), string(needle))
}

func pcts(ds []time.Duration) string {
	s := append([]time.Duration(nil), ds...)
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return fmt.Sprintf("p50 %v p95 %v p99 %v", s[len(s)/2], s[len(s)*95/100], s[len(s)*99/100])
}
