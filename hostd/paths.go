package hostd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

// Smart placement measurements (ADR-029, Proposed).
//
// The targets a workload connects to live on its tenant's tailnet; this
// host's own node is on the operator tailnet. So hostd measures from a node
// it already runs on that tenant's tailnet (any ready workload or object
// node): it resolves each target against that node's netmap (tags → peers,
// names → MagicDNS/host names/IPs) and sends disco pings, which show the
// path class (direct UDP endpoint, peer relay, or DERP region) and the RTT.
// Disco pings are WireGuard-level path probes: they need the peer in the
// netmap but no grant to a port, they open no connection, and nothing is
// minted to measure (no new keys, no throttle tokens, no devices).
//
// A host with no node of the tenant has no vantage point and reports
// nothing for it (placement then treats it as unmeasured).

const (
	// DefaultPathEvery is how often hostd re-measures (and reports) the
	// targets of its assignments. Measurements expire after 5 min in mgmt.
	DefaultPathEvery = 60 * time.Second
	maxProbeNodes    = 4  // vantage nodes tried per tenant
	maxPeersPerTag   = 4  // peers measured per tag target
	maxPathTargets   = 32 // per tenant (mgmt's bound)
	pingsPerPeer     = 4  // the first may go via DERP while the path upgrades
	pingGap          = 150 * time.Millisecond
	pingTimeout      = time.Second
	probeBudget      = 1500 * time.Millisecond // one tenant's probe, all targets in parallel
)

// pathTargets returns each tenant's placement targets from this host's
// assignments (sorted, deduplicated, bounded).
func (h *Host) pathTargets() map[string][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string][]string{}
	for _, w := range h.workloads {
		w.mu.Lock()
		ts := w.placementTargets
		w.mu.Unlock()
		for _, t := range ts {
			if !slices.Contains(out[w.tenantID], t) {
				out[w.tenantID] = append(out[w.tenantID], t)
			}
		}
	}
	for k, v := range out {
		sort.Strings(v)
		if len(v) > maxPathTargets {
			v = v[:maxPathTargets]
		}
		out[k] = v
	}
	return out
}

// pathLoop measures and reports every PathEvery, and soon after the set of
// targets changes (a new assignment).
func (h *Host) pathLoop() {
	defer h.wg.Done()
	every := h.cfg.PathEvery
	t := time.NewTicker(every)
	defer t.Stop()
	last, lastAt := "", time.Time{}
	poll := time.NewTicker(min(every, 2*time.Second))
	defer poll.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
			last = "" // periodic refresh
		case <-poll.C:
		}
		ts := h.pathTargets()
		sig, _ := json.Marshal(ts)
		if string(sig) == last && time.Since(lastAt) < every {
			continue
		}
		if len(ts) == 0 {
			last, lastAt = string(sig), time.Now()
			continue
		}
		if h.reportPaths(ts) {
			last, lastAt = string(sig), time.Now()
		}
	}
}

// reportPaths probes every tenant's targets and sends one report. It
// reports whether the report was sent (a tenant whose nodes are not up yet
// is retried at the next poll).
func (h *Host) reportPaths(ts map[string][]string) bool {
	ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	defer cancel()
	tenants := make([]string, 0, len(ts))
	for k := range ts {
		tenants = append(tenants, k)
	}
	sort.Strings(tenants)
	var reports []mgmtapi.PathReport
	complete := true
	for _, tn := range tenants {
		rp, ok := h.probeTenant(ctx, tn, ts[tn])
		if !ok {
			complete = false
			continue
		}
		reports = append(reports, rp)
	}
	if len(reports) == 0 {
		return false
	}
	if err := h.mgmt.ReportPaths(ctx, h.cfg.HostID, reports); err != nil {
		h.log.Warn("path report failed", "err", err)
		return false
	}
	for _, rp := range reports {
		for _, s := range rp.Targets {
			h.log.Debug("path", "tenant", rp.TenantID, "target", s.Target, "peer", s.Peer, "path", s.Path, "rtt_ms", s.RTTMs, "derp", s.DERPRegion)
		}
	}
	return complete
}

