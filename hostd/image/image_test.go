package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

type ent struct {
	name, body, link string
	typ              byte
	mode             int64
	uid              int
	xattr            map[string]string
}

func layerTar(t *testing.T, es ...ent) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Uid: e.uid, Linkname: e.link}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		for k, v := range e.xattr {
			if h.PAXRecords == nil {
				h.PAXRecords = map[string]string{}
			}
			h.PAXRecords["SCHILY.xattr."+k] = v
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	return b.Bytes()
}

func dir(n string) ent { return ent{name: n, typ: tar.TypeDir, mode: 0o755} }

func mem(b []byte) Layer {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}

type got struct {
	typ        byte
	body, link string
	uid        int
	mode       int64
	pax        map[string]string
}

func flat(t *testing.T, layers ...[]byte) (map[string]got, []string) {
	t.Helper()
	var ls []Layer
	for _, l := range layers {
		ls = append(ls, mem(l))
	}
	var out bytes.Buffer
	if err := Flatten(ls, &out); err != nil {
		t.Fatal(err)
	}
	m := map[string]got{}
	var order []string
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		n := strings.TrimSuffix(h.Name, "/")
		if _, dup := m[n]; dup {
			t.Fatalf("%s emitted twice", n)
		}
		m[n] = got{h.Typeflag, string(b), h.Linkname, h.Uid, h.Mode, h.PAXRecords}
		order = append(order, n)
	}
	return m, order
}

func paths(m map[string]got) []string {
	var p []string
	for k := range m {
		p = append(p, k)
	}
	sort.Strings(p)
	return p
}

// The filesystem a container sees must be exactly the OCI layer semantics:
// deleted files stay deleted (a secret removed in a later layer must not
// reappear), opaque dirs hide lower content, replacements win.
func TestFlattenWhiteouts(t *testing.T) {
	l1 := layerTar(t,
		dir("etc"), ent{name: "etc/passwd", body: "root:x:0:0"},
		dir("app"), ent{name: "app/secret.key", body: "SECRET"}, ent{name: "app/keep", body: "k1"},
		dir("cache"), dir("cache/sub"), ent{name: "cache/sub/a", body: "a"}, ent{name: "cache/b", body: "b"},
		dir("data"), ent{name: "data/x", body: "x"},
		ent{name: "f2d", body: "file"},
		dir("bin"), ent{name: "bin/sh", body: "sh", mode: 0o755, uid: 0}, ent{name: "bin/ping", body: "ping", xattr: map[string]string{"security.capability": "\x01\x02"}},
	)
	l2 := layerTar(t,
		ent{name: "app/.wh.secret.key"},               // delete a file
		ent{name: ".wh.data"},                         // delete a dir and its subtree
		dir("cache"), ent{name: "cache/.wh..wh..opq"}, // opaque: lower content hidden
		ent{name: "cache/new", body: "n"},
		ent{name: "etc/passwd", body: "root:x:0:0\nu:x:1000:1000", uid: 0}, // override
		dir("f2d"), ent{name: "f2d/inner", body: "i"}, // a file replaced by a dir
		ent{name: "./app/keep", body: "k2"}, // "./" prefix
	)
	l3 := layerTar(t,
		ent{name: "data", body: "now a file"}, // re-created after the whiteout
		ent{name: "cache", body: "dir replaced by file"},
	)
	m, _ := flat(t, l1, l2, l3)
	want := []string{"app", "app/keep", "bin", "bin/ping", "bin/sh", "cache", "data", "etc", "etc/passwd", "f2d", "f2d/inner"}
	if g := paths(m); strings.Join(g, ",") != strings.Join(want, ",") {
		t.Fatalf("paths:\n got %v\nwant %v", g, want)
	}
	if _, ok := m["app/secret.key"]; ok {
		t.Fatal("a whited-out file survived")
	}
	for p, e := range m {
		if strings.Contains(p, ".wh.") {
			t.Fatalf("whiteout marker %s in the output", p)
		}
		if e.body == "SECRET" {
			t.Fatal("whited-out content survived")
		}
	}
	if m["etc/passwd"].body != "root:x:0:0\nu:x:1000:1000" || m["app/keep"].body != "k2" {
		t.Fatal("upper layer did not win", m["etc/passwd"], m["app/keep"])
	}
	if m["cache"].typ != tar.TypeReg || m["cache"].body != "dir replaced by file" {
		t.Fatal("cache should be a file now", m["cache"])
	}
	if m["data"].body != "now a file" || m["f2d"].typ != tar.TypeDir {
		t.Fatal("recreate/replace failed", m["data"], m["f2d"])
	}
	if m["bin/sh"].mode != 0o755 || m["bin/ping"].pax["SCHILY.xattr.security.capability"] != "\x01\x02" {
		t.Fatal("modes or xattrs lost", m["bin/sh"], m["bin/ping"].pax)
	}
}

