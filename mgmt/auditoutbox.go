package mgmt

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Durable audit (H22, ADR option b). An audit record that describes a state
// change is written as an *intent* row in audit_outbox inside the same store
// transaction as the change: the change and its record commit together, or
// neither does. After commit the intent is appended to the hash chain and
// deleted from the outbox in one transaction (exactly once across replicas);
// intents left behind (store blip, crash) are drained by every replica's
// reaper tick.

// auditIntent is one pending audit record carried in a context to the store
// call that makes the change it describes.
type auditIntent struct {
	ID      string
	Rec     AuditRecord
	mu      sync.Mutex
	skip    bool // the change turned out to be a no-op: don't record it
	written bool // committed to audit_outbox
}

// Skip withdraws the intent (call it from an update function that decides
// not to change anything).
func (ai *auditIntent) Skip() {
	if ai == nil {
		return
	}
	ai.mu.Lock()
	ai.skip = true
	ai.mu.Unlock()
}

type auditCtxKey struct{}

// pendingIntents returns the intents attached to ctx that still need writing.
func pendingIntents(ctx context.Context) []*auditIntent {
	l, _ := ctx.Value(auditCtxKey{}).([]*auditIntent)
	var out []*auditIntent
	for _, ai := range l {
		ai.mu.Lock()
		if !ai.skip && !ai.written {
			out = append(out, ai)
		}
		ai.mu.Unlock()
	}
	return out
}

// withoutIntents strips intents from ctx (for store calls that must not
// flush them, e.g. the chain append itself).
func withoutIntents(ctx context.Context) context.Context {
	return context.WithValue(ctx, auditCtxKey{}, []*auditIntent(nil))
}

// intend prepares an audit record for the state change made by the store
// call that receives the returned context. Pass that context ONLY to that
// call. After it returns successfully, call m.recordIntent.
func (m *Mgmt) intend(ctx context.Context, r AuditRecord, detail map[string]any) (context.Context, *auditIntent) {
	r.Chain = "mgmt"
	r.TS = m.now().UTC().Format(time.RFC3339Nano)
	if detail != nil {
		r.Detail, _ = json.Marshal(detail)
	}
	ai := &auditIntent{ID: newID("aud_"), Rec: r}
	l, _ := ctx.Value(auditCtxKey{}).([]*auditIntent)
	return context.WithValue(ctx, auditCtxKey{}, append(append([]*auditIntent(nil), l...), ai)), ai
}

// recordIntent finishes an intent after its change committed: extra detail
// (known only after the change, e.g. a kill's push results) is merged into
// the outbox row, then the record is appended to the chain. If anything here
// fails the intent stays in the outbox and the drain appends it later (with
// the detail it had). A skipped or unwritten intent is ignored.
func (m *Mgmt) recordIntent(ctx context.Context, ai *auditIntent, extra map[string]any) {
	if ai == nil {
		return
	}
	ai.mu.Lock()
	written := ai.written && !ai.skip
	ai.mu.Unlock()
	if !written {
		return
	}
	ctx = withoutIntents(context.WithoutCancel(ctx))
	if len(extra) > 0 {
		var d map[string]any
		json.Unmarshal(ai.Rec.Detail, &d)
		if d == nil {
			d = map[string]any{}
		}
		for k, v := range extra {
			d[k] = v
		}
		ai.Rec.Detail, _ = json.Marshal(d)
		if err := m.store.AmendAuditIntent(ctx, ai.ID, ai.Rec); err != nil {
			m.log.Warn("audit intent amend failed (base record kept)", "kind", ai.Rec.Kind, "object", ai.Rec.ObjectID, "err", err)
		}
	}
	if _, err := m.store.DrainAuditIntent(ctx, ai.ID); err != nil {
		m.log.Warn("audit append deferred (kept in the outbox)", "kind", ai.Rec.Kind, "object", ai.Rec.ObjectID, "err", err)
	}
}

// audit records an event that is not bound to a state change of its own
// (a refusal, or an admin change made outside an object transaction). It
// goes through the outbox, so a failed chain append is retried by the drain.
func (m *Mgmt) audit(ctx context.Context, r AuditRecord, detail map[string]any) {
	_, ai := m.intend(ctx, r, detail)
	ctx = withoutIntents(ctx)
	if err := m.store.PutAuditIntent(ctx, ai.ID, ai.Rec); err != nil {
		m.log.Error("audit record lost: the store is unavailable", "kind", r.Kind, "object", r.ObjectID, "err", err)
		return
	}
	if _, err := m.store.DrainAuditIntent(ctx, ai.ID); err != nil {
		m.log.Warn("audit append deferred (kept in the outbox)", "kind", r.Kind, "object", r.ObjectID, "err", err)
	}
}

// drainAuditOutbox appends every intent older than a few seconds (younger
// ones are being finished by the request that wrote them).
func (m *Mgmt) drainAuditOutbox(ctx context.Context) {
	ctx = withoutIntents(ctx)
	ids, err := m.store.ListAuditIntents(ctx, m.now().Add(-3*time.Second).UnixMilli(), 1000)
	if err != nil {
		m.log.Warn("audit outbox drain", "err", err)
		return
	}
	for _, id := range ids {
		if _, err := m.store.DrainAuditIntent(ctx, id); err != nil {
			m.log.Warn("audit outbox drain", "intent", id, "err", err)
			return
		}
	}
}
