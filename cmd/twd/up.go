package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/internal/policy"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/mgmt"
	"github.com/rosskukulinski/twd/netd"
	"golang.org/x/term"
)

const prodControl = "https://controlplane.tailscale.com"

// baseSnippet is what `twd up` needs in the tenant policy in apikey mode:
// tag:twd owns twd's tags (DX.md quickstart), plus the tags of mgmt's and
// the hosts' own nodes and the grants between them (normal operation is
// tsnet only: hostd talks to mgmt over the tailnet, mgmt pushes kills to
// the host's node).
func baseSnippet() policy.Snippet { return policy.InfraSnippet(defaultPushPort) }

// defaultPushPort is where hostd's node accepts mgmt's pushes.
const defaultPushPort = 7474

// operatorSnippet is what `twd up` needs in the tailnet policy in oauth
// mode (the default, M12) and the browser flow: InfraSnippet (tag:twd owns
// tag:twd-mgmt and tag:twd-host, so the OAuth client mints both node keys;
// host ⇄ mgmt grants) plus the grant that makes autogroup:admin members
// twd admins over the tailnet (twd link). No nodeAttrs: no Funnel.
func operatorSnippet() policy.Snippet {
	s := policy.InfraSnippet(defaultPushPort)
	s.Grants = append(s.Grants, policy.AdminGrant("autogroup:admin", mgmt.MgmtTag, mgmt.AdminCap))
	return s
}

// keylessSnippet is the policy WIF mode needs (ADR-026): operatorSnippet
// plus the funnel attribute on tag:twd-mgmt (the WIF issuer is served over
// Funnel). Applying it needs an API key (--policy-apply in apikey mode); in
// the browser flow it is printed for the admin console.
func keylessSnippet() policy.Snippet {
	s := operatorSnippet()
	s.NodeAttrs = []map[string]any{{"target": []string{mgmt.MgmtTag}, "attr": []string{"funnel"}}}
	return s
}

// keylessPolicyOptions lets the keyless snippet through Check (RT-06):
// its one non-twd source is autogroup:admin, on tag:twd-mgmt only.
func keylessPolicyOptions() policy.Options {
	return policy.Options{AllowSrcs: []string{"autogroup:admin"}}
}

func tenantIDFor(tailnet string) string {
	l, _, _ := strings.Cut(tailnet, ".")
	l = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(l), "-")
	return "tn_" + strings.Trim(l, "-")
}

func defaultHostID() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	h = strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(h, "-"), "-")
	if h == "" {
		h = "local"
	}
	return "h-" + h
}

