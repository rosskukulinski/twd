package mgmt

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/rosskukulinski/twd/internal/nodecert"
)

// Per-tenant OBO root key (ADR-010, RFC §4.6): an Ed25519 key kept in
// custody under "twd/<tenant>/obo-root" (the 32-byte seed), separate from
// the WIF issuer key. It signs node certificates at `ready`. Its public key
// is published on GET /v0/tenants/{id} (oboPublicKey) so hosts can pin it.

func oboCustodyName(tenantID string) string { return "twd/" + tenantID + "/obo-root" }

// oboKey loads (creating on first use) the tenant's OBO root key.
func (m *Mgmt) oboKey(ctx context.Context, tenantID string) (ed25519.PrivateKey, error) {
	m.mu.Lock()
	if k, ok := m.oboKeys[tenantID]; ok {
		m.mu.Unlock()
		return k, nil
	}
	m.mu.Unlock()
	seed, err := m.custody.Get(ctx, oboCustodyName(tenantID))
	if errors.Is(err, ErrNotFound) {
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		if err := m.custody.Put(ctx, oboCustodyName(tenantID), seed); err != nil {
			return nil, fmt.Errorf("custody: obo root: %w", err)
		}
		// Another replica may have raced us: re-read so every replica signs
		// with the same key.
		if s2, err := m.custody.Get(ctx, oboCustodyName(tenantID)); err == nil && len(s2) == ed25519.SeedSize {
			seed = s2
		}
		m.audit(ctx, AuditRecord{TenantID: tenantID, Kind: "obo_root"}, map[string]any{"created": true})
	} else if err != nil {
		return nil, fmt.Errorf("custody: obo root: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("custody: obo root key has the wrong size")
	}
	k := ed25519.NewKeyFromSeed(seed)
	m.mu.Lock()
	if m.oboKeys == nil {
		m.oboKeys = map[string]ed25519.PrivateKey{}
	}
	m.oboKeys[tenantID] = k
	m.mu.Unlock()
	return k, nil
}

// OBOPublicKey returns the tenant's OBO root public key, base64 (std).
func (m *Mgmt) OBOPublicKey(ctx context.Context, tenantID string) (string, error) {
	k, err := m.oboKey(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey)), nil
}

// signNodeCert issues the object's node certificate (valid nodecert.TTL).
func (m *Mgmt) signNodeCert(ctx context.Context, o Object) (string, error) {
	if o.StableNodeID == "" {
		return "", errors.New("nodecert: object has no stable node ID yet")
	}
	k, err := m.oboKey(ctx, o.TenantID)
	if err != nil {
		return "", err
	}
	now := m.now()
	tags := o.Tags
	if tags == nil {
		tags = []string{}
	}
	return nodecert.Sign(k, nodecert.Payload{TenantID: o.TenantID, ObjectID: o.ID, StableNodeID: o.StableNodeID, Tags: tags,
		Lim: o.Caps, ParentID: o.ParentID, RootID: o.RootID, IAT: now.Unix(), EXP: now.Add(nodecert.TTL).Unix()})
}

// NodeCertView is GET /v0/certs/{stableNodeID}.
type NodeCertView struct {
	TenantID     string            `json:"tenantID"`
	ObjectID     string            `json:"objectID"`
	StableNodeID string            `json:"stableNodeID"`
	Status       string            `json:"status"`
	Cert         string            `json:"cert"` // compact nodecert
	Payload      *nodecert.Payload `json:"payload"`
	OBOPublicKey string            `json:"oboPublicKey"`
}

// NodeCert returns the certificate of the tenant's object whose node is
// stableNodeID. A certificate that is missing (object readied before this
// mgmt version) or within an hour of expiry is re-issued and stored.
func (m *Mgmt) NodeCert(ctx context.Context, tenantID, stableNodeID string) (NodeCertView, error) {
	var v NodeCertView
	if tenantID == "" || stableNodeID == "" {
		return v, errf(400, "bad_request", "tenant and stableNodeID are required")
	}
	objs, err := m.store.ListObjects(ctx, ObjectFilter{TenantID: tenantID, StableNodeID: stableNodeID})
	if err != nil {
		return v, err
	}
	if len(objs) == 0 {
		return v, errf(404, "not_found", "no object of tenant %s has node %s", tenantID, stableNodeID)
	}
	o := objs[len(objs)-1] // the newest, should a stable ID ever be reused
	if !o.Live() {
		return v, errf(410, "killed", "object %s is %s", o.ID, o.Status)
	}
	refresh := o.Cert == ""
	if !refresh {
		if p, err := nodecert.Decode(o.Cert); err != nil || time.Unix(p.EXP, 0).Sub(m.now()) < time.Hour {
			refresh = true
		}
	}
	if refresh {
		c, err := m.signNodeCert(ctx, o)
		if err != nil {
			return v, err
		}
		o, err = m.store.UpdateObject(ctx, o.ID, nil, func(x *Object) error {
			if x.Live() {
				x.Cert = c
			}
			return nil
		})
		if err != nil {
			return v, err
		}
	}
	pub, err := m.OBOPublicKey(ctx, tenantID)
	if err != nil {
		return v, err
	}
	p, _ := nodecert.Decode(o.Cert)
	return NodeCertView{TenantID: o.TenantID, ObjectID: o.ID, StableNodeID: o.StableNodeID, Status: o.Status, Cert: o.Cert, Payload: p, OBOPublicKey: pub}, nil
}
