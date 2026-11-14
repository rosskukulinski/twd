//go:build e2econtainer && linux

package container

// E2E-CONTAINER-REGISTRY (ADR-032 D3): the acceptance examples deployed
// from their own Dockerfiles through twd's registry to Firecracker
// containers. The images were built on the developer machine by twd
// deploy's builder (build_test.go, `make e2e-container-build`: docker
// buildx, alias map as named contexts); here they are pushed with the
// same push code twd deploy uses, the deploy pins them by digest, and
// hostd, whose image store has no other source, pulls exactly those
// digests from the registry as a host, converts them and boots them.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/image"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/vm"
	"github.com/rosskukulinski/twd/internal/imagebuild"
	"github.com/rosskukulinski/twd/internal/oci"
	"github.com/rosskukulinski/twd/internal/registry"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/tailcfg"
)

type builtImage struct {
	App    string `json:"app"`
	Class  string `json:"class"`
	Name   string `json:"name,omitempty"`
	Arch   string `json:"arch"`
	Layout string `json:"layout"`
	Ms     int64  `json:"buildMs"`
}

func TestE2EContainerRegistry(t *testing.T) {
	kernel, ckernel, rootfs := os.Getenv("TWD_VM_KERNEL"), os.Getenv("TWD_VM_CONTAINER_KERNEL"), os.Getenv("TWD_VM_ROOTFS")
	imgs, apps, builtDir := os.Getenv("TWD_CTR_IMAGES"), os.Getenv("TWD_CTR_APPS"), os.Getenv("TWD_CTR_BUILT")
	if kernel == "" || ckernel == "" || rootfs == "" || imgs == "" || apps == "" || builtDir == "" {
		t.Skip("TWD_VM_KERNEL, TWD_VM_CONTAINER_KERNEL, TWD_VM_ROOTFS, TWD_CTR_IMAGES, TWD_CTR_APPS and TWD_CTR_BUILT must be set (make e2e-container-registry)")
	}
	var built []builtImage
	if b, err := os.ReadFile(filepath.Join(builtDir, "built.json")); err != nil {
		t.Fatalf("no built images (make e2e-container-build on the Mac): %v", err)
	} else if err := json.Unmarshal(b, &built); err != nil {
		t.Fatal(err)
	}
	be, err := vm.New(vm.Config{Kernel: kernel, ContainerKernel: ckernel, Rootfs: rootfs, HugePages: os.Getenv("TWD_VM_HUGEPAGES"), Helper: os.Getenv("TWD_VM_HELPER")})
	if err != nil {
		t.Fatalf("microVM backend unavailable: %v", err)
	}
	t.Cleanup(func() { be.Close() })
	// The host's image store: no DirSource, no aliases, no public list.
	// Pinned pulls from the registry are its only source.
	store := &image.Store{Dir: filepath.Join(t.TempDir(), "images")}

	ctl, err := nettest.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctl.Close)
	ctl.Server.SetGlobalAppCaps(tailcfg.PeerCapMap{nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue}})
	regDir := t.TempDir()
	e := hostdtest.Start(t, hostdtest.Options{ControlURL: ctl.URL, Ephemeral: true, KillTombstone: 5 * time.Second,
		Fake:       fakemgmt.Config{StaticKey: nettest.DummyKey, RegistryDir: regDir, ImageAliases: registry.DefaultAliases},
		Tenant:     mgmtapi.Tenant{Tailnet: "ttest.ts.net", ControlURL: ctl.URL, MintMode: "static"},
		Containers: be, Images: store, ContainerGrace: 3 * time.Second})
	ctx := context.Background()
	results := map[string]any{}
	set := func(k string, v any) { results[k] = v }
	defer func() {
		b, _ := json.MarshalIndent(results, "", "  ")
		t.Logf("results:\n%s", b)
		if p := os.Getenv("TWD_E2E_RESULTS"); p != "" {
			os.WriteFile(p, b, 0o644)
		}
	}()

	// The operator pushes the public bases (twd image push): the raw flow's
	// image and the arm64 alias targets.
	adm := e.Mgmt.OCI() // the local admin (no host ID)
	var idx map[string]string
	ib, err := os.ReadFile(filepath.Join(imgs, "images.json"))
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(ib, &idx)
	for ref, file := range idx {
		r, _ := oci.ParseRef(ref)
		if r.Registry != registry.LocalRegistry || strings.HasPrefix(r.Repository, "e2e/") {
			continue // the prebuilt example images are not used here
		}
		l, err := oci.OpenLayout(filepath.Join(imgs, file))
		if err != nil {
			t.Fatal(err)
		}
		st := time.Now()
		cp, err := oci.Copy(ctx, l, "", l.Top.Digest, adm, registry.PublicNamespace+"/"+r.Repository, nil, r.Tag)
		l.Close()
		if err != nil {
			t.Fatalf("push %s: %v", ref, err)
		}
		set("public_push "+ref, map[string]any{"ms": time.Since(st).Milliseconds(), "mb": cp.Bytes >> 20, "digest": cp.Digest})
	}

	// The developer pushes twd deploy's builds (imagebuild.Push) and
	// deploys with containerImages.
	pushed := map[string]map[string]mgmtapi.BuiltImages{}
	refs := map[string]string{}
	for _, b := range built {
		j := imagebuild.Job{Class: b.Class, Name: b.Name}
		wl := map[string]string{"v0min": "sbx0", "v1min": "sbx1"}[b.App]
		st := time.Now()
		res, err := imagebuild.Push(ctx, adm, j.Repo(e.TenantID, wl), map[string]string{b.Arch: filepath.Join(builtDir, b.Layout)}, testLog{t})
		if err != nil {
			t.Fatalf("push %s: %v", b.Layout, err)
		}
		set("dev_push "+b.App, map[string]any{"ms": time.Since(st).Milliseconds(), "ref": res.Ref, "build_ms_on_mac": b.Ms})
		bi := pushed[b.App]
		if bi == nil {
			bi = map[string]mgmtapi.BuiltImages{}
			pushed[b.App] = bi
		}
		c := bi[b.Class]
		if b.Name == "" {
			c.Image = res.Ref
		} else {
			c.Images = map[string]string{b.Name: res.Ref}
		}
		bi[b.Class] = c
		refs[b.App] = res.Ref
	}
	build := func(dir string) map[string][]byte {
		files, err := hostdtest.Build(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	// The twd.jsonc twd deploy sees without pins (the prep script's pins
	// are for the DirSource e2e): the Dockerfile flow.
	v0 := build(filepath.Join(apps, "v0min"))
	v0["twd.jsonc"] = []byte(`{}`)
	v1 := build(filepath.Join(apps, "v1min"))
	v1["twd.jsonc"] = []byte(`{"tailnet": {"objects": {"Sandbox": {"egress": "workload"}}}}`)
	raw := build(filepath.Join(apps, "raw10"))
	deploy := func(files map[string][]byte, wl string, bi map[string]mgmtapi.BuiltImages) string {
		d, err := e.DeployBuilt(files, wl, bi)
		if err != nil {
			t.Fatalf("deploy %s: %v", wl, err)
		}
		return d
	}
	dV0, dV1, dRaw := deploy(v0, "sbx0", pushed["v0min"]), deploy(v1, "sbx1", pushed["v1min"]), deploy(raw, "raw10", nil)
	wV0, wV1, wRaw := e.WaitActive("sbx0", dV0, 90*time.Second), e.WaitActive("sbx1", dV1, 90*time.Second), e.WaitActive("raw10", dRaw, 90*time.Second)

	// The pins mgmt recorded.
	as, err := e.Mgmt.Assignments(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]string{}
	for _, a := range as {
		for _, c := range a.Containers {
			for _, p := range c.Pins {
				pins[a.Workload] = p.Repository + "@" + p.Digest
				if p.Arch != "arm64" {
					t.Fatalf("%s pinned for %s", a.Workload, p.Arch)
				}
			}
		}
	}
	set("pins", pins)
	if !strings.HasPrefix(pins["sbx0"], e.TenantID+"/sbx0-sandbox@sha256:") || !strings.HasPrefix(pins["sbx1"], e.TenantID+"/sbx1-sandbox-sandbox@sha256:") ||
		!strings.HasPrefix(pins["raw10"], "public/debian-trixie-python@sha256:") {
		t.Fatalf("pins %v", pins)
	}

	client, err := nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-client", filepath.Join(e.Dir, "client"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	for _, ip := range []string{wV0.IP, wV1.IP, wRaw.IP} {
		c, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := client.WaitPeers(c, ip); err != nil {
			t.Fatal(err)
		}
		cancel()
	}
	hc := client.HTTPClient(180 * time.Second)
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

	t.Run("A_SandboxSDK0x_minimal_fromDockerfile", func(t *testing.T) {
		r, d := getJSON("http://" + wV0.IP + "/run")
		if r["_status"] != 200 || strings.TrimSpace(fmt.Sprint(r["output"])) != "2 + 2 = 4" || r["success"] != true {
			t.Fatalf("/run: %v", r)
		}
		set("a_first_run_ms (pull + convert + boot + Bun server + exec)", d.Milliseconds())
		if r, _ = getJSON("http://" + wV0.IP + "/file"); r["content"] != "Hello, Sandbox!" {
			t.Fatalf("/file: %v", r)
		}
	})
	t.Run("B_SandboxSDK10_minimal_fromDockerfile", func(t *testing.T) {
		base := "http://" + wV1.IP + "/sandboxes/s1"
		code, b, d := do("POST", base+"/exec", []byte(`{"argv":["sh","-c","echo hi from $(uname -m)"]}`))
		if code != 200 || !strings.Contains(string(b), `"stdout":"hi from aarch64\n"`) {
			t.Fatalf("exec: %d %s", code, b)
		}
		set("b_min_first_exec_ms (pull + convert + boot + exec)", d.Milliseconds())
		if code, b, _ := do("PUT", base+"/files/hello.txt", []byte("Hello, Sandbox!")); code != 204 {
			t.Fatalf("writeFile: %d %s", code, b)
		}
		if code, b, _ := do("GET", base+"/files/hello.txt", nil); code != 200 || string(b) != "Hello, Sandbox!" {
			t.Fatalf("readFile: %d %q", code, b)
		}
	})
	t.Run("B_raw_ctx_container_publicBase", func(t *testing.T) {
		r, d := getJSON("http://" + wRaw.IP + "/run/r1")
		if r["stdout"] != "4\n" || r["exitCode"] != float64(0) {
			t.Fatalf("python: %v", r)
		}
		set("b_raw_first_exec_ms (pull + convert + boot + exec)", d.Milliseconds())
	})
	t.Run("WhatRanIsWhatWasPinned", func(t *testing.T) {
		for _, c := range e.Host.Containers() {
			want := refs[map[string]string{"sbx0": "v0min", "sbx1": "v1min"}[c.Workload]]
			if c.Workload == "raw10" {
				want = "twd.local/debian-trixie-python:1"
			}
			if c.Image != want || !oci.ValidDigest(c.Digest) {
				t.Fatalf("%s runs %s (%s), want %s", c.Workload, c.Image, c.Digest, want)
			}
			set("container "+c.Workload, map[string]any{"image": c.Image, "image_id": c.Digest, "ready_ms": c.ReadyMS, "rss_kb": c.RSSKB})
		}
		// The converted images: the public base in the shared scope, the
		// tenant's builds in its own.
		scopes := map[string]int{}
		filepath.Walk(store.Dir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && strings.HasSuffix(p, ".erofs") {
				scopes[filepath.Base(filepath.Dir(p))]++
			}
			return nil
		})
		if scopes[image.PublicScope] != 1 || scopes[e.TenantID] != 2 || len(scopes) != 2 {
			t.Fatalf("cache scopes %v", scopes)
		}
		set("cache_scopes", scopes)
	})
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}
