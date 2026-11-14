package mgmt

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func h22World(t *testing.T) (*Mgmt, *SQLStore, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := OpenStore(ctx, filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	kf := filepath.Join(dir, "api.env")
	os.WriteFile(kf, []byte("TS_API_KEY=x\n"), 0o600)
	tn, _ := m.CreateTenant(ctx, TenantRequest{Tailnet: "t.ts.net", MintMode: "apikey", APIKeyFile: kf})
	return m, st, tn.ID
}

func killRecords(t *testing.T, st *SQLStore, tenant, obj string) int {
	recs, err := st.ListAudit(context.Background(), AuditFilter{TenantID: tenant, Kinds: []string{"kill"}})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range recs {
		if r.ObjectID == obj {
			n++
		}
	}
	return n
}

// TestMM_H22_KillAndAuditCommitTogether: if the audit intent can't be
// written, the kill doesn't happen (no state change without its record).
func TestMM_H22_KillAndAuditCommitTogether(t *testing.T) {
	ctx := context.Background()
	m, st, tid := h22World(t)
	st.CreateObject(ctx, Object{ID: "o1", TenantID: tid, Workload: "w", Namespace: "A", Name: "o1", Hostname: "o1", RootID: "o1", Status: StatusReady})
	st.db.Exec("ALTER TABLE audit_outbox RENAME TO audit_outbox_off")
	res, _ := m.Kill(ctx, "o1", KillRequest{Reason: "t"})
	if o, _ := st.GetObject(ctx, "o1"); o.Status != StatusReady || len(res.Killed) != 0 {
		t.Fatalf("with the audit outbox unavailable the kill must not happen: status %s, killed %v", o.Status, res.Killed)
	}
	st.db.Exec("ALTER TABLE audit_outbox_off RENAME TO audit_outbox")
	if _, err := m.Kill(ctx, "o1", KillRequest{Reason: "t"}); err != nil {
		t.Fatal(err)
	}
	if o, _ := st.GetObject(ctx, "o1"); o.Status != StatusKilled || killRecords(t, st, tid, "o1") != 1 {
		t.Fatalf("after recovery: status %s, kill records %d; want killed and 1", o.Status, killRecords(t, st, tid, "o1"))
	}
}

// TestMM_H22_DeferredAppendIsNotLost: the kill commits with its intent; the
// chain append fails (store blip); the drain appends it later, once.
func TestMM_H22_DeferredAppendIsNotLost(t *testing.T) {
	ctx := context.Background()
	m, st, tid := h22World(t)
	st.CreateObject(ctx, Object{ID: "o2", TenantID: tid, Workload: "w", Namespace: "A", Name: "o2", Hostname: "o2", RootID: "o2", Status: StatusReady})
	st.db.Exec("ALTER TABLE audit RENAME TO audit_off")
	if _, err := m.Kill(ctx, "o2", KillRequest{Reason: "blip"}); err != nil {
		t.Fatal(err)
	}
	st.db.Exec("ALTER TABLE audit_off RENAME TO audit")
	if o, _ := st.GetObject(ctx, "o2"); o.Status != StatusKilled {
		t.Fatalf("the kill should have committed: %s", o.Status)
	}
	if n := killRecords(t, st, tid, "o2"); n != 0 {
		t.Fatalf("the chain append failed, so no record yet; got %d", n)
	}
	ids, _ := st.ListAuditIntents(ctx, time.Now().Add(time.Hour).UnixMilli(), 10)
	if len(ids) == 0 {
		t.Fatal("the kill's audit intent must be waiting in the outbox")
	}
	m.drainAuditOutbox(ctx) // only intents older than 3 s by default: force
	for _, id := range ids {
		st.DrainAuditIntent(ctx, id)
	}
	if n := killRecords(t, st, tid, "o2"); n != 1 {
		t.Fatalf("after the drain: %d kill records, want exactly 1", n)
	}
	if left, _ := st.ListAuditIntents(ctx, time.Now().Add(time.Hour).UnixMilli(), 10); len(left) != 0 {
		t.Fatalf("outbox not empty after drain: %v", left)
	}
	if cs := m.VerifyChain(ctx, "mgmt"); !cs.OK {
		t.Fatalf("chain must verify after a deferred append: %+v", cs)
	}
}

// A withdrawn intent (the update was a no-op) never reaches the outbox.
func TestMM_H22_SkippedIntentNotRecorded(t *testing.T) {
	ctx := context.Background()
	m, st, tid := h22World(t)
	st.CreateObject(ctx, Object{ID: "o3", TenantID: tid, Workload: "w", Namespace: "A", Name: "o3", Hostname: "o3", RootID: "o3", Status: StatusKilled})
	actx, ai := m.intend(ctx, AuditRecord{TenantID: tid, ObjectID: "o3", Kind: "mint"}, nil)
	st.UpdateObject(actx, "o3", nil, func(o *Object) error { ai.Skip(); return nil })
	m.recordIntent(ctx, ai, nil)
	if ids, _ := st.ListAuditIntents(ctx, time.Now().Add(time.Hour).UnixMilli(), 10); len(ids) != 0 {
		t.Fatalf("a skipped intent was written: %v", ids)
	}
	recs, _ := st.ListAudit(ctx, AuditFilter{TenantID: tid, Kinds: []string{"mint"}})
	if len(recs) != 0 {
		t.Fatalf("a skipped intent reached the chain: %d", len(recs))
	}
}