// probeNodes returns the local clients of up to maxProbeNodes ready nodes of
// the tenant on this host (workload entry nodes first, then by ID).
func (h *Host) probeNodes(tenantID string) []*local.Client {
	h.mu.Lock()
	t := h.tenants[tenantID]
	var entry []string
	for _, w := range h.workloads {
		if w.tenantID == tenantID {
			if o := w.currentWL(); o != nil {
				entry = append(entry, o.NetdID)
			}
		}
	}
	h.mu.Unlock()
	if t == nil || t.reg == nil {
		return nil
	}
	sort.Strings(entry)
	var ids []string
	ids = append(ids, entry...)
	for _, oi := range t.reg.Objects() {
		if !slices.Contains(ids, oi.ID) {
			ids = append(ids, oi.ID)
		}
	}
	var out []*local.Client
	for _, id := range ids {
		if o := t.reg.Get(id); o != nil {
			if lc := o.LocalClient(); lc != nil {
				out = append(out, lc)
				if len(out) == maxProbeNodes {
					break
				}
			}
		}
	}
	return out
}

// probeTenant measures targets from the tenant's nodes on this host. ok is
// false when the host has no ready node of the tenant (nothing to report).
func (h *Host) probeTenant(ctx context.Context, tenantID string, targets []string) (mgmtapi.PathReport, bool) {
	rp := mgmtapi.PathReport{TenantID: tenantID, Targets: []mgmtapi.PathSample{}}
	lcs := h.probeNodes(tenantID)
	if len(lcs) == 0 {
		return rp, false
	}
	ctx, cancel := context.WithTimeout(ctx, probeBudget+time.Duration(pingsPerPeer)*pingGap)
	defer cancel()
	type vantage struct {
		lc *local.Client
		st *ipnstate.Status
	}
	var vs []vantage
	for _, lc := range lcs {
		st, err := lc.Status(ctx)
		if err == nil {
			vs = append(vs, vantage{lc, st})
		}
	}
	if len(vs) == 0 {
		return rp, false
	}
	if len(targets) > maxPathTargets {
		targets = targets[:maxPathTargets]
	}
	samples := make([]mgmtapi.PathSample, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, tgt := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := time.Now()
			s := mgmtapi.PathSample{Target: tgt, Path: "unresolved"}
			for _, v := range vs {
				peers := matchPeers(v.st, tgt)
				if len(peers) == 0 {
					continue
				}
				s = bestOf(ctx, v.lc, tgt, peers)
				break
			}
			s.AgeMs = max(0, time.Since(start).Milliseconds())
			samples[i] = s
		}()
	}
	wg.Wait()
	rp.Targets = samples
	return rp, true
}

// matchPeers resolves a target in a node's netmap: "tag:x" matches peers
// with that tag; anything else matches a peer's host name, MagicDNS name
// (full or first label) or a Tailscale IP. A ":port" suffix is ignored.
// Sorted by host name; at most maxPeersPerTag.
func matchPeers(st *ipnstate.Status, target string) []*ipnstate.PeerStatus {
	var out []*ipnstate.PeerStatus
	isTag := strings.HasPrefix(target, "tag:")
	name := strings.ToLower(target)
	if !isTag {
		if ap, err := netip.ParseAddrPort(name); err == nil {
			name = ap.Addr().String()
		} else if i := strings.LastIndex(name, ":"); i > 0 && !strings.Contains(name, "]") && strings.Count(name, ":") == 1 {
			name = name[:i]
		}
		name = strings.TrimSuffix(name, ".")
	}
	for _, p := range st.Peer {
		if p == nil || len(p.TailscaleIPs) == 0 {
			continue
		}
		ok := false
		if isTag {
			ok = p.Tags != nil && slices.Contains(p.Tags.AsSlice(), target)
		} else {
			dns := strings.ToLower(strings.TrimSuffix(p.DNSName, "."))
			first, _, _ := strings.Cut(dns, ".")
			ok = name == strings.ToLower(p.HostName) || name == dns || name == first
			for _, a := range p.TailscaleIPs {
				ok = ok || a.String() == name
			}
		}
		if ok {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DNSName < out[j].DNSName })
	if len(out) > maxPeersPerTag {
		out = out[:maxPeersPerTag]
	}
	return out
}

