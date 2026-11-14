package image

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rosskukulinski/twd/internal/container"
	"github.com/rosskukulinski/twd/internal/oci"
	"github.com/rosskukulinski/twd/internal/registry"
)

type pinEnv struct {
	reg   *registry.Registry
	srv   *httptest.Server
	adm   *oci.Client
	pulls atomic.Int64 // manifest GETs the host made
	store *Store
}

func newPinEnv(t *testing.T) *pinEnv {
	t.Helper()
	mk, err := exec.LookPath("mkfs.erofs")
	if err != nil {
		mk = fakeMkfs(t)
	}
	e := &pinEnv{}
	e.reg = &registry.Registry{Dir: t.TempDir(), Meta: &registry.MemMeta{}, Authorize: func(r *http.Request, ns string, push bool) error {
		if r.Header.Get("X-Who") == "admin" {
			return nil
		}
		if push {
			return &registry.Denied{Reason: "hosts pull"}
		}
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/manifests/") {
			e.pulls.Add(1)
		}
		return nil
	}}
	e.srv = httptest.NewServer(e.reg)
	t.Cleanup(e.srv.Close)
	e.adm = &oci.Client{Base: e.srv.URL, HTTP: e.srv.Client(), Edit: func(r *http.Request) { r.Header.Set("X-Who", "admin") }}
	host := &oci.Client{Base: e.srv.URL, HTTP: e.srv.Client()}
	e.store = &Store{Dir: t.TempDir(), Mkfs: mk, Puller: RegistryPuller{Client: host}}
	return e
}

// push writes a linux/arch image whose single file holds body into repo.
func (e *pinEnv) push(t *testing.T, repo, arch, body string) string {
	t.Helper()
	ctx := context.Background()
	layer := layerTar(t, ent{name: "etc/id", body: body})
	cfg := cfgJSON(arch)
	for _, b := range [][]byte{layer, cfg} {
		if _, err := e.adm.PutBlobBytes(ctx, repo, b); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest,
		Config: &oci.Descriptor{MediaType: oci.MediaTypeConfig, Digest: oci.Digest(cfg), Size: int64(len(cfg))},
		Layers: []oci.Descriptor{{MediaType: oci.MediaTypeLayer, Digest: oci.Digest(layer), Size: int64(len(layer))}}})
	d, err := e.adm.PutManifest(ctx, repo, oci.Digest(m), oci.MediaTypeManifest, m)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func pin(repo, digest string) container.Pin {
	return container.Pin{Ref: "app-ref", Arch: runtime.GOARCH, Repository: repo, Digest: digest}
}

// TestPinnedScopes (ADR-032 D3): a tenant's pinned image is cached in its
// own scope; the same image pushed by another tenant is pulled and
// converted again for that tenant (no cross-tenant cache hit); a public
// base is pulled once and shared; a pin into another tenant's namespace
// is refused before anything is fetched.
func TestPinnedScopes(t *testing.T) {
	ctx := context.Background()
	e := newPinEnv(t)
	dA := e.push(t, "tna/app", runtime.GOARCH, "same bytes")
	dB := e.push(t, "tnb/app", runtime.GOARCH, "same bytes") // byte-identical image, B's own push
	if dA != dB {
		t.Fatal("expected identical digests")
	}
	a, err := e.store.GetPinned(ctx, "tna", pin("tna/app", dA))
	if err != nil {
		t.Fatal(err)
	}
	if a.Scope != "tna" || !strings.Contains(a.Path, "/tna/") || a.PullMS == 0 && a.ConvertMS == 0 {
		t.Fatalf("A: %+v", a)
	}
	b, err := e.store.GetPinned(ctx, "tnb", pin("tnb/app", dB))
	if err != nil {
		t.Fatal(err)
	}
	if b.Scope != "tnb" || b.Path == a.Path || b.ConvertMS == 0 && b.PullMS == 0 {
		t.Fatalf("tenant B hit tenant A's cache: %+v", b)
	}
	if e.pulls.Load() != 2 {
		t.Fatalf("%d manifest pulls, want 2 (one per tenant)", e.pulls.Load())
	}
	// A second start of A's image is a cache hit: no registry traffic.
	again, err := e.store.GetPinned(ctx, "tna", pin("tna/app", dA))
	if err != nil || again.Path != a.Path || again.ConvertMS != 0 || e.pulls.Load() != 2 {
		t.Fatalf("expected a hit: %+v %v pulls %d", again, err, e.pulls.Load())
	}
	// Tenant B naming A's namespace: refused, nothing fetched.
	n := e.pulls.Load()
	if _, err := e.store.GetPinned(ctx, "tnb", pin("tna/app", dA)); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("cross-tenant pin: %v", err)
	}
	if e.pulls.Load() != n {
		t.Fatal("a refused pin reached the registry")
	}
	// Public: one pull, one conversion, shared.
	dP := e.push(t, "public/base", runtime.GOARCH, "public")
	pa, err := e.store.GetPinned(ctx, "tna", pin("public/base", dP))
	if err != nil {
		t.Fatal(err)
	}
	pb, err := e.store.GetPinned(ctx, "tnb", pin("public/base", dP))
	if err != nil {
		t.Fatal(err)
	}
	if pa.Scope != PublicScope || pa.Path != pb.Path || pb.ConvertMS != 0 || e.pulls.Load() != n+1 {
		t.Fatalf("public base not shared: %+v %+v pulls %d", pa, pb, e.pulls.Load()-n)
	}
	// A pin for another arch is refused.
	other := pin("tna/app", dA)
	other.Arch = "s390x"
	if _, err := e.store.GetPinned(ctx, "tna", other); err == nil || !strings.Contains(err.Error(), "s390x") {
		t.Fatalf("wrong-arch pin: %v", err)
	}
}

