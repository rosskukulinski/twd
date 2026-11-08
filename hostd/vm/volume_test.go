//go:build linux

package vm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm/helperapi"
)

// A worker that reads and writes files on the DO volume through a
// writable workerd disk service.
const volJS = `export default {
  async fetch(req, env) {
    const u = new URL(req.url);
    if (u.pathname === "/_twd/admin") return new Response("ok");
    const k = u.searchParams.get("k");
    if (u.pathname === "/put") { const r = await env.DISK.fetch("http://disk/" + k, { method: "PUT", body: u.searchParams.get("v") }); return new Response("put " + r.status); }
    if (u.pathname === "/get") { const r = await env.DISK.fetch("http://disk/" + k); return new Response(r.status === 200 ? await r.text() : "missing " + r.status); }
    return new Response("?");
  }
};
`

const volCapnp = `using Workerd = import "/workerd/workerd.capnp";
const config :Workerd.Config = (
  services = [ (name = "main", worker = .w), (name = "disk", disk = (path = "/run/twd/do", writable = true)) ],
  sockets = [ (name = "in", address = "*:8080", service = "main", http = (cfBlobHeader = "x-twd-cf")) ],
);
const w :Workerd.Worker = (
  compatibilityDate = "2026-04-01",
  modules = [ (name = "w.js", esModule = embed "w.js") ],
  bindings = [ (name = "DISK", service = "disk") ],
);
`

