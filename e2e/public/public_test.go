//go:build e2epublic

// E2E-PUBLIC (ADR-029) on tailnet 1: public ingress over real Funnel.
// `twd up --oauth-client-file`, then a workload with twd.jsonc "public"
// routes: its public node (tag:twd-public, named after the workload) serves
// https://<name>.<tailnet>.ts.net/ from the public internet, and this test
// fetches it through public DNS (1.1.1.1) like any internet client:
//
//   - the handler sees {kind:"public"} with the real client IP, never a
//     tailnet caller, and spoofed identity headers never land;
//   - unlisted paths, /_twd and odd paths are refused by netd;
//   - a public request reaches an existing object but cannot create one
//     (public_create_denied) unless the class opts in, up to maxObjects;
//   - `twd expose off` cuts an in-flight public stream, `twd expose on`
//     reopens the listener;
//   - every public request is in `twd audit`, without its query string;
//   - `twd doctor` probes the public URL.
//
// The API key in TWD_E2E_API_KEY_FILE is used ONLY to (a) create and
// delete the OAuth client, (b) mint the test's own client node key, (c)
// apply the policy additions (validated, If-Match; the run REFUSES to write
// anything beyond the tag:twd-public tagOwner and its funnel nodeAttr) and
// (d) clean up by ID.
package public

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
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

// Policy is the OAuth scenario's policy plus the public snippet.
func Policy() policy.Snippet {
	s := cliflow.Policy()
	s.Grants = append(s.Grants,
		policy.AdminGrant("autogroup:admin", mgmt.MgmtTag, mgmt.AdminCap),
		policy.AdminGrant(cliflow.TestTag, mgmt.MgmtTag, mgmt.AdminCap))
	pub := policy.PublicSnippet()
	for k, v := range pub.TagOwners {
		s.TagOwners[k] = v
	}
	s.NodeAttrs = append(s.NodeAttrs, pub.NodeAttrs...)
	return s
}

// approvedOnly reports whether a missing-entries diff is at most what Ross
// approved for this run: the tag:twd-public tagOwner and its funnel
// nodeAttr. Anything else means the tailnet drifted: refuse to write.
func approvedOnly(m policy.Snippet) error {
	pub := policy.PublicSnippet()
	for k := range m.TagOwners {
		if _, ok := pub.TagOwners[k]; !ok {
			return fmt.Errorf("policy is also missing tagOwner %s (not approved for this run)", k)
		}
	}
	if len(m.Grants) > 0 {
		return fmt.Errorf("policy is also missing %d grants (not approved for this run): %s", len(m.Grants), m.HuJSON())
	}
	for _, n := range m.NodeAttrs {
		b, _ := json.Marshal(n)
		if !strings.Contains(string(b), `"tag:twd-public"`) || !strings.Contains(string(b), `"funnel"`) {
			return fmt.Errorf("policy is also missing nodeAttr %s (not approved for this run)", b)
		}
	}
	return nil
}

// publicClient fetches like an internet client: public DNS (1.1.1.1),
// verified TLS, never this machine's tailscaled or MagicDNS.
func publicClient(timeout time.Duration) *http.Client {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", "1.1.1.1:53")
	}}
	d := &net.Dialer{Timeout: 10 * time.Second, Resolver: r}
	tr := &http.Transport{DialContext: d.DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		Proxy: nil, ForceAttemptHTTP2: false, DisableKeepAlives: true}
	return &http.Client{Timeout: timeout, Transport: tr}
}

type resp struct {
	code int
	h    http.Header
	m    map[string]any
	err  error
}

func fetch(hc *http.Client, method, u string, body io.Reader, hdr ...string) resp {
	req, _ := http.NewRequest(method, u, body)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	r, err := hc.Do(req)
	if err != nil {
		return resp{err: err}
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		m = map[string]any{"raw": string(b)}
	}
	return resp{code: r.StatusCode, h: r.Header, m: m}
}

