//go:build e2eoauth

// E2E-OAUTH (M12) on tailnet 1: `twd up --oauth-client-file` (mint mode
// oauth, the default for self-hosted installs): no browser, no Funnel, no
// public issuer and no API key given to twd. A real OAuth client (scopes
// auth_keys + devices:core + policy_file:read, tag tag:twd) mints mgmt's
// own node key, the host's node key and every object's key. Then the
// E2E-MAC scenario (a)–(f), `twd link` from a tsnet developer node over
// https AND http, a remote deploy / kill / audit, `twd down`, and zero
// leftovers (devices, keys, the OAuth client itself).
//
// The API key in TWD_E2E_API_KEY_FILE is used ONLY to (a) create and delete
// the OAuth client, (b) mint the test's own client/echo/CLI node keys, (c)
// apply the policy additions (validated, If-Match) and (d) clean up by ID.
package oauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/e2e/cliflow"
	"github.com/rosskukulinski/twd/internal/policy"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/mgmt"
	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
)

const prodControl = "https://controlplane.tailscale.com"

var allowedTailnets = map[string]bool{"tail7906d5.ts.net": true}

// Policy is what this scenario needs on tailnet 1: cliflow.Policy (which
// includes policy.InfraSnippet: tag:twd owns tag:twd-mgmt and
// tag:twd-host) plus the admin grants (autogroup:admin from the operator
// snippet; tag:ttest for the linked CLI node).
func Policy() policy.Snippet {
	s := cliflow.Policy()
	s.Grants = append(s.Grants,
		policy.AdminGrant("autogroup:admin", mgmt.MgmtTag, mgmt.AdminCap),
		policy.AdminGrant(cliflow.TestTag, mgmt.MgmtTag, mgmt.AdminCap))
	return s
}

