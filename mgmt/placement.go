package mgmt

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// Smart placement (ADR-030 draft, Proposed): when a workload declares what it
// connects to (twd.jsonc tailnet.connects, or placement.targets), mgmt
// prefers the eligible host with the best measured tailnet path to those
// targets: direct WireGuard over a peer relay over DERP, then lower RTT.
// Capacity breaks ties. Placement only chooses; the workload lease decides
// (AcquireLease / MoveLease), so a placement disagreement between replicas
// can never produce two holders.
//
// scoreHosts is pure: the same candidates, measurements and requirements
// give the same answer on every replica (stable order, host ID last).

// Path classes, worst to best. A measurement that is missing, stale,
// unresolved or unreachable counts as unmeasured.
const (
	PathUnmeasured = "unmeasured"
	PathDERP       = "derp"
	PathPeerRelay  = "peer-relay"
	PathDirect     = "direct"
	// Reported by hosts, scored as unmeasured.
	PathUnresolved  = "unresolved"  // no peer in the netmap matches the target
	PathUnreachable = "unreachable" // resolved, but no ping answered
)

// pathRank orders path classes (higher is better).
func pathRank(p string) int {
	switch p {
	case PathDirect:
		return 3
	case PathPeerRelay:
		return 2
	case PathDERP:
		return 1
	}
	return 0
}

// Placement modes (twd.jsonc placement.mode).
const (
	PlacementSmart = "smart"
	PlacementOff   = "off"
)

// Placement aggregates over several targets.
const (
	AggregateWorst = "worst" // the worst target decides (default)
	AggregateSum   = "sum"   // sum of ranks, then sum of RTTs
)

// Placement limits: they bound what hosts report and what mgmt stores.
const (
	MaxPlacementTargets = 16              // targets per workload (extra ones are dropped, in order)
	MaxPathTargets      = 32              // targets per (host, tenant) report
	MaxPathTenants      = 64              // tenants per host report
	PathTTL             = 5 * time.Minute // measurements older than this are ignored
	pathRetention       = time.Hour       // rows older than this are deleted when the host reports again
)

// Hysteresis for an explicit re-place (`twd deploy --replace`): the
// current holder is kept unless the best host is in a better path class, or
// the same class with an RTT at least replaceMinGain lower and
// replaceMinRatio of the holder's. Measurement noise does not move
// workloads.
const (
	replaceMinGain  = 2 * time.Millisecond
	replaceMinRatio = 0.8
)

// PathMeasurement is one host's measured path to one target of one tenant
// (stored per (host, tenant, target); every replica reads the same rows).
type PathMeasurement struct {
	HostID   string `json:"hostID"`
	TenantID string `json:"tenantID"`
	Target   string `json:"target"`
	// Peer is the netmap peer the measurement is for (the best one, when a
	// tag matches several).
	Peer       string `json:"peer,omitempty"`
	Path       string `json:"path"`
	RTTMicros  int64  `json:"rttMicros"`
	DERPRegion string `json:"derpRegion,omitempty"`
	// MeasuredAt is unix ms on the store clock (mgmt computes it from the
	// host's reported age, so host clocks never matter).
	MeasuredAt int64 `json:"measuredAt"`
}

// Candidate is a host as placement sees it.
type Candidate struct {
	ID       string
	Pool     string
	Tiers    []string
	Live     bool
	Draining bool
	// Free capacity per unit ("nodes" = capacityNodes - liveNodes; future
	// units such as "vms" or "hugepageMiB" come from the host's
	// registration). A unit the host doesn't report counts as 0.
	Free map[string]int
}

// candidateOf maps a stored host.
func candidateOf(h Host, now time.Time) Candidate {
	tiers := h.Tiers
	if len(tiers) == 0 {
		tiers = []string{"process"} // hosts that predate tier reporting
	}
	return Candidate{ID: h.ID, Pool: h.Pool, Tiers: tiers, Live: h.Live(now),
		Free: map[string]int{"nodes": h.CapacityNodes - h.LiveNodes}}
}

