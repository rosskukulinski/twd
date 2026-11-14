package mgmt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestMM_H09_ConcurrentKillOnce: every replica's reaper (or a reaper and an
// operator) may kill the same object at once. Exactly one kill must win: one
// kill audit record per object, and the tombstone keeps the winner's record.
func TestMM_H09_ConcurrentKillOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := OpenStore(ctx, filepath.Join(dir, "m.db"))
	defer st.Close()
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	mk := func() *Mgmt {
		m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
		return m
	}
	a, b := mk(), mk()
	kf := filepath.Join(dir, "api.env")
	os.WriteFile(kf, []byte("TS_API_KEY=x\n"), 0o600)
	tn, _ := a.CreateTenant(ctx, TenantRequest{Tailnet: "t.ts.net", MintMode: "apikey", APIKeyFile: kf})
	const n = 20
	for i := range n {
		id := fmt.Sprintf("obj%02d", i)
		if err := st.CreateObject(ctx, Object{ID: id, TenantID: tn.ID, Workload: "w", Namespace: "A", Name: id, Hostname: id, RootID: id,
			Status: StatusReady, TTLSeconds: 1, CreatedAt: 1}); err != nil { // long expired
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for _, m := range []*Mgmt{a, b, a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); <-gate; m.Reap(ctx) }()
	}
	close(gate)
	wg.Wait()
	recs, _ := st.ListAudit(ctx, AuditFilter{TenantID: tn.ID, Kinds: []string{"kill"}})
	per := map[string]int{}
	for _, r := range recs {
		per[r.ObjectID]++
	}
	dup := 0
	for i := range n {
		id := fmt.Sprintf("obj%02d", i)
		o, _ := st.GetObject(ctx, id)
		if o.Status != StatusKilled {
			t.Errorf("%s not killed: %s", id, o.Status)
		}
		if per[id] != 1 {
			dup++
		}
	}
	if dup > 0 {
		t.Fatalf("%d/%d objects got more than one kill record (concurrent reapers both killed them)", dup, n)
	}
}

// TestMM_H15_ThrottleFollowsTenantLimit: a changed registrationsPerMinute
// reaches an existing throttle (the mint worker re-reads the tenant on every
// attempt and asks m.throttle for it).
func TestMM_H15_ThrottleFollowsTenantLimit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := OpenStore(ctx, filepath.Join(dir, "m.db"))
	defer st.Close()
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	m, _ := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour})
	defer m.Close()
	tn := Tenant{ID: "tn_x", RegistrationsPerMinute: 60}
	if r := m.throttle(tn).Rate(); r != 60 {
		t.Fatalf("rate %v, want 60", r)
	}
	tn.RegistrationsPerMinute = 6
	if r := m.throttle(tn).Rate(); r != 6 {
		t.Fatalf("after lowering the tenant's limit to 6/min the throttle still allows %v/min", r)
	}
}

// TestMM_F21_DeviceDeleteIntentIsAtomic (soak seed 101): the device delete
// of a killed object is recorded in the kill's own write, and a node that
// joins after its object was killed is recorded in Ready's write, so a store
// blip after the state change can't lose the cleanup. The broker here has no
// API, so the inline delete fails: the intent must still be persisted.
func TestMM_F21_DeviceDeleteIntentIsAtomic(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := OpenStore(ctx, filepath.Join(dir, "m.db"))
	defer st.Close()
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	kf := filepath.Join(dir, "api.env")
	os.WriteFile(kf, []byte("TS_API_KEY=x\n"), 0o600)
	tn, _ := m.CreateTenant(ctx, TenantRequest{Tailnet: "t.ts.net", MintMode: "apikey", APIKeyFile: kf})
	// A ready object with a device, killed.
	st.CreateObject(ctx, Object{ID: "rdy", TenantID: tn.ID, Workload: "w", Namespace: "A", Name: "rdy", Hostname: "rdy", RootID: "rdy",
		Status: StatusReady, NodeID: "1", StableNodeID: "nRDY"})
	if _, err := m.Kill(ctx, "rdy", KillRequest{Reason: "t"}); err != nil {
		t.Fatal(err)
	}
	if o, _ := st.GetObject(ctx, "rdy"); o.DeviceDeleteAt == 0 {
		t.Fatal("kill of an object with a device must record its device delete in the same write")
	}
	// An object killed while minted (no device yet); its node then reports ready.
	st.CreateObject(ctx, Object{ID: "late", TenantID: tn.ID, Workload: "w", Namespace: "A", Name: "late", Hostname: "late", RootID: "late",
		Status: StatusMinted})
	m.Kill(ctx, "late", KillRequest{Reason: "t"})
	m.Ready(ctx, "late", "", ReadyRequest{NodeID: "2", StableNodeID: "nLATE", MagicDNS: "late.t.ts.net"})
	if o, _ := st.GetObject(ctx, "late"); o.StableNodeID != "nLATE" || o.DeviceDeleteAt == 0 {
		t.Fatalf("a device that appears after the kill must be recorded for deletion: %+v", o)
	}
	if ids, _ := st.DueDeviceDeletes(ctx, time.Now().Add(time.Hour).UnixMilli()); len(ids) != 2 {
		t.Fatalf("both device deletes must be pending for the sweep (inline delete failed): %v", ids)
	}
}
