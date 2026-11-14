package registry

import (
	"context"
	"sync"
)

// MemMeta is an in-memory Meta (fakemgmt, tests).
type MemMeta struct {
	mu    sync.Mutex
	blobs map[string]int64       // repo@digest
	mans  map[string]ManifestRec // repo@digest
	tags  map[string]map[string]string
}

func (m *MemMeta) init() {
	if m.blobs == nil {
		m.blobs, m.mans, m.tags = map[string]int64{}, map[string]ManifestRec{}, map[string]map[string]string{}
	}
}

// LinkBlob implements Meta.
func (m *MemMeta) LinkBlob(_ context.Context, repo, digest string, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.blobs[repo+"@"+digest] = size
	return nil
}

// BlobSize implements Meta.
func (m *MemMeta) BlobSize(_ context.Context, repo, digest string) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	n, ok := m.blobs[repo+"@"+digest]
	return n, ok, nil
}

// LinkManifest implements Meta.
func (m *MemMeta) LinkManifest(_ context.Context, repo string, r ManifestRec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.mans[repo+"@"+r.Digest] = r
	return nil
}

// GetManifest implements Meta.
func (m *MemMeta) GetManifest(_ context.Context, repo, digest string) (ManifestRec, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	r, ok := m.mans[repo+"@"+digest]
	return r, ok, nil
}

// SetTag implements Meta.
func (m *MemMeta) SetTag(_ context.Context, repo, tag, digest string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	if m.tags[repo] == nil {
		m.tags[repo] = map[string]string{}
	}
	m.tags[repo][tag] = digest
	return nil
}

// ResolveTag implements Meta.
func (m *MemMeta) ResolveTag(_ context.Context, repo, tag string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	d, ok := m.tags[repo][tag]
	return d, ok, nil
}

// Tags implements Meta.
func (m *MemMeta) Tags(_ context.Context, repo string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	var out []string
	for t := range m.tags[repo] {
		out = append(out, t)
	}
	return out, nil
}
