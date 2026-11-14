package mgmt

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

var t0 = time.UnixMilli(1_800_000_000_000)

func cand(id string, free int, tiers ...string) Candidate {
	if len(tiers) == 0 {
		tiers = []string{"process"}
	}
	return Candidate{ID: id, Tiers: tiers, Live: true, Free: map[string]int{"nodes": free}}
}

func inPool(c Candidate, p string) Candidate { c.Pool = p; return c }

func meas(host, target, path string, rttMs float64, age time.Duration) PathMeasurement {
	return PathMeasurement{HostID: host, Target: target, Path: path, RTTMicros: int64(rttMs * 1000),
		MeasuredAt: t0.Add(-age).UnixMilli(), Peer: "db"}
}

func TestScoreHosts(t *testing.T) {
	db := []string{"tag:db"}
	dead := cand("h9", 500)
	dead.Live = false
	draining := cand("h8", 500)
	draining.Draining = true
	pooled := cand("h7", 500)
	pooled.Pool = "gpu"
	for _, tc := range []struct {
		name    string
		cands   []Candidate
		ms      []PathMeasurement
		req     Requirements
		want    string
		reason  string
		summary string // substring
	}{
		{name: "direct beats relayed beats unmeasured",
			cands: []Candidate{cand("h1", 100), cand("h2", 10), cand("h3", 1000)},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDERP, 24, 0), meas("h2", "tag:db", PathDirect, 1.8, 0)},
			req:   Requirements{Targets: db}, want: "h2", reason: "smart",
			summary: "placed on h2: tag:db direct 1.8 ms (h1: relayed 24 ms, h3: unmeasured)"},
		{name: "relayed beats unmeasured",
			cands: []Candidate{cand("h1", 1), cand("h3", 1000)},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDERP, 80, 0)},
			req:   Requirements{Targets: db}, want: "h1", reason: "smart"},
		{name: "peer relay between direct and DERP",
			cands: []Candidate{cand("h1", 1), cand("h2", 1)},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDERP, 1, 0), meas("h2", "tag:db", PathPeerRelay, 30, 0)},
			req:   Requirements{Targets: db}, want: "h2"},
		{name: "RTT orders the same class",
			cands: []Candidate{cand("h1", 1000), cand("h2", 1)},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDirect, 9, 0), meas("h2", "tag:db", PathDirect, 3, 0)},
			req:   Requirements{Targets: db}, want: "h2"},
		{name: "equal paths: capacity breaks the tie",
			cands: []Candidate{cand("h1", 5), cand("h2", 50)},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDirect, 2, 0), meas("h2", "tag:db", PathDirect, 2, 0)},
			req:   Requirements{Targets: db}, want: "h2"},
		{name: "full tie: host ID",
			cands: []Candidate{cand("hb", 5), cand("ha", 5)},
			req:   Requirements{}, want: "ha", reason: "capacity"},
		{name: "no targets: most free capacity",
			cands: []Candidate{cand("h1", 5), cand("h2", 50)},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDirect, 2, 0)},
			req:   Requirements{}, want: "h2", reason: "capacity"},
		{name: "mode off ignores measurements",
			cands: []Candidate{cand("h1", 5), cand("h2", 50)},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDirect, 2, 0)},
			req:   Requirements{Mode: PlacementOff, Targets: db}, want: "h2", reason: "capacity"},
		{name: "stale measurements are ignored",
			cands: []Candidate{cand("h1", 5), cand("h2", 50)},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDirect, 2, PathTTL+time.Second), meas("h2", "tag:db", PathDERP, 40, time.Minute)},
			req:   Requirements{Targets: db}, want: "h2", summary: "h1: stale"},
		{name: "all stale: capacity",
			cands: []Candidate{cand("h1", 5), cand("h2", 50)},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDirect, 2, time.Hour)},
			req:   Requirements{Targets: db}, want: "h2", reason: "capacity", summary: "no measured path"},
		{name: "unresolved and unreachable count as unmeasured",
			cands: []Candidate{cand("h1", 500), cand("h2", 600), cand("h3", 1)},
			ms: []PathMeasurement{meas("h1", "tag:db", PathUnresolved, 0, 0), meas("h2", "tag:db", PathUnreachable, 0, 0),
				meas("h3", "tag:db", PathDERP, 99, 0)},
			req: Requirements{Targets: db}, want: "h3", summary: "h1: unresolved"},
		{name: "dead, draining and other-pool hosts excluded",
			cands: []Candidate{dead, draining, pooled, inPool(cand("h1", 1), "main"), inPool(dead, "main"), inPool(draining, "main")},
			ms:    []PathMeasurement{meas("h9", "tag:db", PathDirect, 1, 0), meas("h8", "tag:db", PathDirect, 1, 0), meas("h7", "tag:db", PathDirect, 1, 0)},
			req:   Requirements{Pool: "main", Targets: db}, want: "h1", summary: "h7: ineligible: pool"},
		{name: "pool requirement",
			cands: []Candidate{pooled, cand("h1", 1000)},
			req:   Requirements{Pool: "gpu"}, want: "h7"},
		{name: "microvm tier excludes process-only hosts",
			cands: []Candidate{cand("h1", 1000), cand("h2", 1, "process", "microvm")},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDirect, 1, 0)},
			req:   Requirements{Tiers: []string{"microvm"}, Targets: db}, want: "h2"},
		{name: "microvm tier, none offered",
			cands: []Candidate{cand("h1", 1000)},
			req:   Requirements{Tiers: []string{"microvm"}}, want: "", summary: "no live host with tier microvm (h1: no microvm tier"},
		{name: "full hosts excluded",
			cands: []Candidate{cand("h1", 0), cand("h2", 1)},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDirect, 1, 0)},
			req:   Requirements{Targets: db}, want: "h2"},
		{name: "capacity units beyond nodes",
			cands: []Candidate{cand("h1", 100), {ID: "h2", Tiers: []string{"process"}, Live: true, Free: map[string]int{"nodes": 1, "vms": 2}}},
			req:   Requirements{Capacity: map[string]int{"nodes": 1, "vms": 1}}, want: "h2"},
		{name: "no live host",
			cands: []Candidate{dead}, req: Requirements{}, want: "", summary: "h9: not live"},
		{name: "pinned bypasses scoring",
			cands: []Candidate{cand("h1", 1), cand("h2", 1000)},
			ms:    []PathMeasurement{meas("h2", "tag:db", PathDirect, 1, 0)},
			req:   Requirements{Pinned: "h1", Targets: db}, want: "h1", reason: "pinned"},
		{name: "pinned must still be eligible (tier)",
			cands: []Candidate{cand("h1", 1), cand("h2", 1, "process", "microvm")},
			req:   Requirements{Pinned: "h1", Tiers: []string{"microvm"}}, want: "", summary: "pinned host h1 is not eligible"},
		{name: "sticky holder kept although another host is better",
			cands: []Candidate{cand("h1", 1), cand("h2", 1000)},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDERP, 40, 0), meas("h2", "tag:db", PathDirect, 1, 0)},
			req:   Requirements{Sticky: "h1", Targets: db}, want: "h1", reason: "sticky"},
		{name: "sticky holder kept when full",
			cands: []Candidate{cand("h1", 0), cand("h2", 1000)},
			req:   Requirements{Sticky: "h1"}, want: "h1", reason: "sticky"},
		{name: "sticky holder without the tier is not kept",
			cands: []Candidate{cand("h1", 1000), cand("h2", 1, "process", "microvm")},
			req:   Requirements{Sticky: "h1", Tiers: []string{"microvm"}}, want: "h2"},
		{name: "replace moves to a better class",
			cands: []Candidate{cand("h1", 1), cand("h2", 1)},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDERP, 40, 0), meas("h2", "tag:db", PathDirect, 30, 0)},
			req:   Requirements{Sticky: "h1", Replace: true, Targets: db}, want: "h2", reason: "smart"},
		{name: "replace hysteresis: small RTT gain stays",
			cands: []Candidate{cand("h1", 1), cand("h2", 1000)},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDirect, 3.0, 0), meas("h2", "tag:db", PathDirect, 2.0, 0)},
			req:   Requirements{Sticky: "h1", Replace: true, Targets: db}, want: "h1", reason: "sticky", summary: "no eligible host is clearly better"},
		{name: "replace: big RTT gain moves",
			cands: []Candidate{cand("h1", 1), cand("h2", 1)},
			ms:    []PathMeasurement{meas("h1", "tag:db", PathDirect, 20, 0), meas("h2", "tag:db", PathDirect, 2, 0)},
			req:   Requirements{Sticky: "h1", Replace: true, Targets: db}, want: "h2"},
		{name: "replace: capacity alone never moves",
			cands: []Candidate{cand("h1", 1), cand("h2", 1000)},
			req:   Requirements{Sticky: "h1", Replace: true}, want: "h1", reason: "sticky"},
		{name: "worst target decides",
			cands: []Candidate{cand("h1", 1), cand("h2", 1)},
			ms: []PathMeasurement{
				meas("h1", "tag:db", PathDirect, 1, 0), meas("h1", "tag:cache", PathDERP, 30, 0),
				meas("h2", "tag:db", PathDirect, 9, 0), meas("h2", "tag:cache", PathDirect, 9, 0)},
			req: Requirements{Targets: []string{"tag:db", "tag:cache"}}, want: "h2"},
		{name: "worst: one unmeasured target makes the host unmeasured",
			cands: []Candidate{cand("h1", 1), cand("h2", 1)},
			ms: []PathMeasurement{
				meas("h1", "tag:db", PathDirect, 1, 0),
				meas("h2", "tag:db", PathDERP, 9, 0), meas("h2", "tag:cache", PathDERP, 9, 0)},
			req: Requirements{Targets: []string{"tag:db", "tag:cache"}}, want: "h2"},
		{name: "sum: ranks add up",
			cands: []Candidate{cand("h1", 1), cand("h2", 1)},
			ms: []PathMeasurement{
				meas("h1", "tag:db", PathDirect, 1, 0), meas("h1", "tag:cache", PathDirect, 1, 0), meas("h1", "tag:q", PathDERP, 50, 0),
				meas("h2", "tag:db", PathDirect, 5, 0), meas("h2", "tag:cache", PathPeerRelay, 5, 0), meas("h2", "tag:q", PathDERP, 5, 0)},
			req: Requirements{Targets: []string{"tag:db", "tag:cache", "tag:q"}, Aggregate: AggregateSum}, want: "h1"},
		{name: "sum: RTTs add up within a rank",
			cands: []Candidate{cand("h1", 1), cand("h2", 1)},
			ms: []PathMeasurement{
				meas("h1", "tag:db", PathDirect, 1, 0), meas("h1", "tag:cache", PathDirect, 20, 0),
				meas("h2", "tag:db", PathDirect, 8, 0), meas("h2", "tag:cache", PathDirect, 8, 0)},
			req: Requirements{Targets: []string{"tag:db", "tag:cache"}, Aggregate: AggregateSum}, want: "h2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.Now = t0
			d := scoreHosts(tc.cands, tc.ms, tc.req)
			if d.Host != tc.want {
				t.Fatalf("host %q, want %q (%s)", d.Host, tc.want, d.Summary)
			}
			if tc.reason != "" && d.Reason != tc.reason {
				t.Errorf("reason %q, want %q (%s)", d.Reason, tc.reason, d.Summary)
			}
			if !strings.Contains(d.Summary, tc.summary) {
				t.Errorf("summary %q, want it to contain %q", d.Summary, tc.summary)
			}
		})
	}
}

