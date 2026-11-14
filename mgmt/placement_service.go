package mgmt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/hujson"
)

// ---- smart placement: the mgmt side (ADR-029) ----

// PathReport is what a host measured for one tenant (PUT
// /v1/hosts/{id}/paths, and the reply to a probe push).
type PathReport struct {
	TenantID string       `json:"tenantID"`
	Targets  []PathSample `json:"targets"`
}

// PathSample is one measured target. AgeMs is how long ago the host
// measured it (its monotonic clock); mgmt turns it into a store-clock
// timestamp, so host clock skew never ages a measurement (ADR-028 §1).
type PathSample struct {
	Target     string  `json:"target"`
	Peer       string  `json:"peer,omitempty"`
	Path       string  `json:"path"`
	RTTMs      float64 `json:"rttMs,omitempty"`
	DERPRegion string  `json:"derpRegion,omitempty"`
	AgeMs      int64   `json:"ageMs"`
}

func validPath(p string) bool {
	switch p {
	case PathDirect, PathPeerRelay, PathDERP, PathUnresolved, PathUnreachable:
		return true
	}
	return false
}

// ReportHostPaths stores a host's measured paths. A host may report only
// for tenants it has a workload of (it measures through that tenant's
// nodes); other tenants are refused, so a host can't skew another tenant's
// placement. Each tenant's rows are replaced as a whole.
func (m *Mgmt) ReportHostPaths(ctx context.Context, hostID string, reports []PathReport) (int, error) {
	if len(reports) > MaxPathTenants {
		return 0, errf(400, "bad_request", "at most %d tenants per report", MaxPathTenants)
	}
	if _, err := m.store.GetHost(ctx, hostID); err != nil {
		return 0, lookupErr(err, errf(404, "host_not_registered", "host %s is not registered", hostID))
	}
	as, err := m.store.Assignments(ctx, hostID)
	if err != nil {
		return 0, err
	}
	mine := map[string]bool{}
	for _, d := range as {
		mine[d.TenantID] = true
	}
	n := 0
	for _, rp := range reports {
		if !mine[rp.TenantID] {
			return n, errf(403, "forbidden", "host %s has no workload of tenant %q", hostID, rp.TenantID)
		}
		if err := m.storePaths(ctx, hostID, rp); err != nil {
			return n, err
		}
		n += len(rp.Targets)
	}
	return n, nil
}

func (m *Mgmt) storePaths(ctx context.Context, hostID string, rp PathReport) error {
	if len(rp.Targets) > MaxPathTargets {
		return errf(400, "bad_request", "at most %d targets per tenant", MaxPathTargets)
	}
	now := m.now()
	ms := make([]PathMeasurement, 0, len(rp.Targets))
	seen := map[string]bool{}
	for _, s := range rp.Targets {
		if s.Target == "" || len(s.Target) > 255 || !validPath(s.Path) || s.AgeMs < 0 || s.RTTMs < 0 || len(s.Peer) > 255 || len(s.DERPRegion) > 64 {
			return errf(400, "bad_request", "bad path sample %q", s.Target)
		}
		if seen[s.Target] {
			continue
		}
		seen[s.Target] = true
		ms = append(ms, PathMeasurement{HostID: hostID, TenantID: rp.TenantID, Target: s.Target, Peer: s.Peer, Path: s.Path,
			RTTMicros: int64(s.RTTMs * 1000), DERPRegion: s.DERPRegion, MeasuredAt: now.Add(-time.Duration(s.AgeMs) * time.Millisecond).UnixMilli()})
	}
	return m.store.PutHostPaths(ctx, hostID, rp.TenantID, ms, now)
}

// placementInput is what place needs from a deploy.
type placementInput struct {
	Workload string
	Pinned   string
	Replace  bool
	Config   *TwdConfig
}

