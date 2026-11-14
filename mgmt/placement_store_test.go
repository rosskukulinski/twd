package mgmt

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestLeaseMove: MoveLease is a CAS on (holder, epoch). Concurrent moves of
// the same lease have one winner, the epoch goes up by one, and the old
// holder can no longer renew (I1: one holder per lease, always).
func TestLeaseMove(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.UnixMilli(1_000_000)
		l, err := s.AcquireLease(ctx, "tn/mv", "h1", 45*time.Second, now)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wins := make(chan Lease, 8)
		for _, to := range []string{"h2", "h3", "h4", "h5", "h2", "h3", "h4", "h5"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if nl, err := s.MoveLease(ctx, "tn/mv", "h1", l.Epoch, to, 45*time.Second, now); err == nil {
					wins <- nl
				} else if !errors.Is(err, ErrLeaseLost) {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		close(wins)
		var got []Lease
		for w := range wins {
			got = append(got, w)
		}
		if len(got) != 1 || got[0].Epoch != l.Epoch+1 {
			t.Fatalf("moves won: %+v, want exactly one at epoch %d", got, l.Epoch+1)
		}
		if _, err := s.RenewLease(ctx, "tn/mv", "h1", l.Epoch, 45*time.Second, now); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("old holder renewed after the move: %v", err)
		}
		if _, err := s.RenewLease(ctx, "tn/mv", got[0].Holder, got[0].Epoch, 45*time.Second, now); err != nil {
			t.Fatalf("new holder can't renew: %v", err)
		}
		if _, err := s.MoveLease(ctx, "tn/none", "h1", 1, "h2", time.Second, now); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("move of a missing lease: %v", err)
		}
	})
}

// TestStoreHostPaths: per-(host, tenant) replace, tenant isolation,
// retention, host delete.
func TestStoreHostPaths(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.UnixMilli(5_000_000_000)
		at := now.UnixMilli()
		for _, h := range []string{"h1", "h2"} {
			if err := s.UpsertHost(ctx, Host{ID: h, CapacityNodes: 10, Tiers: []string{"process"}, LastSeen: at, CreatedAt: at}); err != nil {
				t.Fatal(err)
			}
		}
		put := func(h, tn string, ms ...PathMeasurement) {
			t.Helper()
			if err := s.PutHostPaths(ctx, h, tn, ms, now); err != nil {
				t.Fatal(err)
			}
		}
		put("h1", "tA", PathMeasurement{Target: "tag:db", Path: PathDERP, RTTMicros: 24000, DERPRegion: "nyc", MeasuredAt: at, Peer: "pg"},
			PathMeasurement{Target: "tag:cache", Path: PathDirect, RTTMicros: 900, MeasuredAt: at})
		put("h2", "tA", PathMeasurement{Target: "tag:db", Path: PathDirect, RTTMicros: 1800, MeasuredAt: at, Peer: "pg"})
		put("h2", "tB", PathMeasurement{Target: "tag:db", Path: PathDirect, RTTMicros: 1, MeasuredAt: at})
		got, err := s.HostPaths(ctx, "tA", []string{"tag:db"})
		if err != nil || len(got) != 2 || got[0].HostID != "h1" || got[0].Path != PathDERP || got[0].DERPRegion != "nyc" || got[1].RTTMicros != 1800 || got[1].TenantID != "tA" {
			t.Fatalf("paths: %+v %v", got, err)
		}
		// A new report replaces the host's rows for that tenant only.
		put("h1", "tA", PathMeasurement{Target: "tag:db", Path: PathDirect, RTTMicros: 3000, MeasuredAt: at})
		if got, _ := s.HostPaths(ctx, "tA", []string{"tag:cache"}); len(got) != 0 {
			t.Fatalf("tag:cache survived the replace: %+v", got)
		}
		if got, _ := s.HostPaths(ctx, "tB", []string{"tag:db", "tag:cache"}); len(got) != 1 || got[0].HostID != "h2" {
			t.Fatalf("tenant B: %+v", got)
		}
		// Rows past retention go when the host reports again (any tenant).
		put("h2", "tB", PathMeasurement{Target: "tag:db", Path: PathDirect, RTTMicros: 1, MeasuredAt: now.Add(-2 * pathRetention).UnixMilli()})
		put("h2", "tC")
		if got, _ := s.HostPaths(ctx, "tB", []string{"tag:db"}); len(got) != 0 {
			t.Fatalf("expired row kept: %+v", got)
		}
		if err := s.DeleteHost(ctx, "h2"); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.HostPaths(ctx, "tA", []string{"tag:db"}); len(got) != 1 || got[0].HostID != "h1" {
			t.Fatalf("after host delete: %+v", got)
		}
		// The placement decision round-trips with the deployment.
		d := Deployment{ID: "dp_pl", TenantID: "tA", Workload: "w", HostID: "h1", Epoch: 1, CreatedAt: at,
			Placement: &PlacementDecision{Host: "h1", Reason: "smart", Mode: PlacementSmart, Targets: []string{"tag:db"}, Summary: "placed on h1"}}
		if err := s.CreateDeployment(ctx, d); err != nil {
			t.Fatal(err)
		}
		g, err := s.GetDeployment(ctx, "dp_pl")
		if err != nil || g.Placement == nil || g.Placement.Summary != "placed on h1" || g.Placement.Targets[0] != "tag:db" {
			t.Fatalf("placement round trip: %+v %v", g.Placement, err)
		}
		ls, _ := s.LatestDeployments(ctx, "tA")
		if len(ls) != 1 || ls[0].Placement == nil || ls[0].Placement.Reason != "smart" {
			t.Fatalf("latest: %+v", ls)
		}
	})
}
