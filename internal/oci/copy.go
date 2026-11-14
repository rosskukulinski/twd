package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Source is where images are read from: a registry (Client) or a local
// image layout (Layout). repo is ignored by a layout.
type Source interface {
	Manifest(ctx context.Context, repo, reference string) (body []byte, mediaType, digest string, err error)
	Blob(ctx context.Context, repo, digest string, size int64) (io.ReadCloser, error)
}

// Target is where images are written: a registry (Client) or mgmt's own
// registry in-process.
type Target interface {
	HasBlob(ctx context.Context, repo, digest string) (bool, error)
	// WriteBlob stores r (size bytes, which must hash to digest).
	WriteBlob(ctx context.Context, repo, digest string, size int64, r io.Reader) error
	WriteManifest(ctx context.Context, repo, reference, mediaType string, b []byte) error
}

// Manifest implements Source.
func (c *Client) Manifest(ctx context.Context, repo, reference string) ([]byte, string, string, error) {
	return c.GetManifest(ctx, repo, reference)
}

// Blob implements Source.
func (c *Client) Blob(ctx context.Context, repo, digest string, size int64) (io.ReadCloser, error) {
	return c.GetBlob(ctx, repo, digest, size)
}

// HasBlob implements Target.
func (c *Client) HasBlob(ctx context.Context, repo, digest string) (bool, error) {
	return c.BlobExists(ctx, repo, digest)
}

// WriteBlob implements Target (one PUT; r is read once).
func (c *Client) WriteBlob(ctx context.Context, repo, digest string, size int64, r io.Reader) error {
	once := false
	return c.PutBlob(ctx, repo, digest, size, func() (io.ReadCloser, error) {
		if once {
			return nil, fmt.Errorf("blob %s: cannot replay the upload body", digest)
		}
		once = true
		return io.NopCloser(r), nil
	})
}

// WriteManifest implements Target.
func (c *Client) WriteManifest(ctx context.Context, repo, reference, mediaType string, b []byte) error {
	_, err := c.PutManifest(ctx, repo, reference, mediaType, b)
	return err
}

// Copied is what Copy wrote.
type Copied struct {
	// Digest and MediaType are the top-level manifest's (an index, or an
	// image manifest).
	Digest    string
	MediaType string
	// Platforms maps each copied architecture to its image manifest digest
	// (a single-platform image maps its config's architecture to Digest).
	Platforms map[string]string
	// Index is whether Digest is an index; IndexWritten whether the index
	// itself was written (only when every arch it lists was copied).
	Index, IndexWritten bool
	// Bytes is how much blob content was transferred (0 = all present).
	Bytes int64
}

