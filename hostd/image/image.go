// Package image turns OCI container images into what twd's container
// guests boot (ADR-032 D3): a read-only erofs, addressed by the image's
// digest, plus the parsed image config (entrypoint, cmd, env, working
// directory, user).
//
// The host never runs a container engine. An image arrives as a file (a
// `docker save` tarball or an OCI image layout; the mgmt registry pull
// comes later), its layers are flattened here in Go (whiteouts, opaque
// directories, replaced subtrees, hard links), and mkfs.erofs writes the
// filesystem from the merged tar without root, keeping every owner, mode
// and xattr from the layers.
//
// The cache is content-addressed and scoped: per tenant by default, or
// "public" for operator-marked base images, so that a cache hit can't tell
// one tenant which images another uses.
package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// PublicScope is the cache scope of operator-marked public base images.
const PublicScope = "public"

// Image is one converted image.
type Image struct {
	Ref    string // what was asked for (after aliasing)
	Digest string // sha256:<hex> of the image config (the image ID)
	Scope  string // tenant ID or PublicScope
	Path   string // the erofs (read-only, 0644 inside 0700 dirs)
	Config Config
	SizeB  int64
	// ConvertMS is the conversion time; 0 on a cache hit.
	ConvertMS int64
}

// Store converts and caches images.
type Store struct {
	// Dir is the cache root: Dir/<scope>/<hex>.erofs (+ .json config).
	Dir string
	// Mkfs is mkfs.erofs (default: from PATH).
	Mkfs string
	// Arch is the host's GOARCH-style architecture (default runtime.GOARCH).
	Arch string
	// Emulated are further image architectures the container backend runs
	// (amd64 under Rosetta for Linux on a Mac, ADR-032 D7). An image index
	// still resolves to the native variant first.
	Emulated []string
	// Sources resolve refs to image files.
	Sources []Source
	// Aliases rewrite refs before resolution (arm64 rebuilds of amd64-only
	// images; see Alias).
	Aliases []Alias
	// Public lists refs (after aliasing) shared across tenants.
	Public map[string]bool

	mu       sync.Mutex
	inflight map[string]*call
}

type call struct {
	done chan struct{}
	img  *Image
	err  error
}

// Source finds the file for an image ref (a docker-save tarball or an OCI
// layout dir), or returns ErrNotFound.
type Source interface {
	Find(ctx context.Context, ref string) (string, error)
}

// ErrNotFound: no source has the image.
var ErrNotFound = errors.New("image not found")

// DirSource serves images from a directory: Dir/images.json maps refs to
// files in Dir ({"docker.io/library/debian:trixie": "debian-trixie.tar"}).
type DirSource struct{ Dir string }

// Find implements Source.
func (d DirSource) Find(_ context.Context, ref string) (string, error) {
	b, err := os.ReadFile(filepath.Join(d.Dir, "images.json"))
	if err != nil {
		return "", ErrNotFound
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("%s/images.json: %w", d.Dir, err)
	}
	f, ok := m[ref]
	if !ok {
		f, ok = m[NormalizeRef(ref)]
	}
	if !ok {
		return "", ErrNotFound
	}
	p := filepath.Join(d.Dir, filepath.FromSlash(f))
	if !strings.HasPrefix(filepath.Clean(p), filepath.Clean(d.Dir)+string(filepath.Separator)) {
		return "", fmt.Errorf("images.json: %q is outside %s", f, d.Dir)
	}
	return p, nil
}

// Alias rewrites From (an exact, normalised ref) to To when the host's
// architecture is Arch ("" = any). ADR-032 D3: Cloudflare publishes
// cloudflare/sandbox for linux/amd64 only; on arm64 hosts twd substitutes
// its own rebuild from the Apache-2.0 source (guest/images/README.md).
type Alias struct {
	From, To, Arch string
}

// DefaultAliases are the arm64 rebuilds twd ships (built by
// guest/images/build-sandbox-images.sh; see guest/images/README.md).
var DefaultAliases = []Alias{
	{From: "docker.io/cloudflare/sandbox:0.12.14", To: "twd.local/cloudflare-sandbox:0.12.14-arm64", Arch: "arm64"},
	{From: "docker.io/cloudflare/sandbox:1.0.1", To: "twd.local/cloudflare-sandbox:1.0.1-arm64", Arch: "arm64"},
}

// NormalizeRef adds docker.io/ and library/ (and :latest) the way Docker
// does, so "cloudflare/sandbox:0.12.14" and "docker.io/cloudflare/sandbox:0.12.14"
// are one ref.
func NormalizeRef(ref string) string {
	if ref == "" || strings.Contains(ref, "@") && !strings.Contains(ref, "/") {
		return ref
	}
	name, tag := ref, ""
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name, tag = ref[:i], ref[i:]
	}
	if at := strings.Index(name, "@"); at >= 0 {
		name, tag = name[:at], name[at:]
	}
	first, _, hasSlash := strings.Cut(name, "/")
	if !hasSlash || !strings.ContainsAny(first, ".:") && first != "localhost" {
		if !hasSlash {
			name = "library/" + name
		}
		name = "docker.io/" + name
	}
	if tag == "" {
		tag = ":latest"
	}
	return name + tag
}

