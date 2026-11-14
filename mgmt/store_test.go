package mgmt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// storesUnderTest returns SQLite always, plus Postgres when
// TWD_TEST_POSTGRES_DSN is set (`make test-pg` starts one in OrbStack).
func storesUnderTest(t *testing.T) map[string]*SQLStore {
	t.Helper()
	ctx := context.Background()
	out := map[string]*SQLStore{}
	s, err := OpenStore(ctx, filepath.Join(t.TempDir(), "mgmt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	out["sqlite"] = s
	if dsn := os.Getenv("TWD_TEST_POSTGRES_DSN"); dsn != "" {
		p, err := OpenStore(ctx, dsn)
		if err != nil {
			t.Fatalf("postgres: %v", err)
		}
		// Fresh schema per test: drop everything and migrate again.
		for _, tbl := range []string{"tenants", "hosts", "deployments", "objects", "leases", "audit", "audit_heads", "revocations", "container_reservations"} {
			if err := p.rawExec(ctx, "DROP TABLE IF EXISTS "+tbl+" CASCADE"); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.migrate(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Close() })
		out["postgres"] = p
	}
	return out
}

func forEachStore(t *testing.T, fn func(t *testing.T, s *SQLStore)) {
	for name, s := range storesUnderTest(t) {
		t.Run(name, func(t *testing.T) { fn(t, s) })
	}
}

func TestLeaseFencing(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.UnixMilli(1_000_000)
		l1, err := s.AcquireLease(ctx, "tn/app", "h1", 15*time.Second, now)
		if err != nil || l1.Epoch != 1 || l1.Holder != "h1" {
			t.Fatalf("acquire: %+v %v", l1, err)
		}
		// Another holder can't take an unexpired lease.
		if _, err := s.AcquireLease(ctx, "tn/app", "h2", 15*time.Second, now.Add(time.Second)); !errors.Is(err, ErrLeaseHeld) {
			t.Fatalf("h2 took held lease: %v", err)
		}
		// Renew at the right epoch works.
		if _, err := s.RenewLease(ctx, "tn/app", "h1", 1, 15*time.Second, now.Add(5*time.Second)); err != nil {
			t.Fatal(err)
		}
		// After expiry h2 takes over with epoch+1.
		l2, err := s.AcquireLease(ctx, "tn/app", "h2", 15*time.Second, now.Add(30*time.Second))
		if err != nil || l2.Epoch != 2 || l2.Holder != "h2" {
			t.Fatalf("takeover: %+v %v", l2, err)
		}
		// The old holder is fenced everywhere: renew, release, and commits.
		if _, err := s.RenewLease(ctx, "tn/app", "h1", 1, 15*time.Second, now.Add(31*time.Second)); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("stale renew: %v", err)
		}
		if err := s.ReleaseLease(ctx, "tn/app", "h1", 1); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("stale release: %v", err)
		}
		// h2 at a wrong epoch is stale too.
		if _, err := s.RenewLease(ctx, "tn/app", "h2", 1, 15*time.Second, now.Add(31*time.Second)); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("wrong-epoch renew: %v", err)
		}
		o := Object{ID: "o1", TenantID: "tn", Namespace: "Room", Name: "a", Hostname: "room-a", Lease: "tn/app",
			Tags: []string{"tag:x"}, RootID: "o1", Status: StatusMinted, CreatedAt: 1, UpdatedAt: 1}
		if err := s.CreateObject(ctx, o); err != nil {
			t.Fatal(err)
		}
		_, err = s.UpdateObject(ctx, "o1", &Fence{Lease: "tn/app", Holder: "h1", Epoch: 1}, func(o *Object) error {
			o.Status = StatusReady
			return nil
		})
		if !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("stale fenced commit: %v", err)
		}
		if got, _ := s.GetObject(ctx, "o1"); got.Status != StatusMinted {
			t.Fatalf("stale commit was applied: %s", got.Status)
		}
		if _, err := s.UpdateObject(ctx, "o1", &Fence{Lease: "tn/app", Holder: "h2", Epoch: 2}, func(o *Object) error {
			o.Status = StatusReady
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		// Release keeps epochs monotonic: the next acquire is epoch 3.
		if err := s.ReleaseLease(ctx, "tn/app", "h2", 2); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RenewLease(ctx, "tn/app", "h2", 2, time.Second, now.Add(32*time.Second)); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("renew after release: %v", err)
		}
		l3, err := s.AcquireLease(ctx, "tn/app", "h1", 15*time.Second, now.Add(33*time.Second))
		if err != nil || l3.Epoch != 3 {
			t.Fatalf("after release: %+v %v", l3, err)
		}
	})
}

func TestLeaseAcquireRace(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now()
		var wg sync.WaitGroup
		var mu sync.Mutex
		winners := 0
		for i := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.AcquireLease(ctx, "tn/race", fmt.Sprintf("h%d", i), time.Minute, now); err == nil {
					mu.Lock()
					winners++
					mu.Unlock()
				} else if !errors.Is(err, ErrLeaseHeld) {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if winners != 1 {
			t.Fatalf("%d holders acquired the same lease", winners)
		}
	})
}

