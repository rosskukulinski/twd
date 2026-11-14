package sandboxtest

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"

	"github.com/rosskukulinski/twd/hostd/image"
)

// ImageStore returns an image.Store over dir holding one tiny docker-save
// image per ref (config: entrypoint, env, workdir), converted by a stand-in
// for mkfs.erofs that copies the merged tar (the fake backend never mounts
// it).
func ImageStore(dir string, entrypoint []string, refs ...string) (*image.Store, error) {
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		return nil, err
	}
	cfg, _ := json.Marshal(map[string]any{"architecture": runtime.GOARCH, "os": "linux",
		"config": map[string]any{"Entrypoint": entrypoint, "Env": []string{"PATH=/usr/bin:/bin", "IMAGE_ENV=1"}, "WorkingDir": "/"}})
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	tw.WriteHeader(&tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "etc/hostname", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
	tw.Write([]byte("box\n"))
	tw.Close()
	man, _ := json.Marshal([]any{map[string]any{"Config": "c.json", "Layers": []string{"l.tar"}}})
	var img bytes.Buffer
	ow := tar.NewWriter(&img)
	for _, f := range []struct {
		n string
		b []byte
	}{{"manifest.json", man}, {"c.json", cfg}, {"l.tar", layer.Bytes()}} {
		ow.WriteHeader(&tar.Header{Name: f.n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(f.b))})
		ow.Write(f.b)
	}
	ow.Close()
	if err := os.WriteFile(filepath.Join(src, "img.tar"), img.Bytes(), 0o600); err != nil {
		return nil, err
	}
	idx := map[string]string{}
	for _, r := range refs {
		idx[r] = "img.tar"
	}
	ib, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(src, "images.json"), ib, 0o600); err != nil {
		return nil, err
	}
	mk := filepath.Join(dir, "mkfs.erofs")
	if err := os.WriteFile(mk, []byte("#!/bin/sh\nfor a; do out=$src; src=$a; done\ncp \"$src\" \"$out\"\n"), 0o755); err != nil {
		return nil, err
	}
	return &image.Store{Dir: filepath.Join(dir, "cache"), Mkfs: mk, Sources: []image.Source{image.DirSource{Dir: src}}}, nil
}