// place decides the deployment's host. It returns the decision and the
// workload's current lease if a live host holds it (the caller acquires or
// moves the lease: placement only chooses).
func (m *Mgmt) place(ctx context.Context, t Tenant, in placementInput) (PlacementDecision, *Lease, error) {
	now := m.now()
	hosts, err := m.store.ListHosts(ctx, Page{})
	if err != nil {
		return PlacementDecision{}, nil, err
	}
	cands := make([]Candidate, 0, len(hosts))
	live := map[string]bool{}
	byID := map[string]Host{}
	for _, h := range hosts {
		c := candidateOf(h, now)
		cands = append(cands, c)
		live[h.ID], byID[h.ID] = c.Live, h
	}
	pc := resolvePlacement(in.Config)
	tier := placementTier(t, in.Config) // "" = any host (process)
	req := Requirements{Pool: t.Pool, Pinned: in.Pinned, Replace: in.Replace,
		Mode: pc.Mode, Targets: pc.Targets, Aggregate: pc.Aggregate, Now: now}
	if tier != "" {
		req.Tiers = []string{tier}
	}
	if h, ok := byID[in.Pinned]; ok && live[in.Pinned] && !h.offersTier(tier) {
		return PlacementDecision{}, nil, errf(409, "tier_unavailable", "host %q does not offer the %q tier (it offers %s); deploy without --host, or pick a host set up with `sudo twd host setup --microvm`", in.Pinned, tier, tierList(h))
	}
	var held *Lease
	if l, err := m.store.GetLease(ctx, LeaseName(t.ID, in.Workload)); err == nil && l.Holder != "" && live[l.Holder] {
		held = &l
		req.Sticky = l.Holder
		// Stickiness never keeps a workload on the wrong tier: refuse (or,
		// with --replace, let placement move it).
		if h := byID[l.Holder]; !h.offersTier(tier) && !in.Replace {
			return PlacementDecision{}, held, errf(409, "tier_unavailable", "workload %q runs on host %s, which does not offer the %q tier it now asks for (the host offers %s); redeploy with --replace to move it to a host with that tier, or run `twd undeploy %s` and deploy again", in.Workload, h.ID, tier, tierList(h), in.Workload)
		}
	}
	var ms []PathMeasurement
	scoring := len(req.Targets) > 0 && req.Mode == PlacementSmart && in.Pinned == "" && (held == nil || in.Replace)
	if scoring {
		if ms, err = m.store.HostPaths(ctx, t.ID, req.Targets); err != nil {
			return PlacementDecision{}, nil, err
		}
		if m.probeMissing(ctx, t, cands, ms, req) {
			if ms, err = m.store.HostPaths(ctx, t.ID, req.Targets); err != nil {
				return PlacementDecision{}, nil, err
			}
			req.Now = m.now()
		}
	}
	d := scoreHosts(cands, ms, req)
	if len(pc.Warnings) > 0 {
		d.Summary += " [" + strings.Join(pc.Warnings, "; ") + "]"
	}
	if len(d.Hosts) > maxDecisionHosts {
		d.Hosts = d.Hosts[:maxDecisionHosts] // eligible hosts come first
	}
	if d.Host == "" {
		if in.Pinned != "" && !live[in.Pinned] {
			return d, held, errf(503, "no_host", "host %q is not live (no heartbeat within %v)", in.Pinned, HostLeaseTTL)
		}
		return d, held, errf(503, "no_host", "%s", d.Summary)
	}
	return d, held, nil
}

// maxDecisionHosts bounds the per-deployment decision record.
const maxDecisionHosts = 8

// probeTimeout bounds the on-demand probe at deploy (all hosts in parallel).
const probeTimeout = 2 * time.Second