func TestAuditChainTamper(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		for i := range 5 {
			if _, err := s.AppendAudit(ctx, AuditRecord{Chain: "mgmt", TS: fmt.Sprint(i), Kind: "spawn", ObjectID: fmt.Sprintf("o%d", i)}); err != nil {
				t.Fatal(err)
			}
		}
		if st := VerifyChain(ctx, s, "mgmt", nil); !st.OK || st.Records != 5 {
			t.Fatalf("clean chain: %+v", st)
		}
		// Append-only at the store: UPDATE and DELETE abort.
		if err := s.rawExec(ctx, "UPDATE audit SET kind='allow' WHERE seq=3"); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("update allowed: %v", err)
		}
		if err := s.rawExec(ctx, "DELETE FROM audit WHERE seq=5"); err == nil {
			t.Fatal("delete allowed")
		}
		// An attacker with DDL rights drops the guard and edits a record: the
		// chain detects it.
		dropGuards(t, s)
		if err := s.rawExec(ctx, `UPDATE audit SET raw=REPLACE(raw, '"kind":"spawn"', '"kind":"allow"') WHERE seq=3`); err != nil {
			t.Fatal(err)
		}
		st := VerifyChain(ctx, s, "mgmt", nil)
		if st.OK || st.BrokenAt != 3 {
			t.Fatalf("modified record not detected: %+v", st)
		}
	})
}

func TestAuditChainTruncateAndRelink(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		for i := range 4 {
			s.AppendAudit(ctx, AuditRecord{Chain: "mgmt", TS: fmt.Sprint(i), Kind: "kill"})
		}
		dropGuards(t, s)
		// Deleting the tail is caught by the head.
		s.rawExec(ctx, "DELETE FROM audit WHERE seq=4")
		if st := VerifyChain(ctx, s, "mgmt", nil); st.OK {
			t.Fatalf("truncation not detected: %+v", st)
		}
		// Deleting a middle record is a sequence gap.
		s.rawExec(ctx, "DELETE FROM audit WHERE seq=2")
		if st := VerifyChain(ctx, s, "mgmt", nil); st.OK || st.BrokenAt != 3 {
			t.Fatalf("gap not detected: %+v", st)
		}
	})
}

func dropGuards(t *testing.T, s *SQLStore) {
	ctx := context.Background()
	var err error
	if s.d == dialectPostgres {
		err = s.rawExec(ctx, "DROP TRIGGER audit_append_only ON audit")
	} else {
		err = errors.Join(s.rawExec(ctx, "DROP TRIGGER audit_no_update"), s.rawExec(ctx, "DROP TRIGGER audit_no_delete"))
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestStoreObjectsAndDeployments(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		if err := s.CreateTenant(ctx, Tenant{ID: "tn", Tailnet: "t.ts.net", MintMode: MintAPIKey, RegistrationsPerMinute: 200, MaxObjects: 10}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateTenant(ctx, Tenant{ID: "tn", Tailnet: "x", MintMode: MintAPIKey}); !errors.Is(err, ErrExists) {
			t.Fatalf("dup tenant: %v", err)
		}
		for i, ns := range [][]ClassConfig{{{Class: "A", Tags: []string{"tag:a"}}}, {{Class: "A", Tags: []string{"tag:a2"}}, {Class: "B", Tags: []string{"tag:b"}}}} {
			if err := s.CreateDeployment(ctx, Deployment{ID: fmt.Sprintf("d%d", i), TenantID: "tn", Workload: "w", HostID: "h1",
				Namespaces: ns, Bundle: []byte{1, 2, 3}, CreatedAt: int64(i + 1)}); err != nil {
				t.Fatal(err)
			}
		}
		d, c, err := s.FindClass(ctx, "tn", "", "A")
		if err != nil || d.ID != "d1" || c.Tags[0] != "tag:a2" {
			t.Fatalf("FindClass: %+v %+v %v", d, c, err)
		}
		if _, _, err := s.FindClass(ctx, "tn", "", "Z"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if got, err := s.GetDeployment(ctx, "d0"); err != nil || len(got.Bundle) != 3 {
			t.Fatal(got, err)
		}
		if as, _ := s.Assignments(ctx, "h1"); len(as) != 0 {
			t.Fatal("assignment without lease")
		}
		s.AcquireLease(ctx, LeaseName("tn", "w"), "h1", time.Minute, time.Now())
		if as, _ := s.Assignments(ctx, "h1"); len(as) != 1 || as[0].ID != "d1" || as[0].Epoch != 1 {
			t.Fatalf("assignments %+v", as)
		}
		mk := func(id, parent, root string, st string) {
			if err := s.CreateObject(ctx, Object{ID: id, TenantID: "tn", Workload: "w", Namespace: "A", Name: id, Hostname: id, ParentID: parent,
				RootID: root, Status: st, Tags: []string{"tag:a"}}); err != nil {
				t.Fatal(err)
			}
		}
		mk("r", "", "r", StatusReady)
		mk("c1", "r", "r", StatusReady)
		mk("c2", "r", "r", StatusKilled)
		mk("g1", "c1", "r", StatusPending)
		if err := s.CreateObject(ctx, Object{ID: "dup", TenantID: "tn", Workload: "w", Namespace: "A", Name: "r"}); !errors.Is(err, ErrExists) {
			t.Fatalf("dup name: %v", err)
		}
		if n, _ := s.CountLive(ctx, "tn", "r"); n != 2 {
			t.Fatalf("live descendants %d", n)
		}
		if n, _ := s.CountLive(ctx, "tn", ""); n != 3 {
			t.Fatalf("live %d", n)
		}
		kids, _ := s.ListObjects(ctx, ObjectFilter{ParentID: "r"})
		if len(kids) != 2 {
			t.Fatal(kids)
		}
		if err := s.AddRevocation(ctx, "h1", "c2", time.Now()); err != nil {
			t.Fatal(err)
		}
		s.AddRevocation(ctx, "h1", "c2", time.Now())
		if rv, _ := s.Revocations(ctx, "h1", time.Now().Add(-time.Minute)); len(rv) != 1 {
			t.Fatal(rv)
		}
	})
}
