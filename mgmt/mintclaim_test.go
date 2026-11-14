package mgmt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStoreMintClaim: one replica at a time may mint a pending object
// (mm-loop F3, H02). Runs on SQLite and, with TWD_TEST_POSTGRES_DSN, Postgres.
func TestStoreMintClaim(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		t0 := time.Unix(1_800_000_000, 0)
		stale := func(now time.Time) time.Time { return now.Add(-MintClaimTTL) }
		if err := s.CreateObject(ctx, Object{ID: "o1", TenantID: "tn", Workload: "w", Namespace: "A", Name: "o1", Hostname: "o1",
			Status: StatusPending}); err != nil {
			t.Fatal(err)
		}
		claim := func(who string, now time.Time) bool {
			t.Helper()
			ok, err := s.ClaimMint(ctx, "o1", who, now, stale(now))
			if err != nil {
				t.Fatal(err)
			}
			return ok
		}
		if !claim("A", t0) {
			t.Fatal("A: first claim on an unclaimed pending object must succeed")
		}
		if claim("B", t0.Add(30*time.Second)) {
			t.Fatal("B: must not take a fresh claim held by A")
		}
		if !claim("A", t0.Add(30*time.Second)) {
			t.Fatal("A: must be able to refresh its own claim")
		}
		if claim("B", t0.Add(80*time.Second)) {
			t.Fatal("B: A's refreshed claim (t0+30 s) is still fresh at t0+80 s")
		}
		if !claim("B", t0.Add(91*time.Second)) {
			t.Fatal("B: must take over a claim older than MintClaimTTL")
		}
		if err := s.ReleaseMint(ctx, "o1", "A"); err != nil { // not A's any more: no-op
			t.Fatal(err)
		}
		if claim("C", t0.Add(92*time.Second)) {
			t.Fatal("A's release must not drop B's claim")
		}
		if err := s.ReleaseMint(ctx, "o1", "B"); err != nil {
			t.Fatal(err)
		}
		if !claim("C", t0.Add(93*time.Second)) {
			t.Fatal("C: a released claim must be free")
		}
		// Minted (key set) or no longer pending: nobody can claim.
		if _, err := s.UpdateObject(ctx, "o1", nil, func(o *Object) error {
			o.Status, o.KeyID = StatusMinted, "k1"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if claim("C", t0.Add(94*time.Second)) || claim("D", t0.Add(500*time.Second)) {
			t.Fatal("a minted object must not be claimable")
		}
	})
}

// Custody names are per key, so concurrent mints can't overwrite or delete
// each other's entry; objects minted before keyed names keep the legacy one.
func TestCustodyNamesPerKey(t *testing.T) {
	o := Object{TenantID: "tn", ID: "o1"}
	if got := custodyName(o); got != "twd/tn/o1/authkey" {
		t.Fatalf("unminted/legacy name = %q", got)
	}
	o.KeyID = "k7"
	if a, b := custodyName(o), custodyKeyName(o, "k8"); a == b || a != "twd/tn/o1/authkey/k7" {
		t.Fatalf("keyed names must differ per key: %q vs %q", a, b)
	}
}

// TestStoreDeviceDelete (mm-loop F10, H07): a killed object's device delete
// is durable; only one claimer runs it, not before it is due, and a retry
// makes it due again.
func TestStoreDeviceDelete(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		if err := s.CreateObject(ctx, Object{ID: "dd1", TenantID: "tn", Workload: "w", Namespace: "A", Name: "dd1", Hostname: "dd1",
			Status: StatusKilled}); err != nil {
			t.Fatal(err)
		}
		const t0 = int64(1_800_000_000_000)
		if ids, _ := s.DueDeviceDeletes(ctx, t0); len(ids) != 0 {
			t.Fatalf("nothing scheduled yet, got %v", ids)
		}
		s.SetDeviceDelete(ctx, "dd1", t0+8000)
		if ok, _ := s.ClaimDeviceDelete(ctx, "dd1", t0); ok {
			t.Fatal("claimed before it was due")
		}
		if ids, _ := s.DueDeviceDeletes(ctx, t0+8000); len(ids) != 1 {
			t.Fatalf("due at t0+8s: %v", ids)
		}
		if ok, _ := s.ClaimDeviceDelete(ctx, "dd1", t0+9000); !ok {
			t.Fatal("first claimer after due must win")
		}
		if ok, _ := s.ClaimDeviceDelete(ctx, "dd1", t0+9000); ok {
			t.Fatal("second claimer must lose")
		}
		if ids, _ := s.DueDeviceDeletes(ctx, t0+99000); len(ids) != 0 {
			t.Fatalf("claimed delete must not be due again: %v", ids)
		}
		s.SetDeviceDelete(ctx, "dd1", t0+39000) // failed: retry later
		if ok, _ := s.ClaimDeviceDelete(ctx, "dd1", t0+39000); !ok {
			t.Fatal("a rescheduled delete must be claimable when due")
		}
	})
}

