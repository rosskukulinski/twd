package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func wantLimit(t *testing.T, err error, what string) {
	t.Helper()
	var le *LimitError
	if !errors.Is(err, ErrLimit) || !errors.As(err, &le) || le.What != what {
		t.Fatalf("want a %q LimitError, got %v", what, err)
	}
}

// bombLayer is a gzip layer of a few KiB whose tar holds one file of n
// zero bytes.
func bombLayer(t *testing.T, n int64) []byte {
	t.Helper()
	var b bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "bomb", Typeflag: tar.TypeReg, Mode: 0o644, Size: n})
	zero := make([]byte, 1<<20)
	for left := n; left > 0; left -= int64(len(zero)) {
		tw.Write(zero[:min(left, int64(len(zero)))])
	}
	tw.Close()
	zw.Close()
	return b.Bytes()
}

// countingLayer opens a gzip layer and records how many decompressed bytes
// were read from it.
func countingLayer(gz []byte, read *int64) Layer {
	return func() (io.ReadCloser, error) {
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			return nil, err
		}
		return readCloser{&tally{r: zr, n: read}, zr.Close}, nil
	}
}

type tally struct {
	r io.Reader
	n *int64
}

func (t *tally) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	*t.n += int64(n)
	return n, err
}

// A decompression bomb stops at the byte cap while it is read: nothing is
// written, and far less than the bomb is ever decompressed.
func TestFlattenCapsUncompressedBytes(t *testing.T) {
	bomb := bombLayer(t, 64<<20) // 64 MiB of zeros
	t.Logf("bomb layer: %d bytes compressed, 64 MiB uncompressed", len(bomb))
	var read int64
	var out bytes.Buffer
	err := FlattenLimited([]Layer{countingLayer(bomb, &read)}, &out, Limits{MaxBytes: 1 << 20})
	wantLimit(t, err, "uncompressed bytes")
	if out.Len() != 0 {
		t.Fatalf("%d bytes written for an image over the cap", out.Len())
	}
	if read > 2<<20 {
		t.Fatalf("decompressed %d bytes before stopping at a 1 MiB cap", read)
	}
	// The cap is on all layers together: two halves over the cap fail too.
	half := mem(layerTar(t, ent{name: "a", body: strings.Repeat("x", 600<<10)}))
	half2 := mem(layerTar(t, ent{name: "b", body: strings.Repeat("y", 600<<10)}))
	wantLimit(t, FlattenLimited([]Layer{half, half2}, io.Discard, Limits{MaxBytes: 1 << 20}), "uncompressed bytes")
	if err := FlattenLimited([]Layer{half}, io.Discard, Limits{MaxBytes: 1 << 20}); err != nil {
		t.Fatalf("an image under the cap: %v", err)
	}
}

// An entry that says it is huge (a sparse file, say) fails on its header
// size before any of it is written.
func TestFlattenCapsEntrySizes(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "huge", Typeflag: tar.TypeReg, Size: 8 << 30})
	// Truncated: the archive stops after the header (the size alone trips).
	layer := b.Bytes()
	var out bytes.Buffer
	wantLimit(t, FlattenLimited([]Layer{mem(layer)}, &out, Limits{MaxBytes: 1 << 30}), "uncompressed bytes")
	if out.Len() != 0 {
		t.Fatalf("%d bytes written", out.Len())
	}
}

func TestFlattenCapsLayers(t *testing.T) {
	l := mem(layerTar(t, ent{name: "f", body: "x"}))
	var ls []Layer
	for i := 0; i < 129; i++ {
		ls = append(ls, l)
	}
	wantLimit(t, Flatten(ls, io.Discard), "layers") // the default: 128
	if err := Flatten(ls[:128], io.Discard); err != nil {
		t.Fatalf("128 layers: %v", err)
	}
	wantLimit(t, FlattenLimited(ls[:3], io.Discard, Limits{MaxLayers: 2}), "layers")
}