// probeMissing asks every eligible host that has no fresh measurement of
// some target to measure now (push /v1/push/probe; the host answers with a
// PathReport, which is stored). It is best effort: hosts without a push
// target, or without a node of this tenant to probe from, stay unmeasured.
// It reports whether anything was stored.
func (m *Mgmt) probeMissing(ctx context.Context, t Tenant, cands []Candidate, ms []PathMeasurement, req Requirements) bool {
	fresh := map[string]int{}
	for _, x := range ms {
		if req.Now.Sub(time.UnixMilli(x.MeasuredAt)) <= PathTTL/2 {
			fresh[x.HostID]++
		}
	}
	need := map[string]int{"nodes": 1}
	var ask []string
	for _, c := range cands {
		if ineligible(c, req, need, c.ID == req.Sticky) == "" && fresh[c.ID] < len(req.Targets) && m.pushTarget(c.ID) != "" {
			ask = append(ask, c.ID)
		}
	}
	sort.Strings(ask)
	if len(ask) == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	stored := false
	for _, h := range ask {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var rp PathReport
			if err := m.pushJSON(ctx, h, "/v1/push/probe", map[string]any{"tenantID": t.ID, "targets": req.Targets}, &rp); err != nil {
				m.log.Debug("probe push", "host", h, "err", err)
				return
			}
			rp.TenantID = t.ID
			if len(rp.Targets) > MaxPathTargets {
				rp.Targets = rp.Targets[:MaxPathTargets]
			}
			// Merge with what is stored for the host's other targets of this
			// tenant (the report replaces the (host, tenant) rows).
			for _, x := range ms {
				if x.HostID == h && !slices.ContainsFunc(rp.Targets, func(s PathSample) bool { return s.Target == x.Target }) && len(rp.Targets) < MaxPathTargets {
					rp.Targets = append(rp.Targets, PathSample{Target: x.Target, Peer: x.Peer, Path: x.Path, RTTMs: float64(x.RTTMicros) / 1000,
						DERPRegion: x.DERPRegion, AgeMs: max(0, req.Now.UnixMilli()-x.MeasuredAt)})
				}
			}
			if err := m.storePaths(context.WithoutCancel(ctx), h, rp); err != nil {
				m.log.Warn("probe result", "host", h, "err", err)
				return
			}
			mu.Lock()
			stored = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	return stored
}

// pushJSON is a push that reads a JSON reply (bounded to 64 KiB).
func (m *Mgmt) pushJSON(ctx context.Context, hostID, path string, body, out any) error {
	u := m.pushTarget(hostID)
	if u == "" {
		return errors.New("host has no push URL")
	}
	base := "http://hostd"
	var dial func(ctx context.Context, network, addr string) (net.Conn, error)
	if sock, ok := strings.CutPrefix(u, "unix://"); ok {
		dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}
	} else {
		base = u
		dial = m.cfg.TailnetDial
		if dial == nil {
			dial = (&net.Dialer{}).DialContext
		}
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: dial}}
	defer hc.CloseIdleConnections()
	b, _ := json.Marshal(body)
	rq, err := http.NewRequestWithContext(ctx, "POST", base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	rq.Header.Set("content-type", "application/json")
	resp, err := hc.Do(rq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("push %s: %s", path, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out)
}

// bundlePlacement fills cfg.Placement from wrangler.jsonc / wrangler.json
// when twd.jsonc has no placement block (a Cloudflare project keeps its
// `placement` where wrangler reads it).
func bundlePlacement(bundle []byte, cfg *TwdConfig) {
	p := cfg.Placement
	if p.Mode != "" || len(p.Targets) > 0 || p.Host != "" || p.Hostname != "" || p.Region != "" {
		return
	}
	files, err := bundleFiles(bundle, "wrangler.jsonc", "wrangler.json")
	if err != nil {
		return
	}
	for _, f := range []string{"wrangler.jsonc", "wrangler.json"} {
		b, ok := files[f]
		if !ok {
			continue
		}
		std, err := hujson.Standardize(b)
		if err != nil {
			continue
		}
		var w struct {
			Placement *struct {
				Mode     string `json:"mode"`
				Region   string `json:"region"`
				Host     string `json:"host"`
				Hostname string `json:"hostname"`
			} `json:"placement"`
		}
		if json.Unmarshal(std, &w) != nil || w.Placement == nil {
			continue
		}
		cfg.Placement.Mode, cfg.Placement.Region, cfg.Placement.Host, cfg.Placement.Hostname = w.Placement.Mode, w.Placement.Region, w.Placement.Host, w.Placement.Hostname
		return
	}
}
