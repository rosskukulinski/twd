package oci

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Layout reads an OCI image layout (a directory, or a tar of one: what
// `docker buildx build --output type=oci` writes) or a `docker save`
// tarball (converted on the fly into an OCI manifest with uncompressed
// layers). It is a Source; repo arguments are ignored.
type Layout struct {
	dir string
	tmp bool
	// Top is the image the layout holds: its index.json's one entry (an
	// image manifest or an index), or the synthesised docker-save manifest.
	Top Descriptor
	// synth maps digests of synthesised content (docker save) to bytes or
	// files.
	synthBytes map[string][]byte
	synthFiles map[string]string
}

// OpenLayout opens p (a directory or a tarball). Close removes any
// extracted copy.
func OpenLayout(p string) (*Layout, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	l := &Layout{dir: p}
	if !st.IsDir() {
		d, err := os.MkdirTemp("", "twd-oci-layout-")
		if err != nil {
			return nil, err
		}
		l.dir, l.tmp = d, true
		if err := extract(p, d); err != nil {
			l.Close()
			return nil, err
		}
	}
	if err := l.load(); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Close removes the extracted copy (a directory layout is left alone).
func (l *Layout) Close() error {
	if l.tmp {
		return os.RemoveAll(l.dir)
	}
	return nil
}

func (l *Layout) load() error {
	if b, err := os.ReadFile(filepath.Join(l.dir, "index.json")); err == nil {
		var idx Manifest
		if err := json.Unmarshal(b, &idx); err != nil {
			return fmt.Errorf("index.json: %w", err)
		}
		var tops []Descriptor
		for _, d := range idx.Manifests {
			if IsManifestType(d.MediaType) && ValidDigest(d.Digest) {
				tops = append(tops, d)
			}
		}
		switch len(tops) {
		case 0:
			return errors.New("index.json lists no image")
		case 1:
			l.Top = tops[0]
		default:
			// Several images (docker save of a multi-platform image, or
			// several tags): present index.json itself as the index.
			ib, _ := json.Marshal(Manifest{SchemaVersion: 2, MediaType: MediaTypeIndex, Manifests: tops})
			l.synthBytes = map[string][]byte{Digest(ib): ib}
			l.Top = Descriptor{MediaType: MediaTypeIndex, Digest: Digest(ib), Size: int64(len(ib))}
		}
		return nil
	}
	b, err := os.ReadFile(filepath.Join(l.dir, "manifest.json"))
	if err != nil {
		return errors.New("not an OCI image layout or docker-save tarball (no index.json or manifest.json)")
	}
	var dm []struct {
		Config string   `json:"Config"`
		Layers []string `json:"Layers"`
	}
	if err := json.Unmarshal(b, &dm); err != nil || len(dm) == 0 {
		return fmt.Errorf("manifest.json: %v", err)
	}
	l.synthBytes, l.synthFiles = map[string][]byte{}, map[string]string{}
	cfg, err := l.confined(dm[0].Config)
	if err != nil {
		return err
	}
	cb, err := os.ReadFile(cfg)
	if err != nil {
		return err
	}
	m := Manifest{SchemaVersion: 2, MediaType: MediaTypeManifest,
		Config: &Descriptor{MediaType: MediaTypeConfig, Digest: Digest(cb), Size: int64(len(cb))}}
	l.synthBytes[m.Config.Digest] = cb
	for _, lp := range dm[0].Layers {
		p, err := l.confined(lp)
		if err != nil {
			return err
		}
		d, n, err := fileDigest(p)
		if err != nil {
			return err
		}
		mt := MediaTypeLayer
		if isGzip(p) {
			mt = MediaTypeLayerGzip
		}
		m.Layers = append(m.Layers, Descriptor{MediaType: mt, Digest: d, Size: n})
		l.synthFiles[d] = p
	}
	mb, _ := json.Marshal(m)
	l.synthBytes[Digest(mb)] = mb
	l.Top = Descriptor{MediaType: MediaTypeManifest, Digest: Digest(mb), Size: int64(len(mb))}
	return nil
}

func (l *Layout) confined(rel string) (string, error) {
	p := filepath.Join(l.dir, filepath.FromSlash(rel))
	if !strings.HasPrefix(filepath.Clean(p), filepath.Clean(l.dir)+string(filepath.Separator)) {
		return "", fmt.Errorf("layout path %q escapes the layout", rel)
	}
	return p, nil
}

func (l *Layout) open(d string) (io.ReadCloser, int64, error) {
	if !ValidDigest(d) {
		return nil, 0, fmt.Errorf("bad digest %q", d)
	}
	if b, ok := l.synthBytes[d]; ok {
		return io.NopCloser(strings.NewReader(string(b))), int64(len(b)), nil
	}
	p := filepath.Join(l.dir, "blobs", "sha256", Hex(d))
	if f, ok := l.synthFiles[d]; ok {
		p = f
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, &Error{Status: 404, Code: "BLOB_UNKNOWN", Message: d + " is not in the layout"}
		}
		return nil, 0, err
	}
	st, _ := f.Stat()
	return f, st.Size(), nil
}

// Manifest implements Source (reference must be a digest).
func (l *Layout) Manifest(_ context.Context, _, reference string) ([]byte, string, string, error) {
	rc, _, err := l.open(reference)
	if err != nil {
		return nil, "", "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(NewVerifier(io.LimitReader(rc, MaxManifest+1), reference, -1))
	if err != nil {
		return nil, "", "", err
	}
	var probe struct {
		MediaType string `json:"mediaType"`
	}
	json.Unmarshal(b, &probe)
	mt := probe.MediaType
	if mt == "" && reference == l.Top.Digest {
		mt = l.Top.MediaType
	}
	return b, mt, reference, nil
}

// Blob implements Source.
func (l *Layout) Blob(_ context.Context, _, digest string, size int64) (io.ReadCloser, error) {
	rc, _, err := l.open(digest)
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{NewVerifier(rc, digest, size), rc}, nil
}

func fileDigest(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, err
}

func isGzip(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [2]byte
	_, err = io.ReadFull(f, b[:])
	return err == nil && b[0] == 0x1f && b[1] == 0x8b
}

// extract unpacks regular files and directories of a tarball into dst
// (names confined to dst).
func extract(src, dst string) error {
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
		name := path.Clean("/" + h.Name)[1:]
		if name == "" || name == "." {
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

// WriteLayout is a Target that writes an OCI image layout directory (host
// pulls: hostd/image converts the directory). Blobs are verified as they
// are written; WriteManifest by tag records the image in index.json.
type WriteLayout struct {
	Dir string
	top []Descriptor
}

func (w *WriteLayout) blobPath(d string) string {
	return filepath.Join(w.Dir, "blobs", "sha256", Hex(d))
}

// HasBlob implements Target.
func (w *WriteLayout) HasBlob(_ context.Context, _, d string) (bool, error) {
	if !ValidDigest(d) {
		return false, fmt.Errorf("bad digest %q", d)
	}
	_, err := os.Stat(w.blobPath(d))
	return err == nil, nil
}

// WriteBlob implements Target.
func (w *WriteLayout) WriteBlob(_ context.Context, _, d string, size int64, r io.Reader) error {
	if !ValidDigest(d) {
		return fmt.Errorf("bad digest %q", d)
	}
	if err := os.MkdirAll(filepath.Join(w.Dir, "blobs", "sha256"), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(w.Dir, "blobs", "sha256"), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, NewVerifier(r, d, size)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), w.blobPath(d))
}

// WriteManifest implements Target: a digest reference stores the blob; a
// tag also adds it to index.json.
func (w *WriteLayout) WriteManifest(ctx context.Context, repo, ref, mt string, b []byte) error {
	d := Digest(b)
	if ValidDigest(ref) && ref != d {
		return ErrDigestMismatch
	}
	if err := w.WriteBlob(ctx, repo, d, int64(len(b)), strings.NewReader(string(b))); err != nil {
		return err
	}
	if !ValidDigest(ref) {
		w.top = append(w.top, Descriptor{MediaType: mt, Digest: d, Size: int64(len(b)), Annotations: map[string]string{"org.opencontainers.image.ref.name": ref}})
	}
	return nil
}

// Finish writes oci-layout and index.json naming top.
func (w *WriteLayout) Finish(top Descriptor) error {
	if err := os.WriteFile(filepath.Join(w.Dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o600); err != nil {
		return err
	}
	b, _ := json.Marshal(Manifest{SchemaVersion: 2, MediaType: MediaTypeIndex, Manifests: []Descriptor{top}})
	return os.WriteFile(filepath.Join(w.Dir, "index.json"), b, 0o600)
}
