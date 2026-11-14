//go:build e2econtainer && (linux || darwin)

// Package container is E2E-CONTAINER (ADR-032 v1 acceptance): Cloudflare's
// container API on twd's microVM tier. Hermetic: testcontrol + local
// DERP/STUN, fakemgmt, real twd-hostd, real workerd (the parent DOs,
// process tier) and one real VM per container (erofs image + scratch
// overlay): Firecracker on Linux (jailer, own uid/netns/cgroup; Lima arm64
// or an x86-64 KVM host with the stock images) or Virtualization.framework
// on macOS (backend_darwin_test.go; with Rosetta, the stock amd64 images).
//
//	(a) the Sandbox SDK 0.x examples/minimal (v0 branch) and
//	(b) the 1.0 flow (examples/minimal at @cloudflare/sandbox@1.0.1, and the
//	    raw ctx.container.start + exec().output() + Files app),
//
// unmodified (scripts/e2e-container-prep.sh checks every example file
// against the upstream git blob), plus the lifecycle and isolation checks
// on hostd/testdata/apps/ctr booted on a real image.
//
// Run with `make e2e-container` on Linux (needs root, KVM, the guest image
// + erofs kernel, and the images from guest/images/build-sandbox-images.sh)
// or `make e2e-vz-container` on a Mac (no sudo).
package container

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/image"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/tailcfg"
)

