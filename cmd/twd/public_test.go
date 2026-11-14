package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/mgmt"
)

// ADR-029 CLI: `twd tenant set --public-ingress`, `twd expose on|off`, and
// the deploy preflight for twd.jsonc "public" against a real twd-mgmt (in
// process, on the local-dev unix socket) and a fake Tailscale policy API.

type pubEnv struct {
	dir, tenantID string
	m             *mgmt.Mgmt
	st            *mgmt.SQLStore
	mu            sync.Mutex
	policy        string
}

func newPubEnv(t *testing.T) *pubEnv {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "twdpub-") // short: unix socket path limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	e := &pubEnv{dir: dir, tenantID: "tn_pubtest"}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/acl") {
			e.mu.Lock()
			p := e.policy
			e.mu.Unlock()
			w.Header().Set("ETag", `"`+strconv.Itoa(len(p))+`"`)
			w.Write([]byte(p))
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(ts.Close)
	keyFile := filepath.Join(dir, "api.env")
	os.WriteFile(keyFile, []byte("TS_API_KEY=fake-not-a-key\n"), 0o600)

	ctx := context.Background()
	st, err := mgmt.OpenStore(ctx, filepath.Join(dir, "mgmt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cust, err := mgmt.OpenFileCustody(filepath.Join(dir, "custody"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := mgmt.New(mgmt.Config{Store: st, Custody: cust, Broker: &mgmt.Broker{BaseURL: ts.URL},
		IDSecret: bytes.Repeat([]byte{7}, 32), ReapInterval: time.Hour, PolicyMaxAge: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	e.m, e.st = m, st

	s := &state{Dir: dir}
	s.Cfg.Tailnet, s.Cfg.TenantID, s.Cfg.MintMode, s.Cfg.APIKeyFile = "t.ts.net", e.tenantID, "apikey", keyFile
	s.Cfg.ControlURL, s.Cfg.HostID, s.Cfg.LocalDev = "http://control.invalid", "h1", true
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	tok := mgmt.NewHostToken()
	os.MkdirAll(filepath.Dir(s.hostTokenFile()), 0o700)
	os.WriteFile(s.hostTokenFile(), []byte(tok), 0o600)
	ln, err := mgmt.ListenUnix(s.mgmtSock())
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mgmt.LocalHandler((&mgmt.Server{M: m}).Handler(), tok)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	os.WriteFile(s.pidFile(), []byte(strconv.Itoa(os.Getpid())), 0o600) // "running"

	if _, err := m.CreateTenant(ctx, mgmt.TenantRequest{ID: e.tenantID, Tailnet: "t.ts.net", MintMode: mgmt.MintAPIKey, APIKeyFile: keyFile}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.RegisterHost(ctx, mgmt.RegisterRequest{HostID: "h1", CapacityNodes: 10, Tiers: []string{"process"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *pubEnv) setPolicy(p string) {
	e.mu.Lock()
	e.policy = p
	e.mu.Unlock()
}

// call runs a command function with stdout captured.
func (e *pubEnv) call(t *testing.T, fn func([]string) error, args ...string) (string, error) {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	err := fn(append([]string{"--state-dir", e.dir}, args...))
	w.Close()
	os.Stdout = old
	return <-done, err
}

func cliCode(err error) string {
	if err == nil {
		return ""
	}
	return asCLIErr(err).Code
}

func cliText(err error) string {
	e := asCLIErr(err)
	return e.Message + "\n" + strings.Join(e.Lines, "\n") + "\n" + strings.Join(e.Fix, "\n")
}

const pubApp = `{ "name": "support",
  "public": { "routes": ["/webhooks/*"] },
  "tailnet": { "tags": ["tag:twd-wl-support"] } }`

func writePubApp(t *testing.T) string {
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, "src"), 0o755)
	os.WriteFile(filepath.Join(d, "twd.jsonc"), []byte(pubApp), 0o644)
	os.WriteFile(filepath.Join(d, "wrangler.jsonc"), []byte(`{"name": "support", "main": "src/index.js", "compatibility_date": "2026-04-01"}`), 0o644)
	os.WriteFile(filepath.Join(d, "src", "index.js"), []byte(`export default { fetch() { return new Response("ok"); } };`), 0o644)
	return d
}

func TestCLIPublicTenantGateAndPreflight(t *testing.T) {
	e := newPubEnv(t)
	owned := `"tag:twd": ["autogroup:admin"], "tag:twd-wl-support": ["autogroup:admin", "tag:twd"]`
	e.setPolicy(`{"tagOwners": {` + owned + `}}`)
	app := writePubApp(t)

	// Gate deny (a fresh `twd tenant add` tenant): the dry run refuses first.
	_, err := e.call(t, cmdDeploy, app)
	if cliCode(err) != "public_ingress_not_allowed" || !strings.Contains(cliText(err), "twd tenant set "+e.tenantID+" --public-ingress allow") {
		t.Fatalf("deploy with the gate off: %v\n%s", err, cliText(err))
	}
	if _, err := e.call(t, cmdTenantSet, e.tenantID, "--public-ingress", "maybe"); cliCode(err) != "bad_request" {
		t.Fatalf("bad --public-ingress: %v", err)
	}
	out, err := e.call(t, cmdTenantSet, e.tenantID, "--public-ingress", "allow")
	if err != nil || !strings.Contains(out, "public ingress allow") {
		t.Fatalf("tenant set: %v\n%s", err, out)
	}

	// No tag:twd-public owner, no funnel nodeAttr: funnel_required with the public snippet once.
	out, err = e.call(t, cmdDeploy, app)
	if cliCode(err) != "funnel_required" || strings.Count(cliText(err), `"tag:twd-public": ["autogroup:admin", "tag:twd"]`) != 1 || !strings.Contains(cliText(err), `"attr": ["funnel"]`) {
		t.Fatalf("deploy without the public policy: %v\n%s\n%s", err, cliText(err), out)
	}
	if !strings.Contains(out, "nodeAttrs funnel on tag:twd-public … ✗ missing") {
		t.Fatalf("preflight output lacks the funnel line:\n%s", out)
	}
	// A missing grant source too: acl_denied, every snippet.
	e.setPolicy(`{"tagOwners": {"tag:twd": ["autogroup:admin"]}}`)
	time.Sleep(5 * time.Millisecond) // PolicyMaxAge
	if _, err := e.call(t, cmdDeploy, app); cliCode(err) != "acl_denied" || !strings.Contains(cliText(err), "tag:twd-wl-support") || !strings.Contains(cliText(err), `"attr": ["funnel"]`) {
		t.Fatalf("deploy missing a workload tag owner too: %v\n%s", err, cliText(err))
	}
	// Owner present, funnel only on an object tag: funnel_required.
	e.setPolicy(`{"tagOwners": {` + owned + `, "tag:twd-public": ["autogroup:admin", "tag:twd"]}, "nodeAttrs": [{"target": ["tag:twd-do-x"], "attr": ["funnel"]}]}`)
	time.Sleep(5 * time.Millisecond) // PolicyMaxAge
	_, err = e.call(t, cmdDeploy, app)
	if cliCode(err) != "funnel_required" || !strings.Contains(cliText(err), `"target": ["tag:twd-public"], "attr": ["funnel"]`) {
		t.Fatalf("deploy without the funnel nodeAttr: %v\n%s", err, cliText(err))
	}
	// The full snippet: preflight passes (the deploy itself then needs a running host; not here).
	e.setPolicy(`{"tagOwners": {` + owned + `, "tag:twd-public": ["autogroup:admin", "tag:twd"]}, "nodeAttrs": [{"target": ["tag:twd-public"], "attr": ["funnel"]}]}`)
	time.Sleep(5 * time.Millisecond)
	out, _ = e.call(t, cmdDeploy, app, "--no-wait")
	if !strings.Contains(out, "✓ preflight  public: funnel allowed on tag:twd-public") || !strings.Contains(out, "✓ deploying  support@h1") {
		t.Fatalf("deploy with the public policy:\n%s", out)
	}
	tn, _ := e.st.GetTenant(context.Background(), e.tenantID)
	if !tn.PublicAllowed() {
		t.Fatalf("gate %q", tn.PublicIngress)
	}
}

func TestCLIExpose(t *testing.T) {
	e := newPubEnv(t)
	if _, err := e.call(t, cmdExpose, "maybe", "support"); cliCode(err) != "bad_request" {
		t.Fatalf("expose maybe: %v", err)
	}
	if _, err := e.call(t, cmdExpose, "off", "support"); cliCode(err) != "unknown_workload" {
		t.Fatalf("expose off before any deploy: %v", err)
	}
	e.setPolicy(`{}`)
	if _, err := e.call(t, cmdDeploy, writePubApp(t), "--allow-missing-grants", "--no-wait"); cliCode(err) != "public_ingress_not_allowed" {
		t.Fatalf("gate: %v", err)
	}
	e.call(t, cmdTenantSet, e.tenantID, "--public-ingress", "allow")
	if out, err := e.call(t, cmdDeploy, writePubApp(t), "--allow-missing-grants", "--no-wait"); err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	out, err := e.call(t, cmdExpose, "off", "support")
	if err != nil || !strings.Contains(out, "unexposed  support") || !strings.Contains(out, "twd expose on support") {
		t.Fatalf("expose off: %v\n%s", err, out)
	}
	ctx := context.Background()
	if wp, _ := e.m.GetWorkloadPublic(ctx, e.tenantID, "support"); wp.Exposed || wp.UpdatedBy == "" {
		t.Fatalf("after expose off: %+v", wp)
	}
	if out, err := e.call(t, cmdExpose, "on", "support"); err != nil || !strings.Contains(out, "exposed    support") {
		t.Fatalf("expose on: %v\n%s", err, out)
	}
	if wp, _ := e.m.GetWorkloadPublic(ctx, e.tenantID, "support"); !wp.Exposed {
		t.Fatalf("after expose on: %+v", wp)
	}
	// The audit renders both toggles.
	recs, _ := e.st.ListAudit(ctx, mgmt.AuditFilter{TenantID: e.tenantID, Kinds: []string{"expose_on", "expose_off"}})
	if len(recs) != 2 {
		t.Fatalf("expose records %d", len(recs))
	}
	for _, r := range recs {
		if d := describe(r, nil); !strings.Contains(d, "support public routes") || !strings.Contains(d, "by ") {
			t.Fatalf("describe %s: %q", r.Kind, d)
		}
	}
}

func TestDescribePublicKinds(t *testing.T) {
	mk := func(kind, detail string) mgmt.AuditRecord {
		return mgmt.AuditRecord{Kind: kind, Detail: []byte(detail)}
	}
	cases := map[string]mgmt.AuditRecord{
		"public GET /webhooks/github → 200 from 203.0.113.7 12 ms":    mk("public_request", `{"method":"GET","path":"/webhooks/github","status":200,"ip":"203.0.113.7","ms":12}`),
		"public refused: 41 rate-limited, 2 too large, 0 connections": mk("public_rejected", `{"rateLimited":41,"tooLarge":2,"connsRefused":0}`),
		"Room/lobby created by a public request from 203.0.113.7":     mk("public_object_created", `{"class":"Room","name":"lobby","ip":"203.0.113.7"}`),
	}
	for want, r := range cases {
		if got := describe(r, nil); !strings.Contains(got, want) {
			t.Errorf("%s: got %q, want %q", r.Kind, got, want)
		}
	}
}