// Requirements is what a placement must satisfy and optimize.
type Requirements struct {
	// Eligibility.
	Pool     string         // "" = any pool
	Tiers    []string       // the host must offer every one
	Capacity map[string]int // free units needed (nil = {"nodes": 1})
	// Pinned bypasses scoring: an explicit --host, or a sandbox that must
	// run next to its parent DO. The pinned host must still be eligible.
	Pinned string
	// Sticky is the current live lease holder: kept if it is still eligible
	// (capacity aside: it already holds its share). "" = none.
	Sticky string
	// Replace ignores Sticky unless the best host is not clearly better
	// (hysteresis): `twd deploy --replace`.
	Replace bool
	// Path scoring. Empty Targets (or Mode off) = capacity only.
	Mode      string
	Targets   []string
	Aggregate string
	Now       time.Time
	MaxAge    time.Duration // 0 = PathTTL
}

// TargetScore is one host's path to one target, as scored.
type TargetScore struct {
	Target     string  `json:"target"`
	Path       string  `json:"path"` // a path class; "unmeasured" also covers stale/unresolved/unreachable
	Detail     string  `json:"detail,omitempty"`
	RTTMs      float64 `json:"rttMs,omitempty"`
	Peer       string  `json:"peer,omitempty"`
	DERPRegion string  `json:"derpRegion,omitempty"`
	AgeSeconds int64   `json:"ageSeconds,omitempty"`
}

// HostScore is one candidate's evaluation.
type HostScore struct {
	Host     string        `json:"host"`
	Eligible bool          `json:"eligible"`
	Why      string        `json:"why,omitempty"` // why not eligible
	Free     int           `json:"free"`          // free nodes
	Path     string        `json:"path,omitempty"`
	RTTMs    float64       `json:"rttMs,omitempty"`
	Targets  []TargetScore `json:"targets,omitempty"`
	rank     int           // aggregate path rank
	rtt      int64         // aggregate RTT (µs)
	measured bool
}

// PlacementDecision is why a deployment is on its host. It is stored with
// the deployment, returned by deploy and GET /v1/deployments/{id}, and
// summarized in the deploy audit record.
type PlacementDecision struct {
	Host string `json:"host"`
	// Reason: pinned | sticky | smart | capacity.
	Reason    string      `json:"reason"`
	Mode      string      `json:"mode"`
	Targets   []string    `json:"targets,omitempty"`
	Aggregate string      `json:"aggregate,omitempty"`
	Summary   string      `json:"summary"`
	Hosts     []HostScore `json:"hosts,omitempty"`
}