func pct(ds []time.Duration, q float64) time.Duration {
	c := append([]time.Duration(nil), ds...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[int(float64(len(c)-1)*q)]
}

func TestE2EContainer(t *testing.T) {
	imgs, apps := os.Getenv("TWD_CTR_IMAGES"), os.Getenv("TWD_CTR_APPS")
	if imgs == "" || apps == "" {
		t.Skip("TWD_CTR_IMAGES and TWD_CTR_APPS must be set (make e2e-container / e2e-vz-container)")
	}
	be, emulated := newBackend(t)
	// The store as twd-hostd builds it: the arm64 alias map, unless the
	// backend runs amd64 images itself (vz + Rosetta: stock images, no alias).
	store := &image.Store{Dir: filepath.Join(t.TempDir(), "images"), Sources: []image.Source{image.DirSource{Dir: imgs}}, Aliases: image.DefaultAliases,
		Public: map[string]bool{"twd.local/cloudflare-sandbox:0.12.14-arm64": true, "docker.io/cloudflare/sandbox:0.12.14": true}}
	if len(emulated) > 0 {
		store.Emulated, store.Aliases = emulated, nil
	}
	if b, err := os.ReadFile(filepath.Join(apps, "unmodified.txt")); err == nil {
		t.Logf("example sources (checked against upstream git blobs):\n%s", b)
	} else {
		t.Fatalf("prep manifest missing: %v", err)
	}

	ctl, err := nettest.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctl.Close)
	ctl.Server.SetGlobalAppCaps(tailcfg.PeerCapMap{nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue}})
	e := hostdtest.Start(t, hostdtest.Options{ControlURL: ctl.URL, Fake: fakemgmt.Config{StaticKey: nettest.DummyKey}, Ephemeral: true, KillTombstone: 5 * time.Second,
		Tenant:     mgmtapi.Tenant{Tailnet: "ttest.ts.net", ControlURL: ctl.URL, MintMode: "static"},
		Containers: be, Images: store, ContainerGrace: 3 * time.Second})
	ctx := context.Background()
	client, err := nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-client", filepath.Join(e.Dir, "client"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	hc := client.HTTPClient(120 * time.Second)
	do := func(method, u string, body []byte) (int, []byte, time.Duration) {
		t.Helper()
		req, _ := http.NewRequest(method, u, bytes.NewReader(body))
		st := time.Now()
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, u, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b, time.Since(st)
	}
	getJSON := func(u string) (map[string]any, time.Duration) {
		t.Helper()
		code, b, d := do("GET", u, nil)
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			m = map[string]any{"raw": string(b)}
		}
		m["_status"] = code
		return m, d
	}
	waitPeer := func(ip string) {
		c, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if err := client.WaitPeers(c, ip); err != nil {
			t.Fatal(err)
		}
	}
	build := func(dir string) map[string][]byte {
		files, err := hostdtest.Build(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	results := map[string]any{}
	var rmu sync.Mutex
	set := func(k string, v any) { rmu.Lock(); results[k] = v; rmu.Unlock() }
	defer func() {
		b, _ := json.MarshalIndent(results, "", "  ")
		t.Logf("results:\n%s", b)
		if p := os.Getenv("TWD_E2E_RESULTS"); p != "" {
			os.WriteFile(p, b, 0o644)
		}
	}()

	// The alias map (ADR-032 D3): on arm64 the stock ref is twd's rebuild;
	// on amd64 (and under Rosetta) it is the stock image itself.
	wantRef, wantArch, machine := "twd.local/cloudflare-sandbox:0.12.14-arm64", "arm64", "aarch64"
	if runtime.GOARCH == "amd64" || len(emulated) > 0 {
		wantRef, wantArch, machine = "cloudflare/sandbox:0.12.14", "amd64", "x86_64"
	}
	if img, err := store.Get(ctx, e.TenantID, "cloudflare/sandbox:0.12.14"); err != nil {
		t.Fatal(err)
	} else if img.Ref != wantRef || img.Config.Architecture != wantArch ||
		strings.Join(img.Config.Config.Entrypoint, " ") != "/container-server/sandbox" || img.Scope != image.PublicScope {
		t.Fatalf("cloudflare/sandbox:0.12.14 resolved to %+v, want %s (linux/%s)", img, wantRef, wantArch)
	} else {
		set("image_cloudflare_sandbox_0.12.14", map[string]any{"ref": img.Ref, "arch": img.Config.Architecture, "digest": img.Digest, "erofs_mib": img.SizeB >> 20, "convert_ms": img.ConvertMS})
	}

	ctrFiles := build(hostdtest.Fixture("ctr"))
	ctrFiles["twd.jsonc"] = []byte(strings.ReplaceAll(string(ctrFiles["twd.jsonc"]), "twd.test/box:1", "twd.local/debian-trixie-python:1"))
	dV0 := e.DeployFiles(build(filepath.Join(apps, "v0min")), "sbx0")
	dV1 := e.DeployFiles(build(filepath.Join(apps, "v1min")), "sbx1")
	dRaw := e.DeployFiles(build(filepath.Join(apps, "raw10")), "raw10")
	dCtr := e.DeployFiles(ctrFiles, "ctr")
	wV0, wV1 := e.WaitActive("sbx0", dV0, 90*time.Second), e.WaitActive("sbx1", dV1, 90*time.Second)
	wRaw, wCtr := e.WaitActive("raw10", dRaw, 90*time.Second), e.WaitActive("ctr", dCtr, 90*time.Second)
	for _, ip := range []string{wV0.IP, wV1.IP, wRaw.IP, wCtr.IP} {
		waitPeer(ip)
	}

	t.Run("A_SandboxSDK0x_minimal", func(t *testing.T) {
		r, d := getJSON("http://" + wV0.IP + "/run")
		if r["_status"] != 200 || strings.TrimSpace(fmt.Sprint(r["output"])) != "2 + 2 = 4" || r["exitCode"] != float64(0) || r["success"] != true {
			t.Fatalf("/run: %v", r)
		}
		set("a_first_run_ms (container boot + Bun server ready + exec)", d.Milliseconds())
		r, _ = getJSON("http://" + wV0.IP + "/file")
		if r["content"] != "Hello, Sandbox!" {
			t.Fatalf("/file: %v", r)
		}
		var lat []time.Duration
		for i := 0; i < 20; i++ {
			r, d := getJSON("http://" + wV0.IP + "/run")
			if strings.TrimSpace(fmt.Sprint(r["output"])) != "2 + 2 = 4" {
				t.Fatalf("/run #%d: %v", i, r)
			}
			lat = append(lat, d)
		}
		set("a_exec_via_sdk_p50_ms", pct(lat, .5).Milliseconds())
		set("a_exec_via_sdk_p95_ms", pct(lat, .95).Milliseconds())
		for _, c := range e.Host.Containers() {
			if c.Workload == "sbx0" {
				set("a_container", map[string]any{"ready_ms": c.ReadyMS, "rss_kb": c.RSSKB, "image": c.Image, "jail": c.Jail})
			}
		}
	})

	t.Run("B_SandboxSDK10_minimal", func(t *testing.T) {
		base := "http://" + wV1.IP + "/sandboxes/s1"
		code, b, d := do("POST", base+"/exec", []byte(`{"argv":["sh","-c","echo hi from $(uname -m); exit 0"]}`))
		var r map[string]any
		json.Unmarshal(b, &r)
		if code != 200 || r["stdout"] != "hi from "+machine+"\n" || r["exitCode"] != float64(0) {
			t.Fatalf("exec: %d %s", code, b)
		}
		set("b_min_first_exec_ms (boot + exec)", d.Milliseconds())
		if code, b, _ := do("POST", base+"/exec", []byte(`{"argv":["sh","-c","exit 9"]}`)); code != 200 || !strings.Contains(string(b), `"exitCode":9`) {
			t.Fatalf("exit code: %d %s", code, b)
		}
		if code, b, _ := do("PUT", base+"/files/hello.txt", []byte("Hello, Sandbox!")); code != 204 {
			t.Fatalf("Files.writeFile: %d %s", code, b)
		}
		if code, b, _ := do("GET", base+"/files/hello.txt", nil); code != 200 || string(b) != "Hello, Sandbox!" {
			t.Fatalf("Files.readFile: %d %q", code, b)
		}
		if code, _, _ := do("GET", base+"/files/missing.txt", nil); code != 404 {
			t.Fatalf("missing file: %d", code)
		}
		if code, b, _ := do("DELETE", base, nil); code != 204 {
			t.Fatalf("destroy: %d %s", code, b)
		}
		for _, c := range e.Host.Containers() {
			if c.Workload == "sbx1" && c.State != "stopped" {
				t.Fatalf("after DELETE: %+v", c)
			}
		}
	})

	t.Run("B_raw_ctx_container_python_Files", func(t *testing.T) {
		r, d := getJSON("http://" + wRaw.IP + "/run/r1")
		if r["stdout"] != "4\n" || r["exitCode"] != float64(0) {
			t.Fatalf("python: %v", r)
		}
		set("b_raw_first_exec_ms (boot + exec)", d.Milliseconds())
		r, _ = getJSON("http://" + wRaw.IP + "/file/r1")
		if r["content"] != "Hello, Sandbox!" {
			t.Fatalf("Files: %v", r)
		}
	})

	// Lifecycle and isolation on a real image (the ctr fixture).
	cbase := "http://" + wCtr.IP
	call := func(path string, q ...string) map[string]any {
		t.Helper()
		v := url.Values{}
		for i := 0; i+1 < len(q); i += 2 {
			v.Set(q[i], q[i+1])
		}
		u := cbase + path
		if len(v) > 0 {
			u += "?" + v.Encode()
		}
		r, _ := getJSON(u)
		return r
	}
	ctrOf := func(class, name string) hostd.ContainerStatus {
		for _, c := range e.Host.Containers() {
			if c.Workload == "ctr" && c.Class == class && c.Object == name {
				return c
			}
		}
		return hostd.ContainerStatus{}
	}
	waitStopped := func(name string, within time.Duration) (hostd.ContainerStatus, time.Duration) {
		st := time.Now()
		for time.Since(st) < within {
			if c := ctrOf("Box", name); c.State == "stopped" {
				return c, time.Since(st)
			}
			time.Sleep(20 * time.Millisecond)
		}
		return ctrOf("Box", name), time.Since(st)
	}
	vmGone := func(pid int) bool { return processGone(pid) }

	t.Run("Lifecycle", func(t *testing.T) {
		st := time.Now()
		call("/f/a/start")
		r := call("/f/a/exec", "argv", `["python3","-c","print(2 + 2)"]`)
		if r["stdout"] != "4\n" || r["exitCode"] != float64(0) {
			t.Fatalf("exec: %v", r)
		}
		set("ctr_start_to_first_exec_ms", time.Since(st).Milliseconds())
		c := ctrOf("Box", "a")
		set("ctr_boot_ready_ms (hostd BootContainer)", c.ReadyMS)
		set("ctr_rss_kb_after_python (lite: 256 MiB guest)", c.RSSKB)
		checkConfinement(t, c.Jail, "6250 ") // lite: cpu.max 1/16
		if r := call("/f/a/exec", "argv", `["sh","-c","echo out; echo err >&2; exit 3"]`); r["exitCode"] != float64(3) || r["stdout"] != "out\n" || r["stderr"] != "err\n" {
			t.Fatalf("exit/stderr: %v", r)
		}
		if r := call("/f/a/stream"); r["first"] != "first\n" || r["code"] != float64(7) || r["tFirst"].(float64) > 900 {
			t.Fatalf("streaming: %v", r)
		}
		body := strings.Repeat("0123456789", 30000)
		if code, b, _ := do("POST", cbase+"/f/a/stdin", []byte(body)); code != 200 || !strings.Contains(string(b), body[:5000]) {
			t.Fatalf("stdin: %d %.200s", code, b)
		}
		if r := call("/f/a/kill"); r["code"] != float64(137) {
			t.Fatalf("kill(9): %v", r)
		}
		var lat []time.Duration
		for i := 0; i < 30; i++ {
			st := time.Now()
			if r := call("/f/a/exec", "argv", `["true"]`); r["exitCode"] != float64(0) {
				t.Fatal(r)
			}
			lat = append(lat, time.Since(st))
		}
		set("ctr_exec_true_e2e_p50_ms (client→tailnet→DO→hostd→VM, lite)", pct(lat, .5).Milliseconds())
		set("ctr_exec_true_e2e_p95_ms", pct(lat, .95).Milliseconds())

		call("/f/m/start", "entrypoint", `["sh","-c","sleep 1; exit 5"]`)
		if r := call("/f/m/monitor"); !strings.Contains(fmt.Sprint(r["rejected"]), "unexpected exit code: 5") || r["running"] != false {
			t.Fatalf("monitor: %v", r)
		}
		if r := call("/f/a/exec", "argv", `["sh","-c","echo x > /workspace/persist.txt && cat /workspace/persist.txt"]`); r["stdout"] != "x\n" {
			t.Fatalf("write to scratch: %v", r)
		}
		pid := ctrOf("Box", "a").PID
		if r := call("/f/a/destroy"); r["running"] != false {
			t.Fatal(r)
		}
		if !vmGone(pid) {
			t.Fatalf("VMM %d still alive after destroy", pid)
		}
		call("/f/a/start")
		if r := call("/f/a/exec", "argv", `["cat","/workspace/persist.txt"]`); r["exitCode"] == float64(0) {
			t.Fatalf("scratch survived destroy (disks must be ephemeral): %v", r)
		}
		if c := ctrOf("Box", "a"); c.Gen != 2 || c.State != "running" {
			t.Fatalf("restart: %+v", c)
		}
	})

	t.Run("PortsAndAttribution", func(t *testing.T) {
		call("/f/srv/start", "entrypoint", `["python3","-m","http.server","8000","--bind","0.0.0.0","--directory","/etc"]`)
		var r map[string]any
		for i := 0; i < 50; i++ {
			r = call("/f/srv/port", "p", "8000")
			if r["status"] != nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if r["status"] != float64(404) { // python's 404 for /hello: the request reached the container
			t.Fatalf("getTcpPort: %v", r)
		}
		if r := call("/f/srv/port", "p", "8001"); !strings.Contains(fmt.Sprint(r["error"]), "the container is not listening") {
			t.Fatalf("closed port: %v", r)
		}
		if r := call("/f/other/running"); r["running"] != false {
			t.Fatalf("another object sees srv's container: %v", r)
		}
		if r := call("/f/other/exec", "argv", `["true"]`); r["error"] == nil {
			t.Fatalf("another object's exec ran: %v", r)
		}
		if r := call("/p/z/forge", "target", "srv", "op", "destroy"); r["ok"] != false {
			t.Fatalf("plain object destroyed a facet object's container: %v", r)
		}
		if c := ctrOf("Box", "srv"); c.State != "running" {
			t.Fatalf("srv after attempts: %+v", c)
		}
	})

	t.Run("InactivityTimeout", func(t *testing.T) {
		call("/f/i/start")
		call("/f/i/exec", "argv", `["true"]`)
		pid := ctrOf("Box", "i").PID
		call("/f/i/idle", "ms", "1500")
		c, d := waitStopped("i", 20*time.Second)
		if c.State != "stopped" || c.ExitReason != "inactivity timeout" || !vmGone(pid) {
			t.Fatalf("idle container: %+v (vm gone %v)", c, vmGone(pid))
		}
		set("ctr_idle_1500ms_stopped_after_ms", d.Milliseconds())
	})

	t.Run("KillCascades", func(t *testing.T) {
		call("/f/k/start")
		call("/f/k/exec", "argv", `["true"]`)
		pid := ctrOf("Box", "k").PID
		e.WaitObject("ctr", "Box", "k", 60*time.Second)
		st := time.Now()
		call("/f/k/selfkill")
		c, _ := waitStopped("k", 10*time.Second)
		d := time.Since(st)
		if c.State != "stopped" || !strings.HasPrefix(c.ExitReason, "object killed") || !vmGone(pid) {
			t.Fatalf("kill cascade: %+v (vm gone %v)", c, vmGone(pid))
		}
		set("ctr_kill_to_vm_gone_ms", d.Milliseconds())
		if d > time.Second {
			t.Fatalf("kill → container VM gone took %v (ADR-032: sub-second)", d)
		}
	})

	t.Run("RSS", func(t *testing.T) {
		rss := map[string]int64{}
		for _, c := range e.Host.Containers() {
			if c.State == "running" {
				rss[c.Workload+"/"+c.Class+"/"+c.Object] = c.RSSKB
			}
		}
		set("rss_kb_per_running_container_vm", rss)
	})
}