// TestPinnedTamperedBlob: content that doesn't hash to its digest is never
// converted.
func TestPinnedTamperedBlob(t *testing.T) {
	ctx := context.Background()
	e := newPinEnv(t)
	d := e.push(t, "tna/app", runtime.GOARCH, "original")
	b, _, _, _ := e.reg.Manifest(ctx, "tna/app", d)
	var m oci.Manifest
	json.Unmarshal(b, &m)
	p := e.reg.Dir + "/blobs/sha256/" + oci.Hex(m.Layers[0].Digest)
	orig, _ := os.ReadFile(p)
	os.WriteFile(p, bytes.Replace(orig, []byte("original"), []byte("tampered"), 1), 0o600)
	if _, err := e.store.GetPinned(ctx, "tna", pin("tna/app", d)); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered layer: %v", err)
	}
}

// TestCacheGC: past MaxBytes the least recently used images go; an image
// in use (Hold) stays.
func TestCacheGC(t *testing.T) {
	ctx := context.Background()
	e := newPinEnv(t)
	var imgs []*Image
	var mds []string
	for i, body := range []string{"one1", "two2", "thr3"} {
		d := e.push(t, "tna/app", runtime.GOARCH, strings.Repeat(body, 4000))
		img, err := e.store.GetPinned(ctx, "tna", pin("tna/app", d))
		if err != nil {
			t.Fatal(err)
		}
		imgs, mds = append(imgs, img), append(mds, d)
		if i == 0 {
			defer e.store.Hold(img.Path)() // the first one runs a container
		}
	}
	size := imgs[0].SizeB
	e.store.MaxBytes = size*2 + size/2 // room for two
	e.store.gc()
	t.Logf("sizes %d %d %d cap %d cache %d evicted %d", imgs[0].SizeB, imgs[1].SizeB, imgs[2].SizeB, e.store.MaxBytes, e.store.CacheBytes(), e.store.Evicted)
	if _, err := os.Stat(imgs[0].Path); err != nil {
		t.Fatal("GC removed an image in use")
	}
	if _, err := os.Stat(imgs[1].Path); err == nil {
		t.Fatal("GC kept the least recently used image")
	}
	if _, err := os.Stat(imgs[2].Path); err != nil {
		t.Fatal("GC removed the newest image")
	}
	if e.store.CacheBytes() > e.store.MaxBytes {
		t.Fatalf("cache %d > cap %d", e.store.CacheBytes(), e.store.MaxBytes)
	}
	// An evicted image is pulled again on the next start.
	n := e.pulls.Load()
	if img, err := e.store.GetPinned(ctx, "tna", pin("tna/app", mds[1])); err != nil || img.ConvertMS == 0 && img.PullMS == 0 || e.pulls.Load() != n+1 {
		t.Fatalf("evicted image not pulled again: %+v %v", img, err)
	}
}
