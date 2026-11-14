package image

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Config is the part of an OCI image config twd uses (image-spec config.md).
type Config struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Config       struct {
		Entrypoint   []string            `json:"Entrypoint"`
		Cmd          []string            `json:"Cmd"`
		Env          []string            `json:"Env"`
		WorkingDir   string              `json:"WorkingDir"`
		User         string              `json:"User"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
		StopSignal   string              `json:"StopSignal,omitempty"`
		Labels       map[string]string   `json:"Labels,omitempty"`
	} `json:"config"`
	RootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// archive is an image tarball extracted to a temp dir.
type archive struct {
	dir    string
	config []byte
	digest string // sha256:<hex> of the config blob (the image ID)
	layers []string
}

func (a *archive) close() { os.RemoveAll(a.dir) }

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

// openArchive reads a `docker save` tarball or an OCI image layout (as a
// tar, or a directory) holding one image for arch.
func openArchive(src, arch string) (*archive, error) {
	st, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	a := &archive{}
	if st.IsDir() {
		a.dir = src
	} else {
		d, err := os.MkdirTemp("", "twd-image-")
		if err != nil {
			return nil, err
		}
		a.dir = d
		if err := untar(src, d); err != nil {
			a.close()
			return nil, err
		}
	}
	fail := func(err error) (*archive, error) {
		if !st.IsDir() {
			a.close()
		}
		return nil, err
	}
	var cfgPath string
	if b, err := os.ReadFile(filepath.Join(a.dir, "index.json")); err == nil {
		m, err := a.ociManifest(b, arch, 0)
		if err != nil {
			return fail(err)
		}
		cfgPath = a.blob(m.Config.Digest)
		for _, l := range m.Layers {
			a.layers = append(a.layers, a.blob(l.Digest))
		}
	} else if b, err := os.ReadFile(filepath.Join(a.dir, "manifest.json")); err == nil {
		var dm []struct {
			Config string   `json:"Config"`
			Layers []string `json:"Layers"`
		}
		if err := json.Unmarshal(b, &dm); err != nil || len(dm) == 0 {
			return fail(fmt.Errorf("manifest.json: %v", err))
		}
		cfgPath = filepath.Join(a.dir, filepath.FromSlash(dm[0].Config))
		for _, l := range dm[0].Layers {
			a.layers = append(a.layers, filepath.Join(a.dir, filepath.FromSlash(l)))
		}
	} else {
		return fail(errors.New("not a docker-save tarball or OCI image layout (no index.json or manifest.json)"))
	}
	for _, p := range append([]string{cfgPath}, a.layers...) {
		if !strings.HasPrefix(filepath.Clean(p), filepath.Clean(a.dir)+string(filepath.Separator)) {
			return fail(fmt.Errorf("image blob %q outside the archive", p))
		}
	}
	a.config, err = os.ReadFile(cfgPath)
	if err != nil {
		return fail(err)
	}
	sum := sha256.Sum256(a.config)
	a.digest = "sha256:" + hex.EncodeToString(sum[:])
	return a, nil
}

func (a *archive) blob(digest string) string {
	alg, hx, _ := strings.Cut(digest, ":")
	return filepath.Join(a.dir, "blobs", alg, hx)
}

type ociManifestDoc struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
	Config    descriptor   `json:"config"`
	Layers    []descriptor `json:"layers"`
}

// ociManifest resolves an index (or nested index) to the image manifest
// for linux/arch.
func (a *archive) ociManifest(b []byte, arch string, depth int) (*ociManifestDoc, error) {
	if depth > 4 {
		return nil, errors.New("OCI index nesting too deep")
	}
	var doc ociManifestDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if len(doc.Manifests) == 0 {
		if doc.Config.Digest == "" {
			return nil, errors.New("OCI manifest without a config")
		}
		return &doc, nil
	}
	var pick *descriptor
	for i := range doc.Manifests {
		d := &doc.Manifests[i]
		if d.Platform != nil {
			if d.Platform.OS == "linux" && d.Platform.Architecture == arch {
				pick = d
				break
			}
			continue // e.g. attestation manifests (platform unknown/unknown)
		}
		if pick == nil {
			pick = d // no platform: a single-image layout
		}
	}
	if pick == nil {
		return nil, fmt.Errorf("the image has no linux/%s variant", arch)
	}
	mb, err := os.ReadFile(a.blob(pick.Digest))
	if err != nil {
		return nil, err
	}
	return a.ociManifest(mb, arch, depth+1)
}

// openLayer opens a layer blob uncompressed (tar, gzip or zstd).
func openLayer(p string) (io.ReadCloser, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	magic, _ := br.Peek(4)
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		zr, err := gzip.NewReader(br)
		if err != nil {
			f.Close()
			return nil, err
		}
		return readCloser{zr, func() error { zr.Close(); return f.Close() }}, nil
	case len(magic) == 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd:
		zr, err := zstd.NewReader(br)
		if err != nil {
			f.Close()
			return nil, err
		}
		return readCloser{zr, func() error { zr.Close(); return f.Close() }}, nil
	}
	return readCloser{br, f.Close}, nil
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r readCloser) Close() error { return r.close() }

// untar extracts the outer image tarball (regular files and dirs only;
// names are confined to dst).
func untar(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(bufio.NewReaderSize(f, 1<<20))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name, err := cleanName(h.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		p := filepath.Join(dst, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}
}