// scoreHosts filters cands by req and ranks the eligible ones. It never
// returns an ineligible host; Host is "" when none is eligible (Summary says
// why).
func scoreHosts(cands []Candidate, ms []PathMeasurement, req Requirements) PlacementDecision {
	if req.Aggregate == "" {
		req.Aggregate = AggregateWorst
	}
	if req.Mode == "" {
		req.Mode = PlacementSmart
	}
	maxAge := req.MaxAge
	if maxAge == 0 {
		maxAge = PathTTL
	}
	need := req.Capacity
	if need == nil {
		need = map[string]int{"nodes": 1}
	}
	targets := req.Targets
	if req.Mode == PlacementOff {
		targets = nil
	}
	d := PlacementDecision{Mode: req.Mode, Targets: targets}
	if len(targets) > 0 {
		d.Aggregate = req.Aggregate
	}
	// Measurements by (host, target).
	type hk struct{ host, target string }
	byHT := map[hk]PathMeasurement{}
	for _, m := range ms {
		byHT[hk{m.HostID, m.Target}] = m
	}
	cands = slices.Clone(cands)
	sort.Slice(cands, func(i, j int) bool { return cands[i].ID < cands[j].ID })
	var scores []HostScore
	for _, c := range cands {
		s := HostScore{Host: c.ID, Free: c.Free["nodes"]}
		s.Why = ineligible(c, req, need, c.ID == req.Sticky)
		s.Eligible = s.Why == ""
		if s.Eligible && len(targets) > 0 {
			scoreTargets(&s, targets, func(t string) (PathMeasurement, bool) { m, ok := byHT[hk{c.ID, t}]; return m, ok }, req.Now, maxAge, req.Aggregate)
		}
		scores = append(scores, s)
	}
	elig := []HostScore{}
	for _, s := range scores {
		if s.Eligible {
			elig = append(elig, s)
		}
	}
	sort.SliceStable(elig, func(i, j int) bool { return better(elig[i], elig[j]) })
	// The decision table lists eligible hosts best first, then the others.
	for _, s := range scores {
		if !s.Eligible {
			elig = append(elig, s)
		}
	}
	d.Hosts = elig
	find := func(id string) (HostScore, bool) {
		for _, s := range d.Hosts {
			if s.Host == id {
				return s, true
			}
		}
		return HostScore{}, false
	}
	switch {
	case req.Pinned != "":
		s, ok := find(req.Pinned)
		if !ok {
			d.Summary = fmt.Sprintf("pinned host %s is not registered", req.Pinned)
			return d
		}
		if !s.Eligible {
			d.Summary = fmt.Sprintf("pinned host %s is not eligible: %s", req.Pinned, s.Why)
			return d
		}
		d.Host, d.Reason = s.Host, "pinned"
		d.Summary = "pinned to " + s.Host + d.pathSuffix(s)
		return d
	case req.Sticky != "" && !req.Replace:
		if s, ok := find(req.Sticky); ok && s.Eligible {
			d.Host, d.Reason = s.Host, "sticky"
			d.Summary = "kept on " + s.Host + " (current lease holder; `twd deploy --replace` re-places it)" + d.pathSuffix(s)
			return d
		}
	}
	if len(d.Hosts) == 0 || !d.Hosts[0].Eligible {
		d.Summary = noHostSummary(d.Hosts, req)
		return d
	}
	best := d.Hosts[0]
	if req.Sticky != "" && req.Replace {
		if cur, ok := find(req.Sticky); ok && cur.Eligible && !clearlyBetter(best, cur) {
			d.Host, d.Reason = cur.Host, "sticky"
			d.Summary = "kept on " + cur.Host + ": no eligible host is clearly better" + d.pathSuffix(cur) + d.others(cur.Host)
			return d
		}
	}
	d.Host = best.Host
	if best.measured {
		d.Reason = "smart"
		d.Summary = "placed on " + best.Host + ": " + targetsText(best) + d.others(best.Host)
	} else {
		d.Reason = "capacity"
		why := "most free capacity"
		if len(targets) > 0 {
			why = "no measured path, most free capacity"
		}
		d.Summary = fmt.Sprintf("placed on %s: %s (%d free)", best.Host, why, best.Free) + d.others(best.Host)
	}
	return d
}

// ineligible returns why c can't take the workload ("" = eligible).
// holder: c is the current lease holder, which is exempt from the capacity
// check (it already holds the workload's share).
func ineligible(c Candidate, req Requirements, need map[string]int, holder bool) string {
	switch {
	case !c.Live:
		return "not live"
	case c.Draining:
		return "draining"
	case req.Pool != "" && c.Pool != req.Pool:
		return fmt.Sprintf("pool %q, want %q", c.Pool, req.Pool)
	}
	for _, t := range req.Tiers {
		if !slices.Contains(c.Tiers, t) {
			return fmt.Sprintf("no %s tier (offers %s)", t, strings.Join(c.Tiers, ","))
		}
	}
	if holder {
		return ""
	}
	units := make([]string, 0, len(need))
	for u := range need {
		units = append(units, u)
	}
	sort.Strings(units)
	for _, u := range units {
		if n := need[u]; n > 0 && c.Free[u] < n {
			return fmt.Sprintf("no free capacity (%s: %d free, need %d)", u, c.Free[u], n)
		}
	}
	return ""
}

