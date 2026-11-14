package main

// Public ingress (ADR-029), CLI side: `twd expose on|off`, the deploy's
// public URL and --wait-public, and the doctor's public probe.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/internal/policy"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/mgmt"
)

// publicCertNote is what a first public deploy costs (STATUS.md: 76–107 s
// for the first certificate of a new node name; public DNS up to 10 min).
const publicCertNote = "first certificate ~1–2 min, public DNS up to 10 min"

type exposeResult struct {
	Workload string              `json:"workload"`
	TenantID string              `json:"tenantID"`
	Exposed  bool                `json:"exposed"`
	Setting  mgmt.WorkloadPublic `json:"setting"`
	Timings  map[string]float64  `json:"timingsMs"`
}

// cmdExpose is `twd expose on|off <workload>` (PATCH
// /v1/tenants/{id}/workloads/{workload}/public): an operator override that
// survives redeploys; mgmt pushes the lease holder, which opens or closes
// the public node's Funnel listener.
func cmdExpose(args []string) error {
	f := newFlags("expose")
	tenantF := tenantFlag(f)
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || (pos[0] != "on" && pos[0] != "off") {
		return fail("bad_request", "twd expose needs on|off and a workload", "usage: twd "+usageOf("expose"))
	}
	on, workload := pos[0] == "on", pos[1]
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	if err := s.requireUp(); err != nil {
		return err
	}
	tn, err := s.tenant(*tenantF)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t0 := time.Now()
	res := exposeResult{Workload: workload, TenantID: tn.TenantID, Exposed: on, Timings: map[string]float64{}}
	if err := s.mgmt().DoHeaders(ctx, "PATCH", "/v1/tenants/"+tn.TenantID+"/workloads/"+url.PathEscape(workload)+"/public",
		map[string]any{"exposed": on}, &res.Setting,
		map[string]string{"Content-Type": "application/merge-patch+json", "Twd-Actor": actor()}); err != nil {
		return err
	}
	res.Timings["api"] = ms(time.Since(t0))
	emit(f, res, func() {
		if on {
			fmt.Printf("✓ exposed    %s: its public routes are served again over Funnel (host pushed; %.0f ms)\n", workload, res.Timings["api"])
		} else {
			fmt.Printf("✓ unexposed  %s: the host closes its Funnel listener and resets public flows (host pushed; %.0f ms)\n", workload, res.Timings["api"])
			fmt.Printf("  the setting survives redeploys · undo: twd expose on %s\n", workload)
		}
	})
	return nil
}

// printPublicPreflight prints the public part of a deploy preflight.
func printPublicPreflight(pf mgmt.PreflightResult) {
	if pf.PublicURL == "" {
		return
	}
	if len(pf.MissingNodeAttrs) == 0 {
		fmt.Printf("✓ preflight  public: funnel allowed on %s\n", policy.PublicTag)
		return
	}
	for _, m := range pf.MissingNodeAttrs {
		fmt.Printf("› preflight: nodeAttrs %s … ✗ missing (the public node could not serve Funnel)\n", m)
	}
	fmt.Printf("  add to your tailnet policy:\n")
	for _, l := range strings.Split(pf.NodeAttrsSnippet, "\n") {
		fmt.Printf("    %s\n", l)
	}
}

// preflightFailure is the typed error for a failed deploy preflight.
func preflightFailure(pf mgmt.PreflightResult) *cliErr {
	code, msg := "acl_denied", "preflight failed: the tenant policy is missing grants or tagOwners this app needs"
	if len(pf.MissingNodeAttrs) > 0 && len(pf.Missing) == 0 && onlyPublicTagOwner(pf.MissingTagOwners) {
		code, msg = "funnel_required", "preflight failed: this app declares public routes, but the tenant policy does not let "+policy.PublicTag+" use Funnel"
	}
	e := fail(code, msg)
	// The public snippet already owns tag:twd-public; don't print it twice.
	if len(pf.MissingTagOwners) > 0 && !(len(pf.MissingNodeAttrs) > 0 && onlyPublicTagOwner(pf.MissingTagOwners)) {
		e.Fix = append(e.Fix, "add to your tailnet policy:", "  "+pf.TagOwnersSnippet)
	}
	if len(pf.Missing) > 0 {
		e.Fix = append(e.Fix, `add to "grants" in your tailnet policy:`)
		for _, m := range pf.Missing {
			e.Fix = append(e.Fix, "  "+m.Snippet)
		}
	}
	if len(pf.MissingNodeAttrs) > 0 {
		e.Fix = append(e.Fix, "add to your tailnet policy (public ingress, ADR-029):")
		for _, l := range strings.Split(pf.NodeAttrsSnippet, "\n") {
			e.Fix = append(e.Fix, "  "+l)
		}
	}
	e.Fix = append(e.Fix, "then re-run, or deploy anyway with --allow-missing-grants")
	return e
}

