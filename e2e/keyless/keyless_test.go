//go:build e2ekeyless

// E2E-KEYLESS (M10, ADR-026) on tailnet 1: `twd up` in keyless mode, so
// twd-mgmt never sees a Tailscale API key: mgmt's own tagged node, the
// OIDC issuer over Funnel, a trust credential, and every object node
// minted through WIF. Then a developer machine, played by a second state
// dir whose CLI dials through its own tsnet node (tag:ttest, granted
// twd.dev/cap/admin): `twd link` → remote deploy / ls / kill / audit, with
// deployedBy = the WhoIs principal; a tag:ttest-noadmin node is refused
// with the grant snippet.
//
// The API key in TWD_E2E_API_KEY_FILE is used ONLY (a) to create and
// delete the trust credentials this run needs, (b) to mint the auth keys
// of mgmt's node and the test clients (an interactive browser login can't
// be automated here; the product flow prints a login URL instead), and
// (c) to apply the policy additions (validated, If-Match) and clean up by
// ID. twd itself gets none of it: twd.json has no apiKeyFile.
package keyless

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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

// NoAdminTag is a test tag that may reach mgmt but holds no admin cap.
const NoAdminTag = "tag:ttest-noadmin"

// Policy is what this scenario adds to tailnet 1 (on top of cliflow.Policy):
// the keyless base (mgmt/host/issuer tags, Funnel on the issuer's own node
// only, autogroup:admin's admin cap),
// the admin cap for tag:ttest (the linked CLI) and a non-admin tag.
func Policy() policy.Snippet {
	s := cliflow.Policy() // includes policy.InfraSnippet: twd/mgmt/host tagOwners + host ⇄ mgmt grants
	s.TagOwners[NoAdminTag] = []string{"autogroup:admin"}
	adminApp := map[string]any{mgmt.AdminCap: []any{map[string]any{}}}
	s.Grants = append(s.Grants,
		map[string]any{"src": []string{"autogroup:admin"}, "dst": []string{mgmt.MgmtTag}, "ip": []string{"tcp:443", "tcp:80"}, "app": adminApp},
		map[string]any{"src": []string{cliflow.TestTag}, "dst": []string{mgmt.MgmtTag}, "ip": []string{"tcp:443", "tcp:80"}, "app": adminApp},
		map[string]any{"src": []string{NoAdminTag}, "dst": []string{mgmt.MgmtTag}, "ip": []string{"tcp:443", "tcp:80"}},
	)
	s.TagOwners[mgmt.IssuerTag] = []string{"autogroup:admin", "tag:twd"}
	s.NodeAttrs = []map[string]any{{"target": []string{mgmt.IssuerTag}, "attr": []string{"funnel"}}}
	return s
}

func publicHTTP() *http.Client {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", "1.1.1.1:53")
	}}
	d := &net.Dialer{Timeout: 10 * time.Second, Resolver: r}
	return &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{DialContext: d.DialContext, DisableKeepAlives: true}}
}

