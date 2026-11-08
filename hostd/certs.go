package hostd

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/internal/nodecert"
	"github.com/rosskukulinski/twd/netd"
)

// certSource is a tenant registry's netd.CertSource (RT-01, ADR-010): for
// each inbound caller it asks mgmt for the caller's node certificate (keyed
// by the WhoIs stable node ID), verifies it against the tenant's OBO root
// public key pinned when the tenant was opened, and hands netd the lim to
// intersect with the caller's CapMap. Results are cached briefly; a
// lookup failure is reported as an error so netd fails closed.
type certSource struct {
	h        *Host
	tenantID string
	pub      ed25519.PublicKey

	mu    sync.Mutex
	cache map[string]certEntry
}

type certEntry struct {
	obj     *netd.CallerObject // nil: not a twd object (ErrNoCert)
	expires time.Time
}

// certCacheTTL bounds how long a verified certificate (or a "not an
// object" answer) is reused before mgmt is asked again. A kill revokes
// the node itself (RST + device delete), so the cache only delays
// seeing a re-issued certificate.
const certCacheTTL = 60 * time.Second

func newCertSource(h *Host, tenantID string, pubB64 string) (*certSource, error) {
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("tenant %s: mgmt published no usable OBO public key (node certificates can't be verified)", tenantID)
	}
	return &certSource{h: h, tenantID: tenantID, pub: ed25519.PublicKey(pub), cache: map[string]certEntry{}}, nil
}

func (c *certSource) Lookup(ctx context.Context, stableNodeID string) (*netd.CallerObject, error) {
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.cache[stableNodeID]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		if e.obj == nil {
			return nil, netd.ErrNoCert
		}
		return e.obj, nil
	}
	c.mu.Unlock()
	nc, err := c.h.mgmt.NodeCert(ctx, c.tenantID, stableNodeID)
	if errors.Is(err, mgmtapi.ErrNoCert) {
		c.put(stableNodeID, nil, now)
		return nil, netd.ErrNoCert
	}
	if err != nil {
		if e, ok := mgmtapi.AsError(err); ok && e.Code == mgmtapi.CodeKilled {
			// A killed object's node: nothing it says is trusted.
			return nil, fmt.Errorf("caller %s is a killed object", stableNodeID)
		}
		return nil, err // netd fails closed
	}
	p, err := nodecert.Verify(c.pub, nc.Cert, stableNodeID, now)
	if err != nil {
		return nil, fmt.Errorf("certificate of %s rejected: %w", stableNodeID, err)
	}
	if p.TenantID != c.tenantID {
		return nil, fmt.Errorf("certificate of %s is for tenant %s, not %s", stableNodeID, p.TenantID, c.tenantID)
	}
	obj := &netd.CallerObject{ObjectID: p.ObjectID, Lim: p.Lim, ParentID: p.ParentID, RootID: p.RootID}
	c.put(stableNodeID, obj, now)
	return obj, nil
}

func (c *certSource) put(id string, obj *netd.CallerObject, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) > 10000 {
		c.cache = map[string]certEntry{}
	}
	c.cache[id] = certEntry{obj: obj, expires: now.Add(certCacheTTL)}
}

// forget drops a cached entry (a kill pushed to this host).
func (c *certSource) forget(stableNodeID string) {
	c.mu.Lock()
	delete(c.cache, stableNodeID)
	c.mu.Unlock()
}

// FlushCertCache drops every tenant's cached node certificates, so the
// next request from each caller asks mgmt again (tests, ops).
func (h *Host) FlushCertCache() {
	h.mu.Lock()
	ts := make([]*tenant, 0, len(h.tenants))
	for _, t := range h.tenants {
		ts = append(ts, t)
	}
	h.mu.Unlock()
	for _, t := range ts {
		if t.certs != nil {
			t.certs.mu.Lock()
			t.certs.cache = map[string]certEntry{}
			t.certs.mu.Unlock()
		}
	}
}