func onlyPublicTagOwner(tags []string) bool {
	for _, t := range tags {
		if t != policy.PublicTag {
			return false
		}
	}
	return true
}

// printPublic prints a deployed workload's public URL and state.
func printPublic(p *mgmt.DeploymentPublicView) {
	if p == nil {
		return
	}
	routes := strings.Join(p.Routes, " ")
	switch {
	case !p.Allowed:
		fmt.Printf("› public     %s declared (%s) but NOT served: the tenant's publicIngress is not allow\n", p.URL, routes)
	case !p.Exposed:
		fmt.Printf("› public     %s (%s) is unexposed (twd expose off); undo: twd expose on\n", p.URL, routes)
	default:
		fmt.Printf("✓ public     %s  routes %s  (%s)\n", p.URL, routes, publicCertNote)
	}
}

// waitPublicURL polls u over the public internet (public DNS) until any
// HTTP response arrives: the public node's Funnel listener is up with a
// certificate. A 404 not_public on "/" counts: the listener answered.
func waitPublicURL(ctx context.Context, u string, timeout time.Duration, progress func(string)) (time.Duration, error) {
	t0 := time.Now()
	hc := publicHTTP()
	var last string
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		resp, err := hc.Do(req)
		if err == nil {
			resp.Body.Close()
			return time.Since(t0), nil
		}
		last = err.Error()
		if time.Since(t0) > timeout {
			return time.Since(t0), fail("public_unreachable", "the workload's public URL did not answer from the public internet after "+timeout.Round(time.Second).String(),
				"url:  "+u, "last: "+last).
				withFix("twd doctor (https_certs, funnel_attr, public_probe)", "the first certificate for a new name takes ~1–2 min and public DNS up to 10 min: retry with a longer --wait-public")
		}
		if progress != nil {
			progress(last)
		}
		select {
		case <-ctx.Done():
			return time.Since(t0), ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// publicProbe is one public-URL check from the public internet.
type publicProbe struct {
	Status   string    // ok | pending | fail
	Detail   string    // human detail
	NotAfter time.Time // certificate expiry (ok only)
}

// probePublicHost resolves host through public DNS and completes a TLS
// handshake on :443, verifying the certificate for host. No DNS answer
// yet is "pending" (new public names take up to 10 min); a handshake
// failure is "fail".
func probePublicHost(ctx context.Context, host string) publicProbe {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", "1.1.1.1:53")
	}}
	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	addrs, err := r.LookupHost(rctx, host)
	if err != nil || len(addrs) == 0 {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && (dnsErr.IsNotFound || dnsErr.IsTimeout) {
			return publicProbe{Status: "pending", Detail: "no public DNS record for " + host + " yet (" + publicCertNote + ")"}
		}
		return publicProbe{Status: "pending", Detail: fmt.Sprintf("public DNS lookup of %s: %v", host, err)}
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort(addrs[0], "443"), &tls.Config{ServerName: host})
	if err != nil {
		return publicProbe{Status: "fail", Detail: fmt.Sprintf("TLS to %s (%s): %v", host, addrs[0], err)}
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return publicProbe{Status: "fail", Detail: "no certificate from " + host}
	}
	return publicProbe{Status: "ok", NotAfter: certs[0].NotAfter,
		Detail: fmt.Sprintf("%s answers over Funnel · certificate valid until %s", host, certs[0].NotAfter.UTC().Format("2006-01-02"))}
}