func TestE2EOAuth(t *testing.T) {
	keyFile, tailnet := os.Getenv("TWD_E2E_API_KEY_FILE"), os.Getenv("TWD_E2E_TAILNET")
	if keyFile == "" || tailnet == "" {
		t.Skip("TWD_E2E_API_KEY_FILE / TWD_E2E_TAILNET not set")
	}
	if !allowedTailnets[tailnet] {
		t.Fatalf("refusing to run against tailnet %q (not in the allow list)", tailnet)
	}
	wd, err := nettest.WorkerdBinary()
	if err != nil {
		t.Skip(err)
	}
	t.Setenv("TWD_WORKERD", wd)
	apiKey, err := tsapi.LoadEnvFile(keyFile, "TS_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	api := &tsapi.Client{Tailnet: tailnet, APIKey: apiKey}
	b := make([]byte, 3)
	rand.Read(b)
	run := hex.EncodeToString(b)
	testPrefix, twdPrefix, label := cliflow.RunPrefixes("m12", run)
	root, _ := nettest.FindRepoRoot()
	res := cliflow.NewResults(label+"-"+run, tailnet)
	resultsTo := filepath.Join(root, "e2e", "real", "results", "m12-oauth-"+time.Now().Format("20060102-150405")+"-"+run+".json")
	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Minute)
	defer cancel()
	check := func(name string, ok bool, detail string) {
		t.Helper()
		if ok {
			res.Pass(name, detail)
			return
		}
		res.Checks[name] = "FAIL " + detail
		t.Fatalf("%s: %s", name, detail)
	}

	// 1. Policy additions (no-op if present); API key use (c).
	ar, err := policy.ApplyWith(ctx, api, Policy(), policy.Options{AllowTags: []string{cliflow.TestTag}, AllowSrcs: []string{"autogroup:admin"}})
	if err != nil {
		t.Fatalf("policy: %v", netd.RedactError(err))
	}
	if ar.Changed {
		bk := filepath.Join(os.Getenv("HOME"), ".config", "tailnet-agents-design", "tailnet1_acl.pre-m12-"+run+".hujson")
		os.WriteFile(bk, ar.Before, 0o600)
		res.Notes = append(res.Notes, "policy changed (validated, If-Match "+ar.OldETag+"): added "+ar.Added.HuJSON()+" (previous copy: "+bk+")")
	} else {
		res.Notes = append(res.Notes, "policy already had the scenario's tagOwners and grants (not written)")
	}

	// 2. The OAuth client (API key use (a)): what the admin generates in
	// the console. Its secret goes only into a 0600 file.
	work := t.TempDir()
	oc, err := api.CreateOAuthClient(ctx, []string{"auth_keys", "devices:core", "policy_file:read"}, []string{"tag:twd"}, testPrefix+"oauth")
	check("create_oauth_client", err == nil, fmt.Sprint(netd.RedactError(err)))
	clientFile := filepath.Join(work, "oauth.env")
	if err := os.WriteFile(clientFile, []byte("TS_OAUTH_CLIENT_ID="+oc.ID+"\nTS_OAUTH_CLIENT_SECRET="+oc.Secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oc.Secret = ""

	quiet := slog.New(netd.NewRedactingHandler(slog.NewTextHandler(io.Discard, nil)))
	minter := tsapi.MintSource(api, quiet)
	tracker := tsapi.NewTracker(api)
	bin := cliflow.BuildBinaries(t)
	state := filepath.Join("/tmp", "twd-e2e-m12-"+run)
	var nodes []*nettest.Node
	newNode := func(role string, tags []string) *nettest.Node {
		k, err := minter.AuthKey(ctx, netd.KeyRequest{ObjectID: role, Tags: tags, Ephemeral: true})
		if err != nil {
			t.Fatalf("mint %s: %v", role, netd.RedactError(err))
		}
		n, err := nettest.NewNode(ctx, prodControl, k, testPrefix+role, filepath.Join(work, role))
		if err != nil {
			t.Fatalf("%s: %v", role, netd.RedactError(err))
		}
		nodes = append(nodes, n)
		if st, err := n.LC.StatusWithoutPeers(ctx); err == nil && st.Self != nil {
			tracker.Track(string(st.Self.ID), role)
		}
		return n
	}
	e := &cliflow.Env{T: t, BinDir: bin, StateDir: state, WorkDir: work, Tailnet: tailnet, OAuthClientFile: clientFile,
		ControlURL: prodControl, Prefix: twdPrefix, Res: res, ClientTagged: true, PolicyGrantsParentRefund: true,
		NewNode: func(role string) *nettest.Node { return newNode(role, []string{cliflow.TestTag}) }}
	keyTo := func(name string, tags []string) string {
		k, err := api.CreateKey(ctx, tsapi.KeyOptions{Tags: tags, Ephemeral: true, Preauthorized: true, ExpirySeconds: 900, Description: testPrefix + name})
		if err != nil {
			t.Fatalf("mint %s key: %v", name, netd.RedactError(err))
		}
		res.KeyIDs = append(res.KeyIDs, k.ID)
		p := filepath.Join(work, name+".authkey")
		os.WriteFile(p, []byte("TS_AUTHKEY="+k.Secret+"\n"), 0o600)
		return p
	}
	downDone := false
	t.Cleanup(func() {
		if !downDone {
			if _, se, _, err := e.Twd("down", "--purge"); err != nil {
				t.Logf("twd down (cleanup): %v %s", err, netd.Redact(se))
			}
		}
		for _, c := range []string{"supervisor", "mgmt", "hostd"} {
			lb, _ := os.ReadFile(filepath.Join(state, "logs", c+".log"))
			if t.Failed() {
				t.Logf("%s log (tail):\n%s", c, netd.Redact(string(lb[max(0, len(lb)-8000):])))
			}
		}
		for _, n := range nodes {
			n.Close()
		}
		cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer ccancel()
		devs, keys := cliflow.Leftovers(cctx, api, res)
		res.Cleanup["twd_devices_tracked"] = len(res.NodeIDs)
		res.Cleanup["twd_devices_left_after_down"] = len(devs)
		res.Cleanup["twd_keys_valid_after_down"] = len(keys)
		for _, id := range res.NodeIDs {
			tracker.Track(id, "twd")
		}
		for _, id := range res.InfraNodeIDs {
			tracker.Track(id, "twd-infra") // mgmt's and the host's own nodes: kept by twd down, deleted here by ID
		}
		time.Sleep(3 * time.Second)
		crep := tracker.Cleanup(cctx, testPrefix)
		// twd's own nodes are named twd-<label>-<run>-: report any left.
		var untrackedTwd []string
		if all, err := api.ListDevices(cctx); err == nil {
			for _, d := range all {
				if strings.HasPrefix(d.Hostname, twdPrefix) || strings.HasPrefix(d.Name, twdPrefix) || strings.HasPrefix(d.Hostname, "twd-host-"+strings.TrimSuffix(twdPrefix, "-")) {
					untrackedTwd = append(untrackedTwd, d.Name)
				}
			}
		}
		for _, k := range keys {
			api.DeleteKey(cctx, k)
		}
		var errs []string
		if oc.ID != "" {
			if err := api.DeleteKey(cctx, oc.ID); err != nil {
				errs = append(errs, "oauth client "+oc.ID+": "+netd.Redact(err.Error()))
			} else if _, err := api.GetKey(cctx, oc.ID); err == nil {
				if k, _ := api.GetKey(cctx, oc.ID); k.Revoked == "" && !k.Invalid {
					errs = append(errs, "oauth client "+oc.ID+" still valid after delete")
				}
			}
		}
		if err := minter.RevokeAll(cctx); err != nil {
			errs = append(errs, netd.Redact(err.Error()))
		}
		for _, id := range res.KeyIDs {
			api.DeleteKey(cctx, id)
		}
		res.Cleanup["devices_tracked"] = crep.Tracked
		res.Cleanup["devices_deleted_by_cleanup"] = crep.Deleted
		res.Cleanup["devices_left"] = crep.Left
		res.Cleanup["untracked_run_devices"] = crep.Untracked
		res.Cleanup["twd_named_devices_left"] = untrackedTwd
		res.Cleanup["oauth_client_deleted"] = oc.ID
		res.Cleanup["errors"] = errs
		os.RemoveAll(state)
		os.MkdirAll(filepath.Dir(resultsTo), 0o755)
		out, _ := json.MarshalIndent(res, "", "  ")
		out = []byte(netd.Redact(string(out)))
		os.WriteFile(resultsTo, out, 0o644)
		t.Logf("results: %s\n%s", resultsTo, out)
		if len(devs) > 0 || len(keys) > 0 {
			t.Errorf("after twd down: leftover twd devices %v, valid keys %d", devs, len(keys))
		}
		if crep.Left != 0 || len(crep.Untracked) > 0 || len(untrackedTwd) > 0 || len(errs) > 0 {
			t.Errorf("cleanup: %+v twd-named left %v errors %v", crep, untrackedTwd, errs)
		}
	})

	// 3. twd up --oauth-client-file … then (a)–(f), all from the host's CLI.
	t0 := time.Now()
	e.Run()
	res.Numbers["scenario_ms"] = float64(time.Since(t0).Milliseconds())
	var cfg struct {
		Tailnet, MintMode, APIKeyFile, OAuthClientFile, OAuthClientID, IssuerURL, MgmtDNSName string
		HTTPS                                                                                 bool
	}
	cb, _ := os.ReadFile(filepath.Join(state, "twd.json"))
	json.Unmarshal(cb, &cfg)
	check("oauth_mode_no_api_key_no_funnel", cfg.MintMode == "oauth" && cfg.APIKeyFile == "" && cfg.OAuthClientID == oc.ID && cfg.OAuthClientFile == clientFile &&
		cfg.IssuerURL == "" && cfg.Tailnet == tailnet && !strings.Contains(string(cb), "tskey-"),
		fmt.Sprintf("twd.json: mode %s, apiKeyFile %q, client %s, issuer %q, tailnet %s (discovered)", cfg.MintMode, cfg.APIKeyFile, cfg.OAuthClientID, cfg.IssuerURL, cfg.Tailnet))
	var doc struct {
		Checks []struct{ Name, Status, Detail string }
	}
	e.MustJSON(&doc, "doctor")
	dmode := ""
	for _, c := range doc.Checks {
		if c.Name == "mint_mode" {
			dmode = c.Status + " " + c.Detail
		}
	}
	check("doctor_mint_mode_oauth", strings.HasPrefix(dmode, "ok oauth: OAuth client "+oc.ID), dmode)
	mlog, _ := os.ReadFile(filepath.Join(state, "logs", "mgmt.log"))
	check("no_secret_in_logs", !strings.Contains(string(mlog), "tskey-client-") && !strings.Contains(string(mlog), "-funnel"), "mgmt.log has no tskey-client- and no Funnel listener")

	// 4. twd link from a developer node (tag:ttest, admin cap) over https,
	// then a second developer state over plain http.
	dev := &cliflow.Env{T: t, BinDir: bin, StateDir: filepath.Join(work, "dev-state"), WorkDir: work, Res: res}
	cliKey := keyTo("cli", []string{cliflow.TestTag})
	type link struct {
		Endpoint, Scheme, Tailnet, Actor, NodeID string
		Admin                                    bool
		Tenants                                  []struct{ TenantID, MintMode string }
	}
	var lk link
	d := dev.MustJSON(&lk, "link", cfg.MgmtDNSName, "--tsnet-dir", filepath.Join(work, "cli-node"), "--tsnet-hostname", testPrefix+"cli",
		"--control-url", prodControl, "--tsnet-authkey-file", cliKey, "--tsnet-tags", cliflow.TestTag)
	res.Numbers["link_https_ms"] = float64(d.Milliseconds())
	tracker.Track(lk.NodeID, "cli-node")
	check("link_https", lk.Admin && lk.Scheme == "https" && lk.Endpoint == "https://"+cfg.MgmtDNSName && len(lk.Tenants) == 1 && lk.Tenants[0].MintMode == "oauth",
		fmt.Sprintf("%+v in %v (no scheme given: https first)", lk, d.Round(time.Millisecond)))
	dev2 := &cliflow.Env{T: t, BinDir: bin, StateDir: filepath.Join(work, "dev2-state"), WorkDir: work, Res: res}
	cli2Key := keyTo("cli2", []string{cliflow.TestTag})
	var lk2 link
	d = dev2.MustJSON(&lk2, "link", "http://"+cfg.MgmtDNSName, "--tsnet-dir", filepath.Join(work, "cli2-node"), "--tsnet-hostname", testPrefix+"cli2",
		"--control-url", prodControl, "--tsnet-authkey-file", cli2Key, "--tsnet-tags", cliflow.TestTag)
	res.Numbers["link_http_ms"] = float64(d.Milliseconds())
	tracker.Track(lk2.NodeID, "cli2-node")
	check("link_http", lk2.Admin && lk2.Scheme == "http" && lk2.Endpoint == "http://"+cfg.MgmtDNSName, fmt.Sprintf("%+v in %v (plain HTTP on :80, WhoIs)", lk2, d.Round(time.Millisecond)))
	var ls2 struct {
		Deployments []struct{ Workload string }
	}
	dev2.MustJSON(&ls2, "ls")
	check("remote_ls_over_http", len(ls2.Deployments) >= 2, fmt.Sprintf("%d deployments over http", len(ls2.Deployments)))

	// 5. Remote deploy (bundled on the dev box, uploaded over https), ls by
	// the WhoIs actor, remote kill (device deleted via devices:core), audit.
	rdir := filepath.Join(work, "remote", "mcp2")
	dev.MustJSON(nil, "init", rdir, "--template", "mcp")
	var dep struct {
		DeploymentID string
		Endpoint     struct{ Node, MagicDNS string }
		Timings      map[string]float64 `json:"timingsMs"`
	}
	d = dev.MustJSON(&dep, "deploy", rdir)
	res.Numbers["remote_deploy_cli_ms"] = float64(d.Milliseconds())
	e.TrackObjectNodes()
	check("remote_deploy", dep.Endpoint.MagicDNS != "" && dep.Endpoint.Node != "", fmt.Sprintf("mcp2 %s in %v (node minted by the OAuth client)", dep.Endpoint.MagicDNS, d.Round(time.Millisecond)))
	var ls struct {
		Deployments []struct{ Workload, DeployedBy string }
	}
	dev.MustJSON(&ls, "ls")
	by := ""
	for _, dp := range ls.Deployments {
		if dp.Workload == "mcp2" {
			by = dp.DeployedBy
		}
	}
	check("remote_ls_deployed_by", by == lk.Actor && strings.Contains(by, lk.NodeID), "deployedBy "+by)
	var kr struct {
		Result struct {
			Killed          []string
			DevicesDeleted  []string `json:"devicesDeleted"`
			DevicesDeleting []string `json:"devicesDeleting"`
			Warnings        []struct{ Code string }
		}
	}
	d = dev.MustJSON(&kr, "kill", dep.Endpoint.Node, "--cascade", "--reason", "m12 remote kill")
	res.Numbers["remote_kill_cli_ms"] = float64(d.Milliseconds())
	check("remote_kill", len(kr.Result.Killed) == 1 && len(kr.Result.DevicesDeleted)+len(kr.Result.DevicesDeleting) == 1 && len(kr.Result.Warnings) == 0,
		fmt.Sprintf("killed %v, device delete %v/%v, warnings %v in %v", kr.Result.Killed, kr.Result.DevicesDeleted, kr.Result.DevicesDeleting, kr.Result.Warnings, d.Round(time.Millisecond)))
	var aud struct {
		Verified bool
		Records  []struct {
			Kind, ActingFor string
			ObjectID        string `json:"object"`
		}
	}
	dev.MustJSON(&aud, "audit", "--all")
	var mints int
	deployBy, killed := false, false
	for _, r := range aud.Records {
		switch {
		case r.Kind == "mint":
			mints++
		case r.Kind == "deploy" && r.ActingFor == lk.Actor:
			deployBy = true
		case r.Kind == "kill" && r.ObjectID == dep.Endpoint.Node:
			killed = true
		}
	}
	check("remote_audit", aud.Verified && mints >= 3 && deployBy && killed, fmt.Sprintf("verified=%v mints=%d deployBy=%v kill=%v", aud.Verified, mints, deployBy, killed))

	// 6. Down (purge: ephemeral) and leftovers (in Cleanup).
	e.TrackObjectNodes()
	e.Down()
	downDone = true
	time.Sleep(2 * time.Second)
}