// Resolve applies the alias map for this host.
func (s *Store) Resolve(ref string) string {
	n := NormalizeRef(ref)
	for _, a := range s.Aliases {
		if NormalizeRef(a.From) == n && (a.Arch == "" || a.Arch == s.arch()) {
			return a.To
		}
	}
	return ref
}

func (s *Store) arch() string {
	if s.Arch != "" {
		return s.Arch
	}
	return runtime.GOARCH
}

// Runs reports whether images of arch run here (natively or emulated).
func (s *Store) Runs(arch string) bool {
	if arch == s.arch() {
		return true
	}
	for _, e := range s.Emulated {
		if arch == e {
			return true
		}
	}
	return false
}

var scopeRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Get returns ref converted for tenant (from the cache, or converted now).
func (s *Store) Get(ctx context.Context, tenant, ref string) (*Image, error) {
	r := s.Resolve(ref)
	scope := tenant
	if s.Public[NormalizeRef(r)] || s.Public[r] {
		scope = PublicScope
	}
	if !scopeRE.MatchString(scope) || scope == "." || scope == ".." {
		return nil, fmt.Errorf("image: bad cache scope %q", scope)
	}
	var src string
	var err error
	for _, so := range s.Sources {
		src, err = so.Find(ctx, r)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	if src == "" {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, r)
	}
	key := scope + "|" + src
	s.mu.Lock()
	if s.inflight == nil {
		s.inflight = map[string]*call{}
	}
	if c := s.inflight[key]; c != nil {
		s.mu.Unlock()
		select {
		case <-c.done:
			return c.img, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	s.inflight[key] = c
	s.mu.Unlock()
	c.img, c.err = s.convert(r, scope, src)
	close(c.done)
	s.mu.Lock()
	delete(s.inflight, key)
	s.mu.Unlock()
	return c.img, c.err
}

// convert converts src into the scope's cache (a no-op on a hit).
func (s *Store) convert(ref, scope, src string) (*Image, error) {
	st := time.Now()
	a, err := openArchive(src, append([]string{s.arch()}, s.Emulated...)...)
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	defer func() {
		if a.dir != src {
			a.close()
		}
	}()
	var cfg Config
	if err := json.Unmarshal(a.config, &cfg); err != nil {
		return nil, fmt.Errorf("image %s: config: %w", ref, err)
	}
	if cfg.OS != "" && cfg.OS != "linux" || cfg.Architecture != "" && !s.Runs(cfg.Architecture) {
		return nil, fmt.Errorf("image %s is %s/%s; this host runs linux/%s (ADR-032 D3: the image must match the host's arch)", ref, cfg.OS, cfg.Architecture,
			strings.Join(append([]string{s.arch()}, s.Emulated...), ", linux/"))
	}
	dir := filepath.Join(s.Dir, scope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	os.Chmod(s.Dir, 0o700)
	hx := strings.TrimPrefix(a.digest, "sha256:")
	out := filepath.Join(dir, hx+".erofs")
	img := &Image{Ref: ref, Digest: a.digest, Scope: scope, Path: out, Config: cfg}
	if fi, err := os.Stat(out); err == nil {
		img.SizeB = fi.Size()
		return img, nil
	}
	tmp := out + fmt.Sprintf(".tmp%d", os.Getpid())
	defer os.Remove(tmp)
	if err := s.mkfs(a, tmp, hx); err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil { // the jail uid reads it through a bind mount
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, hx+".json"), a.config, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, out); err != nil {
		return nil, err
	}
	fi, _ := os.Stat(out)
	img.SizeB = fi.Size()
	img.ConvertMS = time.Since(st).Milliseconds()
	return img, nil
}

// Check reports whether the store can convert images (mkfs.erofs).
func (s *Store) Check() error {
	if s.Mkfs != "" {
		_, err := os.Stat(s.Mkfs)
		return err
	}
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		return errors.New("mkfs.erofs not found (install erofs-utils)")
	}
	return nil
}

func (s *Store) mkfs(a *archive, out, hx string) error {
	mk := s.Mkfs
	if mk == "" {
		var err error
		if mk, err = exec.LookPath("mkfs.erofs"); err != nil {
			return errors.New("mkfs.erofs not found (install erofs-utils)")
		}
	}
	layers := make([]Layer, len(a.layers))
	for i, p := range a.layers {
		layers[i] = func() (io.ReadCloser, error) { return openLayer(p) }
	}
	merged, err := os.CreateTemp("", "twd-image-merged-")
	if err != nil {
		return err
	}
	defer os.Remove(merged.Name())
	if err := Flatten(layers, merged); err != nil {
		merged.Close()
		return err
	}
	if err := merged.Close(); err != nil {
		return err
	}
	// A fixed UUID from the digest and preserved mtimes: the same image
	// gives the same erofs bytes (on one erofs-utils version).
	uuid := hx[0:8] + "-" + hx[8:12] + "-" + hx[12:16] + "-" + hx[16:20] + "-" + hx[20:32]
	cmd := exec.Command(mk, "--quiet", "--tar=f", "-b4096", "--preserve-mtime", "-U"+uuid, "-Ltwd-image", out, merged.Name())
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.erofs: %v: %s", err, strings.TrimSpace(string(b)))
	}
	return nil
}
