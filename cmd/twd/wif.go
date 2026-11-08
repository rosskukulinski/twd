package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/mgmt"
	"golang.org/x/term"
)

// Keyless minting (ADR-026, RFC §4.4): mgmt is an OIDC issuer served over
// Funnel on its own tailnet node; the tenant admin creates a Tailscale
// *trust credential* that trusts that issuer for one pinned subject and
// may only mint auth keys for tag:twd. twd stores the credential's public
// client ID and nothing else. Deleting the credential in the console is
// the tenant's kill switch.

const consoleTrustURL = "https://login.tailscale.com/admin/settings/trust-credentials"

// wifPlan is what the operator has to create in the admin console.
type wifPlan struct {
	TenantID         string `json:"tenantID"`
	Issuer           string `json:"issuer"`
	MintSubject      string `json:"mintSubject"`
	LifecycleSubject string `json:"lifecycleSubject"`
	ConsoleURL       string `json:"consoleURL"`
}

func planFor(issuerBase, tid string) wifPlan {
	return wifPlan{TenantID: tid, Issuer: strings.TrimSuffix(issuerBase, "/") + "/t/" + tid,
		MintSubject: mgmt.MintSubject(tid), LifecycleSubject: mgmt.LifecycleSubject(tid), ConsoleURL: consoleTrustURL}
}

// printWIFSteps prints the exact admin-console steps.
func printWIFSteps(w io.Writer, p wifPlan, tailnet string) {
	fmt.Fprintf(w, `
› trust    twd mints its nodes' auth keys WITHOUT an API key: Tailscale trusts mgmt's OIDC issuer (served over
           Funnel from this machine) for one pinned subject. Create the trust credential(s) once, in the admin console:

  1. Open  %s   (tailnet %s)
     › Trust credentials › Add trust credential   (the console has no pre-fill for custom issuers yet; paste these)
  2. Mint credential (required):
       Issuer:    %s
       Subject:   %s
       Scopes:    auth_keys                   Tags: tag:twd
       Audience:  leave the default (api.tailscale.com/<client ID>)
  3. Lifecycle credential (recommended; enables kill tier 2 = device delete, the reaper, spawn checks and deploy preflight):
       Issuer:    %s
       Subject:   %s
       Scopes:    devices:core, policy_file:read     Tags: tag:twd
  4. Copy each credential's client ID (it is public; twd stores only that) and paste it below.
     Non-interactive: twd up --wif-client-id <mint ID> [--wif-lifecycle-client-id <lifecycle ID>]

`, p.ConsoleURL, tailnet, p.Issuer, p.MintSubject, p.Issuer, p.LifecycleSubject)
}

// publicHTTP resolves through public DNS (1.1.1.1, then the system
// resolver): a machine on another tailnet may not resolve *.ts.net Funnel
// names through its own MagicDNS.
func publicHTTP() *http.Client {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", "1.1.1.1:53")
	}}
	d := &net.Dialer{Timeout: 10 * time.Second, Resolver: r}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			c, err = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
		}
		return c, err
	}
	return &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{DialContext: dial, DisableKeepAlives: true}}
}