func TestFlattenCapsEntries(t *testing.T) {
	var es []ent
	for i := 0; i < 50; i++ {
		es = append(es, ent{name: "f" + strings.Repeat("x", i)})
	}
	l := mem(layerTar(t, es...))
	wantLimit(t, FlattenLimited([]Layer{l}, io.Discard, Limits{MaxEntries: 49}), "entries")
	// Across layers.
	wantLimit(t, FlattenLimited([]Layer{l, l}, io.Discard, Limits{MaxEntries: 99}), "entries")
	if err := FlattenLimited([]Layer{l, l}, io.Discard, Limits{MaxEntries: 100}); err != nil {
		t.Fatal(err)
	}
}

func TestFlattenCapsPaths(t *testing.T) {
	deep := strings.Repeat("d/", 300) + "f"
	wantLimit(t, Flatten([]Layer{mem(layerTar(t, ent{name: deep}))}, io.Discard), "path depth")
	long := strings.Repeat("n", 5000)
	wantLimit(t, Flatten([]Layer{mem(layerTar(t, ent{name: long}))}, io.Discard), "path length")
	if err := Flatten([]Layer{mem(layerTar(t, ent{name: strings.Repeat("d/", 100) + "f"}))}, io.Discard); err != nil {
		t.Fatalf("depth 101: %v", err)
	}
}

// The image tarball itself is extracted within the limits too.
func TestUntarCaps(t *testing.T) {
	files := map[string][]byte{"a": bytes.Repeat([]byte("a"), 3000), "b": []byte("b"), "c": []byte("c")}
	p := filepath.Join(t.TempDir(), "img.tar")
	os.WriteFile(p, tarFiles(t, files), 0o600)
	wantLimit(t, untar(p, t.TempDir(), Limits{MaxBytes: 2048}), "uncompressed bytes")
	wantLimit(t, untar(p, t.TempDir(), Limits{MaxEntries: 2}), "entries")
	if err := untar(p, t.TempDir(), Limits{}); err != nil {
		t.Fatal(err)
	}
}

// Store.Get on an image over a limit fails with the typed error and leaves
// nothing behind: no cache entry, no temp files.
func TestStoreConvertOverLimitCleansUp(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	src := t.TempDir()
	cfg := cfgJSON("arm64")
	files := map[string][]byte{"config.json": cfg, "layer.tar.gz": bombLayer(t, 16<<20)}
	m, _ := json.Marshal([]map[string]any{{"Config": "config.json", "Layers": []string{"layer.tar.gz"}}})
	files["manifest.json"] = m
	os.WriteFile(filepath.Join(src, "bomb.tar"), tarFiles(t, files), 0o600)
	os.WriteFile(filepath.Join(src, "images.json"), []byte(`{"twd.local/bomb:1":"bomb.tar"}`), 0o600)
	cache := filepath.Join(t.TempDir(), "cache")
	st := &Store{Dir: cache, Arch: "arm64", Mkfs: "/bin/true", Sources: []Source{DirSource{Dir: src}}, Limits: Limits{MaxBytes: 4 << 20}}
	_, err := st.Get(t.Context(), "t1", "twd.local/bomb:1")
	wantLimit(t, err, "uncompressed bytes")
	left, _ := filepath.Glob(filepath.Join(tmp, "*"))
	entries, _ := os.ReadDir(filepath.Join(cache, "t1"))
	if len(left) != 0 || len(entries) != 0 {
		t.Fatalf("left behind: tmp %v, cache %v", left, entries)
	}
}

// Bytes that no entry size accounts for (here: big PAX headers on empty
// files) still count: the decompressed stream itself is capped as read.
func TestFlattenCapsStreamBytes(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for i := 0; i < 10; i++ {
		tw.WriteHeader(&tar.Header{Name: "f" + strings.Repeat("x", i), Typeflag: tar.TypeReg, Format: tar.FormatPAX,
			PAXRecords: map[string]string{"twd.pad": strings.Repeat("p", 900<<10)}})
	}
	tw.Close()
	if b.Len() < 8<<20 {
		t.Fatalf("layer only %d bytes", b.Len())
	}
	wantLimit(t, FlattenLimited([]Layer{mem(b.Bytes())}, io.Discard, Limits{MaxBytes: 4 << 20}), "uncompressed bytes")
	if err := FlattenLimited([]Layer{mem(b.Bytes())}, io.Discard, Limits{MaxBytes: 16 << 20}); err != nil {
		t.Fatal(err)
	}
}
