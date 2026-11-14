package registry

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
)

// testImage builds a one-layer linux/arch image in mem (repo "src").
func testImage(t *testing.T, mem *oci.MemTarget, arch, content string) (manifest []byte, digest string) {
	t.Helper()
	ctx := context.Background()
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	tw.WriteHeader(&tar.Header{Name: "hello.txt", Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write([]byte(content))
	tw.Close()
	cfg, _ := json.Marshal(map[string]any{"architecture": arch, "os": "linux", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{oci.Digest(layer.Bytes())}},
		"config": map[string]any{"Cmd": []string{"/bin/true"}}})
	for _, b := range [][]byte{layer.Bytes(), cfg} {
		if err := mem.WriteBlob(ctx, "src", oci.Digest(b), int64(len(b)), bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest,
		Config: &oci.Descriptor{MediaType: oci.MediaTypeConfig, Digest: oci.Digest(cfg), Size: int64(len(cfg))},
		Layers: []oci.Descriptor{{MediaType: oci.MediaTypeLayer, Digest: oci.Digest(layer.Bytes()), Size: int64(layer.Len())}}})
	if err := mem.WriteManifest(ctx, "src", oci.Digest(m), oci.MediaTypeManifest, m); err != nil {
		t.Fatal(err)
	}
	return m, oci.Digest(m)
}

// testIndex adds an index over per-arch images, tagged tag.
func testIndex(t *testing.T, mem *oci.MemTarget, tag string, arches ...string) (string, map[string]string) {
	t.Helper()
	idx := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeIndex}
	per := map[string]string{}
	for _, a := range arches {
		imgSeq++
		m, d := testImage(t, mem, a, fmt.Sprintf("hello from %s %s #%d", a, tag, imgSeq))
		per[a] = d
		idx.Manifests = append(idx.Manifests, oci.Descriptor{MediaType: oci.MediaTypeManifest, Digest: d, Size: int64(len(m)), Platform: &oci.Platform{OS: "linux", Architecture: a}})
	}
	b, _ := json.Marshal(idx)
	if err := mem.WriteManifest(context.Background(), "src", tag, oci.MediaTypeIndex, b); err != nil {
		t.Fatal(err)
	}
	mem.WriteManifest(context.Background(), "src", oci.Digest(b), oci.MediaTypeIndex, b)
	return oci.Digest(b), per
}

var imgSeq int

func testRegistry(t *testing.T) (*Registry, *httptest.Server) {
	t.Helper()
	g := &Registry{Dir: t.TempDir(), Meta: &MemMeta{}}
	g.Authorize = func(r *http.Request, ns string, push bool) error {
		w := r.Header.Get("X-Test-Who")
		switch {
		case w == "admin":
			return nil
		case strings.HasPrefix(w, "host:"):
			if push {
				return &Denied{"hosts only pull"}
			}
			if ns == "" || ns == PublicNamespace || ns == strings.TrimPrefix(w, "host:") {
				return nil
			}
			return &Denied{"this host serves no workload of " + ns}
		}
		return &Denied{"who are you"}
	}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv
}

func client(srv *httptest.Server, who string) *oci.Client {
	return &oci.Client{Base: srv.URL, HTTP: srv.Client(), Edit: func(r *http.Request) { r.Header.Set("X-Test-Who", who) }}
}

