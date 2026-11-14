// Package oci is the small slice of the OCI image and distribution specs
// twd needs for ADR-032 D3: manifests and indexes, digests, image
// references, a distribution client (push, pull, Docker Hub's anonymous
// bearer tokens) and copying an image between registries for the
// architectures a deployment needs.
//
// Everything here verifies content by digest: a blob or manifest whose
// bytes don't hash to the digest it was asked for is an error, never data.
package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
	"strings"
)

// Media types twd reads and writes.
const (
	MediaTypeManifest       = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeIndex          = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeConfig         = "application/vnd.oci.image.config.v1+json"
	MediaTypeLayer          = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeLayerGzip      = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// ManifestAccept is the Accept header for manifest requests.
var ManifestAccept = strings.Join([]string{MediaTypeIndex, MediaTypeManifest, MediaTypeDockerList, MediaTypeDockerManifest}, ", ")

// MaxManifest bounds a manifest or index (the distribution spec suggests
// registries accept at least 4 MiB).
const MaxManifest = 4 << 20

// Platform is an image's platform.
type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

// Descriptor points at content by digest.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *Platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Manifest is an image manifest or an image index (Docker's v2 schema 2
// manifest and manifest list share the shape).
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Config        *Descriptor  `json:"config,omitempty"`
	Layers        []Descriptor `json:"layers,omitempty"`
	Manifests     []Descriptor `json:"manifests,omitempty"`
	Subject       *Descriptor  `json:"subject,omitempty"`
}

// IsIndex reports whether m lists manifests (an index / manifest list).
func (m *Manifest) IsIndex() bool {
	return m.MediaType == MediaTypeIndex || m.MediaType == MediaTypeDockerList || m.Config == nil && len(m.Manifests) > 0
}

// IsIndexType reports whether a media type is an index or manifest list.
func IsIndexType(mt string) bool { return mt == MediaTypeIndex || mt == MediaTypeDockerList }

// IsManifestType reports whether mt is one of the manifest media types.
func IsManifestType(mt string) bool {
	switch mt {
	case MediaTypeIndex, MediaTypeManifest, MediaTypeDockerList, MediaTypeDockerManifest:
		return true
	}
	return false
}