// doctorPublic adds the public-ingress rows (ADR-029 D6): https_certs (from
// what twd up recorded), public_ingress (the tenant gate) and, per workload
// with public routes, funnel_attr (from the tailnet policy when this
// machine can read it) and public_probe (public DNS + TLS from here).
func doctorPublic(run func(string, func() (string, string, string)), s *state, apiKeyFile string) {
	if s == nil || !s.configured() || !s.supervisorAlive() {
		run("https_certs", func() (string, string, string) { return "skip", "twd is not running here", "" })
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tn, err := s.tenant("")
	if err != nil {
		run("public_ingress", func() (string, string, string) { return "skip", "no tenant: " + firstLine(err.Error()), "" })
		return
	}
	deps, _ := listAll[mgmt.DeploymentView](ctx, s.mgmt(), "/v1/deployments?latest=true&tenant="+url.QueryEscape(tn.TenantID), 0)
	var pub []mgmt.DeploymentView
	for _, d := range deps {
		if d.Public != nil {
			pub = append(pub, d)
		}
	}
	run("https_certs", func() (string, string, string) {
		if s.Cfg.HTTPS {
			return "ok", "the tailnet has HTTPS certificates (Funnel and the public routes of twd.jsonc \"public\" need them)", ""
		}
		st := "warn"
		if len(pub) > 0 {
			st = "fail"
		}
		return st, "twd up recorded no HTTPS certificates for " + s.Cfg.Tailnet + ": public (Funnel) routes cannot be served",
			"admin console › DNS › HTTPS Certificates › Enable HTTPS, then twd up again (it re-reads the netmap)"
	})
	var t mgmt.Tenant
	terr := s.mgmt().Do(ctx, "GET", "/v1/tenants/"+tn.TenantID, nil, &t)
	run("public_ingress", func() (string, string, string) {
		switch {
		case terr != nil:
			return "skip", "tenant unavailable: " + firstLine(terr.Error()), ""
		case t.PublicAllowed():
			return "ok", fmt.Sprintf("tenant %s allows public ingress (%d workload(s) with public routes)", t.ID, len(pub)), ""
		case len(pub) > 0:
			return "fail", fmt.Sprintf("tenant %s does not allow public ingress, so the public routes of %d workload(s) are not served", t.ID, len(pub)),
				"twd tenant set " + t.ID + " --public-ingress allow"
		}
		return "info", "tenant " + t.ID + " does not allow public ingress (no workload declares twd.jsonc \"public\")", ""
	})
	if len(pub) == 0 {
		return
	}
	var pol *mgmt.Policy
	polWhy := "this machine cannot read the tailnet policy (twd deploy preflights it through mgmt)"
	if apiKeyFile != "" {
		if key, err := tsapi.LoadEnvFile(apiKeyFile, "TS_API_KEY"); err == nil {
			api := &tsapi.Client{Tailnet: s.Cfg.Tailnet, APIKey: key, BaseURL: s.Cfg.TSAPIURL}
			if b, _, err := api.GetPolicyHuJSON(ctx); err == nil {
				pol, _ = mgmt.ParsePolicy(b)
			} else {
				polWhy = "policy read failed: " + firstLine(err.Error())
			}
		}
	}
	for _, d := range pub {
		d := d
		run("funnel_attr:"+d.Workload, func() (string, string, string) {
			if pol == nil {
				return "skip", polWhy, ""
			}
			if !pol.HasNodeAttr(policy.PublicTag, "funnel") {
				return "fail", policy.PublicTag + " has no funnel nodeAttr: " + d.Workload + "'s public node cannot serve Funnel",
					"add to your tailnet policy: " + strings.ReplaceAll(policy.PublicSnippet().HuJSON(), "\n", " ")
			}
			return "ok", "funnel allowed on " + policy.PublicTag, ""
		})
		run("public_probe:"+d.Workload, func() (string, string, string) {
			p := d.Public
			switch {
			case !p.Allowed:
				return "skip", p.URL + " is not served (tenant gate)", ""
			case !p.Exposed:
				return "skip", p.URL + " is unexposed (twd expose off)", ""
			}
			u, err := url.Parse(p.URL)
			if err != nil {
				return "fail", "bad public URL " + p.URL, ""
			}
			pr := probePublicHost(ctx, u.Hostname())
			switch pr.Status {
			case "ok":
				if time.Until(pr.NotAfter) < 7*24*time.Hour {
					return "warn", pr.Detail + " (expires within 7 days)", "certificates renew automatically while the public node runs; check twd logs --component hostd"
				}
				return "ok", pr.Detail, ""
			case "pending":
				return "warn", pr.Detail, "wait, then twd doctor again; if it persists: https_certs and funnel_attr above"
			}
			return "fail", pr.Detail, "check https_certs and funnel_attr above, and twd logs --component hostd"
		})
	}
}