func scoreTargets(s *HostScore, targets []string, get func(string) (PathMeasurement, bool), now time.Time, maxAge time.Duration, agg string) {
	minRank, sumRank := 3, 0
	var maxRTT, sumRTT int64
	for _, t := range targets {
		ts := TargetScore{Target: t, Path: PathUnmeasured}
		m, ok := get(t)
		if ok {
			age := now.Sub(time.UnixMilli(m.MeasuredAt))
			ts.AgeSeconds, ts.Peer = int64(age/time.Second), m.Peer
			switch {
			case age > maxAge:
				ts.Detail = fmt.Sprintf("stale (%v old)", age.Round(time.Second))
			case pathRank(m.Path) == 0:
				ts.Detail = m.Path
			default:
				ts.Path, ts.RTTMs, ts.DERPRegion = m.Path, float64(m.RTTMicros)/1000, m.DERPRegion
				sumRTT += m.RTTMicros
				maxRTT = max(maxRTT, m.RTTMicros)
			}
		}
		r := pathRank(ts.Path)
		minRank, sumRank = min(minRank, r), sumRank+r
		s.Targets = append(s.Targets, ts)
	}
	if agg == AggregateSum {
		s.rank, s.rtt = sumRank, sumRTT
	} else {
		s.rank, s.rtt = minRank, maxRTT
	}
	s.measured = s.rank > 0
	s.Path = rankPath(minRank)
	if s.measured {
		s.RTTMs = float64(s.rtt) / 1000
	}
}

func rankPath(r int) string {
	switch r {
	case 3:
		return PathDirect
	case 2:
		return PathPeerRelay
	case 1:
		return PathDERP
	}
	return PathUnmeasured
}

// better is the total order on eligible hosts: path rank (higher first),
// then RTT (lower first; only between measured hosts), then free capacity
// (more first), then host ID.
func better(a, b HostScore) bool {
	if a.rank != b.rank {
		return a.rank > b.rank
	}
	if a.measured && b.measured && a.rtt != b.rtt {
		return a.rtt < b.rtt
	}
	if a.Free != b.Free {
		return a.Free > b.Free
	}
	return a.Host < b.Host
}

// clearlyBetter: best beats cur by enough to justify a move.
func clearlyBetter(best, cur HostScore) bool {
	if best.Host == cur.Host {
		return false
	}
	if best.rank != cur.rank {
		return best.rank > cur.rank
	}
	if !best.measured || !cur.measured {
		return false // same (unmeasured) class: capacity alone never moves a workload
	}
	gain := time.Duration(cur.rtt-best.rtt) * time.Microsecond
	return gain >= replaceMinGain && float64(best.rtt) <= replaceMinRatio*float64(cur.rtt)
}

func targetsText(s HostScore) string {
	parts := make([]string, 0, len(s.Targets))
	for _, t := range s.Targets {
		parts = append(parts, t.Target+" "+pathText(t))
	}
	return strings.Join(parts, ", ")
}

func pathText(t TargetScore) string {
	switch t.Path {
	case PathUnmeasured:
		if t.Detail != "" {
			return t.Detail
		}
		return "unmeasured"
	case PathDERP:
		s := "relayed"
		if t.DERPRegion != "" {
			s += " via DERP " + t.DERPRegion
		}
		return s + " " + fmtMs(t.RTTMs)
	}
	return t.Path + " " + fmtMs(t.RTTMs)
}

// hostText is one host's path in the "(h1: …, h3: …)" list.
func hostText(s HostScore) string {
	if !s.Eligible {
		return "ineligible: " + s.Why
	}
	if len(s.Targets) == 1 {
		return pathText(s.Targets[0])
	}
	if len(s.Targets) == 0 {
		return fmt.Sprintf("%d free", s.Free)
	}
	if !s.measured {
		return "unmeasured"
	}
	return targetsText(s)
}

func fmtMs(ms float64) string {
	if ms < 10 {
		return fmt.Sprintf("%.1f ms", ms)
	}
	return fmt.Sprintf("%.0f ms", ms)
}