// ParseManifest parses and sanity-checks a manifest or index. mediaType
// (the Content-Type it came with; "" = unknown) must agree with the
// document's own mediaType when both are set.
func ParseManifest(b []byte, mediaType string) (*Manifest, error) {
	if len(b) > MaxManifest {
		return nil, fmt.Errorf("manifest is %d bytes (max %d)", len(b), MaxManifest)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.SchemaVersion != 2 {
		return nil, fmt.Errorf("manifest schemaVersion %d (want 2)", m.SchemaVersion)
	}
	mediaType = strings.TrimSpace(strings.Split(mediaType, ";")[0])
	if m.MediaType == "" {
		m.MediaType = mediaType
	}
	if mediaType != "" && m.MediaType != mediaType && IsManifestType(mediaType) {
		return nil, fmt.Errorf("manifest mediaType %q does not match Content-Type %q", m.MediaType, mediaType)
	}
	if m.MediaType == "" {
		if m.Config != nil {
			m.MediaType = MediaTypeManifest
		} else {
			m.MediaType = MediaTypeIndex
		}
	}
	if !IsManifestType(m.MediaType) {
		return nil, fmt.Errorf("unsupported manifest mediaType %q", m.MediaType)
	}
	if m.IsIndex() {
		if m.Config != nil || len(m.Layers) > 0 {
			return nil, errors.New("an index has no config or layers")
		}
	} else if m.Config == nil {
		return nil, errors.New("an image manifest needs a config")
	}
	for _, d := range m.Refs() {
		if !ValidDigest(d.Digest) {
			return nil, fmt.Errorf("manifest references a bad digest %q", d.Digest)
		}
		if d.Size < 0 {
			return nil, fmt.Errorf("manifest references %s with a negative size", d.Digest)
		}
	}
	return &m, nil
}

// Refs lists every descriptor m references (config, layers, manifests;
// not the subject).
func (m *Manifest) Refs() []Descriptor {
	var out []Descriptor
	if m.Config != nil {
		out = append(out, *m.Config)
	}
	out = append(out, m.Layers...)
	return append(out, m.Manifests...)
}

// ForPlatform picks an index's manifest for linux/arch (nil if none).
// Entries with a non-linux or unknown platform (attestations) are skipped.
func (m *Manifest) ForPlatform(arch string) *Descriptor {
	for i := range m.Manifests {
		d := &m.Manifests[i]
		if d.Platform != nil && d.Platform.OS == "linux" && d.Platform.Architecture == arch {
			return d
		}
	}
	return nil
}

var digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ValidDigest reports whether d is a sha256 digest (the only algorithm twd
// accepts).
func ValidDigest(d string) bool { return digestRE.MatchString(d) }

// Digest is the sha256 digest of b.
func Digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// Hex is d without its "sha256:" prefix.
func Hex(d string) string { return strings.TrimPrefix(d, "sha256:") }

// ErrDigestMismatch: content does not hash to the digest it was fetched by.
var ErrDigestMismatch = errors.New("digest mismatch")

// Verifier hashes what it reads and checks the digest (and size, if ≥ 0)
// at EOF: a reader that returns io.EOF only if both match.
type Verifier struct {
	r      io.Reader
	h      hash.Hash
	want   string
	size   int64
	n      int64
	failed error
}

// NewVerifier wraps r.
func NewVerifier(r io.Reader, digest string, size int64) *Verifier {
	return &Verifier{r: r, h: sha256.New(), want: digest, size: size}
}

func (v *Verifier) Read(p []byte) (int, error) {
	if v.failed != nil {
		return 0, v.failed
	}
	n, err := v.r.Read(p)
	v.n += int64(n)
	v.h.Write(p[:n])
	if v.size >= 0 && v.n > v.size {
		v.failed = fmt.Errorf("%w: %s is larger than its %d bytes", ErrDigestMismatch, v.want, v.size)
		return n, v.failed
	}
	if err == io.EOF {
		if got := "sha256:" + hex.EncodeToString(v.h.Sum(nil)); got != v.want {
			v.failed = fmt.Errorf("%w: got %s, want %s", ErrDigestMismatch, got, v.want)
			return n, v.failed
		}
		if v.size >= 0 && v.n != v.size {
			v.failed = fmt.Errorf("%w: %s is %d bytes, want %d", ErrDigestMismatch, v.want, v.n, v.size)
			return n, v.failed
		}
	}
	return n, err
}

// N is how many bytes were read.
func (v *Verifier) N() int64 { return v.n }

// ---- names and references ----

var (
	componentRE = `[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*`
	nameRE      = regexp.MustCompile(`^` + componentRE + `(?:/` + componentRE + `)*$`)
	componentOK = regexp.MustCompile(`^` + componentRE + `$`)
	tagRE       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// ValidName reports whether s is a repository name (distribution spec
// grammar, at most 255 characters).
func ValidName(s string) bool { return len(s) <= 255 && nameRE.MatchString(s) }

// ValidComponent reports whether s is one path component of a name (twd
// tenant IDs must be, to own a namespace).
func ValidComponent(s string) bool { return componentOK.MatchString(s) }

// ValidTag reports whether s is a tag.
func ValidTag(s string) bool { return tagRE.MatchString(s) }

// Ref is a parsed image reference: registry host, repository, and a tag
// and/or digest.
type Ref struct {
	Registry   string // "docker.io", "ghcr.io", "localhost:5000", "twd.local"
	Repository string // "library/debian", "cloudflare/sandbox"
	Tag        string
	Digest     string
}

// ParseRef parses ref the way Docker does: no registry means docker.io, a
// single-component docker.io name gets library/, and no tag or digest means
// :latest.
func ParseRef(ref string) (Ref, error) {
	var r Ref
	s := ref
	if i := strings.Index(s, "@"); i >= 0 {
		r.Digest = s[i+1:]
		s = s[:i]
		if !ValidDigest(r.Digest) {
			return r, fmt.Errorf("image reference %q: bad digest", ref)
		}
	}
	if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") {
		r.Tag = s[i+1:]
		s = s[:i]
		if !ValidTag(r.Tag) {
			return r, fmt.Errorf("image reference %q: bad tag", ref)
		}
	}
	first, rest, ok := strings.Cut(s, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Registry, r.Repository = first, rest
	} else {
		r.Registry, r.Repository = "docker.io", s
	}
	if r.Registry == "index.docker.io" || r.Registry == "registry-1.docker.io" {
		r.Registry = "docker.io"
	}
	if r.Registry == "docker.io" && !strings.Contains(r.Repository, "/") {
		r.Repository = "library/" + r.Repository
	}
	if !ValidName(r.Repository) {
		return r, fmt.Errorf("image reference %q: bad repository name %q", ref, r.Repository)
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r, nil
}

// Name is registry/repository.
func (r Ref) Name() string { return r.Registry + "/" + r.Repository }

// Reference is what to ask the registry for: the digest if pinned, else
// the tag.
func (r Ref) Reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

// String is the normalised reference.
func (r Ref) String() string {
	s := r.Name()
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// Normalize returns ref in normalised form (ref itself if it doesn't parse).
func Normalize(ref string) string {
	r, err := ParseRef(ref)
	if err != nil {
		return ref
	}
	return r.String()
}