// TestStoreCreateChildBudget (mm-loop F14, H04): concurrent spawns under one
// root never exceed maxLive, on SQLite and Postgres.
func TestStoreCreateChildBudget(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		root := "root-" + strings.ReplaceAll(t.Name(), "/", "-")
		if err := s.CreateObject(ctx, Object{ID: root, TenantID: "tn", Workload: "w", Namespace: "A", Name: root, Hostname: root,
			RootID: root, Status: StatusReady}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		ok, full := 0, 0
		for i := range 40 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := fmt.Sprintf("%s-c%02d", root, i)
				err := s.CreateChild(ctx, Object{ID: id, TenantID: "tn", Workload: "w", Namespace: "B", Name: id, Hostname: id,
					RootID: root, ParentID: root, Depth: 1, Status: StatusPending}, 5)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					ok++
				case errors.Is(err, ErrSpawnBudget):
					full++
				default:
					t.Errorf("child %d: %v", i, err)
				}
			}()
		}
		wg.Wait()
		if ok != 5 || full != 35 {
			t.Fatalf("inserted %d, refused %d; want exactly 5 inserted and 35 refused", ok, full)
		}
	})
}

// TestStoreTenantQuota (mm-loop F17, H16): concurrent creates never exceed
// the tenant's maxObjects, on SQLite and Postgres.
func TestStoreTenantQuota(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		tid := "tnq-" + strings.ReplaceAll(t.Name(), "/", "-")
		if err := s.CreateTenant(ctx, Tenant{ID: tid, Tailnet: tid + ".ts.net", MintMode: "apikey", MaxObjects: 7, CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		ok, full := 0, 0
		for i := range 40 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := fmt.Sprintf("%s-o%02d", tid, i)
				err := s.CreateObjectChecked(ctx, Object{ID: id, TenantID: tid, Workload: "w", Namespace: "A", Name: id, Hostname: id,
					RootID: id, Status: StatusPending}, 7, 0)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					ok++
				case errors.Is(err, ErrQuota):
					full++
				default:
					t.Errorf("object %d: %v", i, err)
				}
			}()
		}
		wg.Wait()
		if ok != 7 || full != 33 {
			t.Fatalf("inserted %d, refused %d; want exactly 7 inserted and 33 refused", ok, full)
		}
	})
}

// TestStoreDeviceDeleteClaimLapses (H21, soak seed 101): a claimed device
// delete whose outcome was never recorded (claimer died, or the store blipped
// so neither "done" nor "retry" could be written) becomes due again after
// DeviceDeleteClaimTTL; a recorded success (-1) never does.
func TestStoreDeviceDeleteClaimLapses(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		id := "ddl-" + strings.ReplaceAll(t.Name(), "/", "-")
		if err := s.CreateObject(ctx, Object{ID: id, TenantID: "tn", Workload: "w", Namespace: "A", Name: id, Hostname: id, Status: StatusKilled}); err != nil {
			t.Fatal(err)
		}
		const t0 = int64(1_800_000_000_000)
		ttl := DeviceDeleteClaimTTL.Milliseconds()
		s.SetDeviceDelete(ctx, id, t0)
		if ok, _ := s.ClaimDeviceDelete(ctx, id, t0); !ok {
			t.Fatal("claim when due")
		}
		// No outcome recorded (crash / store blip).
		if ids, _ := s.DueDeviceDeletes(ctx, t0+ttl-1000); len(ids) != 0 {
			t.Fatalf("still claimed before the TTL: %v", ids)
		}
		if ids, _ := s.DueDeviceDeletes(ctx, t0+ttl+1000); len(ids) != 1 {
			t.Fatalf("an unrecorded claim must lapse and be due again after %v: %v", DeviceDeleteClaimTTL, ids)
		}
		if ok, _ := s.ClaimDeviceDelete(ctx, id, t0+ttl+1000); !ok {
			t.Fatal("a lapsed claim must be claimable")
		}
		s.SetDeviceDelete(ctx, id, -1) // success recorded
		if ids, _ := s.DueDeviceDeletes(ctx, t0+100*ttl); len(ids) != 0 {
			t.Fatalf("a completed delete must never be due again: %v", ids)
		}
	})
}