type upResult struct {
	StateDir   string             `json:"stateDir"`
	Tailnet    string             `json:"tailnet"`
	MintMode   string             `json:"mintMode"`
	TenantID   string             `json:"tenantID"`
	HostID     string             `json:"hostID"`
	Started    bool               `json:"started"`
	LoggedIn   bool               `json:"loggedIn,omitempty"` // an interactive login happened
	MgmtNode   string             `json:"mgmtNode,omitempty"` // twd-mgmt.<tailnet>.ts.net
	MgmtNodeID string             `json:"mgmtNodeID,omitempty"`
	MgmtIP     string             `json:"mgmtIP,omitempty"`
	HTTPS      bool               `json:"https,omitempty"`
	Funnel     bool               `json:"funnel,omitempty"`
	Issuer     string             `json:"issuer,omitempty"`
	WIF        *wifResult         `json:"wif,omitempty"`
	PolicyOK   bool               `json:"policyOK"`
	PolicyDiff string             `json:"policySnippet,omitempty"`
	Applied    bool               `json:"policyApplied"`
	Service    string             `json:"service,omitempty"`
	LingerHint string             `json:"lingerHint,omitempty"`
	SleepHint  string             `json:"sleepHint,omitempty"`
	Timings    map[string]float64 `json:"timingsMs"`
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

const defaultMgmtHostname = "twd-mgmt"

func cmdUp(args []string) error {
	f := newFlags("up")
	tailnet := f.String("tailnet", "", "the tailnet to serve (apikey mode: required; oauth: read from the client's devices or mgmt's node; wif: read from the login). Overrides discovery")
	oauthFile := f.String("oauth-client-file", "", "RECOMMENDED mint mode \"oauth\": 0600 file with TS_OAUTH_CLIENT_ID=… and TS_OAUTH_CLIENT_SECRET=tskey-client-… (an OAuth client with Auth Keys write, tag tag:twd). No browser, no Funnel")
	keyFile := f.String("api-key-file", "", "FALLBACK mint mode \"apikey\": file with TS_API_KEY=… (read at use, never copied); needs --tailnet")
	wifF := f.Bool("wif", false, "mint mode \"wif\": keyless browser flow (tailnet login, mgmt's OIDC issuer over Funnel + HTTPS, a WIF trust credential); for hosted / multi-tenant mgmt")
	yes := f.Bool("yes", false, "non-interactive: don't ask for confirmation")
	apply := f.Bool("policy-apply", false, "apikey mode: apply the policy snippet (validated first, written with If-Match); default: print it")
	control := f.String("control-url", prodControl, "coordination server for the tailnet's nodes")
	tsapiURL := f.String("tsapi-url", "", "Tailscale API base URL (testing)")
	hostID := f.String("host-id", defaultHostID(), "this host's ID")
	prefix := f.String("hostname-prefix", "twd-", "prefix for every object node hostname")
	ephemeral := f.Bool("ephemeral", false, "join object nodes as ephemeral; `twd down` then deletes every device (tests, demos)")
	insecureSH := f.Bool("insecure-shared-host", false, "on a host without a process sandbox (Linux without Landlock), still serve process-tier workloads of more than one tenant; tenants are then NOT isolated from each other")
	workerdFlag := f.String("workerd", "", "workerd binary")
	hb := f.Duration("heartbeat", time.Second, "host heartbeat period")
	tomb := f.Duration("kill-tombstone", 12*time.Second, "keep a killed node answering object_killed this long")
	delDelay := f.Duration("device-delete-delay", 8*time.Second, "delete a killed object's device this long after the kill (< --kill-tombstone)")
	siblings := f.Bool("allow-sibling-egress", false, "let objects reach other objects on this host over the tailnet (hairpin; single-host tests and demos)")
	service := f.Bool("service", false, "run the supervisor as a service that starts at login: Linux systemd user unit (~/.config/systemd/user/twd.service), macOS LaunchAgent (~/Library/LaunchAgents/dev.twd.up.plist)")
	mgmtHost := f.String("mgmt-hostname", "", "mgmt's own tailnet node name (default "+defaultMgmtHostname+")")
	mgmtKey := f.String("mgmt-authkey-file", "", "TS_AUTHKEY=… file for mgmt's node (unattended join: tests, automation); default: oauth/apikey mint it, wif logs in interactively")
	wifID := f.String("wif-client-id", "", "wif: the mint trust credential's client ID (default: prompt after printing the console steps)")
	wifLife := f.String("wif-lifecycle-client-id", "", "wif: the lifecycle trust credential's client ID (optional)")
	loginTO := f.Duration("login-timeout", 5*time.Minute, "wif: how long to wait for the browser login")
	localDev := f.Bool("local-dev", false, "DEVELOPMENT ONLY: hostd and the CLI use mgmt's unix socket with the per-host token instead of the tailnet (hermetic tests). Normal operation is tsnet only")
	pushPort := f.Int("push-port", defaultPushPort, "port on this host's own tailnet node where mgmt pushes kills")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fail("bad_request", "twd up takes no arguments", "usage: twd "+usageOf("up"))
	}
	t0 := time.Now()
	timings := map[string]float64{}
	if *delDelay >= *tomb {
		return fail("bad_request", "--device-delete-delay must be shorter than --kill-tombstone",
			"the tombstone answers object_killed only while the device still exists in peers' netmaps")
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	// The mint mode: one of the flags, else the state dir's, else the
	// choice is printed (no browser flow starts by default).
	var modes []string
	if *oauthFile != "" {
		modes = append(modes, "oauth")
	}
	if *keyFile != "" {
		modes = append(modes, "apikey")
	}
	if *wifF || *wifID != "" || *wifLife != "" {
		modes = append(modes, "wif")
	}
	if len(modes) > 1 {
		return fail("bad_request", "pick one mint mode, not "+strings.Join(modes, " + ")).
			withFix("twd up --oauth-client-file <file>   (recommended)", "twd up --wif", "twd up --tailnet <tailnet> --api-key-file <file>")
	}
	mode := ""
	if len(modes) == 1 {
		mode = modes[0]
	}
	if s.configured() {
		was := s.Cfg.modeName()
		if mode != "" && mode != was {
			return fail("state_conflict", s.Dir+" is in mint mode "+s.Cfg.mintMode()+"; this twd up asks for "+mode).
				withFix("twd up   (keeps mode "+was+")", "or twd down --purge, remove "+s.Dir+", then twd up with the new mode")
		}
		mode = was
		switch mode {
		case "apikey":
			if *keyFile == "" {
				*keyFile = s.Cfg.APIKeyFile
			}
			if *tailnet == "" {
				*tailnet = s.Cfg.Tailnet
			}
		case "oauth":
			if *oauthFile == "" {
				*oauthFile = s.Cfg.OAuthClientFile
			}
		}
		if *tailnet != "" && s.Cfg.Tailnet != *tailnet {
			return fail("state_conflict", s.Dir+" is bound to tailnet "+s.Cfg.Tailnet).
				withFix("twd down --purge, then remove "+s.Dir, "or use another --state-dir")
		}
	}
	if mode == "" {
		return mintModeRequired()
	}
	apikeyMode, oauthMode, wifMode := mode == "apikey", mode == "oauth", mode == "wif"
	if apikeyMode && *tailnet == "" {
		return fail("tailnet_required", "twd up --api-key-file (mint mode apikey) needs --tailnet",
			"an API key does not name its tailnet, and twd never uses this machine's tailscaled or its identity.").
			withFix("twd up --tailnet <tailnet>.ts.net --api-key-file <file with TS_API_KEY=…> --yes", "or (recommended): twd up --oauth-client-file <file>")
	}
	// oauth: the credential file must be the owner's only (checked before
	// anything else happens; twd-mgmt refuses it too).
	absOAuth := ""
	var oauthCreds tsapi.OAuthCredentials
	if oauthMode {
		absOAuth, _ = filepath.Abs(*oauthFile)
		c, err := tsapi.LoadOAuthFile(absOAuth)
		if err != nil {
			if tsapi.IsInsecureFile(err) {
				return fail("credential_file_insecure", absOAuth+" is readable by other users", err.Error()).withFix("chmod 600 " + absOAuth + "   then twd up again")
			}
			return fail("bad_request", "can't read the OAuth client from "+absOAuth, netd.Redact(err.Error())).
				withFix("the file needs TS_OAUTH_CLIENT_ID=<id> and TS_OAUTH_CLIENT_SECRET=tskey-client-… (mode 0600)")
		}
		oauthCreds = c
	}
	if *service {
		if err := serviceAvailable(); err != nil {
			return err
		}
	}
	if !*yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fail("bad_request", "not a terminal: pass --yes to run non-interactively")
		}
		what := "for tailnet " + *tailnet
		switch {
		case wifMode:
			what = "keyless (tailnet login + WIF trust credential)"
		case oauthMode:
			what = "with OAuth client " + oauthCreds.ClientID
		}
		if !apikeyMode && s.Cfg.Tailnet != "" {
			what += " for tailnet " + s.Cfg.Tailnet
		}
		fmt.Printf("Start twd on this machine %s (state in %s)? [y/N] ", what, s.Dir)
		ln, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ln)); a != "y" && a != "yes" {
			return fail("cancelled", "not started")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	// Progress, the login URL and the console steps go to stdout, or to
	// stderr with --json (stdout then carries only the result).
	out := io.Writer(os.Stdout)
	if *f.json {
		out = os.Stderr
	}

	// apikey / oauth: the credential must work for this tailnet; read the
	// policy (never written unless --policy-apply in apikey mode).
	var api *tsapi.Client
	var pol []byte
	policyReadable := false
	absKey := ""
	if apikeyMode {
		absKey, _ = filepath.Abs(*keyFile)
		apiKey, err := tsapi.LoadEnvFile(absKey, "TS_API_KEY")
		if err != nil {
			return fail("bad_request", "can't read TS_API_KEY from "+absKey, err.Error())
		}
		if st, err := os.Stat(absKey); err == nil && st.Mode().Perm()&0o077 != 0 {
			fmt.Fprintf(os.Stderr, "warning: %s is readable by other users (mode %v); chmod 600 it\n", absKey, st.Mode().Perm())
		}
		api = &tsapi.Client{Tailnet: *tailnet, APIKey: apiKey, BaseURL: *tsapiURL}
		ps := time.Now()
		pol, _, err = api.GetPolicyHuJSON(ctx)
		if err != nil {
			return fail("bootstrap_failed", "the Tailscale API rejected the key for "+*tailnet,
				"step:  GET /api/v2/tailnet/"+*tailnet+"/acl → "+err.Error()).
				withFix("check the tailnet name and that the key in " + absKey + " is valid and can read the policy")
		}
		policyReadable = true
		timings["api_check"] = ms(time.Since(ps))
	}
	if oauthMode {
		ps := time.Now()
		src := &tsapi.OAuthSource{BaseURL: *tsapiURL, Load: func() (tsapi.OAuthCredentials, error) { return tsapi.LoadOAuthFile(absOAuth) }}
		tn := *tailnet
		if tn == "" {
			tn = "-" // the token's own tailnet
		}
		api = &tsapi.Client{Tailnet: tn, Tokens: src, BaseURL: *tsapiURL}
		found, p, readable, err := oauthPreflight(ctx, api, src, oauthCreds.ClientID, absOAuth, out)
		if err != nil {
			return err
		}
		pol, policyReadable = p, readable
		if *tailnet == "" && found != "" {
			*tailnet = found
		}
		timings["api_check"] = ms(time.Since(ps))
	}

	wd, err := findWorkerd(*workerdFlag)
	if err != nil {
		return err
	}
	for _, b := range []string{"twd-mgmt", "twd-hostd"} {
		if _, err := findBin(b); err != nil {
			return err
		}
	}
	res := upResult{StateDir: s.Dir, HostID: *hostID, Timings: timings}

	// mgmt's own tailnet node carries the API (WhoIs auth, and the local
	// CLI through its loopback) and, in wif mode, the WIF issuer. It is the
	// only transport in normal operation; --local-dev may skip it.
	mgmtHostname := *mgmtHost
	if mgmtHostname == "" {
		mgmtHostname = s.Cfg.MgmtHostname
		if mgmtHostname == "" && (!*localDev || !apikeyMode) {
			mgmtHostname = defaultMgmtHostname
		}
	}
	if s.configured() && s.Cfg.LocalDev != *localDev {
		return fail("state_conflict", fmt.Sprintf("%s was set up with --local-dev=%v", s.Dir, s.Cfg.LocalDev)).withFix("twd down --purge, remove " + s.Dir + ", then twd up again")
	}
	if s.configured() && s.Cfg.MgmtHostname != "" && mgmtHostname != "" && mgmtHostname != s.Cfg.MgmtHostname {
		return fail("state_conflict", "mgmt's node in "+s.Dir+" is named "+s.Cfg.MgmtHostname).withFix("drop --mgmt-hostname, or use another --state-dir")
	}
	if mgmtHostname != "" && !s.supervisorAlive() {
		// The join runs in this terminal (oauth/apikey: a minted key; wif:
		// interactive login), then the node is handed to twd-mgmt. While
		// the supervisor runs, mgmt holds the node and its state is
		// authoritative.
		authKey := ""
		if *mgmtKey != "" {
			if authKey, err = tsapi.LoadEnvFile(*mgmtKey, "TS_AUTHKEY"); err != nil {
				return fail("bad_request", "can't read TS_AUTHKEY from "+*mgmtKey, err.Error())
			}
		}
		firstJoin := !isFile(filepath.Join(s.mgmtTsnetDir(), "tailscaled.state"))
		if firstJoin && authKey == "" && (apikeyMode || oauthMode) {
			// The credential mints mgmt's node key (tag:twd-mgmt): no
			// browser login. The key is used in-process and never stored.
			if authKey, err = mintNodeKey(ctx, api, mgmt.MgmtTag, "twd mgmt node "+mgmtHostname); err != nil {
				if apikeyMode {
					if ce, ok := err.(*cliErr); ok && ce.Code == "bootstrap_failed" {
						ce.Fix = append(ce.Fix, "the policy needs tagOwners \""+mgmt.MgmtTag+"\": [\"autogroup:admin\"] (twd up --policy-apply adds it), and the key's owner must be an admin")
					}
				}
				return err
			}
		}
		if firstJoin && authKey == "" {
			// Browser flow: the policy comes first, and the login URL is
			// printed only once tag ownership is confirmed (interactive) or
			// at least explained (--yes): a login whose tags the policy
			// doesn't permit fails only after the browser round trip.
			fmt.Fprintf(out, "twd up · wif (keyless) · mgmt joins your tailnet as %q (tag %s), then you create one trust credential (no API key)\n", mgmtHostname, mgmt.MgmtTag)
			fmt.Fprintf(out, "› policy   BEFORE logging in, your tailnet policy must have (admin console › Access controls):\n")
			for _, l := range snippetLines(keylessSnippet()) {
				fmt.Fprintln(out, l)
			}
			fmt.Fprintf(out, "  (tagOwners: the account you log in with must be allowed to own %s · nodeAttrs: Funnel serves the WIF issuer · grants: admins may use twd over the tailnet)\n", mgmt.MgmtTag)
			if !*yes && isTerminal() {
				fmt.Fprintf(out, "Is this in your policy now? The login fails with \"requested tags [%s] are invalid or not permitted\" otherwise. [y/N] ", mgmt.MgmtTag)
				ln, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(ln)); a != "y" && a != "yes" {
					return policyMissingTagOwners("not confirmed: add the snippet above first; no login URL was requested", nil)
				}
			}
		}
		js := time.Now()
		if err := os.MkdirAll(s.mgmtTsnetDir(), 0o700); err != nil {
			return err
		}
		node, err := joinMgmtNode(ctx, joinOpts{Dir: s.mgmtTsnetDir(), Hostname: mgmtHostname, ControlURL: *control, Tags: []string{mgmt.MgmtTag},
			AuthKey: authKey, Timeout: *loginTO, Out: out})
		if err != nil {
			return err
		}
		timings["mgmt_join"] = ms(time.Since(js))
		res.LoggedIn, res.MgmtNode, res.MgmtNodeID, res.MgmtIP, res.HTTPS, res.Funnel = node.Login, node.DNSName, node.NodeID, node.IP, node.HTTPS, node.Funnel
		if !slices.Contains(node.Tags, mgmt.MgmtTag) && *control == prodControl {
			return fail("bootstrap_failed", "mgmt's node joined without the "+mgmt.MgmtTag+" tag (tags: "+strings.Join(node.Tags, ",")+")",
				"the account that logged in is not a tagOwner of "+mgmt.MgmtTag+", so control dropped the tag").
				withFix("add to tagOwners: \"" + mgmt.MgmtTag + "\": [\"autogroup:admin\", \"tag:twd\"], then remove the node " + node.DNSName + " in the admin console and run twd up again")
		}
		if *tailnet == "" {
			*tailnet = node.Tailnet
		} else if *control == prodControl && node.Tailnet != "" && node.Tailnet != *tailnet {
			return fail("state_conflict", "mgmt's node joined tailnet "+node.Tailnet+", not --tailnet "+*tailnet).
				withFix("drop --tailnet (it is discovered), or use the right credential")
		}
		if wifMode {
			if !node.HTTPS {
				return fail("https_required", "this tailnet has no HTTPS certificates, so mgmt's WIF issuer can't be served over Funnel",
					"node "+node.DNSName+" joined fine (tags "+strings.Join(node.Tags, ",")+"); its state is kept, so the next twd up needs no login").
					withFix("admin console › DNS › HTTPS Certificates › Enable HTTPS, then run twd up again", "or use an OAuth client (no HTTPS or Funnel needed): twd up --oauth-client-file <file>")
			}
			if !node.Funnel {
				return fail("funnel_required", "mgmt's node has no Funnel permission, so the WIF issuer can't be published for Tailscale to verify tokens",
					"node "+node.DNSName+" joined (tags "+strings.Join(node.Tags, ",")+"); state kept, the next twd up needs no login").
					withFix(`add to your tailnet policy: "nodeAttrs": [{"target": ["`+mgmt.MgmtTag+`"], "attr": ["funnel"]}]`, "then run twd up again (it re-reads the policy on join)")
			}
		}
		s.Cfg.MgmtHostname, s.Cfg.MgmtDNSName, s.Cfg.MgmtNodeID, s.Cfg.MgmtIP, s.Cfg.HTTPS = mgmtHostname, node.DNSName, node.NodeID, node.IP, node.HTTPS
		if wifMode {
			s.Cfg.IssuerURL = "https://" + node.DNSName
		}
	} else if mgmtHostname != "" {
		res.MgmtNode, res.MgmtNodeID, res.MgmtIP, res.HTTPS, res.Funnel = s.Cfg.MgmtDNSName, s.Cfg.MgmtNodeID, s.Cfg.MgmtIP, s.Cfg.HTTPS, s.Cfg.IssuerURL != ""
		if *tailnet == "" {
			*tailnet = s.Cfg.Tailnet
		}
	}
	if *tailnet == "" {
		return fail("tailnet_required", "could not determine the tailnet").withFix("twd up --tailnet <tailnet>")
	}
	res.Tailnet = *tailnet
	if !s.configured() {
		s.Cfg = Config{Tailnet: *tailnet, CreatedAt: time.Now().UTC().Format(time.RFC3339), MgmtHostname: s.Cfg.MgmtHostname, MgmtDNSName: s.Cfg.MgmtDNSName,
			MgmtNodeID: s.Cfg.MgmtNodeID, MgmtIP: s.Cfg.MgmtIP, HTTPS: s.Cfg.HTTPS, IssuerURL: s.Cfg.IssuerURL}
	}
	switch {
	case apikeyMode:
		s.Cfg.MintMode, s.Cfg.APIKeyFile = "apikey", absKey
	case oauthMode:
		s.Cfg.MintMode, s.Cfg.OAuthClientFile, s.Cfg.OAuthClientID = "oauth", absOAuth, oauthCreds.ClientID
	default:
		s.Cfg.MintMode = "wif"
		if *wifID != "" {
			s.Cfg.WIFClientID, s.Cfg.WIFLifecycleClientID = *wifID, *wifLife
		}
	}
	res.MintMode = s.Cfg.mintMode()
	res.Issuer = s.Cfg.IssuerURL
	s.Cfg.ControlURL, s.Cfg.TSAPIURL, s.Cfg.HostID = *control, *tsapiURL, *hostID
	s.Cfg.HostnamePrefix, s.Cfg.Ephemeral, s.Cfg.Workerd = *prefix, *ephemeral, wd
	s.Cfg.InsecureSharedHost = *insecureSH
	s.Cfg.Heartbeat, s.Cfg.KillTombstone, s.Cfg.DeviceDeleteDelay = hb.String(), tomb.String(), delDelay.String()
	s.Cfg.AllowSiblingEgress = *siblings
	s.Cfg.LocalDev, s.Cfg.PushPort = *localDev, *pushPort
	if err := s.save(); err != nil {
		return err
	}
	if *service {
		was := s.supervisorAlive()
		changed, err := installService(s)
		if err != nil {
			return err
		}
		res.Started = !was || changed
		res.Service = serviceUnitPath()
		res.LingerHint = lingerHint()
		res.SleepHint = sleepHint()
	} else if inst, act := serviceOf(s); inst && act {
		return fail("service_running", "twd runs as the "+serviceKind()+" "+serviceUnit+" (installed by twd up --service)").
			withFix("twd up --service … to change its settings", serviceRestartHint(), "twd down --service to remove it")
	} else if !s.supervisorAlive() {
		if err := startSupervisor(s); err != nil {
			return err
		}
		res.Started = true
	}
	ms0 := time.Now()
	for !s.mgmtHealthy(ctx) {
		if time.Since(ms0) > 90*time.Second || !s.supervisorAlive() {
			return fail("mgmt_unavailable", "twd-mgmt did not become healthy", "log: "+s.log("mgmt")).withFix("twd logs --component mgmt")
		}
		time.Sleep(50 * time.Millisecond)
	}
	timings["mgmt_ready"] = ms(time.Since(t0))

	// Tenant (operator = tenant = you). In normal operation the host's own
	// node needs a join key minted through the tenant's credential, so the
	// tenant comes first; in wif mode that key exists only once the trust
	// credential does.

	tid := s.Cfg.TenantID
	if tid == "" {
		tid = tenantIDFor(*tailnet)
	}
	switch {
	case apikeyMode || oauthMode:
		want := map[string]any{"mintMode": "apikey", "apiKeyFile": absKey}
		if oauthMode {
			want = map[string]any{"mintMode": "oauth", "oauthClientID": oauthCreds.ClientID, "oauthClientFile": absOAuth}
		}
		var cur struct {
			MintMode        string `json:"mintMode"`
			APIKeyFile      string `json:"apiKeyFile"`
			OAuthClientID   string `json:"oauthClientID"`
			OAuthClientFile string `json:"oauthClientFile"`
		}
		if err := s.mgmt().Do(ctx, "GET", "/v1/tenants/"+tid, nil, &cur); err != nil {
			var tr struct {
				TenantID string `json:"tenantID"`
			}
			body := map[string]any{"tenantID": tid, "tailnet": *tailnet, "controlURL": *control}
			maps.Copy(body, want)
			if err := s.mgmt().Do(ctx, "POST", "/v1/tenants", body, &tr); err != nil {
				return err
			}
			tid = tr.TenantID
		} else if oauthMode && (cur.MintMode != "oauth" || cur.OAuthClientID != oauthCreds.ClientID || cur.OAuthClientFile != absOAuth) {
			// A regenerated client (e.g. one with devices:core added).
			if err := s.mgmt().Do(ctx, "PATCH", "/v1/tenants/"+tid, want, nil); err != nil {
				return err
			}
		}
	default:
		wr, err := wifOnboard(ctx, s, tid, *tailnet, s.Cfg.WIFClientID, s.Cfg.WIFLifecycleClientID, !*yes || isTerminal(), out)
		res.WIF = wr
		if err != nil {
			if ce, ok := err.(*cliErr); ok && ce.Code == "wif_credential_required" {
				// Keep what we have so the next run completes the setup.
				s.save()
				if !*localDev {
					fmt.Fprintf(out, "› host     twd-hostd waits for its join key: it is minted through the trust credential once the IDs are given\n")
				}
			}
			return err
		}
		s.Cfg.WIFClientID, s.Cfg.WIFLifecycleClientID = wr.ClientID, wr.Lifecycle
	}
	s.Cfg.TenantID = tid
	if err := s.save(); err != nil {
		return err
	}
	res.TenantID = tid
	timings["tenant_ready"] = ms(time.Since(t0))

	// The host's own node: minted by mgmt (POST /v1/hosts) through the
	// tenant's credential, handed to hostd as a single-use join file.
	if !*localDev && !isFile(filepath.Join(s.hostTsnetDir(), "tailscaled.state")) {
		js := time.Now()
		if err := s.mintHostJoin(ctx, *hostID, tid); err != nil {
			return err
		}
		timings["host_join_key"] = ms(time.Since(js))
	}
	// Host registered and heartbeating.
	hs := time.Now()
	for {
		var h struct {
			ID       string `json:"hostID"`
			LastSeen int64  `json:"lastSeen"`
			Status   string `json:"status"`
		}
		if s.mgmt().Do(ctx, "GET", "/v1/hosts/"+*hostID, nil, &h) == nil && h.Status == "live" {
			if s.hostd().Do(ctx, "GET", "/v1/status", nil, nil) == nil {
				break
			}
		}
		if time.Since(hs) > 3*time.Minute || !s.supervisorAlive() {
			return fail("host_unavailable", "twd-hostd did not register with twd-mgmt", "log: "+s.log("hostd")).withFix("twd logs --component hostd")
		}
		time.Sleep(100 * time.Millisecond)
	}
	timings["host_ready"] = ms(time.Since(t0))

	if apikeyMode {
		miss, err := policy.Missing(pol, baseSnippet())
		if err != nil {
			return err
		}
		res.PolicyOK = miss.Empty()
		if !res.PolicyOK {
			res.PolicyDiff = miss.HuJSON()
			if *apply {
				ar, err := policy.Apply(ctx, api, baseSnippet())
				if err != nil {
					return fail("policy_apply_failed", "could not apply the policy snippet", err.Error()).withFix("add it in the admin console:", res.PolicyDiff)
				}
				res.Applied, res.PolicyOK = ar.Changed, true
			}
		}
	} else if oauthMode {
		// Tag ownership is proven (mgmt's and the host's keys minted); the
		// grants are checked when the client can read the policy.
		res.PolicyOK = true
		if policyReadable {
			// Grants only: tag ownership is proven by the mints, and other
			// owners of tag:twd (a group, say) are fine.
			if miss, err := policy.Missing(pol, policy.Snippet{Grants: operatorSnippet().Grants}); err == nil && !miss.Empty() {
				res.PolicyOK, res.PolicyDiff = false, miss.HuJSON()
			}
		}
	} else {
		res.PolicyOK = true // mgmt's node joined tagged and with funnel: the snippet is in place
	}
	timings["total"] = ms(time.Since(t0))
	emit(f, res, func() {
		if apikeyMode {
			fmt.Printf("twd up · all-in-one on this machine · tailnet: %s (API key, mint mode apikey)\n", *tailnet)
		} else if oauthMode {
			fmt.Printf("twd up · all-in-one on this machine · tailnet: %s (OAuth client %s, mint mode oauth: no browser, no Funnel)\n", *tailnet, oauthCreds.ClientID)
		} else {
			fmt.Printf("twd up · all-in-one on this machine · tailnet: %s (keyless: WIF trust credential, no API key)\n", *tailnet)
		}
		started := "already running"
		if res.Started {
			started = "started"
		}
		fmt.Printf("✓ mgmt     twd-mgmt %s · leases in SQLite (%s) · custody: file (homelab mode)\n", started, filepath.Join(s.Dir, "mgmt", "mgmt.db"))
		if res.MgmtNode != "" {
			sch := "http"
			if res.HTTPS {
				sch = "https"
			}
			fmt.Printf("✓ node     %s (%s) · API on the tailnet: %s://%s (WhoIs auth; `twd link %s` from your machine)\n", res.MgmtNode, mgmt.MgmtTag, sch, res.MgmtNode, res.MgmtNode)
		}
		fmt.Printf("✓ host     this machine (%s) · %s\n", *hostID, tierLine(s))
		if res.WIF != nil {
			life := "no lifecycle credential: kill deletes no devices, no reaper device deletes, spawn checks and deploy preflight unavailable"
			switch res.WIF.Check.Lifecycle {
			case "ok":
				life = "lifecycle credential ok"
				if res.WIF.Check.PolicyRead {
					life += ", policy readable (preflight + spawn checks on)"
				} else {
					life += ", but it can't read the policy (add scope policy_file:read for preflight + spawn checks)"
				}
			case "error":
				life = "lifecycle credential FAILED: " + firstLine(res.WIF.Check.LifecycleError)
			}
			fmt.Printf("✓ trust    mint credential %s exchanges (issuer %s) · %s\n", res.WIF.ClientID, res.WIF.Plan.Issuer, life)
		}
		fmt.Printf("✓ tenant   %s connected (tenant %s, mint mode %s) · control %s\n", *tailnet, tid, s.Cfg.mintMode(), *control)
		switch {
		case res.Applied:
			fmt.Printf("✓ policy   applied tag:twd tagOwners (validated, If-Match)\n")
		case res.PolicyOK && apikeyMode:
			fmt.Printf("✓ policy   tag:twd tagOwners present\n")
		case res.PolicyOK && oauthMode && policyReadable:
			fmt.Printf("✓ policy   tag:twd owns %s and %s; host ⇄ mgmt and admin grants present\n", mgmt.MgmtTag, mgmt.HostTag)
		case res.PolicyOK && oauthMode:
			fmt.Printf("✓ policy   tag:twd owns %s and %s (both keys minted); grants not checked (the OAuth client can't read the policy)\n", mgmt.MgmtTag, mgmt.HostTag)
		case res.PolicyOK:
			fmt.Printf("✓ policy   %s tagged, Funnel allowed (your tailnet policy has the keyless snippet)\n", mgmt.MgmtTag)
		default:
			if oauthMode {
				fmt.Printf("› policy   missing (add in the admin console › Access controls):\n")
			} else {
				fmt.Printf("› policy   not applied (re-run with --policy-apply, or add in the admin console):\n")
			}
			for _, l := range strings.Split(strings.TrimSpace(res.PolicyDiff), "\n") {
				fmt.Printf("    %s\n", l)
			}
		}
		if res.Service != "" {
			fmt.Printf("✓ service  %s %s (starts at login; `twd down --service` removes it)\n", serviceKind(), res.Service)
			if res.LingerHint != "" {
				fmt.Printf("› linger   %s\n", res.LingerHint)
			}
			if res.SleepHint != "" {
				fmt.Printf("› sleep    %s\n", res.SleepHint)
			}
		}
		fmt.Printf("ready in %.1fs · `twd init hello --template agent && cd hello && twd deploy`\n", timings["total"]/1000)
	})
	return nil
}