func TestFlattenOpaqueKeepsSameLayer(t *testing.T) {
	l1 := layerTar(t, dir("d"), ent{name: "d/old", body: "o"})
	l2 := layerTar(t, dir("d"), ent{name: "d/new1", body: "1"}, ent{name: "d/.wh..wh..opq"}, ent{name: "d/new2", body: "2"})
	m, _ := flat(t, l1, l2)
	if strings.Join(paths(m), ",") != "d,d/new1,d/new2" {
		t.Fatal(paths(m))
	}
}

func TestFlattenHardLinks(t *testing.T) {
	l1 := layerTar(t, dir("usr"), ent{name: "usr/a", body: "AAA"}, ent{name: "usr/b", typ: tar.TypeLink, link: "usr/a"},
		ent{name: "usr/c", typ: tar.TypeLink, link: "./usr/b"})
	// Kept target: the links stay links, and follow it in the stream.
	m, order := flat(t, l1)
	if m["usr/b"].typ != tar.TypeLink || m["usr/b"].link != "usr/a" || m["usr/c"].link != "usr/a" {
		t.Fatal("links", m["usr/b"], m["usr/c"])
	}
	idx := map[string]int{}
	for i, p := range order {
		idx[p] = i
	}
	if idx["usr/b"] < idx["usr/a"] {
		t.Fatal("link before its target")
	}
	// The target is deleted (or replaced) above: the link keeps the
	// content it had when it was made.
	l2 := layerTar(t, ent{name: "usr/.wh.a"})
	m, _ = flat(t, l1, l2)
	if _, ok := m["usr/a"]; ok {
		t.Fatal("usr/a survived its whiteout")
	}
	if m["usr/b"].typ != tar.TypeReg || m["usr/b"].body != "AAA" || m["usr/c"].body != "AAA" {
		t.Fatal("orphaned links not materialised", m["usr/b"], m["usr/c"])
	}
	l3 := layerTar(t, ent{name: "usr/a", body: "NEW"})
	m, _ = flat(t, l1, l3)
	if m["usr/a"].body != "NEW" || m["usr/b"].body != "AAA" || m["usr/b"].typ != tar.TypeReg {
		t.Fatal("replaced target", m["usr/a"], m["usr/b"])
	}
}

func TestFlattenRefusesEscapes(t *testing.T) {
	for _, l := range [][]byte{
		layerTar(t, ent{name: "../../etc/shadow", body: "x"}),
		layerTar(t, ent{name: "a", body: "x"}, ent{name: "b", typ: tar.TypeLink, link: "../../etc/shadow"}),
	} {
		// "../../etc/shadow" cleans to "etc/shadow" under a rooted Clean, so
		// it must land inside the image, never outside it.
		var out bytes.Buffer
		err := Flatten([]Layer{mem(l)}, &out)
		if err != nil {
			continue
		}
		tr := tar.NewReader(&out)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			if strings.HasPrefix(h.Name, "/") || strings.Contains(h.Name, "..") || strings.Contains(h.Linkname, "..") {
				t.Fatalf("escaping entry %q -> %q", h.Name, h.Linkname)
			}
		}
	}
}

// ---- archives ----

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func tarFiles(t *testing.T, files map[string][]byte) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[n]))})
		tw.Write(files[n])
	}
	tw.Close()
	return b.Bytes()
}

func gz(b []byte) []byte {
	var o bytes.Buffer
	w := gzip.NewWriter(&o)
	w.Write(b)
	w.Close()
	return o.Bytes()
}

func cfgJSON(arch string) []byte {
	b, _ := json.Marshal(map[string]any{"architecture": arch, "os": "linux", "config": map[string]any{
		"Entrypoint": []string{"/container-server/sandbox"}, "Env": []string{"PATH=/usr/bin:/bin", "A=1"}, "WorkingDir": "/container-server", "User": "1000:1000"}})
	return b
}

