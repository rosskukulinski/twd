package imagebuild

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosskukulinski/twd/internal/container"
	"github.com/rosskukulinski/twd/internal/oci"
	"github.com/rosskukulinski/twd/internal/registry"
)

func pushBase(t *testing.T, c *oci.Client, repo, tag string, arches ...string) {
	t.Helper()
	ctx := context.Background()
	idx := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeIndex}
	for _, a := range arches {
		var layer bytes.Buffer
		tw := tar.NewWriter(&layer)
		body := "base for " + a + "\n"
		tw.WriteHeader(&tar.Header{Name: "base.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
		tw.Close()
		cfg, _ := json.Marshal(map[string]any{"architecture": a, "os": "linux", "config": map[string]any{"Cmd": []string{"/bin/true"}},
			"rootfs": map[string]any{"type": "layers", "diff_ids": []string{oci.Digest(layer.Bytes())}}})
		for _, b := range [][]byte{layer.Bytes(), cfg} {
			if _, err := c.PutBlobBytes(ctx, repo, b); err != nil {
				t.Fatal(err)
			}
		}
		m, _ := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest,
			Config: &oci.Descriptor{MediaType: oci.MediaTypeConfig, Digest: oci.Digest(cfg), Size: int64(len(cfg))},
			Layers: []oci.Descriptor{{MediaType: oci.MediaTypeLayer, Digest: oci.Digest(layer.Bytes()), Size: int64(layer.Len())}}})
		d, err := c.PutManifest(ctx, repo, oci.Digest(m), oci.MediaTypeManifest, m)
		if err != nil {
			t.Fatal(err)
		}
		idx.Manifests = append(idx.Manifests, oci.Descriptor{MediaType: oci.MediaTypeManifest, Digest: d, Size: int64(len(m)), Platform: &oci.Platform{OS: "linux", Architecture: a}})
	}
	b, _ := json.Marshal(idx)
	if _, err := c.PutManifest(ctx, repo, tag, oci.MediaTypeIndex, b); err != nil {
		t.Fatal(err)
	}
}

// TestBuildAndPush runs twd deploy's builder for real (docker buildx): a
// Dockerfile whose FROM is an alias source (written through an ARG
// default, as the Sandbox SDK 1.0 example does) builds for both arches
// against the alias target fetched from twd's registry, without touching
// the network, and lands in the tenant's namespace as an index whose
// per-arch images carry the base layer plus the app's.
func TestBuildAndPush(t *testing.T) {
	if os.Getenv("TWD_TEST_DOCKER") == "" {
		t.Skip("set TWD_TEST_DOCKER=1 to run docker buildx (make test-images)")
	}
	if err := exec.Command("docker", "buildx", "version").Run(); err != nil {
		t.Skip("docker buildx unavailable:", err)
	}
	ctx := context.Background()
	g := &registry.Registry{Dir: t.TempDir(), Meta: &registry.MemMeta{}, Authorize: func(r *http.Request, ns string, push bool) error {
		if ns != "" && ns != "tna" && ns != registry.PublicNamespace {
			return &registry.Denied{Reason: "tna only"}
		}
		return nil
	}}
	srv := httptest.NewServer(g)
	defer srv.Close()
	reg := &oci.Client{Base: srv.URL, HTTP: srv.Client()}
	pushBase(t, reg, "public/twdtest-base", "1", "amd64", "arm64")

	app := t.TempDir()
	os.MkdirAll(filepath.Join(app, "box"), 0o755)
	os.WriteFile(filepath.Join(app, "box", "Dockerfile"), []byte("ARG BASE=docker.io/twdtest/base:1\nFROM ${BASE}\nCOPY hello.txt /hello.txt\n"), 0o644)
	os.WriteFile(filepath.Join(app, "box", "hello.txt"), []byte("hello from the app\n"), 0o644)
	classes, err := container.Parse([]byte(`{"containers": [{"class_name": "Box", "image": "./box/Dockerfile"},
		{"class_name": "Multi", "images": {"tools": {"dockerfile": "./box"}}}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := Plan(app, classes)
	if err != nil || len(jobs) != 2 || jobs[1].Name != "tools" || jobs[1].Dockerfile != filepath.Join(app, "box", "Dockerfile") {
		t.Fatalf("plan %+v %v", jobs, err)
	}
	var log bytes.Buffer
	built, results, err := BuildAndPush(ctx, jobs[:1], Options{Tenant: "tna", Workload: "w", Arches: []string{"amd64", "arm64"},
		Aliases: []registry.Alias{{From: "docker.io/twdtest/base:1", To: "twd.local/twdtest-base:1"}}, Registry: reg, Log: &log})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	ref := built["Box"].Image
	if !strings.HasPrefix(ref, "twd.local/tna/w-box@sha256:") || len(results[0].Platforms) != 2 {
		t.Fatalf("built %+v %+v", built, results)
	}
	r, _ := oci.ParseRef(ref)
	_, mt, _, err := reg.GetManifest(ctx, "tna/w-box", r.Digest)
	if err != nil || mt != oci.MediaTypeIndex {
		t.Fatalf("index: %s %v", mt, err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		d, err := oci.Resolve(ctx, reg, "tna/w-box", r.Digest, arch)
		if err != nil {
			t.Fatal(err)
		}
		mb, _, _, _ := reg.GetManifest(ctx, "tna/w-box", d)
		m, _ := oci.ParseManifest(mb, "")
		cfg, err := oci.ReadConfig(ctx, reg, "tna/w-box", m)
		if err != nil || cfg.Architecture != arch || len(m.Layers) != 2 {
			t.Fatalf("%s image: %+v layers %d %v", arch, cfg, len(m.Layers), err)
		}
		// The first layer is the alias target's (twd's registry), not
		// anything fetched from Docker Hub.
		bd, _ := oci.Resolve(ctx, reg, "public/twdtest-base", "1", arch)
		bb, _, _, _ := reg.GetManifest(ctx, "public/twdtest-base", bd)
		bm, _ := oci.ParseManifest(bb, "")
		if m.Layers[0].Digest != bm.Layers[0].Digest {
			rc, _ := reg.GetBlob(ctx, "tna/w-box", m.Layers[0].Digest, -1)
			got, _ := io.ReadAll(rc)
			t.Fatalf("%s base layer %s, want the alias target's %s (%d bytes)", arch, m.Layers[0].Digest, bm.Layers[0].Digest, len(got))
		}
	}
	if !strings.Contains(log.String(), "alias      docker.io/twdtest/base:1") {
		t.Fatalf("no alias line:\n%s", log.String())
	}
}