// startSupervisor starts `twd __supervise` detached (its own session), with
// output in <state>/logs/supervisor.log.
func startSupervisor(s *state) error {
	if err := os.MkdirAll(s.logDir(), 0o700); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	lf, err := os.OpenFile(s.log("supervisor"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	cmd := exec.Command(exe, "__supervise", "--state-dir", s.Dir)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = lf, lf, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()
	for i := 0; i < 100; i++ {
		if s.supervisorPID() == pid {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !pidAlive(pid) {
		return fail("supervisor_failed", "twd supervisor exited at start", "log: "+s.log("supervisor"))
	}
	return nil
}

// ---- supervisor ----

type child struct {
	name    string
	args    []string
	logPath string

	mu       sync.Mutex
	cmd      *exec.Cmd
	done     chan struct{}
	restarts []time.Time
}

func (c *child) start(slog func(string, ...any)) error {
	lf, err := os.OpenFile(c.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(c.args[0], c.args[1:]...)
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		lf.Close()
		return err
	}
	done := make(chan struct{})
	c.mu.Lock()
	c.cmd, c.done = cmd, done
	c.mu.Unlock()
	slog("started %s pid %d", c.name, cmd.Process.Pid)
	go func() {
		err := cmd.Wait()
		lf.Close()
		slog("%s exited: %v", c.name, err)
		close(done)
	}()
	return nil
}

func (c *child) stop(timeout time.Duration) {
	c.mu.Lock()
	cmd, done := c.cmd, c.done
	c.mu.Unlock()
	if cmd == nil {
		return
	}
	cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(timeout):
		cmd.Process.Kill()
		<-done
	}
}

func cmdSupervise(args []string) error {
	f := newFlags("__supervise")
	if _, err := f.parse(args); err != nil {
		return err
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	if !s.configured() {
		return fail("not_up", "no config in "+s.Dir)
	}
	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "%s supervisor: %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, a...))
	}
	if pidAlive(s.supervisorPID()) && s.supervisorPID() != os.Getpid() {
		return fail("already_running", "a supervisor is already running")
	}
	if err := os.WriteFile(s.pidFile(), []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		return err
	}
	defer os.Remove(s.pidFile())
	for _, d := range []string{s.runDir(), s.hostdRunDir(), filepath.Join(s.Dir, "mgmt"), filepath.Join(s.Dir, "hostd")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		os.Chmod(d, 0o700)
	}
	mgmtBin, err := findBin("twd-mgmt")
	if err != nil {
		return err
	}
	hostdBin, err := findBin("twd-hostd")
	if err != nil {
		return err
	}
	margs := []string{mgmtBin, "-state-dir", filepath.Join(s.Dir, "mgmt"), "-device-delete-delay", s.Cfg.DeviceDeleteDelay, "-version", version}
	if s.Cfg.LocalDev {
		// RT-03: a fresh per-host token on every mgmt start (twd up, a
		// supervisor/systemd restart). hostd re-reads the file when it
		// changes and the CLI reads it per call, so rotation never strands
		// a running hostd.
		margs = append(margs, "-local-dev", "-socket", s.mgmtSock(), "-host-token-file", s.hostTokenFile(), "-rotate-host-token")
	}
	if s.Cfg.TSAPIURL != "" {
		margs = append(margs, "-tsapi-url", s.Cfg.TSAPIURL)
	}
	if s.Cfg.MgmtHostname != "" {
		// mgmt's own tailnet node (joined by twd up): the API over WhoIs
		// and, in keyless mode, the WIF issuer over Funnel.
		margs = append(margs, "-tsnet-hostname", s.Cfg.MgmtHostname, "-tsnet-control-url", s.Cfg.ControlURL, "-tsnet-tags", mgmt.MgmtTag)
		if s.Cfg.keyless() && s.Cfg.IssuerURL != "" {
			margs = append(margs, "-funnel", "-issuer-url", s.Cfg.IssuerURL)
		}
	}
	hargs := []string{hostdBin, "-host-id", s.Cfg.HostID,
		"-data-dir", filepath.Join(s.Dir, "hostd"), "-run-dir", s.hostdRunDir(), "-workerd", s.Cfg.Workerd,
		"-heartbeat", s.Cfg.Heartbeat, "-kill-tombstone", s.Cfg.KillTombstone, "-hostname-prefix", s.Cfg.HostnamePrefix,
		// No tenant's workerd may touch twd's state (mgmt custody, every
		// tenant's node keys and DO storage) or its sockets, except its own.
		"-sandbox", "auto", "-sandbox-deny", strings.Join([]string{filepath.Join(s.Dir, "mgmt"), filepath.Join(s.Dir, "hostd"),
			s.logDir(), s.configPath(), filepath.Join(s.Dir, "tenants"), s.runDir()}, ",")}
	if s.Cfg.OAuthClientFile != "" {
		hargs[len(hargs)-1] += "," + s.Cfg.OAuthClientFile // the OAuth client secret (also denied via the tenant list)
	}
	if s.Cfg.LocalDev {
		hargs = append(hargs, "-local-dev", "-mgmt", "unix://"+s.mgmtSock(), "-mgmt-token-file", s.hostTokenFile())
	} else {
		// Normal operation: hostd's own node, mgmt over the tailnet (by
		// tailnet IP: a MagicDNS name resolves only once the peer is in the
		// netmap, and mgmt's node keeps its IP across restarts).
		mgmtURL := "http://" + s.Cfg.MgmtIP
		if s.Cfg.MgmtIP == "" {
			mgmtURL = "http://" + s.Cfg.MgmtDNSName
		}
		port := s.Cfg.PushPort
		if port == 0 {
			port = defaultPushPort
		}
		hargs = append(hargs, "-mgmt", mgmtURL, "-tsnet-control-url", s.Cfg.ControlURL, "-tsnet-dir", s.hostTsnetDir(),
			"-join-token-file", s.hostJoinFile(), "-push-port", strconv.Itoa(port))
	}
	if s.Cfg.Ephemeral {
		hargs = append(hargs, "-ephemeral")
	}
	if s.Cfg.InsecureSharedHost {
		hargs = append(hargs, "-insecure-shared-host")
	}
	if s.Cfg.AllowSiblingEgress {
		hargs = append(hargs, "-allow-sibling-egress")
	}
	mg := &child{name: "twd-mgmt", args: margs, logPath: s.log("mgmt")}
	hd := &child{name: "twd-hostd", args: hargs, logPath: s.log("hostd")}

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	stopping := make(chan struct{})
	go func() {
		<-sig
		close(stopping)
	}()
	waitMgmt := func() bool {
		for i := 0; i < 600; i++ {
			if s.mgmtHealthy(context.Background()) {
				return true
			}
			select {
			case <-stopping:
				return false
			case <-time.After(50 * time.Millisecond):
			}
		}
		return false
	}
	// supervise runs c, restarting it with backoff (≤10 restarts per 5 min).
	var wg sync.WaitGroup
	supervise := func(c *child, before func() bool) {
		defer wg.Done()
		backoff := 500 * time.Millisecond
		for {
			if before != nil && !before() {
				return
			}
			if err := c.start(logf); err != nil {
				logf("start %s: %v", c.name, err)
			} else {
				st := time.Now()
				select {
				case <-c.done:
				case <-stopping:
					return
				}
				if time.Since(st) > time.Minute {
					backoff = 500 * time.Millisecond
				}
			}
			now := time.Now()
			c.restarts = append(c.restarts, now)
			for len(c.restarts) > 0 && now.Sub(c.restarts[0]) > 5*time.Minute {
				c.restarts = c.restarts[1:]
			}
			if len(c.restarts) > 10 {
				logf("%s keeps exiting; giving up (see %s)", c.name, c.logPath)
				return
			}
			select {
			case <-stopping:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
		}
	}
	logf("supervising in %s (tailnet %s)", s.Dir, s.Cfg.Tailnet)
	wg.Add(2)
	go supervise(mg, nil)
	go supervise(hd, waitMgmt)
	<-stopping
	logf("stopping: hostd first (nodes close in parallel), then mgmt (flushes pending device deletes)")
	hd.stop(45 * time.Second)
	mg.stop(90 * time.Second)
	wg.Wait()
	logf("stopped")
	return nil
}

// ---- down ----

type downResult struct {
	ServiceRemoved bool               `json:"serviceRemoved,omitempty"`
	ServiceStopped bool               `json:"serviceStopped,omitempty"`
	Purged         int                `json:"objectsPurged"`
	Killed         []string           `json:"killed"`
	Flushed        int                `json:"devicesDeleted"`
	Timings        map[string]float64 `json:"timingsMs"`
}

func cmdDown(args []string) error {
	f := newFlags("down")
	service := f.Bool("service", false, "also disable and remove the service installed by twd up --service (systemd user unit / macOS LaunchAgent)")
	purge := f.Bool("purge", false, "kill every live object (audit: \"twd down --purge\") and delete its device before stopping (default with --ephemeral)")
	if _, err := f.parse(args); err != nil {
		return err
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	t0 := time.Now()
	res := downResult{Killed: []string{}, Timings: map[string]float64{}}
	if !s.supervisorAlive() {
		if *service && serviceIsFor(s) {
			removed, err := removeService()
			if err != nil {
				return err
			}
			res.ServiceRemoved = removed
		}
		emit(f, res, func() {
			fmt.Println("twd is not running in " + s.Dir)
			if res.ServiceRemoved {
				fmt.Println("✓ removed  " + serviceUnitPath())
			}
		})
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if (*purge || s.Cfg.Ephemeral) && s.Cfg.TenantID != "" && s.mgmtHealthy(ctx) {
		// Every tenant this host serves (each kill goes to its own tailnet).
		var objs []mgmt.Object
		for _, tn := range s.tenants() {
			o, err := s.objectsOf(ctx, tn.TenantID)
			if err == nil {
				objs = append(objs, o...)
			}
		}
		{
			live := map[string]bool{}
			for _, o := range objs {
				if o.Status != "killed" && o.Status != "failed" {
					live[o.ID] = true
				}
			}
			for _, o := range objs {
				if !live[o.ID] || live[o.ParentID] {
					continue // dead, or killed with its root (cascade)
				}
				var kr struct {
					Killed []string `json:"killed"`
				}
				if err := s.mgmt().DoHeaders(ctx, "DELETE", "/v1/objects/"+o.ID+"?cascade=true&reason="+url.QueryEscape("twd down --purge"), nil,
					&kr, map[string]string{"Twd-Actor": actor()}); err == nil {
					res.Killed = append(res.Killed, kr.Killed...)
				}
			}
		}
		var fr struct {
			Deleted []string `json:"deleted"`
		}
		s.mgmt().Do(ctx, "POST", "/v1/device-delete-runs", nil, &fr)
		res.Flushed = len(fr.Deleted)
		// Forget the killed objects (mgmt rows, hostd tombstones) so the
		// same workload deploys again on this state dir.
		for _, tn := range s.tenants() {
			var pr mgmt.PurgeResult
			if err := s.mgmt().Do(ctx, "DELETE", "/v1/tenants/"+tn.TenantID+"/objects", nil, &pr); err == nil {
				res.Purged += len(pr.Purged)
			}
		}
		s.hostd().Do(ctx, "POST", "/v1/purge", nil, nil)
		res.Timings["purge"] = ms(time.Since(t0))
	}
	pid := s.supervisorPID()
	inst, act := serviceOf(s) // never another state dir's service (e.g. the real ~/.twd while tests run)
	switch {
	case *service && inst:
		// disable --now stops it (the supervisor's own SIGTERM path) and
		// keeps it from starting again at login/boot.
		removed, err := removeService()
		if err != nil {
			return err
		}
		res.ServiceRemoved = removed
	case inst && act:
		// Stop through the service manager so it doesn't count as a
		// failure; the unit stays installed (and enabled: it starts at the
		// next login/boot).
		if err := serviceStop(); err != nil {
			return err
		}
		res.ServiceStopped = true
	default:
		syscall.Kill(pid, syscall.SIGTERM)
	}
	for pidAlive(pid) {
		if time.Since(t0) > 3*time.Minute {
			syscall.Kill(pid, syscall.SIGKILL)
			return fail("stop_timeout", "the supervisor did not stop in 3 min; killed it", "log: "+s.log("supervisor"))
		}
		time.Sleep(50 * time.Millisecond)
	}
	res.Timings["total"] = ms(time.Since(t0))
	emit(f, res, func() {
		if len(res.Killed) > 0 || res.Purged > 0 {
			fmt.Printf("✓ purged   %d objects killed · %d devices deleted · %d killed objects forgotten\n", len(res.Killed), res.Flushed, res.Purged)
		}
		fmt.Printf("✓ stopped  twd-hostd, twd-mgmt (%.1fs) · state kept in %s\n", res.Timings["total"]/1000, s.Dir)
		if res.ServiceRemoved {
			fmt.Println("✓ removed  " + serviceUnitPath() + " (disabled)")
		} else if res.ServiceStopped {
			fmt.Println("› service  " + serviceUnit + " stopped but still installed (starts at next login/boot); `twd down --service` removes it")
		}
	})
	return nil
}

func actor() string {
	u := os.Getenv("USER")
	if u == "" {
		u = "local"
	}
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	return u + "@" + h + " (twd cli)"
}

// ---- status ----

type tenantStatus struct {
	Tailnet   string         `json:"tailnet"`
	TenantID  string         `json:"tenantID"`
	MintQueue int            `json:"mintQueue"`
	Rate      float64        `json:"registrationsPerMinute"`
	Objects   map[string]int `json:"objectsByStatus"`
}

type statusResult struct {
	Transport  string                 `json:"transport"`
	HostStatus string                 `json:"hostStatus,omitempty"`
	StateDir   string                 `json:"stateDir"`
	Tailnet    string                 `json:"tailnet"`
	MintMode   string                 `json:"mintMode"`
	MgmtNode   string                 `json:"mgmtNode,omitempty"`
	TenantID   string                 `json:"tenantID"`
	Tenants    []tenantStatus         `json:"tenants"`
	MgmtSocket string                 `json:"mgmtSocket"`
	Supervisor bool                   `json:"supervisor"`
	Mgmt       bool                   `json:"mgmt"`
	Host       bool                   `json:"host"`
	HostAgeMS  int64                  `json:"hostHeartbeatAgeMs"`
	Pending    int                    `json:"pendingDeviceDeletes"`
	MintQueue  int                    `json:"mintQueue"`
	Rate       float64                `json:"registrationsPerMinute"`
	Workloads  []hostd.WorkloadStatus `json:"workloads"`
	Objects    map[string]int         `json:"objectsByStatus"`
	Healthy    bool                   `json:"healthy"`
}

func cmdStatus(args []string) error {
	f := newFlags("status")
	if _, err := f.parse(args); err != nil {
		return err
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	if s.remote() {
		return remoteStatus(f, s)
	}
	if !s.configured() {
		return fail("not_up", "twd is not set up in "+s.Dir).withFix("twd up   (on the host)", "twd link <twd-mgmt.<tailnet>.ts.net>   (on a developer machine)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := statusResult{StateDir: s.Dir, Tailnet: s.Cfg.Tailnet, TenantID: s.Cfg.TenantID, Objects: map[string]int{}, MgmtSocket: s.mgmtSock(),
		MintMode: s.Cfg.mintMode(), MgmtNode: s.Cfg.MgmtDNSName}
	r.Supervisor = s.supervisorAlive()
	if r.Supervisor && s.mgmtHealthy(ctx) {
		r.Mgmt = true
		var st struct {
			Pending int `json:"pendingDeviceDeletes"`
		}
		s.mgmt().Do(ctx, "GET", "/v1/status", nil, &st)
		r.Pending = st.Pending
		var tn struct {
			Q    int     `json:"mintQueue"`
			Rate float64 `json:"effectiveRegistrationsPerMinute"`
		}
		s.mgmt().Do(ctx, "GET", "/v1/tenants/"+s.Cfg.TenantID, nil, &tn)
		r.MintQueue, r.Rate = tn.Q, tn.Rate
		var h struct {
			ID       string `json:"hostID"`
			LastSeen int64  `json:"lastSeen"`
			Status   string `json:"status"`
		}
		if s.mgmt().Do(ctx, "GET", "/v1/hosts/"+s.Cfg.HostID, nil, &h) == nil {
			r.HostAgeMS = time.Since(time.UnixMilli(h.LastSeen)).Milliseconds()
			r.Host = h.Status == "live"
			r.HostStatus = h.Status
		} else if !s.Cfg.LocalDev && !isFile(filepath.Join(s.hostTsnetDir(), "tailscaled.state")) {
			r.HostStatus = "pending" // hostd waits for its join key (keyless: minted once the trust credential exists)
		}
	}
	r.Transport = s.transport()
	var hv hostd.StatusView
	hostOK := r.Supervisor && s.hostd().Do(ctx, "GET", "/v1/status", nil, &hv) == nil
	if hostOK {
		r.Workloads = hv.Workloads
		for _, o := range hv.Objects {
			r.Objects[o.Status]++
		}
	} else {
		r.Host = false
	}
	for _, t := range s.tenants() {
		ts := tenantStatus{Tailnet: t.Tailnet, TenantID: t.TenantID, Objects: map[string]int{}}
		if r.Mgmt {
			var tn struct {
				Q    int     `json:"mintQueue"`
				Rate float64 `json:"effectiveRegistrationsPerMinute"`
			}
			s.mgmt().Do(ctx, "GET", "/v1/tenants/"+t.TenantID, nil, &tn)
			ts.MintQueue, ts.Rate = tn.Q, tn.Rate
		}
		for _, o := range hv.Objects {
			if o.TenantID == t.TenantID {
				ts.Objects[o.Status]++
			}
		}
		r.Tenants = append(r.Tenants, ts)
	}
	r.Healthy = r.Supervisor && r.Mgmt && r.Host
	emit(f, r, func() {
		mark := func(ok bool) string {
			if ok {
				return "✓"
			}
			return "✗"
		}
		fmt.Printf("twd status · %s · tailnet %s · mint mode %s · transport %s\n", s.Dir, s.Cfg.Tailnet, r.MintMode, r.Transport)
		fmt.Printf("%s supervisor  pid %d\n", mark(r.Supervisor), s.supervisorPID())
		if r.MgmtNode != "" {
			fmt.Printf("%s node        %s (%s; API over the tailnet, WhoIs auth)\n", mark(r.Mgmt), r.MgmtNode, mgmt.MgmtTag)
		}
		fmt.Printf("%s mgmt        tenant %s · mint queue %d · %.0f registrations/min · %d pending device deletes\n", mark(r.Mgmt), r.TenantID, r.MintQueue, r.Rate, r.Pending)
		fmt.Printf("%s host        %s · heartbeat %.1fs ago · objects %v\n", mark(r.Host), s.Cfg.HostID, float64(r.HostAgeMS)/1000, r.Objects)
		if len(r.Tenants) > 1 {
			for _, t := range r.Tenants {
				fmt.Printf("  tenant      %s (%s) · mint queue %d · %.0f registrations/min · objects %v\n", t.Tailnet, t.TenantID, t.MintQueue, t.Rate, t.Objects)
			}
		}
		for _, w := range r.Workloads {
			e := ""
			if w.Error != "" {
				e = " · " + w.Error
			}
			sb := ""
			if w.Sandbox != "" {
				sb = ", sandbox " + w.Sandbox
			}
			if w.Egress != "" {
				sb += ", egress " + w.Egress
				if w.Egress == hostd.EgressUnenforced {
					sb += " (Node tier: the egress proxy is cooperative here, not a boundary)"
				}
			}
			fmt.Printf("%s workload    %s %s (tenant %s, %s, epoch %d, %d requests%s)%s\n", mark(w.Status == "active"), w.Workload, w.Status, w.TenantID, w.DeploymentID, w.Epoch, w.Requests, sb, e)
		}
	})
	if !r.Healthy {
		return fail("unhealthy", "twd is not healthy in "+s.Dir).withFix("twd logs --component supervisor", "twd up")
	}
	return nil
}

// remoteStatus is `twd status` on a linked machine: who we are to mgmt,
// its tenants and hosts, and the latest deployments.
func remoteStatus(f *flags, s *state) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	w, err := s.whoami(ctx)
	if err != nil {
		return err
	}
	hosts, _ := listAll[mgmt.Host](ctx, s.mgmt(), "/v1/hosts", 0)
	type tenantView struct {
		TenantRef
		Deployments []mgmt.DeploymentView `json:"deployments"`
	}
	var tenants []tenantView
	for _, t := range s.tenants() {
		tv := tenantView{TenantRef: t}
		tv.Deployments, _ = listAll[mgmt.DeploymentView](ctx, s.mgmt(), "/v1/deployments?latest=true&tenant="+url.QueryEscape(t.TenantID), 0)
		tenants = append(tenants, tv)
	}
	live := 0
	for _, h := range hosts {
		if h.Live(time.Now()) {
			live++
		}
	}
	res := map[string]any{"linked": s.Link, "whoami": w, "hosts": hosts, "tenants": tenants, "healthy": live > 0}
	emit(f, res, func() {
		fmt.Printf("twd status · linked to %s · tailnet %s · you are %s (admin by WhoIs)\n", s.Link.Endpoint, w.Mgmt.Tailnet, w.Actor)
		for _, h := range hosts {
			mark := "✗"
			if h.Live(time.Now()) {
				mark = "✓"
			}
			fmt.Printf("%s host        %s · heartbeat %.1fs ago · %d live nodes · tiers %s\n", mark, h.ID, time.Since(time.UnixMilli(h.LastSeen)).Seconds(), h.LiveNodes, strings.Join(h.Tiers, ","))
		}
		for _, t := range tenants {
			fmt.Printf("✓ tenant      %s (%s, mint mode %s)\n", t.Tailnet, t.TenantID, mintModeName(t.MintMode))
			for _, d := range t.Deployments {
				by := ""
				if d.DeployedBy != "" {
					by = " · by " + d.DeployedBy
				}
				fmt.Printf("  workload    %s %s · http://%s/%s\n", d.Workload, d.Status, d.Endpoint.MagicDNS, by)
			}
		}
	})
	if live == 0 {
		return fail("unhealthy", "no live host at "+s.Link.Endpoint).withFix("twd status on the host")
	}
	return nil
}

// tierLine describes the isolation tiers the running host offers.
// tierLine says which tiers this host offers and how each isolates
// tenants: the process tier by the OS sandbox (Seatbelt on macOS,
// Landlock+seccomp on Linux; "none" is loud), the microVM tier by the
// Firecracker jailer + VM.
func tierLine(s *state) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var hv hostd.StatusView
	_ = s.hostd().Do(ctx, "GET", "/v1/status", nil, &hv)
	sb := hv.Sandbox
	switch {
	case sb == "":
		sb = "sandbox unknown"
	case sb == "none":
		sb = "NO PROCESS SANDBOX: see `twd doctor`"
	case strings.HasPrefix(sb, "landlock"):
		sb = "Landlock+seccomp, " + sb
	case sb == "seatbelt":
		sb = "Seatbelt, dev-grade isolation"
	}
	if runtime.GOOS == "darwin" {
		if slices.Contains(hv.Tiers, "microvm") {
			return "tiers: process [" + sb + "], microvm [Virtualization.framework, one VMM + VM service per guest, via " + hv.VMVia + "; \"tier\": \"microvm\" in twd.jsonc]"
		}
		return "tier: process [" + sb + "]"
	}
	if slices.Contains(hv.Tiers, "microvm") {
		return "tiers: process [" + sb + "], microvm [Firecracker, jailed, via " + hv.VMVia + "; \"tier\": \"microvm\" in twd.jsonc]"
	}
	return "tier: process [" + sb + "; microVM tier off: see `twd doctor`, or run `sudo ~/.twd/bin/twd host setup --microvm` once]"
}