// An OCI layout with an index listing amd64, arm64 and an attestation:
// the host's arch is picked; gzip layers decompress.
func TestOpenArchiveOCIIndex(t *testing.T) {
	files := map[string][]byte{}
	put := func(b []byte) string {
		d := sha(b)
		files["blobs/sha256/"+d] = b
		return "sha256:" + d
	}
	mk := func(arch string) string {
		cfg := put(cfgJSON(arch))
		l := put(gz(layerTar(t, ent{name: "hello-" + arch, body: arch})))
		m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "config": map[string]any{"digest": cfg}, "layers": []any{map[string]any{"digest": l}}})
		return put(m)
	}
	amd, arm := mk("amd64"), mk("arm64")
	att := put([]byte(`{"schemaVersion":2,"config":{"digest":"sha256:00"},"layers":[]}`))
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []any{
		map[string]any{"digest": att, "platform": map[string]any{"architecture": "unknown", "os": "unknown"}},
		map[string]any{"digest": amd, "platform": map[string]any{"architecture": "amd64", "os": "linux"}},
		map[string]any{"digest": arm, "platform": map[string]any{"architecture": "arm64", "os": "linux"}},
	}})
	files["index.json"] = idx
	p := filepath.Join(t.TempDir(), "img.tar")
	os.WriteFile(p, tarFiles(t, files), 0o600)
	for _, arch := range []string{"amd64", "arm64"} {
		a, err := openArchive(p, Limits{}, arch)
		if err != nil {
			t.Fatal(err)
		}
		var c Config
		json.Unmarshal(a.config, &c)
		if c.Architecture != arch || len(a.layers) != 1 || a.digest != "sha256:"+sha(cfgJSON(arch)) {
			t.Fatalf("%s: %+v %v %s", arch, c, a.layers, a.digest)
		}
		m, _ := flatLayers(t, a.layers)
		if _, ok := m["hello-"+arch]; !ok {
			t.Fatal("wrong layer for", arch, m)
		}
		a.close()
	}
	if _, err := openArchive(p, Limits{}, "riscv64"); err == nil {
		t.Fatal("riscv64 should be refused")
	}
}