// Replicas must agree: the answer doesn't depend on input order.
func TestScoreHostsDeterministic(t *testing.T) {
	cands := []Candidate{cand("h1", 10), cand("h2", 10), cand("h3", 10), cand("h4", 7)}
	ms := []PathMeasurement{meas("h1", "tag:db", PathDirect, 2, 0), meas("h2", "tag:db", PathDirect, 2, 0),
		meas("h3", "tag:db", PathDERP, 1, 0), meas("h4", "tag:db", PathDirect, 2, 0)}
	req := Requirements{Targets: []string{"tag:db"}, Now: t0}
	want := scoreHosts(cands, ms, req)
	if want.Host != "h1" {
		t.Fatalf("host %s, want h1 (%s)", want.Host, want.Summary)
	}
	r := rand.New(rand.NewSource(1))
	for range 200 {
		r.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
		r.Shuffle(len(ms), func(i, j int) { ms[i], ms[j] = ms[j], ms[i] })
		got := scoreHosts(cands, ms, req)
		if got.Host != want.Host || got.Summary != want.Summary {
			t.Fatalf("order-dependent: %s / %q vs %s / %q", got.Host, got.Summary, want.Host, want.Summary)
		}
	}
}

func TestResolvePlacement(t *testing.T) {
	parse := func(s string) *TwdConfig {
		c, err := ParseTwdConfig([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, tc := range []struct {
		in      string
		mode    string
		targets string
		warn    string
	}{
		{`{"tailnet": {"connects": ["tag:db", "*", "tag:db", "pg-1"]}}`, "smart", "tag:db,pg-1", ""},
		{`{"placement": {"mode": "off"}, "tailnet": {"connects": ["tag:db"]}}`, "off", "", ""},
		{`{"placement": {"mode": "smart", "targets": ["tag:cache"]}, "tailnet": {"connects": ["tag:db"]}}`, "smart", "tag:cache", ""},
		{`{"placement": {"host": "pg.example.ts.net:5432"}}`, "smart", "pg.example.ts.net", ""},
		{`{"placement": {"hostname": "api.internal"}}`, "smart", "api.internal", ""},
		{`{"placement": {"region": "aws:us-east-1"}, "tailnet": {"connects": ["tag:db"]}}`, "smart", "tag:db", "region is ignored"},
		{`{"placement": {"mode": "fast"}}`, "smart", "", "not \"smart\" or \"off\""},
	} {
		pc := resolvePlacement(parse(tc.in))
		if pc.Mode != tc.mode || strings.Join(pc.Targets, ",") != tc.targets {
			t.Errorf("%s: mode %q targets %v", tc.in, pc.Mode, pc.Targets)
		}
		if w := strings.Join(pc.Warnings, ";"); !strings.Contains(w, tc.warn) || (tc.warn == "" && w != "") {
			t.Errorf("%s: warnings %q, want %q", tc.in, w, tc.warn)
		}
	}
}