// bestOf measures each peer and keeps the best path (class, then RTT).
func bestOf(ctx context.Context, lc *local.Client, target string, peers []*ipnstate.PeerStatus) mgmtapi.PathSample {
	best := mgmtapi.PathSample{Target: target, Path: "unreachable"}
	for _, p := range peers {
		s := measurePeer(ctx, lc, p)
		s.Target = target
		if pathRank(s.Path) > pathRank(best.Path) || pathRank(s.Path) == pathRank(best.Path) && pathRank(s.Path) > 0 && s.RTTMs < best.RTTMs {
			best = s
		}
		if best.Peer == "" {
			best.Peer = s.Peer
		}
	}
	return best
}

func pathRank(p string) int {
	switch p {
	case "direct":
		return 3
	case "peer-relay":
		return 2
	case "derp":
		return 1
	}
	return 0
}

// measurePeer sends a few disco pings: the first may travel via DERP while
// magicsock upgrades the path, so the class is the last answered ping's and
// the RTT the lowest among pings of that class.
func measurePeer(ctx context.Context, lc *local.Client, p *ipnstate.PeerStatus) mgmtapi.PathSample {
	name, _, _ := strings.Cut(strings.TrimSuffix(p.DNSName, "."), ".")
	if name == "" {
		name = p.HostName
	}
	s := mgmtapi.PathSample{Peer: name, Path: "unreachable"}
	ip := p.TailscaleIPs[0]
	var lat = map[string]float64{}
	region := ""
	for i := range pingsPerPeer {
		if i > 0 {
			select {
			case <-ctx.Done():
				return finish(s, lat, region)
			case <-time.After(pingGap):
			}
		}
		pctx, cancel := context.WithTimeout(ctx, pingTimeout)
		pr, err := lc.Ping(pctx, ip, tailcfg.PingDisco)
		cancel()
		if err != nil || pr == nil || pr.Err != "" {
			continue
		}
		cls := "derp"
		switch {
		case pr.Endpoint != "":
			cls = "direct"
		case pr.PeerRelay != "":
			cls = "peer-relay"
		default:
			region = pr.DERPRegionCode
			if region == "" && pr.DERPRegionID != 0 {
				region = "r" + strconv.Itoa(int(pr.DERPRegionID))
			}
		}
		ms := pr.LatencySeconds * 1000
		if old, ok := lat[cls]; !ok || ms < old {
			lat[cls] = ms
		}
		s.Path = cls
	}
	return finish(s, lat, region)
}

func finish(s mgmtapi.PathSample, lat map[string]float64, region string) mgmtapi.PathSample {
	if ms, ok := lat[s.Path]; ok {
		s.RTTMs = float64(int64(ms*1000)) / 1000 // µs precision
	}
	if s.Path == "derp" {
		s.DERPRegion = region
	}
	return s
}

// probeHandler is POST /v1/push/probe (mgmt → hostd, on demand at deploy):
// measure these targets for this tenant now and answer with the report.
func (h *Host) probeHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TenantID string   `json:"tenantID"`
		Targets  []string `json:"targets"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.TenantID == "" {
		writeAPIError(w, &apiError{Code: "bad_request", Detail: "tenantID and targets required"})
		return
	}
	if len(req.Targets) > maxPathTargets {
		req.Targets = req.Targets[:maxPathTargets]
	}
	rp, ok := h.probeTenant(r.Context(), req.TenantID, req.Targets)
	if !ok {
		rp = mgmtapi.PathReport{TenantID: req.TenantID, Targets: []mgmtapi.PathSample{}}
	}
	writeJSON(w, 200, rp)
}