// TestVolume: the persistent DO volume through the helper: data survives
// a VM restart; one volume is never attached to two VMs; an older epoch is
// fenced; a wrong key doesn't open it; the key is in no guest memory.
func TestVolume(t *testing.T) {
	kernel, rootfs := os.Getenv("TWD_VM_KERNEL"), os.Getenv("TWD_VM_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("TWD_VM_KERNEL / TWD_VM_ROOTFS not set")
	}
	b, err := New(Config{Kernel: kernel, Rootfs: rootfs, HugePages: os.Getenv("TWD_VM_HUGEPAGES"), Helper: os.Getenv("TWD_VM_HELPER")})
	if err != nil {
		t.Skip(err)
	}
	defer b.Close()
	t.Logf("backend via %s", b.Via())
	app := t.TempDir()
	os.WriteFile(filepath.Join(app, "w.js"), []byte(volJS), 0o600)
	os.WriteFile(filepath.Join(app, "config.capnp"), []byte(volCapnp), 0o600)
	img := filepath.Join(t.TempDir(), "do.img")
	key := make([]byte, 32)
	rand.Read(key)
	ctx := context.Background()
	boot := func(name string, epoch int64, k []byte) (sandbox.VM, error) {
		return b.Boot(ctx, sandbox.Spec{ID: name, AppDir: app, WorkerdArgs: []string{"serve", "config.capnp", "--experimental"}, HealthCF: `{}`,
			Volume: &sandbox.Volume{Path: img, Name: "ten/wl", Epoch: epoch, Key: k, SizeMiB: 256}})
	}
	get := func(m sandbox.VM, path string) string {
		cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return m.DialGuest(ctx) }}}
		resp, err := cl.Get("http://guest" + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	st := time.Now()
	a, err := boot("va", 2, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first boot (format) %v: %+v", time.Since(st), a.Info())
	if !a.Info().VolumeNew {
		t.Fatal("first boot did not create the volume")
	}
	if r := get(a, "/put?k=x&v=persisted-1"); r != "put 204" && r != "put 200" && r != "put 201" {
		t.Fatalf("put: %s", r)
	}
	sr, _ := a.Op(ctx, proto.Msg{Op: "stats"})
	if sr.Stats == nil || !strings.HasPrefix(sr.Stats.DOFS, proto.VolumeDev+" ext4") {
		t.Fatalf("DO dir is not the volume: %+v", sr.Stats)
	}
	br, err := a.Op(ctx, proto.Msg{Op: "fsbench", N: 200})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fsbench %+v", *br.Bench)
	// No key in the guest: not in its RAM, nor in the (encrypted) image.
	dir := t.TempDir()
	mem, err := a.MemoryDump(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := os.ReadFile(mem)
	ib, _ := os.ReadFile(img)
	for _, n := range [][]byte{key, []byte(hex.EncodeToString(key)), []byte(base64.StdEncoding.EncodeToString(key))} {
		if bytes.Contains(mb, n) || bytes.Contains(ib, n) {
			t.Fatal("volume key found in guest memory or the image")
		}
	}
	if !bytes.Contains(mb, []byte("persisted-1")) {
		t.Log("(the written value is not in the RAM dump: fine)")
	}
	os.Remove(mem)
	t.Logf("no volume key in %d MiB of guest RAM or the %d MiB image", len(mb)>>20, len(ib)>>20)
	if bytes.Contains(ib, []byte("persisted-1")) {
		t.Fatal("plaintext found in the volume image: not encrypted")
	}

	// Two VMs never share a volume.
	if _, err := boot("vbusy", 2, key); !helperapi.IsCode(err, helperapi.CodeVolumeBusy) {
		t.Fatalf("second attach while attached: want volume_busy, got %v", err)
	}
	st = time.Now()
	a.Stop()
	t.Logf("stop (guest unmount + release) %v", time.Since(st))

	st = time.Now()
	bvm, err := boot("vb", 2, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("restart → ready %v (volume attach %.1f ms)", time.Since(st), bvm.Info().VolumeAttachMS)
	if r := get(bvm, "/get?k=x"); r != "persisted-1" {
		t.Fatalf("after restart: %q", r)
	}
	// Crash (SIGKILL of the VMM) after an explicit sync in the guest.
	get(bvm, "/put?k=y&v=persisted-2")
	bvm.Op(ctx, proto.Msg{Op: "fsbench", N: 1}) // fdatasync on the volume
	bvm.Kill()
	<-bvm.Done()
	if _, err := boot("vold", 1, key); !helperapi.IsCode(err, helperapi.CodeVolumeFenced) {
		t.Fatalf("older epoch: want volume_fenced, got %v", err)
	}
	bad := make([]byte, 32)
	rand.Read(bad)
	if _, err := boot("vkey", 3, bad); !helperapi.IsCode(err, helperapi.CodeVolumeKey) {
		t.Fatalf("wrong key: want volume_key, got %v", err)
	}
	c, err := boot("vc", 3, key)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	if r := get(c, "/get?k=x"); r != "persisted-1" {
		t.Fatalf("after crash, x: %q", r)
	}
	t.Logf("after crash: y = %q", get(c, "/get?k=y"))
}

// checkJailed: the VMM runs as the jail uid, chrooted, in its cgroup.
func checkJailed(t *testing.T, pid int, j *sandbox.Jail) {
	st, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range strings.Split(string(st), "\n") {
		if strings.HasPrefix(ln, "Uid:") {
			f := strings.Fields(ln)
			if f[1] != strconv.Itoa(j.UID) {
				t.Fatalf("VMM uid %s, want %d", f[1], j.UID)
			}
		}
		if strings.HasPrefix(ln, "Seccomp:") && strings.TrimSpace(strings.TrimPrefix(ln, "Seccomp:")) != "2" {
			t.Fatalf("VMM seccomp mode %s, want 2 (filter)", ln)
		}
	}
	cg, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if !strings.Contains(string(cg), j.Cgroup) {
		t.Fatalf("VMM cgroup %s, want %s", cg, j.Cgroup)
	}
	// Its root is the jail: the API socket is there, the host's /etc isn't.
	if _, err := os.Stat(fmt.Sprintf("/proc/%d/root/api.sock", pid)); err != nil {
		t.Fatalf("VMM root has no api.sock: not chrooted? %v", err)
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d/root/etc/passwd", pid)); err == nil {
		t.Fatal("VMM root has /etc/passwd: not chrooted")
	}
	cpu, _ := os.ReadFile(filepath.Join("/sys/fs/cgroup", j.Cgroup, "cpu.max"))
	mem, _ := os.ReadFile(filepath.Join("/sys/fs/cgroup", j.Cgroup, "memory.max"))
	t.Logf("VMM pid %d uid %d seccomp filter, cpu.max %s memory.max %s", pid, j.UID, strings.TrimSpace(string(cpu)), strings.TrimSpace(string(mem)))
}