func flatLayers(t *testing.T, ps []string) (map[string]got, []string) {
	var bs [][]byte
	for _, p := range ps {
		rc, err := openLayer(p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		bs = append(bs, b)
	}
	return flat(t, bs...)
}

func TestOpenArchiveDockerSave(t *testing.T) {
	files := map[string][]byte{
		"cfg.json":     cfgJSON("arm64"),
		"l1.tar":       layerTar(t, ent{name: "a", body: "1"}),
		"l2/layer":     layerTar(t, ent{name: ".wh.a"}, ent{name: "b", body: "2"}),
		"evil.json":    []byte("x"),
		"repositories": []byte("{}"),
	}
	m, _ := json.Marshal([]any{map[string]any{"Config": "cfg.json", "Layers": []string{"l1.tar", "l2/layer"}}})
	files["manifest.json"] = m
	p := filepath.Join(t.TempDir(), "img.tar")
	os.WriteFile(p, tarFiles(t, files), 0o600)
	a, err := openArchive(p, Limits{}, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	fm, _ := flatLayers(t, a.layers)
	if strings.Join(paths(fm), ",") != "b" {
		t.Fatal(paths(fm))
	}
	// A manifest naming a blob outside the archive is refused.
	m, _ = json.Marshal([]any{map[string]any{"Config": "../../etc/passwd", "Layers": []string{}}})
	files["manifest.json"] = m
	os.WriteFile(p, tarFiles(t, files), 0o600)
	if _, err := openArchive(p, Limits{}, "arm64"); err == nil {
		t.Fatal("blob outside the archive accepted")
	}
}

func TestNormalizeAndAlias(t *testing.T) {
	for in, want := range map[string]string{
		"debian:trixie":                              "docker.io/library/debian:trixie",
		"cloudflare/sandbox:0.12.14":                 "docker.io/cloudflare/sandbox:0.12.14",
		"docker.io/cloudflare/sandbox:0.12.14":       "docker.io/cloudflare/sandbox:0.12.14",
		"ubuntu":                                     "docker.io/library/ubuntu:latest",
		"localhost:5000/x/y:1":                       "localhost:5000/x/y:1",
		"twd.local/cloudflare-sandbox:0.12.14-arm64": "twd.local/cloudflare-sandbox:0.12.14-arm64",
	} {
		if g := NormalizeRef(in); g != want {
			t.Errorf("NormalizeRef(%q) = %q, want %q", in, g, want)
		}
	}
	s := &Store{Aliases: DefaultAliases, Arch: "arm64"}
	if g := s.Resolve("cloudflare/sandbox:0.12.14"); g != "twd.local/cloudflare-sandbox:0.12.14-arm64" {
		t.Fatal(g)
	}
	s.Arch = "amd64"
	if g := s.Resolve("cloudflare/sandbox:0.12.14"); g != "cloudflare/sandbox:0.12.14" {
		t.Fatal("amd64 hosts run the stock image:", g)
	}
}

// The cache is per tenant unless the image is marked public: tenant B's
// lookup of an image only A converted is a fresh conversion in B's scope,
// never a hit on A's file.
func TestStoreScopesAndArch(t *testing.T) {
	mk, err := exec.LookPath("mkfs.erofs")
	if err != nil {
		mk = fakeMkfs(t)
	}
	src := t.TempDir()
	cfg := cfgJSON(runtime.GOARCH)
	files := map[string][]byte{"c.json": cfg, "l.tar": layerTar(t, ent{name: "x", body: "y"})}
	m, _ := json.Marshal([]any{map[string]any{"Config": "c.json", "Layers": []string{"l.tar"}}})
	files["manifest.json"] = m
	os.WriteFile(filepath.Join(src, "img.tar"), tarFiles(t, files), 0o600)
	cfgA := cfgJSON("s390x")
	filesA := map[string][]byte{"c.json": cfgA, "l.tar": layerTar(t), "manifest.json": m}
	os.WriteFile(filepath.Join(src, "other.tar"), tarFiles(t, filesA), 0o600)
	os.WriteFile(filepath.Join(src, "images.json"), []byte(`{"docker.io/library/debian:trixie":"img.tar","docker.io/library/base:1":"img.tar","docker.io/x/other:1":"other.tar"}`), 0o600)
	s := &Store{Dir: t.TempDir(), Mkfs: mk, Sources: []Source{DirSource{src}}, Public: map[string]bool{"docker.io/library/base:1": true}}
	ctx := context.Background()
	a, err := s.Get(ctx, "tenantA", "debian:trixie")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Get(ctx, "tenantB", "debian:trixie")
	if err != nil {
		t.Fatal(err)
	}
	if a.Path == b.Path || a.Scope != "tenantA" || b.Scope != "tenantB" || b.ConvertMS == 0 && !strings.Contains(b.Path, "tenantB") {
		t.Fatalf("tenants share a cache entry: %s %s", a.Path, b.Path)
	}
	if a.Digest != b.Digest || a.Config.Config.WorkingDir != "/container-server" || a.Config.Config.User != "1000:1000" {
		t.Fatalf("%+v", a)
	}
	again, err := s.Get(ctx, "tenantA", "debian:trixie")
	if err != nil || again.ConvertMS != 0 || again.Path != a.Path {
		t.Fatal("expected a cache hit", again, err)
	}
	p1, _ := s.Get(ctx, "tenantA", "base:1")
	p2, _ := s.Get(ctx, "tenantB", "base:1")
	if p1.Scope != PublicScope || p1.Path != p2.Path {
		t.Fatal("public base not shared", p1, p2)
	}
	if fi, _ := os.Stat(a.Path); fi.Mode().Perm() != 0o644 {
		t.Fatal("erofs mode", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Dir(a.Path)); fi.Mode().Perm() != 0o700 {
		t.Fatal("scope dir mode", fi.Mode())
	}
	if _, err := s.Get(ctx, "tenantA", "x/other:1"); err == nil || !strings.Contains(err.Error(), "arch") {
		t.Fatal("wrong-arch image accepted:", err)
	}
	if _, err := s.Get(ctx, "tenantA", "nope:1"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "../x", "debian:trixie"); err == nil {
		t.Fatal("bad scope accepted")
	}
}

// fakeMkfs stands in for mkfs.erofs where erofs-utils isn't installed
// (macOS): it copies the merged tar, which is what the tests inspect.
func fakeMkfs(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "mkfs.erofs")
	os.WriteFile(p, []byte("#!/bin/sh\nfor a; do out=$src; src=$a; done\ncp \"$src\" \"$out\"\n"), 0o755)
	return p
}
