package mgmt

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/internal/container"
)

// TestStoreContainerReservations (ADR-032 D9), on SQLite and Postgres: the
// quota holds in the transaction that inserts, a key that holds a
// reservation is renewed without new headroom (the restart exception), only
// the lease holder reserves, a stale host's reservations stop counting, and
// heartbeat reconciliation renews, releases and adopts.
func TestStoreContainerReservations(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		tn := fmt.Sprintf("tn_c%d", time.Now().UnixNano()%1e9)
		if err := s.CreateTenant(ctx, Tenant{ID: tn, Tailnet: "t.ts.net", MintMode: MintAPIKey, RegistrationsPerMinute: 1, MaxObjects: 1, CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		now := time.UnixMilli(5_000_000)
		l, err := s.AcquireLease(ctx, LeaseName(tn, "w"), "h1", time.Hour, now)
		if err != nil {
			t.Fatal(err)
		}
		fence := &Fence{Lease: LeaseName(tn, "w"), Holder: "h1", Epoch: l.Epoch}
		basic := container.InstanceTypes["basic"] // 0.25 vCPU, 1024 MiB
		q := ContainerQuota{MaxContainers: 3, MaxMemMiB: 2560, MaxMicroVCPU: 1_000_000, ClassMax: 0, StaleBefore: 0}
		res := func(obj string, r container.Resources, at int64) ContainerReservation {
			return ContainerReservation{ID: newID("cr_"), TenantID: tn, Workload: "w", Class: "Sandbox", Object: obj, HostID: "h1", Resources: r, CreatedAt: at, RenewedAt: at}
		}
		quotaErr := func(err error) string {
			var qe *ContainerQuotaError
			if errors.As(err, &qe) {
				return qe.Quota
			}
			return fmt.Sprint(err)
		}
		a, created, err := s.ReserveContainer(ctx, res("a", basic, 100), q, fence)
		if err != nil || !created {
			t.Fatalf("reserve a: %v %v", created, err)
		}
		if _, _, err := s.ReserveContainer(ctx, res("b", basic, 101), q, fence); err != nil {
			t.Fatal(err)
		}
		// Memory: 2048 used + 1024 > 2560.
		if _, _, err := s.ReserveContainer(ctx, res("c", basic, 102), q, fence); quotaErr(err) != "maxContainerMemMiB" {
			t.Fatalf("over memory: %v", err)
		}
		// The restart exception: the same key again, at quota, is renewed.
		again, created, err := s.ReserveContainer(ctx, res("a", basic, 200), q, fence)
		if err != nil || created || again.ID != a.ID || again.RenewedAt != 200 || again.CreatedAt != 100 {
			t.Fatalf("re-reserve a at quota: %+v created=%v %v", again, created, err)
		}
		// A smaller restart keeps what it holds; a larger one is re-checked.
		if r, _, err := s.ReserveContainer(ctx, res("a", container.InstanceTypes["lite"], 201), q, fence); err != nil || r.Resources != basic {
			t.Fatalf("smaller restart: %+v %v", r, err)
		}
		if _, _, err := s.ReserveContainer(ctx, res("a", container.Resources{VCPU: 0.25, MemMiB: 2048, DiskMiB: 1}, 202), q, fence); quotaErr(err) != "maxContainerMemMiB" {
			t.Fatalf("grown restart over memory: %v", err)
		}
		// vCPU (fractional, summed in micro-vCPU) and count.
		lite := container.InstanceTypes["lite"]
		if _, _, err := s.ReserveContainer(ctx, res("c", lite, 103), q, fence); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.ReserveContainer(ctx, res("d", lite, 104), q, fence); quotaErr(err) != "maxContainers" {
			t.Fatalf("4th container: %v", err)
		}
		qv := q
		qv.MaxContainers, qv.MaxMemMiB, qv.MaxMicroVCPU = 10, 1<<20, 600_000 // 0.5625 used
		if _, _, err := s.ReserveContainer(ctx, res("d", basic, 104), qv, fence); quotaErr(err) != "maxContainerVCPU" {
			t.Fatalf("over vCPU: %v", err)
		}
		qc := q
		qc.MaxContainers, qc.MaxMemMiB, qc.ClassMax = 10, 1<<20, 3
		if _, _, err := s.ReserveContainer(ctx, res("d", lite, 104), qc, fence); quotaErr(err) != "max_instances" {
			t.Fatalf("over max_instances: %v", err)
		}
		// Only the lease holder at its epoch.
		for _, f := range []*Fence{{Lease: fence.Lease, Holder: "h2", Epoch: l.Epoch}, {Lease: fence.Lease, Holder: "h1", Epoch: l.Epoch + 1}} {
			r := res("e", lite, 105)
			r.HostID = f.Holder
			if _, _, err := s.ReserveContainer(ctx, r, qc, f); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("fence %+v: %v", f, err)
			}
		}
		// Reservations not renewed since StaleBefore don't count.
		qs := q
		qs.StaleBefore = 150 // b (101) and c (103) are stale; a (renewed at 201) counts
		if _, _, err := s.ReserveContainer(ctx, res("e", basic, 300), qs, fence); err != nil {
			t.Fatalf("stale reservations still count: %v", err)
		}

		// Reconcile h1: it reports a and an unknown x (adoptable) and y (not);
		// b, c, e are unreported; e is younger than releaseBefore.
		running := []ContainerReservation{res("a", basic, 0), res("x", lite, 0), res("y", lite, 0)}
		adopted, released, err := s.ReconcileContainers(ctx, "h1", running, 400, 250,
			func(r ContainerReservation) bool { return r.Object != "y" })
		if err != nil {
			t.Fatal(err)
		}
		if len(adopted) != 1 || adopted[0].Object != "x" || adopted[0].HostID != "h1" {
			t.Fatalf("adopted %+v", adopted)
		}
		got := map[string]bool{}
		for _, r := range released {
			got[r.Object] = true
		}
		if len(released) != 2 || !got["b"] || !got["c"] {
			t.Fatalf("released %+v", released)
		}
		rs, err := s.ListContainerReservations(ctx, ContainerFilter{TenantID: tn}, Page{})
		if err != nil {
			t.Fatal(err)
		}
		left := map[string]int64{}
		for _, r := range rs {
			left[r.Object] = r.RenewedAt
		}
		if len(left) != 3 || left["a"] != 400 || left["x"] != 400 || left["e"] != 300 {
			t.Fatalf("after reconcile: %v", left)
		}
		// Another host never adopts (or renews) a key h1 holds.
		adopted, released, err = s.ReconcileContainers(ctx, "h2", []ContainerReservation{res("a", basic, 0)}, 500, 450,
			func(ContainerReservation) bool { return true })
		if err != nil || len(adopted)+len(released) != 0 {
			t.Fatalf("h2 reconcile: %v %v %v", adopted, released, err)
		}
		if r, _ := s.GetContainerReservation(ctx, a.ID); r.HostID != "h1" || r.RenewedAt != 400 {
			t.Fatalf("h2 touched h1's reservation: %+v", r)
		}
		// Release: only by the holder.
		if err := s.ReleaseContainer(ctx, a.ID, "h2"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("release by h2: %v", err)
		}
		if err := s.ReleaseContainer(ctx, a.ID, "h1"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetContainerReservation(ctx, a.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("released reservation still there: %v", err)
		}
		// Removing the host drops its reservations.
		s.UpsertHost(ctx, Host{ID: "h1", LastSeen: 1})
		if err := s.DeleteHost(ctx, "h1"); err != nil {
			t.Fatal(err)
		}
		if rs, _ := s.ListContainerReservations(ctx, ContainerFilter{HostID: "h1"}, Page{}); len(rs) != 0 {
			t.Fatalf("host removed, reservations left: %+v", rs)
		}
	})
}