// Copy copies srcRepo@reference from src to dst as dstRepo. For an index,
// only the linux image manifests of arches are copied (all arches when
// arches is empty) and the index is written only if nothing was left out.
// An arch the image doesn't have is an error naming the ones it has.
// Every manifest is written by digest; tag, if set, then names the top
// level (the index when written, else the single copied manifest).
func Copy(ctx context.Context, src Source, srcRepo, reference string, dst Target, dstRepo string, arches []string, tag string) (*Copied, error) {
	body, mt, dg, err := src.Manifest(ctx, srcRepo, reference)
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(body, mt)
	if err != nil {
		return nil, fmt.Errorf("%s@%s: %w", srcRepo, reference, err)
	}
	out := &Copied{Digest: dg, MediaType: m.MediaType, Platforms: map[string]string{}}
	if !m.IsIndex() {
		arch, err := configArch(ctx, src, srcRepo, m)
		if err != nil {
			return nil, err
		}
		if len(arches) > 0 && !contains(arches, arch) {
			return nil, fmt.Errorf("%s@%s is a linux/%s image; need %s", srcRepo, reference, arch, strings.Join(arches, ", "))
		}
		n, err := copyImage(ctx, src, srcRepo, dst, dstRepo, m, body, dg)
		if err != nil {
			return nil, err
		}
		out.Bytes, out.Platforms[arch] = n, dg
		if tag != "" {
			if err := dst.WriteManifest(ctx, dstRepo, tag, m.MediaType, body); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	out.Index = true
	want := arches
	if len(want) == 0 {
		for _, d := range m.Manifests {
			if d.Platform != nil && d.Platform.OS == "linux" && !contains(want, d.Platform.Architecture) {
				want = append(want, d.Platform.Architecture)
			}
		}
	}
	var have []string
	for _, d := range m.Manifests {
		if d.Platform != nil && d.Platform.OS == "linux" {
			have = append(have, d.Platform.Architecture)
		}
	}
	sort.Strings(have)
	for _, a := range want {
		if m.ForPlatform(a) == nil {
			return nil, fmt.Errorf("%s@%s has no linux/%s image (it has %s)", srcRepo, reference, a, strings.Join(have, ", "))
		}
	}
	complete := true // every manifest the index lists gets copied
	for _, d := range m.Manifests {
		arch := ""
		if d.Platform != nil && d.Platform.OS == "linux" {
			arch = d.Platform.Architecture
		}
		if arch == "" || !contains(want, arch) || out.Platforms[arch] != "" {
			complete = false // attestations, other arches, a second variant
			continue
		}
		cb, cmt, cdg, err := src.Manifest(ctx, srcRepo, d.Digest)
		if err != nil {
			return nil, err
		}
		cm, err := ParseManifest(cb, firstNonEmpty(cmt, d.MediaType))
		if err != nil {
			return nil, fmt.Errorf("%s@%s: %w", srcRepo, d.Digest, err)
		}
		if cm.IsIndex() {
			return nil, fmt.Errorf("%s@%s: nested indexes are not supported", srcRepo, d.Digest)
		}
		n, err := copyImage(ctx, src, srcRepo, dst, dstRepo, cm, cb, cdg)
		if err != nil {
			return nil, err
		}
		out.Bytes += n
		out.Platforms[arch] = cdg
	}
	if complete {
		if err := dst.WriteManifest(ctx, dstRepo, dg, m.MediaType, body); err != nil {
			return nil, err
		}
		out.IndexWritten = true
	}
	if tag != "" {
		switch {
		case out.IndexWritten:
			err = dst.WriteManifest(ctx, dstRepo, tag, m.MediaType, body)
		case len(out.Platforms) == 1:
			for _, d := range out.Platforms {
				cb, cmt, _, e := src.Manifest(ctx, srcRepo, d)
				if e != nil {
					return nil, e
				}
				err = dst.WriteManifest(ctx, dstRepo, tag, cmt, cb)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// copyImage copies an image manifest's blobs, then the manifest by digest.
func copyImage(ctx context.Context, src Source, srcRepo string, dst Target, dstRepo string, m *Manifest, body []byte, dg string) (int64, error) {
	var n int64
	for _, d := range m.Refs() {
		ok, err := dst.HasBlob(ctx, dstRepo, d.Digest)
		if err != nil {
			return n, err
		}
		if ok {
			continue
		}
		rc, err := src.Blob(ctx, srcRepo, d.Digest, d.Size)
		if err != nil {
			return n, fmt.Errorf("blob %s: %w", d.Digest, err)
		}
		v := NewVerifier(rc, d.Digest, d.Size)
		err = dst.WriteBlob(ctx, dstRepo, d.Digest, d.Size, v)
		rc.Close()
		if err != nil {
			return n, fmt.Errorf("blob %s: %w", d.Digest, err)
		}
		n += d.Size
	}
	return n, dst.WriteManifest(ctx, dstRepo, dg, m.MediaType, body)
}

// configArch reads an image manifest's config for its architecture.
func configArch(ctx context.Context, src Source, repo string, m *Manifest) (string, error) {
	cfg, err := ReadConfig(ctx, src, repo, m)
	if err != nil {
		return "", err
	}
	if cfg.OS != "" && cfg.OS != "linux" {
		return "", fmt.Errorf("image is %s/%s; twd runs linux images", cfg.OS, cfg.Architecture)
	}
	return cfg.Architecture, nil
}

// ImageConfig is the platform part of an image config.
type ImageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

// ReadConfig fetches and parses an image manifest's config blob.
func ReadConfig(ctx context.Context, src Source, repo string, m *Manifest) (*ImageConfig, error) {
	if m.Config == nil {
		return nil, fmt.Errorf("not an image manifest")
	}
	if m.Config.Size > 16<<20 {
		return nil, fmt.Errorf("image config is %d bytes", m.Config.Size)
	}
	rc, err := src.Blob(ctx, repo, m.Config.Digest, m.Config.Size)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", m.Config.Digest, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(NewVerifier(rc, m.Config.Digest, m.Config.Size))
	if err != nil {
		return nil, err
	}
	var c ImageConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config %s: %w", m.Config.Digest, err)
	}
	return &c, nil
}

// Resolve returns the image manifest digest for linux/arch of
// repo@reference (the reference itself for a single-platform image of
// that arch).
func Resolve(ctx context.Context, src Source, repo, reference, arch string) (string, error) {
	body, mt, dg, err := src.Manifest(ctx, repo, reference)
	if err != nil {
		return "", err
	}
	m, err := ParseManifest(body, mt)
	if err != nil {
		return "", err
	}
	if m.IsIndex() {
		d := m.ForPlatform(arch)
		if d == nil {
			return "", fmt.Errorf("%s@%s has no linux/%s image", repo, reference, arch)
		}
		return d.Digest, nil
	}
	a, err := configArch(ctx, src, repo, m)
	if err != nil {
		return "", err
	}
	if a != arch {
		return "", fmt.Errorf("%s@%s is a linux/%s image, not linux/%s", repo, reference, a, arch)
	}
	return dg, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// MemTarget is an in-memory Target and Source (tests).
type MemTarget struct {
	Blobs     map[string][]byte
	Manifests map[string][]byte // repo@digest and repo:tag
	Types     map[string]string
}

func (t *MemTarget) init() {
	if t.Blobs == nil {
		t.Blobs, t.Manifests, t.Types = map[string][]byte{}, map[string][]byte{}, map[string]string{}
	}
}

// HasBlob implements Target.
func (t *MemTarget) HasBlob(_ context.Context, repo, d string) (bool, error) {
	t.init()
	_, ok := t.Blobs[repo+"@"+d]
	return ok, nil
}

// WriteBlob implements Target.
func (t *MemTarget) WriteBlob(_ context.Context, repo, d string, size int64, r io.Reader) error {
	t.init()
	b, err := io.ReadAll(NewVerifier(r, d, size))
	if err != nil {
		return err
	}
	t.Blobs[repo+"@"+d] = b
	return nil
}

// WriteManifest implements Target.
func (t *MemTarget) WriteManifest(_ context.Context, repo, ref, mt string, b []byte) error {
	t.init()
	k := repo + "@" + ref
	if !ValidDigest(ref) {
		k = repo + ":" + ref
	} else if Digest(b) != ref {
		return ErrDigestMismatch
	}
	t.Manifests[k], t.Types[k] = b, mt
	return nil
}

// Manifest implements Source.
func (t *MemTarget) Manifest(_ context.Context, repo, ref string) ([]byte, string, string, error) {
	t.init()
	k := repo + "@" + ref
	if !ValidDigest(ref) {
		k = repo + ":" + ref
	}
	b, ok := t.Manifests[k]
	if !ok {
		return nil, "", "", &Error{Status: 404, Code: "MANIFEST_UNKNOWN"}
	}
	return b, t.Types[k], Digest(b), nil
}

// Blob implements Source.
func (t *MemTarget) Blob(_ context.Context, repo, d string, _ int64) (io.ReadCloser, error) {
	t.init()
	b, ok := t.Blobs[repo+"@"+d]
	if !ok {
		return nil, &Error{Status: 404, Code: "BLOB_UNKNOWN"}
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