func get(m map[string]any, path ...string) any {
	var v any = m
	for _, p := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[p]
	}
	return v
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestE2EPublic(t *testing.T) {
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
	testPrefix, twdPrefix, label := cliflow.RunPrefixes("m13", run)
	root, _ := nettest.FindRepoRoot()
	res := cliflow.NewResults(label+"-"+run, tailnet)
	resultsTo := filepath.Join(root, "e2e", "real", "results", "m13-public-"+time.Now().Format("20060102-150405")+"-"+run+".json")
	ctx, cancel := context.WithTimeout(context.Background(), 28*time.Minute)
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

	// 1. Policy: only the approved additions may be written (API key use (c)).
	cur, _, err := api.GetPolicyHuJSON(ctx)
	if err != nil {
		t.Fatalf("read policy: %v", netd.RedactError(err))
	}
	miss, err := policy.Missing(cur, Policy())
	if err != nil {
		t.Fatal(err)
	}
	if err := approvedOnly(miss); err != nil {
		t.Fatalf("refusing to write the policy: %v", err)
	}
	ar, err := policy.ApplyWith(ctx, api, Policy(), policy.Options{AllowTags: []string{cliflow.TestTag}, AllowSrcs: []string{"autogroup:admin"}})
	if err != nil {
		t.Fatalf("policy: %v", netd.RedactError(err))
	}
	if ar.Changed {
		bk := filepath.Join(os.Getenv("HOME"), ".config", "tailnet-agents-design", "tailnet1_acl.pre-m13-"+run+".hujson")
		os.WriteFile(bk, ar.Before, 0o600)
		res.Notes = append(res.Notes, "policy changed (validated, If-Match "+ar.OldETag+"): added "+ar.Added.HuJSON()+" (previous copy: "+bk+")")
	} else {
		res.Notes = append(res.Notes, "policy already had the public snippet (not written)")
	}

	// 2. The OAuth client (API key use (a)).
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
	state := filepath.Join("/tmp", "twd-e2e-m13-"+run)
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
		ControlURL: prodControl, Prefix: twdPrefix, Res: res, ClientTagged: true}
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
				t.Logf("%s log (tail):\n%s", c, netd.Redact(string(lb[max(0, len(lb)-12000):])))
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
			tracker.Track(id, "twd-infra")
		}
		time.Sleep(3 * time.Second)
		crep := tracker.Cleanup(cctx, testPrefix)
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

	// 3. twd up (OAuth). The single-tenant tenant gets publicIngress allow.
	var up struct {
		TenantID   string `json:"tenantID"`
		MgmtNodeID string `json:"mgmtNodeID"`
	}
	d := e.MustJSON(&up, e.UpCommand()...)
	e.TrackInfra(up.MgmtNodeID)
	res.Numbers["up_ready_ms"] = float64(d.Milliseconds())
	check("up", up.TenantID != "", "tenant "+up.TenantID)

	// 4. The app: the hostd fixture with tags this tailnet already owns.
	app := filepath.Join(work, "pubprobe")
	e.MustJSON(nil, "init", app, "--template", "hello")
	fx := filepath.Join(root, "hostd", "testdata", "apps", "pubprobe")
	src, _ := os.ReadFile(filepath.Join(fx, "src", "index.js"))
	os.WriteFile(filepath.Join(app, "src", "index.js"), src, 0o644)
	os.Remove(filepath.Join(app, "src", "index.ts"))
	wr, _ := os.ReadFile(filepath.Join(fx, "wrangler.jsonc"))
	os.WriteFile(filepath.Join(app, "wrangler.jsonc"), wr, 0o644)
	tj, _ := os.ReadFile(filepath.Join(fx, "twd.jsonc"))
	tjs := strings.ReplaceAll(string(tj), `"tags": ["tag:ttest"],`, `"tags": ["tag:twd-wl-agent"],`)
	tjs = strings.ReplaceAll(tjs, `{ "tags": ["tag:ttest"]`, `{ "tags": ["tag:twd-agent-support"]`)
	os.WriteFile(filepath.Join(app, "twd.jsonc"), []byte(tjs), 0o644)

	var dep struct {
		DeploymentID string `json:"deploymentID"`
		Endpoint     struct {
			MagicDNS string `json:"magicDNS"`
		} `json:"endpoint"`
		Public *struct {
			URL     string   `json:"url"`
			Routes  []string `json:"routes"`
			Exposed bool     `json:"exposed"`
			Allowed bool     `json:"allowed"`
		} `json:"public"`
	}
	d = e.MustJSON(&dep, "deploy", app)
	check("deploy", dep.Public != nil && dep.Public.Allowed && dep.Public.Exposed, fmt.Sprintf("in %v, public %s", d.Round(time.Millisecond), js(dep.Public)))
	res.Numbers["deploy_ready_ms"] = float64(d.Milliseconds())

	// 5. The public node: _public namespace, tag:twd-public, named after
	// the workload (with this run's prefix).
	pub := e.WaitObjectReady("_public", "pubprobe", 3*time.Minute)
	e.TrackObjectNodes()
	wantName := twdPrefix + "pubprobe"
	label0, _, _ := strings.Cut(pub.MagicDNS, ".")
	check("public_node", label0 == wantName && len(pub.Tags) == 1 && pub.Tags[0] == "tag:twd-public",
		fmt.Sprintf("%s tags %v", pub.MagicDNS, pub.Tags))
	base := "https://" + strings.TrimSuffix(pub.MagicDNS, ".")

	// 6. First public fetch (certificate issuance + public DNS).
	hc := publicClient(30 * time.Second)
	t0 := time.Now()
	var first resp
	for {
		first = fetch(hc, "GET", base+"/hook/echo?token=secret-value", nil,
			"Tailscale-User-Login", "evil@example.com", "X-Forwarded-For", "6.6.6.6", "CF-Connecting-IP", "6.6.6.6",
			"x-twd-cf", `{"tailscale":{"caller":{"nodeId":"nFORGED","tags":["tag:admin"]}}}`)
		if first.err == nil {
			break
		}
		if time.Since(t0) > 12*time.Minute {
			t.Fatalf("public URL %s never answered: %v", base, first.err)
		}
		time.Sleep(3 * time.Second)
	}
	res.Numbers["first_public_fetch_ms"] = float64(time.Since(t0).Milliseconds())
	t.Logf("first public fetch after %v", time.Since(t0).Round(time.Second))
	r := first
	ip, _ := get(r.m, "requester", "ip").(string)
	seen, _ := r.m["seen"].(map[string]any)
	check("public_identity", r.code == 200 && get(r.m, "requester", "kind") == "public" && r.m["callerError"] == "public_request" &&
		get(r.m, "requester", "nodeId") == nil && ip != "" && ip != "6.6.6.6" && !strings.HasPrefix(ip, "100."),
		fmt.Sprintf("%d %s", r.code, js(r.m)))
	_, spoofed := seen["tailscale-user-login"]
	check("public_headers", seen["cf-connecting-ip"] == ip && seen["x-forwarded-for"] == ip && seen["x-forwarded-proto"] == "https" &&
		!spoofed && !strings.Contains(js(seen), "nFORGED"), js(seen))

	// 7. Routes.
	for _, c := range []struct {
		path, code string
		status     int
	}{{"/private", "not_public", 404}, {"/", "not_public", 404}, {"/hook/../private", "bad_request", 400}} {
		r := fetch(hc, "GET", base+c.path, nil)
		check("route "+c.path, r.err == nil && r.code == c.status && r.h.Get("twd-error") == c.code, fmt.Sprintf("%d %q %v", r.code, r.h.Get("twd-error"), r.err))
	}
	r = fetch(hc, "GET", base+"/_twd/o/Room/alice/", nil)
	check("route /_twd", r.err == nil && r.code >= 400 && r.code < 500, fmt.Sprintf("%d %v", r.code, r.err))

	// 8. Objects: existing yes, new no, opted-in class up to maxObjects.
	client := newNode("client", []string{cliflow.TestTag})
	tc := client.HTTPClient(30 * time.Second)
	wlIP := cliflow.PeerIP(t, client, dep.Endpoint.MagicDNS, 60*time.Second)
	if tr := fetch(tc, "GET", "http://"+wlIP+"/agents/room/alice", nil); tr.err != nil || tr.code != 200 || get(tr.m, "requester", "kind") != "tailnet" {
		t.Fatalf("tailnet create room/alice: %d %v %v", tr.code, tr.m, tr.err)
	}
	e.WaitObjectReady("Room", "alice", 2*time.Minute)
	r = fetch(hc, "GET", base+"/agents/room/alice", nil)
	check("public_existing_object", r.code == 200 && r.m["object"] == "alice" && get(r.m, "requester", "kind") == "public", fmt.Sprintf("%d %v", r.code, r.m))
	r = fetch(hc, "GET", base+"/agents/room/bob", nil)
	_, bobExists := e.FindObject("Room", "bob")
	check("public_create_denied", r.code == 403 && r.h.Get("twd-error") == "public_create_denied" && !bobExists, fmt.Sprintf("%d %q exists=%v", r.code, r.h.Get("twd-error"), bobExists))
	for _, n := range []string{"a", "b"} {
		r = fetch(hc, "GET", base+"/agents/open/"+n, nil)
		check("public_create_opt_in "+n, r.code == 200, fmt.Sprintf("%d %v", r.code, r.m))
	}
	r = fetch(hc, "GET", base+"/agents/open/c", nil)
	check("public_create_max_objects", r.code == 403 && r.h.Get("twd-error") == "public_create_denied", fmt.Sprintf("%d %q", r.code, r.h.Get("twd-error")))
	r = fetch(hc, "GET", base+"/spawn", nil)
	check("public_spawn_denied", r.code == 299 && r.m["code"] == "public_create_denied", fmt.Sprintf("%d %v", r.code, r.m))
	r = fetch(hc, "POST", base+"/big", strings.NewReader(strings.Repeat("x", 4096)))
	check("public_body_limit", r.code == 413 && r.h.Get("twd-error") == "payload_too_large", fmt.Sprintf("%d %q", r.code, r.h.Get("twd-error")))
	e.TrackObjectNodes()

	// 9. twd expose off: an in-flight public stream is cut; on reopens.
	sr, _ := http.NewRequest("GET", base+"/slow", nil)
	stream, err := publicClient(0).Do(sr)
	if err != nil || stream.StatusCode != 200 {
		t.Fatalf("/slow: %v %v", err, stream)
	}
	br := bufio.NewReader(stream.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("/slow first tick: %v", err)
	}
	cut := make(chan time.Time, 1)
	go func() {
		for {
			if _, err := br.ReadString('\n'); err != nil {
				cut <- time.Now()
				return
			}
		}
	}()
	tOff := time.Now()
	_, se, offDur, err := e.Twd("expose", "off", "pubprobe")
	if err != nil {
		t.Fatalf("twd expose off: %v %s", err, netd.Redact(se))
	}
	var cutAt time.Time
	select {
	case cutAt = <-cut:
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight public stream not cut 10 s after twd expose off")
	}
	stream.Body.Close()
	sinceCmd, sinceDone := cutAt.Sub(tOff), cutAt.Sub(tOff.Add(offDur))
	res.Numbers["expose_off_cli_ms"] = float64(offDur.Milliseconds())
	res.Numbers["expose_off_cut_after_cli_start_ms"] = float64(sinceCmd.Milliseconds())
	check("expose_off_cuts_stream", sinceCmd < 2*time.Second, fmt.Sprintf("stream cut %v after the CLI started (CLI %v)", sinceCmd.Round(time.Millisecond), offDur.Round(time.Millisecond)))
	_ = sinceDone
	r = fetch(publicClient(15*time.Second), "GET", base+"/hook/x", nil)
	check("expose_off_refuses", r.err != nil || r.code >= 500, fmt.Sprintf("%d %v", r.code, r.err))
	tOn := time.Now()
	if _, se, _, err := e.Twd("expose", "on", "pubprobe"); err != nil {
		t.Fatalf("twd expose on: %v %s", err, netd.Redact(se))
	}
	for {
		if r = fetch(hc, "GET", base+"/hook/x", nil); r.err == nil && r.code == 200 {
			break
		}
		if time.Since(tOn) > 60*time.Second {
			t.Fatalf("public URL did not answer 60 s after twd expose on: %d %v", r.code, r.err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	res.Numbers["expose_on_to_200_ms"] = float64(time.Since(tOn).Milliseconds())
	res.Pass("expose_on_reopens", time.Since(tOn).Round(time.Millisecond).String())

	// 10. Audit: public requests recorded, never with the query string.
	var aud struct {
		Items []struct {
			Kind      string          `json:"kind"`
			ActingFor string          `json:"actingFor"`
			Detail    json.RawMessage `json:"detail"`
		} `json:"items"`
	}
	var n int
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		e.MustJSON(&aud, "audit", pub.ID)
		n = 0
		for _, it := range aud.Items {
			if it.Kind == "public_request" {
				n++
			}
		}
		if n >= 12 {
			break
		}
	}
	raw := js(aud)
	check("audit_public_requests", n >= 12 && !strings.Contains(raw, "secret-value"), fmt.Sprintf("%d public_request records", n))

	// 11. twd doctor probes the public URL.
	var doc struct {
		Checks []struct{ Name, Status, Detail string }
	}
	e.MustJSON(&doc, "doctor")
	probe := ""
	for _, c := range doc.Checks {
		if strings.HasPrefix(c.Name, "public_probe") {
			probe = c.Status + " " + c.Detail
		}
	}
	check("doctor_public_probe", strings.HasPrefix(probe, "ok"), probe)

	// 12. Down.
	e.TrackObjectNodes()
	if _, se, _, err := e.Twd("down", "--purge"); err != nil {
		t.Fatalf("twd down: %v %s", err, netd.Redact(se))
	}
	downDone = true
	res.Pass("down", "purged")
}