// waitIssuerPublic polls the tenant's OIDC discovery document over the
// public internet until it answers (the first Funnel fetch of a new node
// name waits for its HTTPS certificate: ~1–2 min).
func waitIssuerPublic(ctx context.Context, issuer string, timeout time.Duration, progress func(string)) (time.Duration, error) {
	t0 := time.Now()
	hc := publicHTTP()
	u := issuer + "/.well-known/openid-configuration"
	var last string
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		resp, err := hc.Do(req)
		if err == nil {
			var disc struct{ Issuer string }
			json.NewDecoder(resp.Body).Decode(&disc)
			resp.Body.Close()
			if resp.StatusCode == 200 && disc.Issuer == issuer {
				return time.Since(t0), nil
			}
			last = fmt.Sprintf("HTTP %d (issuer %q)", resp.StatusCode, disc.Issuer)
		} else {
			last = err.Error()
		}
		if time.Since(t0) > timeout {
			return time.Since(t0), fail("issuer_unreachable", "the WIF issuer is not reachable from the public internet after "+timeout.Round(time.Second).String(),
				"url:  "+u, "last: "+last).
				withFix("tag:twd-mgmt needs the funnel node attribute and the tailnet needs HTTPS (see the policy snippet above)", "twd logs --component mgmt")
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

type wifResult struct {
	Plan      wifPlan             `json:"plan"`
	ClientID  string              `json:"wifClientID"`
	Lifecycle string              `json:"wifLifecycleClientID,omitempty"`
	IssuerMS  float64             `json:"issuerPublicMs"`
	Check     mgmt.WIFCheckResult `json:"check"`
	Created   bool                `json:"tenantCreated"`
	Updated   bool                `json:"tenantUpdated"`
}

// promptClientIDs asks for the client IDs on a terminal.
func promptClientIDs(in io.Reader, out io.Writer) (mintID, lifeID string) {
	rd := bufio.NewReader(in)
	ask := func(prompt string) string {
		fmt.Fprint(out, prompt)
		ln, _ := rd.ReadString('\n')
		return strings.TrimSpace(ln)
	}
	for mintID == "" {
		mintID = ask("  mint credential client ID: ")
	}
	lifeID = ask("  lifecycle credential client ID (Enter to skip): ")
	return mintID, lifeID
}

// wifOnboard brings the tenant into wif mint mode: wait for the issuer to
// be public, collect the client IDs (flags, or a prompt), create or re-key
// the tenant, and verify the credentials with a real token exchange.
func wifOnboard(ctx context.Context, s *state, tid, tailnet, mintID, lifeID string, interactive bool, out io.Writer) (*wifResult, error) {
	res := &wifResult{Plan: planFor(s.Cfg.IssuerURL, tid), ClientID: mintID, Lifecycle: lifeID}
	var existing mgmt.Tenant
	have := s.mgmt().Do(ctx, "GET", "/v0/tenants/"+tid, nil, &existing) == nil
	if have && existing.MintMode == mgmt.MintWIF && existing.WIFClientID != "" && mintID == "" {
		// Already set up; just verify.
		res.ClientID, res.Lifecycle = existing.WIFClientID, existing.WIFLifecycleClientID
	} else {
		if mintID == "" {
			printWIFSteps(out, res.Plan, tailnet)
		}
		// The issuer must be public before Tailscale can validate a token.
		issuerDone := make(chan error, 1)
		var issuerMS time.Duration
		go func() {
			d, err := waitIssuerPublic(ctx, res.Plan.Issuer, 4*time.Minute, nil)
			issuerMS = d
			issuerDone <- err
		}()
		if mintID == "" {
			if !interactive {
				// Leave the stack running; the next `twd up --wif-client-id …` completes it.
				return res, fail("wif_credential_required", "keyless mode needs the trust credential's client ID (see the steps above)",
					"twd-mgmt and twd-hostd keep running; the tenant is created when the ID is given").
					withFix("twd up --wif-client-id <mint ID> [--wif-lifecycle-client-id <lifecycle ID>]")
			}
			fmt.Fprintf(out, "  (the issuer %s is being published over Funnel meanwhile)\n", res.Plan.Issuer)
			res.ClientID, res.Lifecycle = promptClientIDs(os.Stdin, out)
		}
		fmt.Fprintf(out, "› issuer   waiting until %s answers from the public internet (a new node's first HTTPS certificate takes ~1–2 min) …\n", res.Plan.Issuer)
		if err := <-issuerDone; err != nil {
			return res, err
		}
		res.IssuerMS = ms(issuerMS)
		fmt.Fprintf(out, "✓ issuer   public via Funnel in %.1fs · %s\n", issuerMS.Seconds(), res.Plan.Issuer)
		if have {
			if err := s.mgmt().Do(ctx, "PUT", "/v0/tenants/"+tid+"/wif", mgmt.WIFUpdate{WIFClientID: res.ClientID, WIFLifecycleClientID: res.Lifecycle}, nil); err != nil {
				return res, err
			}
			res.Updated = true
		} else {
			var tr struct {
				TenantID string `json:"tenantID"`
			}
			if err := s.mgmt().Do(ctx, "POST", "/v0/tenants", map[string]any{"tenantID": tid, "tailnet": tailnet, "controlURL": s.Cfg.ControlURL,
				"mintMode": "wif", "wifClientID": res.ClientID, "wifLifecycleClientID": res.Lifecycle}, &tr); err != nil {
				return res, err
			}
			res.Created = true
		}
	}
	if err := s.mgmt().Do(ctx, "POST", "/v0/tenants/"+tid+"/wif-check", nil, &res.Check); err != nil {
		return res, err
	}
	if !res.Check.Mint {
		return res, fail("wif_exchange_failed", "Tailscale did not accept a token from mgmt's issuer for the mint credential "+res.ClientID,
			"token exchange: "+res.Check.MintError,
			"issuer:  "+res.Plan.Issuer, "subject: "+res.Plan.MintSubject).
			withFix("check the credential's issuer and subject match exactly (no trailing slash), then re-run: twd up --wif-client-id <ID>")
	}
	return res, nil
}

// isTerminal reports whether stdin is a terminal (for prompts).
func isTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