func TestE2EKeyless(t *testing.T) {
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
	testPrefix, twdPrefix, _ := cliflow.RunPrefixes("m10", run)
	root, _ := nettest.FindRepoRoot()
	res := cliflow.NewResults("m10-"+run, tailnet)
	resultsTo := filepath.Join(root, "e2e", "real", "results", "m10-"+time.Now().Format("20060102-150405")+"-"+run+".json")
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

	// 1. Policy additions (no-op if present); API key use (c). Only the
	// issuer node's tagOwner and funnel nodeAttr may be new (approved for
	// the issuer move); anything else missing means the tailnet drifted.
	if cur, _, err := api.GetPolicyHuJSON(ctx); err != nil {
		t.Fatalf("read policy: %v", netd.RedactError(err))
	} else if miss, err := policy.Missing(cur, Policy()); err != nil {
		t.Fatal(err)
	} else {
		for k := range miss.TagOwners {
			if k != mgmt.IssuerTag {
				t.Fatalf("refusing to write the policy: it is also missing tagOwner %s", k)
			}
		}
		if len(miss.Grants) > 0 {
			t.Fatalf("refusing to write the policy: it is also missing grants %s", miss.HuJSON())
		}
		for _, n := range miss.NodeAttrs {
			if b, _ := json.Marshal(n); !strings.Contains(string(b), `"`+mgmt.IssuerTag+`"`) || strings.Contains(string(b), `"`+mgmt.MgmtTag+`"`) {
				t.Fatalf("refusing to write the policy: unexpected nodeAttr %s", b)
			}
		}
	}
	ar, err := policy.ApplyWith(ctx, api, Policy(), policy.Options{AllowTags: []string{cliflow.TestTag, NoAdminTag}, AllowSrcs: []string{"autogroup:admin"}})
	if err != nil {
		t.Fatalf("policy: %v", netd.RedactError(err))
	}
	if ar.Changed {
		bk := filepath.Join(os.Getenv("HOME"), ".config", "tailnet-agents-design", "tailnet1_acl.pre-m10-"+run+".hujson")
		os.WriteFile(bk, ar.Before, 0o600)
		res.Notes = append(res.Notes, "policy changed (validated, If-Match "+ar.OldETag+"): added "+ar.Added.HuJSON()+" (previous copy: "+bk+")")
	} else {
		res.Notes = append(res.Notes, "policy already had the scenario's tagOwners, grants and nodeAttrs (not written)")
	}

	quiet := slog.New(netd.NewRedactingHandler(slog.NewTextHandler(io.Discard, nil)))
	minter := tsapi.MintSource(api, quiet)
	tracker := tsapi.NewTracker(api)
	bin := cliflow.BuildBinaries(t)
	state := filepath.Join("/tmp", "twd-e2e-m10-"+run)
	work := t.TempDir()
	var fedIDs []string
	var nodes []*nettest.Node
	host := &cliflow.Env{T: t, BinDir: bin, StateDir: state, WorkDir: work, Tailnet: tailnet, ControlURL: prodControl, Prefix: twdPrefix, Res: res, ClientTagged: true}
	dev := &cliflow.Env{T: t, BinDir: bin, StateDir: filepath.Join(work, "dev-state"), WorkDir: work, Res: res}
	noadmin := &cliflow.Env{T: t, BinDir: bin, StateDir: filepath.Join(work, "noadmin-state"), WorkDir: work, Res: res}
	// Auth keys for the nodes the test itself runs (API key use (b)).
	keyTo := func(name string, tags []string, ephemeral bool) string {
		k, err := api.CreateKey(ctx, tsapi.KeyOptions{Tags: tags, Ephemeral: ephemeral, Preauthorized: true, ExpirySeconds: 900, Description: testPrefix + name})
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
			if _, se, _, err := host.Twd("down", "--purge"); err != nil {
				t.Logf("twd down (cleanup): %v %s", err, netd.Redact(se))
			}
		}
		for _, c := range []string{"supervisor", "mgmt", "hostd"} {
			if t.Failed() {
				b, _ := os.ReadFile(filepath.Join(state, "logs", c+".log"))
				t.Logf("%s log (tail):\n%s", c, netd.Redact(string(b[max(0, len(b)-8000):])))
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
		// Ephemeral nodes (mgmt, the no-admin CLI) may already be gone; the
		// tracker deletes by ID and reports what this run left by prefix.
		time.Sleep(3 * time.Second)
		crep := tracker.Cleanup(cctx, testPrefix)
		for _, k := range keys {
			api.DeleteKey(cctx, k)
		}
		var errs []string
		for _, id := range fedIDs {
			if err := api.DeleteKey(cctx, id); err != nil {
				errs = append(errs, "trust credential "+id+": "+netd.Redact(err.Error()))
			}
		}
		if err := minter.RevokeAll(cctx); err != nil {
			errs = append(errs, netd.Redact(err.Error()))
		}
		for _, id := range res.KeyIDs {
			api.DeleteKey(cctx, id) // the test's own node keys (consumed or not)
		}
		res.Cleanup["devices_tracked"] = crep.Tracked
		res.Cleanup["devices_deleted_by_cleanup"] = crep.Deleted
		res.Cleanup["devices_left"] = crep.Left
		res.Cleanup["untracked_run_devices"] = crep.Untracked
		res.Cleanup["trust_credentials_deleted"] = len(fedIDs)
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
		if crep.Left != 0 || len(crep.Untracked) > 0 || len(errs) > 0 {
			t.Errorf("cleanup: %+v errors %v", crep, errs)
		}
	})

	// 2. `twd up` keyless, step 1: mgmt's node (unattended join with a key
	// minted by the test, standing in for the browser login) → the console
	// steps are printed and the stack keeps running, waiting for the IDs.
	mgmtKey := keyTo("mgmt", []string{mgmt.MgmtTag}, true)
	issuerKey := keyTo("issuer", []string{mgmt.IssuerTag}, true)
	upArgs := []string{"up", "--wif", "--yes", "--control-url", prodControl, "--hostname-prefix", twdPrefix, "--mgmt-hostname", testPrefix + "mgmt",
		"--mgmt-authkey-file", mgmtKey, "--issuer-authkey-file", issuerKey, "--ephemeral", "--allow-sibling-egress"}
	t1 := time.Now()
	so, se, d, err := host.Twd(append(upArgs, "--json")...)
	res.Numbers["up_step1_ms"] = float64(d.Milliseconds())
	check("up_waits_for_trust_credential", err != nil && strings.Contains(se, "wif_credential_required") && strings.Contains(se, "Trust credentials") &&
		strings.Contains(se, "auth_keys") && strings.Contains(se, mgmt.MintSubject("tn_"+strings.Split(tailnet, ".")[0])) && !strings.Contains(so, "Trust credentials"),
		fmt.Sprintf("err=%v stderr=%s stdout=%s", err, netd.Redact(se), netd.Redact(so)))
	var cfg struct {
		MintMode, APIKeyFile, MgmtDNSName, MgmtNodeID, IssuerURL, Tailnet string
		IssuerDNSName, IssuerNodeID                                       string
		HTTPS                                                             bool
	}
	cb, _ := os.ReadFile(filepath.Join(state, "twd.json"))
	json.Unmarshal(cb, &cfg)
	check("no_api_key_in_twd_state", cfg.MintMode == "wif" && cfg.APIKeyFile == "" && cfg.Tailnet == tailnet && cfg.HTTPS && cfg.MgmtDNSName != "",
		fmt.Sprintf("twd.json: %+v", cfg))
	// The issuer runs on its OWN node, never on mgmt's.
	check("issuer_on_own_node", cfg.IssuerDNSName != "" && cfg.IssuerDNSName != cfg.MgmtDNSName && cfg.IssuerURL == "https://"+cfg.IssuerDNSName &&
		cfg.IssuerNodeID != "" && cfg.IssuerNodeID != cfg.MgmtNodeID, fmt.Sprintf("issuer %s (%s), mgmt %s", cfg.IssuerDNSName, cfg.IssuerNodeID, cfg.MgmtDNSName))
	tracker.Track(cfg.MgmtNodeID, "mgmt-node")
	tracker.Track(cfg.IssuerNodeID, "issuer-node")
	tenantID := "tn_" + strings.Split(tailnet, ".")[0]
	issuer := cfg.IssuerURL + "/t/" + tenantID
	// mgmt is up; the host's own node waits for a join key that only the
	// trust credential can mint, so it is "pending" (twd status exits 1).
	var st struct {
		Mgmt, Host bool
		HostStatus string
		Transport  string
	}
	so, _, _, _ = host.Twd("status", "--json")
	json.Unmarshal([]byte(so), &st)
	check("stack_running_before_credential", st.Mgmt && !st.Host && st.HostStatus == "pending" && strings.HasPrefix(st.Transport, "tsnet"),
		fmt.Sprintf("mgmt up, host %q, transport %q while waiting for the trust credential", st.HostStatus, st.Transport))

	// 3. The issuer becomes public (Funnel; first HTTPS cert).
	pub := publicHTTP()
	t0 := time.Now()
	var disc struct{ Issuer string }
	for time.Since(t0) < 4*time.Minute {
		resp, err := pub.Get(issuer + "/.well-known/openid-configuration")
		if err == nil {
			json.NewDecoder(resp.Body).Decode(&disc)
			resp.Body.Close()
			if resp.StatusCode == 200 && disc.Issuer == issuer {
				break
			}
		}
		time.Sleep(3 * time.Second)
	}
	res.Numbers["funnel_first_fetch_ms"] = float64(time.Since(t0).Milliseconds())
	check("issuer_public_via_funnel", disc.Issuer == issuer, fmt.Sprintf("%s in %v", issuer, time.Since(t0).Round(time.Millisecond)))
	// mgmt's node answers nothing from the public internet (it never
	// listens on Funnel), while the issuer, on its own node, does.
	if r, err := pub.Get("https://" + cfg.MgmtDNSName + "/.well-known/openid-configuration"); err == nil {
		r.Body.Close()
		check("mgmt_not_public", false, fmt.Sprintf("https://%s answered %d from the public internet", cfg.MgmtDNSName, r.StatusCode))
	} else {
		check("mgmt_not_public", true, "https://"+cfg.MgmtDNSName+" unreachable from the public internet: "+netd.Redact(err.Error()))
	}

	// 4. The trust credentials (API key use (a)): what the admin creates in the console.
	mint, err := api.CreateFederated(ctx, tsapi.FederatedOptions{Scopes: []string{"auth_keys"}, Tags: []string{"tag:twd"},
		Issuer: issuer, Subject: mgmt.MintSubject(tenantID), Description: testPrefix + "mint"})
	check("create_mint_trust_credential", err == nil, fmt.Sprint(netd.RedactError(err)))
	fedIDs = append(fedIDs, mint.ID)
	life, err := api.CreateFederated(ctx, tsapi.FederatedOptions{Scopes: []string{"devices:core", "policy_file:read"}, Tags: []string{"tag:twd"},
		Issuer: issuer, Subject: mgmt.LifecycleSubject(tenantID), Description: testPrefix + "lifecycle"})
	check("create_lifecycle_trust_credential", err == nil, fmt.Sprint(netd.RedactError(err)))
	fedIDs = append(fedIDs, life.ID)

	// 5. `twd up` step 2: the IDs complete the setup; mgmt proves both
	// exchanges and reads the policy with the lifecycle credential.
	var up struct {
		TenantID, MintMode, MgmtNode string
		WIF                          struct {
			IssuerPublicMs float64 `json:"issuerPublicMs"`
			Check          mgmt.WIFCheckResult
			TenantCreated  bool `json:"tenantCreated"`
		} `json:"wif"`
		Timings map[string]float64 `json:"timingsMs"`
	}
	d = host.MustJSON(&up, append(upArgs, "--wif-client-id", mint.ID, "--wif-lifecycle-client-id", life.ID)...)
	res.Numbers["up_step2_ms"] = float64(d.Milliseconds())
	res.Numbers["up_total_ms"] = float64(time.Since(t1).Milliseconds())
	check("up_keyless_complete", up.TenantID == tenantID && up.MintMode == "wif (keyless)" && up.WIF.TenantCreated && up.WIF.Check.Mint &&
		up.WIF.Check.Lifecycle == "ok" && up.WIF.Check.PolicyRead && up.MgmtNode == cfg.MgmtDNSName,
		fmt.Sprintf("tenant %s mode %s wif %+v", up.TenantID, up.MintMode, up.WIF))
	var doc struct {
		Checks []struct{ Name, Status, Detail string }
	}
	host.MustJSON(&doc, "doctor")
	dmode, dkey := "", ""
	for _, c := range doc.Checks {
		switch c.Name {
		case "mint_mode":
			dmode = c.Status + " " + c.Detail
		case "api_key":
			dkey = c.Status + " " + c.Detail
		}
	}
	check("doctor_labels_keyless", strings.HasPrefix(dmode, "ok wif (keyless)") && strings.HasPrefix(dkey, "skip keyless"), dmode+" | "+dkey)
	// tsnet only: hostd on its own tag:twd-host node, no unix socket.
	dtr := ""
	for _, c := range doc.Checks {
		if c.Name == "transport" {
			dtr = c.Status + " " + c.Detail
		}
	}
	_, sockErr := os.Stat(filepath.Join(state, "run", "mgmt.sock"))
	var hosts []struct{ HostID, NodeID, Status, DNSName string }
	host.MustJSON(&hosts, "host", "ls")
	hostLive := len(hosts) == 1 && hosts[0].Status == "live" && hosts[0].NodeID != "" && strings.HasPrefix(hosts[0].DNSName, "twd-host-")
	if hostLive {
		tracker.Track(hosts[0].NodeID, "host-node")
	}
	check("transport_tsnet_only", strings.HasPrefix(dtr, "ok tsnet:") && sockErr != nil && hostLive,
		fmt.Sprintf("doctor %q · mgmt.sock exists=%v · hosts %+v", dtr, sockErr == nil, hosts))

	// 6. A local deploy on the host (mcp), nodes minted through WIF.
	mcpDir, agentDir := filepath.Join(work, "mcp"), filepath.Join(work, "agent")
	host.MustJSON(nil, "init", mcpDir, "--template", "mcp")
	type depRes struct {
		DeploymentID string
		Endpoint     struct{ Node, MagicDNS string }
		Timings      map[string]float64 `json:"timingsMs"`
	}
	var mcp depRes
	d = host.MustJSON(&mcp, "deploy", mcpDir)
	res.Numbers["deploy_mcp_ready_ms"] = mcp.Timings["ready"]
	check("deploy_local_wif", mcp.Endpoint.MagicDNS != "", fmt.Sprintf("mcp %s in %v", mcp.Endpoint.MagicDNS, d.Round(time.Millisecond)))
	host.TrackObjectNodes()

	// 7. The developer machine: `twd link` through its own tsnet node
	// (tag:ttest, which the policy grants the admin cap).
	cliKey := keyTo("cli", []string{cliflow.TestTag}, false)
	var lk struct {
		Endpoint, Tailnet, Actor, NodeID string
		Admin                            bool
		Tenants                          []struct{ TenantID, MintMode string }
	}
	d = dev.MustJSON(&lk, "link", "https://"+cfg.MgmtDNSName, "--tsnet-dir", filepath.Join(work, "cli-node"), "--tsnet-hostname", testPrefix+"cli",
		"--control-url", prodControl, "--tsnet-authkey-file", cliKey, "--tsnet-tags", cliflow.TestTag)
	res.Numbers["link_ms"] = float64(d.Milliseconds())
	tracker.Track(lk.NodeID, "cli-node")
	check("link_admin_by_whois", lk.Admin && lk.Tailnet == tailnet && strings.HasPrefix(lk.Actor, cliflow.TestTag) && len(lk.Tenants) == 1 && lk.Tenants[0].MintMode == "wif",
		fmt.Sprintf("%+v", lk))

	// 8. Remote deploy (bundled on the dev box, uploaded over HTTPS) + ls.
	dev.MustJSON(nil, "init", agentDir, "--template", "agent")
	tj := filepath.Join(agentDir, "twd.jsonc")
	ab, _ := os.ReadFile(tj)
	os.WriteFile(tj, bytes.Replace(ab, []byte(`"tags": ["tag:twd-wl-agent"],`), []byte(`"tags": ["tag:twd-wl-agent"],
    "connects": ["`+cliflow.TestTag+`"],`), 1), 0o644)
	var agent depRes
	d = dev.MustJSON(&agent, "deploy", agentDir)
	res.Numbers["remote_deploy_cli_ms"] = float64(d.Milliseconds())
	res.Numbers["remote_deploy_ready_ms"] = agent.Timings["ready"]
	check("remote_deploy", agent.Endpoint.MagicDNS != "", fmt.Sprintf("agent %s in %v (preflight via the lifecycle credential's policy read)", agent.Endpoint.MagicDNS, d.Round(time.Millisecond)))
	var ls struct {
		Deployments []struct{ Workload, DeployedBy string }
	}
	dev.MustJSON(&ls, "ls")
	by := ""
	for _, dp := range ls.Deployments {
		if dp.Workload == "agent" {
			by = dp.DeployedBy
		}
	}
	check("remote_ls_deployed_by", by == lk.Actor && strings.Contains(by, lk.NodeID), "deployedBy "+by)

	// 9. Call the agent from a tag:ttest client: its object node is minted via WIF.
	client := func(role string) *nettest.Node {
		k, err := minter.AuthKey(ctx, netd.KeyRequest{ObjectID: role, Tags: []string{cliflow.TestTag}, Ephemeral: true})
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
	}("client")
	cst, _ := client.LC.StatusWithoutPeers(ctx)
	clientID := string(cst.Self.ID)
	hc := client.HTTPClient(45 * time.Second)
	agentIP := cliflow.PeerIP(t, client, agent.Endpoint.MagicDNS, 60*time.Second)
	first := time.Now()
	resp, err := hc.Get("http://" + agentIP + "/?agent=k1")
	var body map[string]any
	if err == nil {
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
	}
	caller, _ := body["caller"].(map[string]any)
	check("a_verified_caller_via_workload", err == nil && resp.StatusCode == 200 && caller["nodeId"] == clientID, fmt.Sprintf("%v %v", err, body))
	ao := dev.WaitObjectReady("SupportAgent", "k1", 120*time.Second)
	res.Numbers["first_object_node_ready_ms"] = float64(time.Since(first).Milliseconds())
	host.TrackObjectNodes()
	aoIP := cliflow.PeerIP(t, client, ao.MagicDNS, 60*time.Second)
	resp, err = hc.Get("http://" + aoIP + "/")
	body = nil
	if err == nil {
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
	}
	caller, _ = body["caller"].(map[string]any)
	check("object_node_minted_via_wif", err == nil && resp.StatusCode == 200 && caller["nodeId"] == clientID && slices.Contains(ao.Tags, "tag:twd-agent-support"),
		fmt.Sprintf("object %s (%v) answers on its own node; mint mode wif, no API key on the host", ao.MagicDNS, ao.Tags))
	var aud struct {
		Verified bool
		Records  []struct {
			Kind, ActingFor string
			ObjectID        string `json:"object"`
		}
	}
	dev.MustJSON(&aud, "audit", "--all")
	mints, deployBy := 0, false
	for _, r := range aud.Records {
		if r.Kind == "mint" {
			mints++
		}
		if r.Kind == "deploy" && r.ActingFor == lk.Actor {
			deployBy = true
		}
	}
	check("remote_audit_mints_and_deployer", aud.Verified && mints >= 3 && deployBy, fmt.Sprintf("verified=%v mints=%d deployBy=%v", aud.Verified, mints, deployBy))

	// 10. Remote kill --cascade, then audit shows it.
	var kr struct {
		Result struct {
			Killed         []string
			DevicesDeleted []string `json:"devicesDeleted"`
		}
		Timings map[string]float64 `json:"timingsMs"`
	}
	d = dev.MustJSON(&kr, "kill", ao.ID, "--cascade", "--reason", "m10 remote kill")
	res.Numbers["remote_kill_cli_ms"] = float64(d.Milliseconds())
	check("remote_kill", len(kr.Result.Killed) == 1, fmt.Sprintf("killed %v in %v", kr.Result.Killed, d.Round(time.Millisecond)))
	dev.MustJSON(&aud, "audit", ao.ID)
	killed := false
	for _, r := range aud.Records {
		if r.Kind == "kill" && r.ObjectID == ao.ID {
			killed = true
		}
	}
	check("remote_audit_kill", aud.Verified && killed, fmt.Sprintf("verified=%v kill=%v", aud.Verified, killed))

	// 11. A non-admin node (tag:ttest-noadmin: network path, no cap) is refused with the snippet.
	naKey := keyTo("noadmin", []string{NoAdminTag}, true)
	_, se, d, err = noadmin.Twd("link", "https://"+cfg.MgmtDNSName, "--tsnet-dir", filepath.Join(work, "noadmin-node"), "--tsnet-hostname", testPrefix+"noadmin",
		"--control-url", prodControl, "--tsnet-authkey-file", naKey, "--tsnet-tags", NoAdminTag)
	res.Numbers["noadmin_link_ms"] = float64(d.Milliseconds())
	check("noadmin_refused_with_snippet", err != nil && strings.Contains(se, "not_admin") && strings.Contains(se, mgmt.AdminCap) &&
		strings.Contains(se, `"dst": ["`+mgmt.MgmtTag+`"]`) && strings.Contains(se, NoAdminTag), netd.Redact(se))
	if _, err := os.Stat(filepath.Join(noadmin.StateDir, "link.json")); !os.IsNotExist(err) {
		t.Fatal("a refused link was saved")
	}
	// The refused CLI node's ID never reached us (the link failed): find
	// its ephemeral device by this run's hostname and track it for cleanup.
	if devs, err := api.ListDevices(ctx); err == nil {
		for _, dv := range devs {
			if dv.Hostname == testPrefix+"noadmin" || strings.HasPrefix(dv.Name, testPrefix+"noadmin.") {
				tracker.Track(dv.NodeID, "noadmin-node")
			}
		}
	}

	// 12. Down (purge: ephemeral) and leftovers (in Cleanup).
	host.TrackObjectNodes()
	host.Down()
	downDone = true
	time.Sleep(2 * time.Second)
}