func (d PlacementDecision) pathSuffix(s HostScore) string {
	if len(s.Targets) == 0 {
		return ""
	}
	return " (" + targetsText(s) + ")"
}

// others lists the other hosts' paths, e.g. " (h1: relayed 24 ms, h3: unmeasured)".
func (d PlacementDecision) others(chosen string) string {
	var parts []string
	for _, s := range d.Hosts {
		if s.Host != chosen {
			parts = append(parts, s.Host+": "+hostText(s))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	if len(parts) > 6 {
		parts = append(parts[:6], fmt.Sprintf("+%d more", len(parts)-6))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func noHostSummary(hosts []HostScore, req Requirements) string {
	if len(hosts) == 0 {
		return "no live host (register twd-hostd and heartbeat first)"
	}
	var parts []string
	for _, s := range hosts {
		parts = append(parts, s.Host+": "+s.Why)
	}
	want, fix := "no eligible host", ""
	if len(req.Tiers) > 0 {
		want = "no live host with tier " + strings.Join(req.Tiers, "+")
		if slices.Contains(req.Tiers, "microvm") {
			fix = "; set one up with `sudo twd host setup --microvm`, or deploy with \"tier\": \"process\""
		}
	}
	return want + " (" + strings.Join(parts, "; ") + ")" + fix
}

// PlacementConfig is a workload's resolved placement configuration.
type PlacementConfig struct {
	Mode      string   `json:"mode"`
	Targets   []string `json:"targets,omitempty"`
	Aggregate string   `json:"aggregate,omitempty"`
	// Warnings are config fields twd accepts but ignores (e.g. a
	// Cloudflare "region" hint).
	Warnings []string `json:"warnings,omitempty"`
}

// resolvePlacement turns twd.jsonc's placement block (or, failing that,
// wrangler's) and tailnet.connects into the targets to score. Smart is the
// default whenever the workload declares connects: stickiness makes it
// safe (a running workload never moves on its own).
func resolvePlacement(cfg *TwdConfig) PlacementConfig {
	pc := PlacementConfig{Mode: PlacementSmart, Aggregate: AggregateWorst}
	if cfg == nil {
		return pc
	}
	p := cfg.Placement
	switch strings.ToLower(p.Mode) {
	case "", PlacementSmart:
	case PlacementOff:
		pc.Mode = PlacementOff
	default:
		pc.Warnings = append(pc.Warnings, fmt.Sprintf("placement.mode %q is not \"smart\" or \"off\"; using smart", p.Mode))
	}
	if p.Aggregate == AggregateSum {
		pc.Aggregate = AggregateSum
	} else if p.Aggregate != "" && p.Aggregate != AggregateWorst {
		pc.Warnings = append(pc.Warnings, fmt.Sprintf("placement.aggregate %q is not \"worst\" or \"sum\"; using worst", p.Aggregate))
	}
	if p.Region != "" {
		pc.Warnings = append(pc.Warnings, "placement.region is ignored: twd places by measured tailnet path, not cloud region")
	}
	var ts []string
	switch {
	case len(p.Targets) > 0:
		ts = p.Targets
	case p.Host != "" || p.Hostname != "":
		// Cloudflare's placement.host ("db.example:5432") / hostname hints
		// name one target; on a tailnet that is a peer name.
		h := p.Host
		if h == "" {
			h = p.Hostname
		}
		if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
			h = h[:i]
		}
		ts = []string{h}
	default:
		ts = cfg.Tailnet.Connects
	}
	seen := map[string]bool{}
	for _, t := range ts {
		t = strings.TrimSpace(t)
		if t == "" || t == "*" || seen[t] {
			continue
		}
		seen[t] = true
		if len(pc.Targets) == MaxPlacementTargets {
			pc.Warnings = append(pc.Warnings, fmt.Sprintf("more than %d placement targets; the rest are ignored", MaxPlacementTargets))
			break
		}
		pc.Targets = append(pc.Targets, t)
	}
	if pc.Mode == PlacementOff {
		pc.Targets = nil
	}
	return pc
}