// TestStoreContainerQuotaConcurrent: parallel reservations of distinct
// objects never exceed maxContainers (the check and the insert are one
// transaction, serialised on the tenant row).
func TestStoreContainerQuotaConcurrent(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		tn := fmt.Sprintf("tn_q%d", time.Now().UnixNano()%1e9)
		if err := s.CreateTenant(ctx, Tenant{ID: tn, Tailnet: "t.ts.net", MintMode: MintAPIKey, RegistrationsPerMinute: 1, MaxObjects: 1, CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		l, _ := s.AcquireLease(ctx, LeaseName(tn, "w"), "h1", time.Hour, time.UnixMilli(1))
		fence := &Fence{Lease: LeaseName(tn, "w"), Holder: "h1", Epoch: l.Epoch}
		q := ContainerQuota{MaxContainers: 3, MaxMemMiB: 1 << 20, MaxMicroVCPU: 1 << 30}
		var wg sync.WaitGroup
		var mu sync.Mutex
		ok, other := 0, []error{}
		for i := range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := ContainerReservation{ID: newID("cr_"), TenantID: tn, Workload: "w", Class: "S", Object: fmt.Sprint("o", i), HostID: "h1",
					Resources: container.InstanceTypes["lite"], CreatedAt: 1, RenewedAt: 1}
				var err error
				for range 20 { // SQLITE_BUSY / serialization failures are retryable
					if _, _, err = s.ReserveContainer(ctx, r, q, fence); err == nil || !isStoreUnavailable(err) {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				mu.Lock()
				defer mu.Unlock()
				var qe *ContainerQuotaError
				switch {
				case err == nil:
					ok++
				case !errors.As(err, &qe):
					other = append(other, err)
				}
			}()
		}
		wg.Wait()
		if ok != 3 || len(other) > 0 {
			t.Fatalf("%d reservations succeeded (want 3); other errors: %v", ok, other)
		}
	})
}