// TestPushPullAndIsolation: an image pushed into tenant A's namespace
// pulls back byte-identical by digest; tenant B's namespace sees none of
// it (HEAD/GET 404 as if absent, no cross-namespace mount), a host serving
// only B is refused A's namespace, and hosts can't push.
func TestPushPullAndIsolation(t *testing.T) {
	ctx := context.Background()
	_, srv := testRegistry(t)
	src := &oci.MemTarget{}
	idx, per := testIndex(t, src, "v1", "amd64", "arm64")
	adm := client(srv, "admin")
	cp, err := oci.Copy(ctx, src, "src", "v1", adm, "tna/app", nil, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if !cp.IndexWritten || cp.Digest != idx || cp.Platforms["arm64"] != per["arm64"] {
		t.Fatalf("copy: %+v", cp)
	}
	// Pull back by digest as a host of tenant A.
	ha := client(srv, "host:tna")
	dst := &oci.MemTarget{}
	got, err := oci.Copy(ctx, ha, "tna/app", per["arm64"], dst, "x", []string{"arm64"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != per["arm64"] {
		t.Fatalf("pulled %s, want %s", got.Digest, per["arm64"])
	}
	if tags, err := adm.Tags(ctx, "tna/app"); err != nil || len(tags) != 1 || tags[0] != "v1" {
		t.Fatalf("tags %v %v", tags, err)
	}
	if d, _, _, err := adm.HeadManifest(ctx, "tna/app", "v1"); err != nil || d != idx {
		t.Fatalf("HEAD tag: %s %v", d, err)
	}

	// Tenant B: the same digests are unknown in its namespace.
	var m oci.Manifest
	b, _, _, _ := src.Manifest(ctx, "src", per["arm64"])
	json.Unmarshal(b, &m)
	for _, d := range append([]string{m.Config.Digest}, m.Layers[0].Digest) {
		if ok, err := adm.BlobExists(ctx, "tnb/app", d); err != nil || ok {
			t.Fatalf("tenant B sees tenant A's blob %s: %v %v", d, ok, err)
		}
		if _, err := adm.GetBlob(ctx, "tnb/app", d, -1); !oci.IsNotFound(err) {
			t.Fatalf("GET tenant A's blob through B: %v", err)
		}
	}
	if _, _, _, err := adm.GetManifest(ctx, "tnb/app", per["arm64"]); !oci.IsNotFound(err) {
		t.Fatalf("tenant B reads tenant A's manifest: %v", err)
	}
	// A cross-namespace mount does not link A's blob into B: B gets an
	// upload session instead (it must send the bytes).
	req, _ := http.NewRequest("POST", srv.URL+"/v2/tnb/app/blobs/uploads/?mount="+m.Layers[0].Digest+"&from=tna/app", nil)
	req.Header.Set("X-Test-Who", "admin")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("cross-namespace mount: %d, want 202 (a fresh upload)", resp.StatusCode)
	}
	if ok, _ := adm.BlobExists(ctx, "tnb/app", m.Layers[0].Digest); ok {
		t.Fatal("mount linked another namespace's blob")
	}
	// A manifest in B referencing A's blobs is refused.
	if _, err := adm.PutManifest(ctx, "tnb/app", "stolen", oci.MediaTypeManifest, b); err == nil || !strings.Contains(err.Error(), "MANIFEST_BLOB_UNKNOWN") {
		t.Fatalf("B manifest over A's blobs: %v", err)
	}
	// A host that serves only tenant B can't read A's namespace at all.
	hb := client(srv, "host:tnb")
	if _, _, _, err := hb.GetManifest(ctx, "tna/app", per["arm64"]); err == nil || !strings.Contains(err.Error(), "DENIED") {
		t.Fatalf("host of B reads A: %v", err)
	}
	if _, err := hb.GetBlob(ctx, "tna/app", m.Config.Digest, -1); err == nil || !strings.Contains(err.Error(), "DENIED") {
		t.Fatalf("host of B reads A's blob: %v", err)
	}
	// Hosts never push, even into a namespace they may read.
	if _, err := ha.PutBlobBytes(ctx, "tna/app", []byte("x")); err == nil || !strings.Contains(err.Error(), "DENIED") {
		t.Fatalf("host push: %v", err)
	}
	if _, _, _, err := client(srv, "nobody").GetManifest(ctx, "tna/app", "v1"); err == nil {
		t.Fatal("anonymous pull allowed")
	}
}

// TestUploadProtocol: chunked uploads with Content-Range, a final PUT
// whose digest doesn't match is refused (and nothing is linked), sessions
// are bound to their repository.
func TestUploadProtocol(t *testing.T) {
	_, srv := testRegistry(t)
	do := func(method, path string, body []byte, hdr ...string) *http.Response {
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		req.Header.Set("X-Test-Who", "admin")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	resp := do("POST", "/v2/tna/x/blobs/uploads/", nil)
	if resp.StatusCode != 202 {
		t.Fatalf("POST: %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	data := bytes.Repeat([]byte("twd"), 1000)
	if r := do("PATCH", loc, data[:1000], "Content-Range", "0-999"); r.StatusCode != 202 || r.Header.Get("Range") != "0-999" {
		t.Fatalf("PATCH 1: %d %s", r.StatusCode, r.Header.Get("Range"))
	}
	if r := do("PATCH", loc, data[1000:2000], "Content-Range", "5-1004"); r.StatusCode != 416 {
		t.Fatalf("out-of-order chunk: %d", r.StatusCode)
	}
	if r := do("PATCH", loc, data[1000:2000], "Content-Range", "1000-1999"); r.StatusCode != 202 {
		t.Fatalf("PATCH 2: %d", r.StatusCode)
	}
	// The session belongs to tna/x.
	other := strings.Replace(loc, "/tna/x/", "/tna/y/", 1)
	if r := do("PUT", other+"?digest="+oci.Digest(data), data[2000:]); r.StatusCode != 404 {
		t.Fatalf("finish another repo's upload: %d", r.StatusCode)
	}
	if r := do("PUT", loc+"?digest="+oci.Digest([]byte("wrong")), data[2000:]); r.StatusCode != 400 {
		t.Fatalf("wrong digest: %d", r.StatusCode)
	}
	if r := do("HEAD", "/v2/tna/x/blobs/"+oci.Digest(data), nil); r.StatusCode != 404 {
		t.Fatalf("a refused upload got linked: %d", r.StatusCode)
	}
	// Monolithic POST.
	if r := do("POST", "/v2/tna/x/blobs/uploads/?digest="+oci.Digest(data), data); r.StatusCode != 201 {
		t.Fatalf("monolithic: %d", r.StatusCode)
	}
	if r := do("HEAD", "/v2/tna/x/blobs/"+oci.Digest(data), nil); r.StatusCode != 200 || r.ContentLength != int64(len(data)) {
		t.Fatalf("HEAD: %d %d", r.StatusCode, r.ContentLength)
	}
	if r := do("GET", "/v2/", nil); r.StatusCode != 200 || r.Header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Fatalf("GET /v2/: %d", r.StatusCode)
	}
}

// TestPinDigestStable: a deploy pins the upstream tag's digest; retagging
// upstream afterwards changes neither the pin nor what the registry serves
// for it, and the next deploy picks up the new digest.
func TestPinDigestStable(t *testing.T) {
	ctx := context.Background()
	g, _ := testRegistry(t)
	up := &oci.MemTarget{}
	upSrv := httptest.NewServer(memRegistry(up))
	t.Cleanup(upSrv.Close)
	_, per1 := testIndex(t, up, "v1", "amd64", "arm64")
	rs := &Resolver{Reg: g, Upstream: func(string) *oci.Client { return &oci.Client{Base: upSrv.URL, HTTP: upSrv.Client()} }}
	cls := []container.Class{{Class: "Box", Image: "upstream.test/src:v1"}}
	pinned, arches, err := rs.Pin(ctx, "tna", cls, []string{"amd64", "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if len(arches) != 2 || pinned[0].PinFor("upstream.test/src:v1", "arm64").Digest != per1["arm64"] {
		t.Fatalf("pins: %+v arches %v", pinned[0].Pins, arches)
	}
	p := pinned[0].PinFor("upstream.test/src:v1", "arm64")
	if p.Repository != "tna/mirror/upstream.test/src" {
		t.Fatalf("mirror repo %q", p.Repository)
	}
	// Upstream retags v1 to new content.
	_, per2 := testIndex(t, up, "v1", "amd64", "arm64")
	if per2["arm64"] == per1["arm64"] {
		t.Fatal("test images collide")
	}
	b, _, _, err := g.Manifest(ctx, p.Repository, p.Digest)
	if err != nil || oci.Digest(b) != per1["arm64"] {
		t.Fatalf("pinned digest no longer served: %v", err)
	}
	again, _, err := rs.Pin(ctx, "tna", cls, []string{"arm64"})
	if err != nil || again[0].Pins[0].Digest != per2["arm64"] {
		t.Fatalf("redeploy pins %+v %v", again, err)
	}
	// The old pin still resolves after the redeploy (running containers
	// keep the image they started from).
	if _, _, _, err := g.Manifest(ctx, p.Repository, per1["arm64"]); err != nil {
		t.Fatal(err)
	}
}

// TestPinAliasAndArches: the arm64 alias swaps the upstream ref for the
// public rebuild on arm64 only; an arch with no image is dropped from the
// placement arches (not silently given another arch's image); a tenant's
// twd.local ref never reaches another tenant's namespace.
func TestPinAliasAndArches(t *testing.T) {
	ctx := context.Background()
	g, srv := testRegistry(t)
	adm := client(srv, "admin")
	up := &oci.MemTarget{}
	upSrv := httptest.NewServer(memRegistry(up))
	t.Cleanup(upSrv.Close)
	testIndex(t, up, "0.12.14", "amd64") // upstream is amd64-only
	rebuild := &oci.MemTarget{}
	_, rb := testIndex(t, rebuild, "x", "arm64")
	if _, err := oci.Copy(ctx, rebuild, "src", "x", adm, "public/cloudflare-sandbox", nil, "0.12.14-arm64"); err != nil {
		t.Fatal(err)
	}
	rs := &Resolver{Reg: g, Aliases: []Alias{{From: "upstream.test/cloudflare/sandbox:0.12.14", To: "twd.local/cloudflare-sandbox:0.12.14-arm64", Arch: "arm64"}},
		Upstream: func(string) *oci.Client { return &oci.Client{Base: upSrv.URL, HTTP: upSrv.Client()} }}
	// The upstream repo is "src" in the fake; map the ref accordingly.
	rs.Aliases[0].From = "upstream.test/src:0.12.14"
	cls := []container.Class{{Class: "Sandbox", Image: "upstream.test/src:0.12.14"}}
	pinned, arches, err := rs.Pin(ctx, "tna", cls, []string{"amd64", "arm64"})
	if err != nil || len(arches) != 2 {
		t.Fatalf("pin: %v %v", arches, err)
	}
	arm := pinned[0].PinFor("upstream.test/src:0.12.14", "arm64")
	if arm.Repository != "public/cloudflare-sandbox" || arm.Digest != rb["arm64"] {
		t.Fatalf("arm64 pin %+v", arm)
	}
	if amd := pinned[0].PinFor("upstream.test/src:0.12.14", "amd64"); amd.Repository != "tna/mirror/upstream.test/src" {
		t.Fatalf("amd64 pin %+v", amd)
	}
	// Without the alias, arm64 has no image: only amd64 is placeable.
	rs.Aliases = nil
	_, arches, err = rs.Pin(ctx, "tna", cls, []string{"amd64", "arm64"})
	if err != nil || len(arches) != 1 || arches[0] != "amd64" {
		t.Fatalf("arches %v %v", arches, err)
	}
	_, _, err = rs.Pin(ctx, "tna", cls, []string{"arm64"})
	if err == nil || !strings.Contains(err.Error(), "no linux/arm64") {
		t.Fatalf("arm64-only pool: %v", err)
	}
	// Tenant A pushed twd.local/tna/own:1; tenant B naming it gets
	// public/tna/own (absent), never A's image.
	if _, err := oci.Copy(ctx, rebuild, "src", "x", adm, "tna/own", nil, "1"); err != nil {
		t.Fatal(err)
	}
	own := []container.Class{{Class: "S", Image: "twd.local/tna/own:1"}}
	if p, _, err := rs.Pin(ctx, "tna", own, []string{"arm64"}); err != nil || p[0].Pins[0].Repository != "tna/own" {
		t.Fatalf("own image: %+v %v", p, err)
	}
	if _, _, err := rs.Pin(ctx, "tnb", own, []string{"arm64"}); err == nil || !strings.Contains(err.Error(), "public/tna/own") {
		t.Fatalf("tenant B pinned tenant A's image: %v", err)
	}
	// An unbuilt Dockerfile is refused at deploy.
	if _, _, err := rs.Pin(ctx, "tna", []container.Class{{Class: "S", Source: "./Dockerfile"}}, []string{"arm64"}); err == nil || !strings.Contains(err.Error(), "not built") {
		t.Fatalf("unbuilt: %v", err)
	}
}

// memRegistry serves a MemTarget read-only as a registry (an "upstream").
func memRegistry(m *oci.MemTarget) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt, ok := parse(r.URL.Path)
		if !ok || rt.kind == "base" {
			w.WriteHeader(200)
			return
		}
		switch rt.kind {
		case "manifest":
			b, mt, d, err := m.Manifest(r.Context(), rt.repo, rt.ref)
			if err != nil {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Type", mt)
			w.Header().Set("Docker-Content-Digest", d)
			w.Write(b)
		case "blob":
			rc, err := m.Blob(r.Context(), rt.repo, rt.ref, -1)
			if err != nil {
				w.WriteHeader(404)
				return
			}
			io.Copy(w, rc)
		default:
			w.WriteHeader(405)
		}
	})
}

// TestCraneConformance pushes and pulls with crane (go-containerregistry),
// an independent client, when it is on PATH (or $TWD_CRANE).
func TestCraneConformance(t *testing.T) {
	crane := os.Getenv("TWD_CRANE")
	if crane == "" {
		crane, _ = exec.LookPath("crane")
	}
	if crane == "" {
		t.Skip("crane not found (go install github.com/google/go-containerregistry/cmd/crane@v0.20.6, or set TWD_CRANE)")
	}
	g := &Registry{Dir: t.TempDir(), Meta: &MemMeta{}, Authorize: func(r *http.Request, ns string, push bool) error {
		if ns != "" && ns != "tna" {
			return &Denied{"tenant tna only"}
		}
		return nil
	}}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	dir := t.TempDir()
	run := func(args ...string) (string, error) {
		cmd := exec.Command(crane, append(args, "--insecure")...)
		cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+dir)
		b, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(b)), err
	}
	// crane append builds an image from a tarball layer and pushes it
	// (POST/PATCH/PUT uploads, PUT manifest by tag).
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	tw.WriteHeader(&tar.Header{Name: "etc/twd", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi\n"))
	tw.Close()
	lp := filepath.Join(dir, "layer.tar")
	os.WriteFile(lp, layer.Bytes(), 0o644)
	ref := host + "/tna/app:v1"
	if out, err := run("append", "-f", lp, "-t", ref, "--platform", "linux/arm64"); err != nil {
		t.Fatalf("crane append: %v\n%s", err, out)
	}
	dg, err := run("digest", ref)
	if err != nil || !oci.ValidDigest(dg) {
		t.Fatalf("crane digest: %v %s", err, dg)
	}
	if out, err := run("manifest", host+"/tna/app@"+dg); err != nil || !strings.Contains(out, "layers") {
		t.Fatalf("crane manifest: %v %s", err, out)
	}
	if out, err := run("export", host+"/tna/app@"+dg, filepath.Join(dir, "fs.tar")); err != nil {
		t.Fatalf("crane export: %v %s", err, out)
	}
	if out, err := run("ls", host+"/tna/app"); err != nil || out != "v1" {
		t.Fatalf("crane ls: %v %q", err, out)
	}
	// crane copy within the namespace (cross-repo mount path) and a retag.
	if out, err := run("copy", ref, host+"/tna/other:v2"); err != nil {
		t.Fatalf("crane copy: %v %s", err, out)
	}
	if out, err := run("digest", host+"/tna/other:v2"); err != nil || out != dg {
		t.Fatalf("copied digest %q want %q (%v)", out, dg, err)
	}
	// The authorizer is enforced for crane too.
	if out, err := run("copy", ref, host+"/tnb/stolen:v1"); err == nil {
		t.Fatalf("push into another namespace allowed: %s", out)
	} else if !strings.Contains(out, "403") {
		t.Fatalf("denied push: %s", out)
	}
}
