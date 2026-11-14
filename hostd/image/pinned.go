package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/internal/container"
	"github.com/rosskukulinski/twd/internal/oci"
	"github.com/rosskukulinski/twd/internal/registry"
)

// Images pinned at deploy (ADR-032 D3): hostd pulls Repository@Digest from
// twd's registry (mgmt, over the host's own tailnet node or the local-dev
// socket) and nowhere else; no container engine, no Docker Hub. Every
// manifest and blob is verified against its digest as it arrives.
//
// The cache scope comes from the pin's repository, not from anything the
// tenant says: "public/…" (operator-marked bases, alias targets) is shared
// by every tenant, a tenant's own namespace is that tenant's alone, and a
// pin into any other namespace is refused before anything is fetched. So
// a cache hit never tells one tenant which images another runs, and a
// tenant can't reach another's converted image by naming its digest.

// Puller fetches a pinned image manifest and its blobs into dir as an OCI
// image layout.
type Puller interface {
	Pull(ctx context.Context, repo, digest, dir string) error
}

// RegistryPuller pulls from twd's registry through an oci.Client.
type RegistryPuller struct{ Client *oci.Client }

// Pull implements Puller.
func (p RegistryPuller) Pull(ctx context.Context, repo, digest, dir string) error {
	w := &oci.WriteLayout{Dir: dir}
	cp, err := oci.Copy(ctx, p.Client, repo, digest, w, "", nil, "")
	if err != nil {
		return err
	}
	if cp.Index {
		return fmt.Errorf("%s@%s is an index; pins name one image manifest", repo, digest)
	}
	fi, err := os.Stat(filepath.Join(dir, "blobs", "sha256", oci.Hex(digest)))
	if err != nil {
		return err
	}
	return w.Finish(oci.Descriptor{MediaType: cp.MediaType, Digest: digest, Size: fi.Size()})
}

// pinScope is the cache scope of a pin for tenant.
func pinScope(tenant string, p container.Pin) (string, error) {
	switch ns := registry.Namespace(p.Repository); ns {
	case registry.PublicNamespace:
		return PublicScope, nil
	case tenant:
		return tenant, nil
	default:
		return "", fmt.Errorf("image pin %s@%s is in namespace %q, not tenant %s's or public: refused", p.Repository, p.Digest, ns, tenant)
	}
}

// GetPinned returns the image of pin for tenant: from the cache, or pulled
// from the registry by digest and converted now.
func (s *Store) GetPinned(ctx context.Context, tenant string, p container.Pin) (*Image, error) {
	if s.Puller == nil {
		return nil, errors.New("this host has no registry to pull pinned images from")
	}
	if !oci.ValidDigest(p.Digest) || !oci.ValidName(p.Repository) {
		return nil, fmt.Errorf("bad image pin %s@%s", p.Repository, p.Digest)
	}
	if p.Arch != s.arch() {
		return nil, fmt.Errorf("image pin %s@%s is for linux/%s; this host runs linux/%s", p.Repository, p.Digest, p.Arch, s.arch())
	}
	scope, err := pinScope(tenant, p)
	if err != nil {
		return nil, err
	}
	if !scopeRE.MatchString(scope) || scope == "." || scope == ".." {
		return nil, fmt.Errorf("image: bad cache scope %q", scope)
	}
	if img := s.pinnedHit(scope, p); img != nil {
		return img, nil
	}
	key := scope + "|" + p.Digest
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
	c.img, c.err = s.pullPinned(ctx, scope, p)
	close(c.done)
	s.mu.Lock()
	delete(s.inflight, key)
	s.mu.Unlock()
	if c.err == nil {
		s.gc()
	}
	return c.img, c.err
}

func (s *Store) pinnedHit(scope string, p container.Pin) *Image {
	// Pinned entries are named by the manifest digest (it covers the
	// config and every layer), not by the image ID alone.
	img, err := s.cached(scope, oci.Hex(p.Digest))
	if err != nil {
		return nil
	}
	img.Ref = p.Ref
	return img
}

func (s *Store) pullPinned(ctx context.Context, scope string, p container.Pin) (*Image, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(s.Dir, ".pull-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	st := time.Now()
	if err := s.Puller.Pull(ctx, p.Repository, p.Digest, tmp); err != nil {
		return nil, fmt.Errorf("pull %s@%s: %w", p.Repository, p.Digest, err)
	}
	pullMS := time.Since(st).Milliseconds()
	img, err := s.convert(p.Ref, scope, tmp, oci.Hex(p.Digest))
	if err != nil {
		return nil, err
	}
	img.PullMS = pullMS
	return img, nil
}

// cached loads a converted image (by its cache key) and marks it used.
func (s *Store) cached(scope, hx string) (*Image, error) {
	out := filepath.Join(s.Dir, scope, hx+".erofs")
	fi, err := os.Stat(out)
	if err != nil {
		return nil, err
	}
	cb, err := os.ReadFile(filepath.Join(s.Dir, scope, hx+".json"))
	if err != nil {
		return nil, err
	}
	img := &Image{Digest: oci.Digest(cb), Scope: scope, Path: out, SizeB: fi.Size()}
	if err := json.Unmarshal(cb, &img.Config); err != nil {
		return nil, err
	}
	now := time.Now()
	os.Chtimes(out, now, now) // LRU
	return img, nil
}

// ---- in use and GC ----

// Hold marks a converted image in use (a running container booted from
// it): GC never removes it until the returned release is called.
func (s *Store) Hold(path string) (release func()) {
	s.mu.Lock()
	if s.held == nil {
		s.held = map[string]int{}
	}
	s.held[path]++
	s.mu.Unlock()
	done := false
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !done {
			done = true
			if s.held[path]--; s.held[path] <= 0 {
				delete(s.held, path)
			}
		}
	}
}

// gc removes the least recently used converted images, across scopes,
// until the cache is within MaxBytes (0 = no cap). Images in use stay.
func (s *Store) gc() {
	if s.MaxBytes <= 0 {
		return
	}
	type ent struct {
		path string
		size int64
		mod  time.Time
	}
	var all []ent
	var total int64
	filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".erofs") {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			all = append(all, ent{p, fi.Size(), fi.ModTime()})
			total += fi.Size()
		}
		return nil
	})
	if total <= s.MaxBytes {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range all {
		if total <= s.MaxBytes {
			break
		}
		if s.held[e.path] > 0 {
			continue
		}
		if os.Remove(e.path) == nil {
			os.Remove(strings.TrimSuffix(e.path, ".erofs") + ".json")
			total -= e.size
			s.Evicted++
		}
	}
}

// CacheBytes is the converted images' total size.
func (s *Store) CacheBytes() int64 {
	var total int64
	filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".erofs") {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

// HostArch is the host's architecture (GOARCH).
func (s *Store) HostArch() string { return s.arch() }
