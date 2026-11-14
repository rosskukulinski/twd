package image

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Emulated arches (amd64 under Rosetta on a Mac, ADR-032 D7): the store
// accepts an amd64-only image on an arm64 host only when the backend
// emulates amd64, and an index with both variants still resolves to the
// native one.
func TestStoreEmulatedArch(t *testing.T) {
	mk, err := exec.LookPath("mkfs.erofs")
	if err != nil {
		mk = fakeMkfs(t)
	}
	src := t.TempDir()
	m, _ := json.Marshal([]any{map[string]any{"Config": "c.json", "Layers": []string{"l.tar"}}})
	os.WriteFile(filepath.Join(src, "amd.tar"), tarFiles(t, map[string][]byte{"c.json": cfgJSON("amd64"), "l.tar": layerTar(t), "manifest.json": m}), 0o600)
	os.WriteFile(filepath.Join(src, "images.json"), []byte(`{"docker.io/cloudflare/sandbox:0.12.14":"amd.tar"}`), 0o600)
	ctx := context.Background()
	native := &Store{Dir: t.TempDir(), Mkfs: mk, Arch: "arm64", Sources: []Source{DirSource{src}}}
	if _, err := native.Get(ctx, "t", "cloudflare/sandbox:0.12.14"); err == nil || !strings.Contains(err.Error(), "arch") {
		t.Fatal("amd64 image accepted on arm64 without emulation:", err)
	}
	rosetta := &Store{Dir: t.TempDir(), Mkfs: mk, Arch: "arm64", Emulated: []string{"amd64"}, Sources: []Source{DirSource{src}}}
	img, err := rosetta.Get(ctx, "t", "cloudflare/sandbox:0.12.14")
	if err != nil || img.Config.Architecture != "amd64" || img.Ref != "cloudflare/sandbox:0.12.14" {
		t.Fatalf("stock amd64 image under emulation: %+v %v", img, err)
	}
	if !rosetta.Runs("amd64") || !rosetta.Runs("arm64") || rosetta.Runs("s390x") || native.Runs("amd64") {
		t.Fatal("Runs")
	}

	// An index with both: native first, whatever the order in the index.
	files := map[string][]byte{}
	put := func(b []byte) string {
		d := sha(b)
		files["blobs/sha256/"+d] = b
		return "sha256:" + d
	}
	mkm := func(arch string) string {
		mm, _ := json.Marshal(map[string]any{"schemaVersion": 2, "config": map[string]any{"digest": put(cfgJSON(arch))}, "layers": []any{}})
		return put(mm)
	}
	amd, arm := mkm("amd64"), mkm("arm64")
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []any{
		map[string]any{"digest": amd, "platform": map[string]any{"architecture": "amd64", "os": "linux"}},
		map[string]any{"digest": arm, "platform": map[string]any{"architecture": "arm64", "os": "linux"}},
	}})
	files["index.json"] = idx
	p := filepath.Join(t.TempDir(), "both.tar")
	os.WriteFile(p, tarFiles(t, files), 0o600)
	a, err := openArchive(p, Limits{}, "arm64", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if a.digest != "sha256:"+sha(cfgJSON("arm64")) {
		t.Fatal("index resolved to the emulated variant though the native one exists")
	}
	if _, err := openArchive(p, Limits{}, "riscv64"); err == nil {
		t.Fatal("riscv64 accepted")
	}
}
