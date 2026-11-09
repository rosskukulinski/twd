package hostd

import (
	"context"
	"crypto/ed25519"
	"sync"
	"time"

	"github.com/rosskukulinski/twd/hostd/mgmtapi"
)

// auditLog is this host's hash-chained audit log (contract POST /v1/audit):
// each record carries prevHash/hash (mgmtapi.ChainHash) and hostSig
// (ed25519 with the host's audit key, registered as auditPubKey). Pending records are flushed to mgmt
// with every heartbeat; on failure they stay queued (bounded).
type auditLog struct {
	mu      sync.Mutex
	key     ed25519.PrivateKey
	prev    string
	pending []mgmtapi.AuditRecord
}

const maxPendingAudit = 10000

func newAuditLog(key ed25519.PrivateKey) *auditLog { return &auditLog{key: key} }

// Audit sources (RT-02). Every record this host signs says who produced
// its content; the field is part of the hashed body.
const (
	sourceHost   = "host"
	sourceTenant = "tenant"
	// kindTenantNote is the only kind tenant code can produce.
	kindTenantNote = "tenant_note"
)

func (a *auditLog) add(r mgmtapi.AuditRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.TS == "" {
		r.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if r.Source == "" {
		r.Source = sourceHost
	}
	if r.Source == sourceTenant && r.Kind != kindTenantNote {
		r.Kind = kindTenantNote // defence in depth: never a privileged kind from tenant content
	}
	r.PrevHash = a.prev
	r.Hash = mgmtapi.ChainHash(a.prev, r)
	r.HostSig = mgmtapi.HostSig(a.key, r.Hash)
	a.prev = r.Hash
	a.pending = append(a.pending, r)
	if len(a.pending) > maxPendingAudit {
		a.pending = a.pending[len(a.pending)-maxPendingAudit:]
	}
}

func (h *Host) flushAudit(ctx context.Context) {
	a := h.audit
	a.mu.Lock()
	recs := a.pending
	a.pending = nil
	a.mu.Unlock()
	if len(recs) == 0 {
		return
	}
	if err := h.mgmt.Audit(ctx, recs); err != nil {
		a.mu.Lock()
		a.pending = append(recs, a.pending...)
		a.mu.Unlock()
		h.log.Warn("audit flush failed (will retry)", "records", len(recs), "err", err)
	}
}
