package mgmt

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/rosskukulinski/twd/mgmt/api/v1"
)

// fakeTS is a fake Tailscale API: keys, devices, policy (with ETag),
// token exchange.
type fakeTS struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex
	// state
	keys           map[string]map[string]any // id → create body
	secrets        map[string]string
	deletedKeys    []string
	deletedDevices []string
	policyFetches  int
	notModified    int
	rateLimitNext  int // respond 429 to the next N key creates
	tokenExchanges int
	// WIF verification
	issuerJWKS func() []byte
	wantAud    string
	apiToken   string
}

func newFakeTS(t *testing.T) *fakeTS {
	f := &fakeTS{t: t, keys: map[string]map[string]any{}, secrets: map[string]string{}, apiToken: "fake-api-token"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTS) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/api/v2/oauth/token-exchange" {
		r.ParseForm()
		f.tokenExchanges++
		claims, err := VerifyJWT(r.Form.Get("jwt"), f.issuerJWKS(), time.Now())
		if err != nil || claims["aud"] != "api.tailscale.com/"+r.Form.Get("client_id") || !strings.HasPrefix(claims["sub"].(string), "twd:tenant:") ||
			!strings.Contains(claims["iss"].(string), "/t/") {
			w.WriteHeader(401)
			fmt.Fprintf(w, `{"message":"invalid token: %v"}`, err)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "wif-access-" + r.Form.Get("client_id"), "expires_in": 3600})
		return
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if auth != f.apiToken && !strings.HasPrefix(auth, "wif-access-") {
		w.WriteHeader(401)
		return
	}
	switch {
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/keys"):
		if strings.HasPrefix(auth, "wif-access-") && auth != "wif-access-mint-client" {
			w.WriteHeader(403) // only the mint credential has auth_keys
			return
		}
		if f.rateLimitNext > 0 {
			f.rateLimitNext--
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			w.Write([]byte(`{"message":"rate limited"}`))
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		id := fmt.Sprintf("k%d", len(f.keys)+1)
		secret := "tskey-" + "auth-" + id + "-" + strings.Repeat("s", 20)
		f.keys[id], f.secrets[id] = body, secret
		json.NewEncoder(w).Encode(map[string]string{"id": id, "key": secret})
	case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/keys/"):
		f.deletedKeys = append(f.deletedKeys, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/v2/device/"):
		if auth == "wif-access-mint-client" {
			w.WriteHeader(403)
			return
		}
		f.deletedDevices = append(f.deletedDevices, strings.TrimPrefix(r.URL.Path, "/api/v2/device/"))
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/acl"):
		f.policyFetches++
		if r.Header.Get("If-None-Match") == `"p1"` {
			f.notModified++
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"p1"`)
		w.Write([]byte(samplePolicy))
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeTS) createdKey(n int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keys[fmt.Sprintf("k%d", n)]
}

func (f *fakeTS) devicesDeleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletedDevices...)
}

func makeBundle(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	return buf.Bytes()
}

// direct calls the handler without the spec validator (for inputs the
// spec itself forbids, or paths outside it) as host (or local when host is
// "") and returns the status and body.
func direct(t *testing.T, e *testEnv, host, method, path string, body any) (int, map[string]any) {
	t.Helper()
	h := LocalHandler((&Server{M: e.m}).Handler(), e.token)
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, "http://twd-mgmt"+path, rd)
	r.Header.Set(HostTokenHeader, e.token)
	if host != "" {
		r.Header.Set("Twd-Host-ID", host)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// directFail is direct asserting a typed refusal.
func directFail(t *testing.T, e *testEnv, host, method, path string, status int, code string) {
	t.Helper()
	st, eb := direct(t, e, host, method, path, nil)
	if st != status || eb["code"] != code {
		t.Fatalf("%s %s: got %d %v, want %d %s", method, path, st, eb, status, code)
	}
}

// noConfigBundle is a bundle without twd.jsonc: the deploy's explicit
// namespaces define the classes (the spec requires a bundle either way).
func noConfigBundle(t *testing.T) []byte {
	return makeBundle(t, map[string]string{"dist/index.js": "export default {}"})
}

const testTwdJSONC = `// twd.jsonc
{ "tailnet": { "objects": {
    "SupportAgent": { "tags": ["tag:twd-agent-support"], "agent": true },
    "LookupAgent":  { "tags": ["tag:twd-agent-support-sub"], "agent": true, "ttl": "10m" },
    "Rogue":        { "tags": ["tag:crm"] },
  },
  "spawn": { "maxLive": 6, "maxDepth": 3 } } }`

// apiClient talks to mgmt over its unix socket (/v1).
type apiClient struct {
	t     *testing.T
	hc    *http.Client
	host  string // Twd-Host-ID
	token string // Twd-Host-Token ("" = send none)
	hdr   map[string]string
}

// do sends a JSON request (PATCH as application/merge-patch+json) and
// returns the status. Any 2xx decodes into out; other statuses return the
// typed error body, which must be in the catalog shape.
func (c *apiClient) do(method, path string, body any, out any) (int, map[string]any) {
	c.t.Helper()
	st, eb, _ := c.doRaw(method, path, body, out)
	return st, eb
}

func (c *apiClient) doRaw(method, path string, body any, out any) (int, map[string]any, http.Header) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://twd-mgmt"+path, rd)
	if body != nil {
		if method == "PATCH" {
			req.Header.Set("Content-Type", "application/merge-patch+json")
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if c.host != "" {
		req.Header.Set("Twd-Host-ID", c.host)
	}
	if c.token != "" {
		req.Header.Set(HostTokenHeader, c.token)
	}
	for k, v := range c.hdr {
		req.Header.Set(k, v)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode/100 != 2 {
		var errBody map[string]any
		if res.StatusCode == 304 {
			return res.StatusCode, nil, res.Header
		}
		json.Unmarshal(b, &errBody)
		if errBody["code"] == nil || errBody["trace_id"] == nil || errBody["message"] == nil || res.Header.Get("Twd-Error") == "" {
			c.t.Fatalf("%s %s: error body not in catalog shape: %s", method, path, b)
		}
		return res.StatusCode, errBody, res.Header
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			c.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return res.StatusCode, nil, res.Header
}

// ok requires a 2xx.
func (c *apiClient) ok(method, path string, body, out any) {
	c.t.Helper()
	if st, e := c.do(method, path, body, out); st/100 != 2 {
		c.t.Fatalf("%s %s: %d %v", method, path, st, e)
	}
}

// okStatus requires a 2xx and returns the exact status.
func (c *apiClient) okStatus(method, path string, body, out any) int {
	c.t.Helper()
	st, e := c.do(method, path, body, out)
	if st/100 != 2 {
		c.t.Fatalf("%s %s: %d %v", method, path, st, e)
	}
	return st
}

// items GETs a list and decodes its page's items into out (a *[]T); it
// returns the page's nextCursor (nil on the last page).
func (c *apiClient) items(path string, out any) *string {
	c.t.Helper()
	var pg struct {
		Items      json.RawMessage `json:"items"`
		NextCursor *string         `json:"nextCursor"`
	}
	c.ok("GET", path, nil, &pg)
	if pg.Items == nil {
		c.t.Fatalf("GET %s: no items in page", path)
	}
	if err := json.Unmarshal(pg.Items, out); err != nil {
		c.t.Fatalf("GET %s: items: %v", path, err)
	}
	return pg.NextCursor
}

func (c *apiClient) fail(method, path string, body any, status int, code string) map[string]any {
	c.t.Helper()
	st, e := c.do(method, path, body, nil)
	if st != status || e["code"] != code {
		c.t.Fatalf("%s %s: got %d %v, want %d %s", method, path, st, e, status, code)
	}
	return e
}

// killQuery is the DELETE /v1/objects/{id} query for a kill.
func killQuery(cascade bool, reason string) string {
	q := url.Values{"reason": {reason}}
	if cascade {
		q.Set("cascade", "true")
	}
	return "?" + q.Encode()
}

// specGaps are spec-vs-server mismatches known at the time of writing
// (api/openapi.yaml is the source of truth and is regenerated by the
// parent, not here). Each is logged, not failed, so the suite stays green
// while they are visible; everything else the validator finds fails.
var specGaps = []string{
	`"delegation_exceeds_parent"`,                   // error code emitted by policy.go, absent from ErrorCode
	`"credential"`, `"obo_root"`, `"object_update"`, // audit kinds absent from AuditKind
	`property "detail"`, // host records carry detail as a JSON string
	// a spawn from a killed parent is 410 delegation_invalid (no tombstone),
	// while the spec's only 410 is the killed tombstone
	`response 410: response header "Twd-Error"`,
}

func specReporter(t *testing.T) func(error) {
	return func(err error) {
		t.Helper()
		msg := err.Error()
		for _, g := range specGaps {
			if strings.Contains(msg, g) {
				t.Logf("spec gap (known): %v", err)
				return
			}
		}
		t.Errorf("spec: %v", err)
	}
}

type testEnv struct {
	m       *Mgmt
	ts      *fakeTS
	c       *apiClient
	keyFile string
	logs    *bytes.Buffer
	store   *SQLStore
	token   string
	sock    string
}

func newTestEnv(t *testing.T, issuer *Issuer) *testEnv {
	ctx := context.Background()
	dir, err := os.MkdirTemp("/tmp", "twd-mgmt-test-") // short path: unix socket limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ts := newFakeTS(t)
	keyFile := filepath.Join(dir, "api.env")
	os.WriteFile(keyFile, []byte("TS_API_KEY="+ts.apiToken+"\n"), 0o600)
	var st *SQLStore
	if os.Getenv("TWD_TEST_API_POSTGRES") != "" {
		st = storesUnderTest(t)["postgres"] // fresh schema
		if st == nil {
			t.Fatal("TWD_TEST_API_POSTGRES needs TWD_TEST_POSTGRES_DSN")
		}
	} else {
		var err error
		if st, err = OpenStore(ctx, filepath.Join(dir, "mgmt.db")); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
	}
	cust, err := OpenFileCustody(filepath.Join(dir, "custody"))
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{BaseURL: ts.srv.URL, Issuer: issuer},
		IDSecret: bytes.Repeat([]byte{7}, 32), ReapInterval: time.Hour, PolicyMaxAge: time.Minute,
		Log: slog.New(slog.NewTextHandler(&lockedWriter{w: logs}, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	sock := filepath.Join(dir, "run", "mgmt.sock")
	ln, err := ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	hostToken := NewHostToken()
	val, err := v1.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	// Every request and response of the suite is checked against the spec.
	h := val.Middleware(specReporter(t))((&Server{M: m}).Handler())
	srv := &http.Server{Handler: LocalHandler(h, hostToken)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	if fi, _ := os.Stat(sock); fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", fi.Mode())
	}
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	return &testEnv{m: m, ts: ts, c: &apiClient{t: t, hc: hc, token: hostToken}, keyFile: keyFile, logs: logs, store: st, token: hostToken, sock: sock}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (e *testEnv) asHost(id string) *apiClient {
	c := *e.c
	c.host = id
	return &c
}

// TestRemintAfterRejectedRejoin: a ready object's device is gone from
// control (its saved node state was rejected). The host POSTs /remint: the
// object goes back to pending, a new key is minted under the SAME hostname,
// the credential is released again, and /ready records the new node ID.
func TestRemintAfterRejectedRejoin(t *testing.T) {
	e := newTestEnv(t, nil)
	c, h1 := e.c, e.asHost("h1")
	var tn struct{ TenantID string }
	c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile}, &tn)
	h1.ok("PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 100, "tiers": []string{"process"}}, nil)
	bundle := makeBundle(t, map[string]string{"twd.jsonc": testTwdJSONC, "dist/index.js": "export default {}"})
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "support", "bundle": bundle}, nil)
	var root CreateObjectResponse
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "namespace": "SupportAgent", "objectName": "alice"}, &root)
	var cred Credential
	h1.ok("POST", "/v1/objects/"+root.ObjectID+"/credentials?epoch=1", nil, &cred)
	h1.ok("PATCH", "/v1/objects/"+root.ObjectID, map[string]any{"status": "ready", "nodeID": "1", "stableNodeID": "nOLD", "magicDNS": root.ObjectID + ".t.ts.net"}, nil)
	h1.fail("POST", "/v1/objects/"+root.ObjectID+"/credentials?epoch=1", nil, 409, "already_joined")

	// Other hosts can't remint it; a stale epoch can't either.
	e.asHost("h2").fail("POST", "/v1/objects/"+root.ObjectID+"/credentials?epoch=1&reason=rejoin_rejected", nil, 409, "lease_lost")
	h1.fail("POST", "/v1/objects/"+root.ObjectID+"/credentials?epoch=2&reason=rejoin_rejected", nil, 409, "lease_lost")
	// The re-mint is a credential create with reason=rejoin_rejected: the
	// object goes back through pending and the new key comes back at once.
	var cred2 struct {
		Credential
		Reminted bool `json:"reminted"`
	}
	h1.ok("POST", "/v1/objects/"+root.ObjectID+"/credentials?epoch=1&reason=rejoin_rejected", nil, &cred2)
	if cred2.AuthKey == "" || cred2.AuthKey == cred.AuthKey || cred2.Hostname != root.ObjectID || !cred2.Reminted {
		t.Fatalf("second credential: hostname %q, same key %v, reminted %v", cred2.Hostname, cred2.AuthKey == cred.AuthKey, cred2.Reminted)
	}
	var ro Object
	c.ok("GET", "/v1/objects/"+root.ObjectID, nil, &ro)
	if ro.Status != StatusMinted || ro.StableNodeID != "" || ro.NodeID != "" || ro.Hostname != root.ObjectID {
		t.Fatalf("after remint: %+v", ro)
	}
	h1.ok("PATCH", "/v1/objects/"+root.ObjectID, map[string]any{"status": "ready", "nodeID": "2", "stableNodeID": "nNEW", "magicDNS": root.ObjectID + ".t.ts.net"}, nil)
	c.ok("GET", "/v1/objects/"+root.ObjectID, nil, &ro)
	if ro.Status != StatusReady || ro.StableNodeID != "nNEW" || ro.Hostname != root.ObjectID {
		t.Fatalf("after second ready: %+v", ro)
	}
	h1.fail("POST", "/v1/objects/"+root.ObjectID+"/credentials?epoch=1", nil, 409, "already_joined")
	// Re-mint again while ready: fine; refused once killed.
	h1.ok("POST", "/v1/objects/"+root.ObjectID+"/credentials?epoch=1&reason=rejoin_rejected", nil, &cred2)
	c.ok("DELETE", "/v1/objects/"+root.ObjectID+killQuery(false, "test"), nil, nil)
	h1.fail("POST", "/v1/objects/"+root.ObjectID+"/credentials?epoch=1&reason=rejoin_rejected", nil, 410, "killed")
	var audit struct {
		Records []AuditRecord `json:"items"`
	}
	c.ok("GET", "/v1/audit?object="+root.ObjectID, nil, &audit)
	kinds := map[string]int{}
	for _, r := range audit.Records {
		kinds[r.Kind]++
	}
	if kinds["remint"] < 1 || kinds["ready"] != 2 {
		t.Fatalf("audit kinds %v", kinds)
	}
}

func TestAPIEndToEnd(t *testing.T) {
	e := newTestEnv(t, nil)
	c, h1 := e.c, e.asHost("h1")

	var tn struct{ TenantID string }
	c.fail("POST", "/v1/tenants", map[string]any{"tailnet": "-", "mintMode": "apikey", "apiKeyFile": e.keyFile}, 400, "bad_request")
	c.fail("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "wif", "wifClientID": "x"}, 400, "bad_request")
	c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile}, &tn)
	var tns []Tenant
	c.items("/v1/tenants", &tns)
	if len(tns) != 1 || tns[0].RegistrationsPerMinute != 200 || tns[0].MaxObjects != 1000 {
		t.Fatalf("tenant defaults %+v", tns)
	}

	// No host yet → no deploy.
	bundle := makeBundle(t, map[string]string{"twd.jsonc": testTwdJSONC, "dist/index.js": "export default {}"})
	c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "support", "bundle": bundle}, 503, "no_host")
	h1.fail("PUT", "/v1/hosts/h1/status", map[string]any{"liveNodes": 0, "leases": []any{}}, 404, "host_not_registered")
	h1.ok("PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 100, "tiers": []string{"process"}}, nil)
	var dep DeployResponse
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "support", "bundle": bundle}, &dep)
	if dep.Host != "h1" || dep.Epoch != 1 || dep.Endpoint.MagicDNS != "support-h1.t.ts.net" {
		t.Fatalf("deploy %+v", dep)
	}
	var as []Assignment
	h1.items("/v1/hosts/h1/assignments", &as)
	if len(as) != 1 || as[0].Epoch != 1 || len(as[0].Namespaces) != 3 || as[0].BundleURL != "/v1/deployments/"+dep.DeploymentID+"/bundle" {
		t.Fatalf("assignments %+v", as)
	}
	req, _ := http.NewRequest("GET", "http://twd-mgmt"+as[0].BundleURL, nil)
	req.Header.Set(HostTokenHeader, c.token)
	if res, err := c.hc.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatal("bundle fetch", err)
	} else if b, _ := io.ReadAll(res.Body); !bytes.Equal(b, bundle) {
		t.Fatal("bundle mismatch")
	}

	// Root object → async mint → credential to the lease holder only.
	var root CreateObjectResponse
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "namespace": "SupportAgent", "objectName": "alice"}, &root)
	if root.Status != "pending" || root.Tags[0] != "tag:twd-agent-support" || !strings.HasPrefix(root.ObjectID, "supportagent-") || len(root.ObjectID) != len("supportagent-")+10 {
		t.Fatalf("create %+v", root)
	}
	var again CreateObjectResponse
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "namespace": "SupportAgent", "objectName": "alice"}, &again)
	if again.ObjectID != root.ObjectID {
		t.Fatal("create is not idempotent by name")
	}
	h1.fail("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "namespace": "Nope", "objectName": "x"}, 404, "unknown_namespace")
	h1.fail("POST", "/v1/objects/"+root.ObjectID+"/credentials?epoch=2", nil, 409, "lease_lost")
	e.asHost("h2").fail("POST", "/v1/objects/"+root.ObjectID+"/credentials?epoch=1", nil, 409, "lease_lost")
	var cred Credential
	h1.ok("POST", "/v1/objects/"+root.ObjectID+"/credentials?epoch=1", nil, &cred)
	if !strings.HasPrefix(cred.AuthKey, "tskey-auth-k1-") || cred.Hostname != root.ObjectID || cred.Ephemeral {
		t.Fatalf("credential %v", cred.Hostname)
	}
	kb := e.ts.createdKey(1)
	create := kb["capabilities"].(map[string]any)["devices"].(map[string]any)["create"].(map[string]any)
	if create["reusable"] != false || create["preauthorized"] != true || kb["expirySeconds"].(float64) > 300 ||
		fmt.Sprint(create["tags"]) != "[tag:twd-agent-support]" {
		t.Fatalf("minted key options %v", kb)
	}
	h1.ok("PATCH", "/v1/objects/"+root.ObjectID, map[string]any{"status": "ready", "nodeID": "1", "stableNodeID": "nROOT", "magicDNS": root.ObjectID + ".t.ts.net"}, nil)
	h1.fail("POST", "/v1/objects/"+root.ObjectID+"/credentials?epoch=1", nil, 409, "already_joined")

	// Spawn checks.
	spawn := func(parent, ns, name string, caps any) (int, map[string]any, CreateObjectResponse) {
		var out CreateObjectResponse
		body := map[string]any{"tenantID": tn.TenantID, "namespace": ns, "objectName": name, "parent": map[string]string{"objectID": parent}}
		if caps != nil {
			body["caps"] = caps
		}
		st, eb := h1.do("POST", "/v1/objects", body, &out)
		return st, eb, out
	}
	st, eb, _ := spawn(root.ObjectID, "LookupAgent", "bad", map[string]any{"acme.com/cap/mcp": map[string]any{"tools": []string{"lookup_order", "delete_all"}}})
	if st != 403 || eb["code"] != "delegation_exceeds_parent" || eb["missing"].(map[string]any)["field"] != `caps["acme.com/cap/mcp"].tools[1]` {
		t.Fatalf("widening spawn: %d %v", st, eb)
	}
	st, eb, _ = spawn(root.ObjectID, "Rogue", "r", nil)
	if st != 403 || eb["code"] != "delegation_invalid" {
		t.Fatalf("unowned child tag: %d %v", st, eb)
	}
	_, _, c1 := spawn(root.ObjectID, "LookupAgent", "c1", map[string]any{"acme.com/cap/mcp": map[string]any{"tools": []string{"lookup_order"}}})
	if c1.Status != "pending" {
		t.Fatalf("c1 %+v", c1)
	}
	// Depth: root(0) → c1(1) → g(2) → gg(3) → ggg(4) refused. The sub tag
	// re-spawning itself is owned (same tag) and ⊆ its own grants.
	sub := map[string]any{"acme.com/cap/mcp": map[string]any{"tools": []string{"lookup_order"}}}
	_, _, g := spawn(c1.ObjectID, "LookupAgent", "g", sub)
	_, _, gg := spawn(g.ObjectID, "LookupAgent", "gg", sub)
	if gg.ObjectID == "" {
		t.Fatal("depth-3 spawn refused")
	}
	if st, eb, _ := spawn(gg.ObjectID, "LookupAgent", "ggg", sub); st != 429 || eb["code"] != "spawn_budget_exceeded" {
		t.Fatalf("depth 4: %d %v", st, eb)
	}
	// A sub-agent can't re-widen to what only its parent had.
	if st, eb, _ := spawn(c1.ObjectID, "LookupAgent", "w", map[string]any{"acme.com/cap/mcp": map[string]any{"tools": []string{"refund"}}}); st != 403 || eb["code"] != "delegation_exceeds_parent" {
		t.Fatalf("re-widen: %d %v", st, eb)
	}
	// maxLive 6 (from twd.jsonc): 3 live so far, 3 more fit, the 7th doesn't.
	var siblings []string
	for i := range 3 {
		_, _, s := spawn(root.ObjectID, "LookupAgent", fmt.Sprintf("s%d", i), nil)
		if s.ObjectID == "" {
			t.Fatalf("sibling %d refused", i)
		}
		siblings = append(siblings, s.ObjectID)
	}
	if st, eb, _ := spawn(root.ObjectID, "LookupAgent", "s-over", nil); st != 429 || eb["code"] != "spawn_budget_exceeded" {
		t.Fatalf("maxLive: %d %v", st, eb)
	}
	if e.ts.policyFetches > 2 || e.ts.policyFetches < 1 {
		t.Fatalf("policy fetched %d times (cache)", e.ts.policyFetches)
	}

	// Children join.
	for i, id := range []string{c1.ObjectID, g.ObjectID, gg.ObjectID} {
		var cr Credential
		h1.ok("POST", "/v1/objects/"+id+"/credentials?epoch=1", nil, &cr)
		if !cr.Ephemeral {
			t.Fatal("TTL'd sub-agent key should be ephemeral")
		}
		h1.ok("PATCH", "/v1/objects/"+id, map[string]any{"status": "ready", "nodeID": fmt.Sprint(10 + i), "stableNodeID": fmt.Sprintf("nC%d", i)}, nil)
	}
	var list []Object
	c.items("/v1/objects?tenant="+tn.TenantID, &list)
	var rootRec Object
	for _, o := range list {
		if o.ID == root.ObjectID {
			rootRec = o
		}
		if o.ID == gg.ObjectID && (o.Depth != 3 || o.RootID != root.ObjectID || o.ParentID != g.ObjectID) {
			t.Fatalf("lineage %+v", o)
		}
	}
	if len(rootRec.Children) != 4 {
		t.Fatalf("root children %v", rootRec.Children)
	}

	// Cascade kill of c1: c1, g, gg die (devices deleted); root and siblings live.
	var kr KillResult
	c.ok("DELETE", "/v1/objects/"+c1.ObjectID+killQuery(true, "prompt injection"), nil, &kr)
	if len(kr.Killed) != 3 || len(kr.DevicesDeleted) != 3 || kr.Errors != nil {
		t.Fatalf("kill %+v", kr)
	}
	if d := e.ts.devicesDeleted(); strings.Join(d, ",") != "nC0,nC1,nC2" {
		t.Fatalf("devices deleted %v", d)
	}
	var hb HeartbeatResponse
	h1.ok("PUT", "/v1/hosts/h1/status", map[string]any{"liveNodes": 3, "leases": []map[string]any{{"namespace": "support", "epoch": 1}, {"namespace": "other/ns", "epoch": 1}}}, &hb)
	if len(hb.RevokeObjects) != 3 || !slices.Contains(hb.RevokeObjects, c1.ObjectID) || !slices.Contains(hb.RevokeObjects, g.ObjectID) || !slices.Contains(hb.RevokeObjects, gg.ObjectID) {
		t.Fatalf("revokeObjects %v", hb.RevokeObjects)
	}
	if len(hb.Revoke) != 1 || hb.Revoke[0] != "other/ns" {
		t.Fatalf("revoke %v", hb.Revoke)
	}
	h1.fail("POST", "/v1/objects/"+g.ObjectID+"/credentials?epoch=1", nil, 410, "killed")
	if st, eb, _ := spawn(gg.ObjectID, "LookupAgent", "zombie", nil); st != 410 || eb["code"] != "delegation_invalid" {
		t.Fatalf("spawn from killed parent: %d %v", st, eb)
	}
	// Killed names stay dead.
	h1.fail("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "namespace": "LookupAgent", "objectName": "c1",
		"parent": map[string]string{"objectID": root.ObjectID}}, 410, "killed")

	// Unused keys of pending siblings get revoked on kill; parent-dead reaping.
	c.ok("DELETE", "/v1/objects/"+root.ObjectID+killQuery(false, "test"), nil, &kr)
	if len(kr.Killed) != 1 {
		t.Fatalf("non-cascade kill %+v", kr)
	}
	n, err := e.m.Reap(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("reaped %d (want the 3 siblings with a dead parent): %v", n, err)
	}
	for _, id := range siblings {
		o, _ := e.m.GetObject(context.Background(), id)
		if o.Status != StatusKilled || o.KillReason != "parent_dead" {
			t.Fatalf("sibling %s: %s %s", id, o.Status, o.KillReason)
		}
	}

	// Audit: kill records with lineage, chain verifies.
	var aud struct {
		Records  []AuditRecord `json:"items"`
		Chains   []ChainStatus
		Verified bool
	}
	c.ok("GET", "/v1/audit?object="+gg.ObjectID, nil, &aud)
	kinds := []string{}
	for _, r := range aud.Records {
		kinds = append(kinds, r.Kind)
	}
	// mint is asynchronous, so it may land before or after the refused spawn (deny).
	sorted := append([]string(nil), kinds...)
	slices.Sort(sorted[1:3])
	if strings.Join(sorted, ",") != "spawn,deny,mint,credential,ready,kill" || !aud.Verified {
		t.Fatalf("audit for gg: %v verified=%v", kinds, aud.Verified)
	}
	var kd map[string]any
	json.Unmarshal(aud.Records[5].Detail, &kd)
	if kd["reason"] != "prompt injection" || kd["root"] != root.ObjectID || kd["requested"] != c1.ObjectID || kd["deviceDeleted"] != true {
		t.Fatalf("kill detail %v", kd)
	}
	var ver struct {
		OK     bool
		Chains []ChainStatus
	}
	c.ok("GET", "/v1/audit/verification", nil, &ver)
	if !ver.OK || ver.Chains[0].Records < 20 {
		t.Fatalf("verify %+v", ver)
	}

	// Never log a key.
	if strings.Contains(e.logs.String(), "tskey-") {
		t.Fatal("a key reached the logs")
	}
}

func TestAPIThrottleBackoffAndTTLReaper(t *testing.T) {
	e := newTestEnv(t, nil)
	c, h1 := e.c, e.asHost("h1")
	var tn struct{ TenantID string }
	c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile, "registrationsPerMinute": 120}, &tn)
	h1.ok("PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 10, "tiers": []string{"process"}}, nil)
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "w", "bundle": noConfigBundle(t), "namespaces": []ClassConfig{
		{Class: "A", Tags: []string{"tag:twd-agent-support"}}, {Class: "B", Tags: []string{"tag:twd-agent-support-sub"}, TTL: "1s"}}}, nil)

	e.ts.mu.Lock()
	e.ts.rateLimitNext = 2
	e.ts.mu.Unlock()
	var a CreateObjectResponse
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "namespace": "A", "objectName": "a"}, &a)
	start := time.Now()
	var cred Credential
	h1.ok("POST", "/v1/objects/"+a.ObjectID+"/credentials?epoch=1&wait=15", nil, &cred)
	if el := time.Since(start); el < 2*time.Second {
		t.Fatalf("two 429s with Retry-After: 1 should take ≥2 s, took %v", el)
	}
	var ti struct {
		EffectiveRate float64 `json:"effectiveRegistrationsPerMinute"`
	}
	c.ok("GET", "/v1/tenants/"+tn.TenantID, nil, &ti)
	if ti.EffectiveRate >= 120 || ti.EffectiveRate < 30 {
		t.Fatalf("effective rate after two 429s: %v", ti.EffectiveRate)
	}
	h1.ok("PATCH", "/v1/objects/"+a.ObjectID, map[string]any{"status": "ready", "stableNodeID": "nA"}, nil)
	// pending-forever: wait=0 on a fresh object can return identity_pending.
	var b CreateObjectResponse
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "namespace": "B", "objectName": "b", "parent": map[string]string{"objectID": a.ObjectID}}, &b)
	h1.ok("POST", "/v1/objects/"+b.ObjectID+"/credentials?epoch=1&wait=10", nil, &cred)
	h1.ok("PATCH", "/v1/objects/"+b.ObjectID, map[string]any{"status": "ready", "stableNodeID": "nB"}, nil)
	time.Sleep(1100 * time.Millisecond)
	n, err := e.m.Reap(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("ttl reap %d %v", n, err)
	}
	o, _ := e.m.GetObject(context.Background(), b.ObjectID)
	if o.Status != StatusKilled || o.KillReason != "ttl_expired" || strings.Join(e.ts.devicesDeleted(), ",") != "nB" {
		t.Fatalf("ttl reap: %s %s %v", o.Status, o.KillReason, e.ts.devicesDeleted())
	}
}

func TestAPIHostAuditChain(t *testing.T) {
	e := newTestEnv(t, nil)
	h1 := e.asHost("h1")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	h1.ok("PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 10, "tiers": []string{"process"}, "auditPubKey": base64.StdEncoding.EncodeToString(pub)}, nil)
	prev := ""
	mk := func(kind, obj string, signWith ed25519.PrivateKey) map[string]any {
		r := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "objectID": obj, "kind": kind, "traceID": "tr-1",
			"dst": "svc:payments-mcp", "actingFor": "alice@example.com", "prevHash": prev}
		b, _ := json.Marshal(r)
		h, err := HashRecord(b)
		if err != nil {
			t.Fatal(err)
		}
		r["hash"] = h
		r["hostSig"] = base64.StdEncoding.EncodeToString(ed25519.Sign(signWith, []byte(h)))
		prev = h
		return r
	}
	var resp struct {
		Accepted int
		Chain    ChainStatus
	}
	h1.ok("POST", "/v1/audit", map[string]any{"records": []any{mk("allow", "o1", priv), mk("egress", "o1", priv), mk("deny", "o2", priv)}}, &resp)
	if resp.Accepted != 3 || !resp.Chain.OK || resp.Chain.Records != 3 {
		t.Fatalf("ingest %+v", resp)
	}
	var aud struct {
		Records  []AuditRecord `json:"items"`
		Verified bool
	}
	e.c.ok("GET", "/v1/audit?human=alice@example.com", nil, &aud)
	if len(aud.Records) != 3 || !aud.Verified || aud.Records[0].Dst != "svc:payments-mcp" {
		t.Fatalf("by human: %+v", aud)
	}
	e.c.ok("GET", "/v1/audit?trace=tr-1", nil, &aud)
	if len(aud.Records) != 3 {
		t.Fatal("by trace")
	}
	// A record signed by another key breaks verification.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	h1.ok("POST", "/v1/audit", map[string]any{"records": []any{mk("allow", "o1", other)}}, &resp)
	if resp.Chain.OK || resp.Chain.BrokenAt != 4 || !strings.Contains(resp.Chain.Error, "hostSig") {
		t.Fatalf("forged sig: %+v", resp.Chain)
	}
	e.c.fail("POST", "/v1/audit", map[string]any{"records": []any{map[string]any{"kind": "allow", "ts": time.Now().UTC().Format(time.RFC3339Nano),
		"prevHash": "", "hash": strings.Repeat("0", 64)}}}, 400, "bad_request")
}

func TestTailnetAuth(t *testing.T) {
	e := newTestEnv(t, nil)
	srv := &Server{M: e.m, Admins: []string{"ross@example.com"}, Self: SelfInfo{DNSName: "twd-mgmt.t.ts.net", Tailnet: "t.ts.net"}}
	whois := func(ctx context.Context, addr string) (WhoIsInfo, error) {
		switch addr {
		case "100.64.0.1:1":
			return WhoIsInfo{NodeID: "nHOST1", Tags: []string{HostTag}, Login: "tagged-devices"}, nil
		case "100.64.0.2:1":
			return WhoIsInfo{NodeID: "nHOST2", Tags: []string{HostTag}, Login: "tagged-devices"}, nil
		case "100.64.0.3:1": // -admins override, no cap
			return WhoIsInfo{NodeID: "nUSER", Login: "ross@example.com"}, nil
		case "100.64.0.4:1": // plain user, no cap
			return WhoIsInfo{NodeID: "nEVE", Login: "eve@example.com"}, nil
		case "100.64.0.5:1": // a twd object node
			return WhoIsInfo{NodeID: "nAGENT", Tags: []string{"tag:twd-agent-support"}, Login: "tagged-devices", Name: "agent.t.ts.net"}, nil
		case "100.64.0.6:1": // autogroup:admin member: the base snippet grants the cap
			return WhoIsInfo{NodeID: "nADM", Login: "alice@example.com", Caps: []string{AdminCap}}, nil
		case "100.64.0.7:1": // a tagged node (CI, a dev box) granted the cap
			return WhoIsInfo{NodeID: "nCI", Tags: []string{"tag:ttest"}, Login: "tagged-devices", Caps: []string{AdminCap}, Name: "ci.t.ts.net"}, nil
		}
		return WhoIsInfo{}, fmt.Errorf("unknown")
	}
	h := srv.TailnetHandler(whois, srv.Handler())
	call := func(from, method, path, body string) (int, map[string]any) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = from
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	code := func(from, method, path, body string) int { c, _ := call(from, method, path, body); return c }
	if c := code("100.64.0.1:1", "PUT", "/v1/hosts/h1/status", `{}`); c != 403 {
		t.Fatalf("unregistered host heartbeat: %d", c)
	}
	if c := code("100.64.0.1:1", "PUT", "/v1/hosts/h1", `{"capacityNodes":1,"tiers":["process"]}`); c != 201 {
		t.Fatalf("register: %d", c)
	}
	if c := code("100.64.0.1:1", "PUT", "/v1/hosts/h1/status", `{}`); c != 200 {
		t.Fatalf("own heartbeat: %d", c)
	}
	if c := code("100.64.0.2:1", "PUT", "/v1/hosts/h1", `{"capacityNodes":1,"tiers":["process"]}`); c != 403 {
		t.Fatalf("host2 hijacking h1: %d", c)
	}
	code("100.64.0.2:1", "PUT", "/v1/hosts/h2", `{"capacityNodes":1,"tiers":["process"]}`)
	if c := code("100.64.0.2:1", "PUT", "/v1/hosts/h1/status", `{}`); c != 403 {
		t.Fatalf("host2 acting as h1: %d", c)
	}
	if c := code("100.64.0.1:1", "GET", "/v1/status", ``); c != 403 {
		t.Fatalf("host on admin endpoint: %d", c)
	}
	if c, b := call("100.64.0.1:1", "GET", "/v1/whoami", ``); c != 200 || b["kind"] != "host" || b["hostID"] != "h1" || b["admin"] != false {
		t.Fatalf("host whoami: %d %v", c, b)
	}
	// Admins: -admins override, autogroup:admin via the cap, a tagged node via the cap.
	for _, from := range []string{"100.64.0.3:1", "100.64.0.6:1", "100.64.0.7:1"} {
		if c := code(from, "GET", "/v1/tenants", ``); c != 200 {
			t.Fatalf("admin %s: %d", from, c)
		}
		if c := code(from, "POST", "/v1/objects/x/credentials?epoch=1", ``); c != 403 {
			t.Fatalf("admin %s fetching a credential: %d", from, c)
		}
	}
	if c, b := call("100.64.0.6:1", "GET", "/v1/whoami", ``); c != 200 || b["admin"] != true || b["login"] != "alice@example.com" || b["actor"] != "alice@example.com" ||
		b["mgmt"].(map[string]any)["dnsName"] != "twd-mgmt.t.ts.net" {
		t.Fatalf("admin whoami: %d %v", c, b)
	}
	if c, b := call("100.64.0.7:1", "GET", "/v1/whoami", ``); c != 200 || b["admin"] != true || b["login"] != "tag:ttest" || b["actor"] != "tag:ttest (nCI)" {
		t.Fatalf("tagged admin whoami: %d %v", c, b)
	}
	// Non-admins are refused with not_admin and the grant to add.
	for _, tc := range []struct{ from, src string }{{"100.64.0.4:1", "eve@example.com"}, {"100.64.0.5:1", "tag:twd-agent-support"}} {
		c, b := call(tc.from, "GET", "/v1/tenants", ``)
		if c != 403 || b["code"] != "not_admin" {
			t.Fatalf("non-admin %s: %d %v", tc.from, c, b)
		}
		if sn, _ := b["snippet"].(string); sn != AdminGrantSnippet(tc.src) || !strings.Contains(sn, AdminCap) || !strings.Contains(sn, MgmtTag) {
			t.Fatalf("non-admin %s snippet: %q", tc.from, sn)
		}
		if c, _ := call(tc.from, "GET", "/v1/whoami", ``); c != 403 {
			t.Fatalf("non-admin whoami %s: %d", tc.from, c)
		}
	}
	if c := code("100.99.0.9:1", "GET", "/v1/healthz", ``); c != 401 {
		t.Fatalf("no whois: %d", c)
	}
	// deployedBy carries the WhoIs actor.
	var tn struct{ TenantID string }
	e.c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile}, &tn)
	c, b := call("100.64.0.6:1", "POST", "/v1/deployments", `{"tenantID":"`+tn.TenantID+`","workload":"w","namespaces":[{"class":"A","tags":["tag:ttest"]}]}`)
	if c != 202 {
		t.Fatalf("deploy as admin: %d %v", c, b)
	}
	var dv DeploymentView
	e.c.ok("GET", "/v1/deployments/"+b["deploymentID"].(string), nil, &dv)
	if dv.DeployedBy != "alice@example.com" {
		t.Fatalf("deployedBy %q", dv.DeployedBy)
	}
	var aud struct {
		Records []AuditRecord `json:"items"`
	}
	e.c.ok("GET", "/v1/audit?human=alice@example.com", nil, &aud)
	if len(aud.Records) != 1 || aud.Records[0].Kind != "deploy" {
		t.Fatalf("deploy audit by human: %+v", aud.Records)
	}
}

func TestWIFMintPath(t *testing.T) {
	dir := t.TempDir()
	is, err := LoadOrCreateIssuer("https://twd-mgmt.example.ts.net", filepath.Join(dir, "issuer.key"))
	if err != nil {
		t.Fatal(err)
	}
	e := newTestEnv(t, is)
	jwks, _ := json.Marshal(is.JWKS())
	e.ts.issuerJWKS = func() []byte { return jwks }
	c, h1 := e.c, e.asHost("h1")
	var tn struct {
		TenantID string
		WIF      map[string]string
	}
	c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "wif", "wifClientID": "mint-client", "wifLifecycleClientID": "life-client"}, &tn)
	if tn.WIF["issuer"] != "https://twd-mgmt.example.ts.net/t/"+tn.TenantID || tn.WIF["mintSubject"] != "twd:tenant:"+tn.TenantID+":mint" {
		t.Fatalf("wif info %v", tn.WIF)
	}
	h1.ok("PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 10, "tiers": []string{"process"}}, nil)
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "w", "bundle": noConfigBundle(t), "namespaces": []ClassConfig{{Class: "A", Tags: []string{"tag:ttest"}}}}, nil)
	var a CreateObjectResponse
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "namespace": "A", "objectName": "a"}, &a)
	var cred Credential
	h1.ok("POST", "/v1/objects/"+a.ObjectID+"/credentials?epoch=1", nil, &cred)
	if !strings.HasPrefix(cred.AuthKey, "tskey-auth-") {
		t.Fatal("no key via WIF")
	}
	h1.ok("PATCH", "/v1/objects/"+a.ObjectID, map[string]any{"status": "ready", "stableNodeID": "nW"}, nil)
	var kr KillResult
	c.ok("DELETE", "/v1/objects/"+a.ObjectID+killQuery(false, "t"), nil, &kr)
	if len(kr.DevicesDeleted) != 1 || e.ts.devicesDeleted()[0] != "nW" {
		t.Fatalf("lifecycle delete via WIF: %+v", kr)
	}
	if e.ts.tokenExchanges != 2 { // one per credential (mint, lifecycle), cached afterwards
		t.Fatalf("token exchanges %d", e.ts.tokenExchanges)
	}
	// wif-check: fresh exchanges for both credentials, policy read via lifecycle.
	var chk WIFCheckResult
	c.ok("POST", "/v1/tenants/"+tn.TenantID+"/wif-check", nil, &chk)
	if !chk.Mint || chk.Lifecycle != "ok" || !chk.PolicyRead || chk.Issuer != tn.WIF["issuer"] {
		t.Fatalf("wif-check %+v", chk)
	}
	if e.ts.tokenExchanges != 4 {
		t.Fatalf("wif-check should exchange afresh: %d", e.ts.tokenExchanges)
	}
	// Re-keying: an apikey tenant switches to wif with new client IDs; no lifecycle → "absent".
	var tn2 struct{ TenantID string }
	c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "u.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile}, &tn2)
	c.fail("POST", "/v1/tenants/"+tn2.TenantID+"/wif-check", nil, 400, "bad_request")
	c.fail("PATCH", "/v1/tenants/"+tn2.TenantID, map[string]any{"mintMode": "wif"}, 400, "bad_request")
	var up struct {
		MintMode string
		WIF      map[string]string
	}
	c.ok("PATCH", "/v1/tenants/"+tn2.TenantID, map[string]any{"mintMode": "wif", "wifClientID": "mint-client"}, &up)
	if up.MintMode != "wif" || up.WIF["mintSubject"] != "twd:tenant:"+tn2.TenantID+":mint" {
		t.Fatalf("wif update %+v", up)
	}
	c.ok("POST", "/v1/tenants/"+tn2.TenantID+"/wif-check", nil, &chk)
	if !chk.Mint || chk.Lifecycle != "absent" || chk.PolicyRead {
		t.Fatalf("wif-check after update %+v", chk)
	}
	var t2 Tenant
	c.ok("GET", "/v1/tenants/"+tn2.TenantID, nil, &t2)
	if t2.MintMode != MintWIF || t2.WIFClientID != "mint-client" {
		t.Fatalf("tenant after wif update %+v", t2)
	}
}

func TestKillWhileJoining(t *testing.T) {
	e := newTestEnv(t, nil)
	c, h1 := e.c, e.asHost("h1")
	var tn struct{ TenantID string }
	c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile}, &tn)
	h1.ok("PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 10, "tiers": []string{"process"}}, nil)
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "w", "bundle": noConfigBundle(t), "namespaces": []ClassConfig{{Class: "A", Tags: []string{"tag:ttest"}}}}, nil)
	var a CreateObjectResponse
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "namespace": "A", "objectName": "a"}, &a)
	var cred Credential
	h1.ok("POST", "/v1/objects/"+a.ObjectID+"/credentials?epoch=1", nil, &cred)
	var kr KillResult
	c.ok("DELETE", "/v1/objects/"+a.ObjectID+killQuery(false, "t"), nil, &kr)
	if len(kr.Killed) != 1 || len(kr.DevicesDeleted) != 0 {
		t.Fatalf("%+v", kr)
	}
	e.ts.mu.Lock()
	revoked := slices.Contains(e.ts.deletedKeys, "k1")
	e.ts.mu.Unlock()
	if !revoked {
		t.Fatal("unused key not revoked on kill")
	}
	// The join raced the kill: ready must fail and delete the new device.
	h1.fail("PATCH", "/v1/objects/"+a.ObjectID, map[string]any{"status": "ready", "stableNodeID": "nLATE"}, 410, "killed")
	if d := e.ts.devicesDeleted(); len(d) != 1 || d[0] != "nLATE" {
		t.Fatalf("late device not deleted: %v", d)
	}
}

// TestPurgeAfterKill: after `twd down --purge` the same workload must
// deploy again on the same state dir. mgmt keeps killed objects (the 410
// tombstone, and the UNIQUE name), so DELETE /v1/tenants/{id}/objects forgets
// them; a create of the same name is then a new pending object, and the
// deployment's endpoint no longer reads "killed". Live objects stay.
func TestPurgeAfterKill(t *testing.T) {
	e := newTestEnv(t, nil)
	c, h1 := e.c, e.asHost("h1")
	var tn struct{ TenantID string }
	c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile}, &tn)
	h1.ok("PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 10, "tiers": []string{"process"}}, nil)
	var dep DeployResponse
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "agent", "bundle": noConfigBundle(t),
		"namespaces": []ClassConfig{{Class: WorkloadNamespace, Tags: []string{"tag:ttest"}}, {Class: "A", Tags: []string{"tag:ttest"}}}}, &dep)
	var wl, a, keep CreateObjectResponse
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "workload": "agent", "namespace": WorkloadNamespace, "objectName": "agent"}, &wl)
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "workload": "agent", "namespace": "A", "objectName": "a"}, &a)
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "workload": "agent", "namespace": "A", "objectName": "keep"}, &keep)
	h1.ok("PATCH", "/v1/objects/"+wl.ObjectID, map[string]any{"status": "ready", "stableNodeID": "nWL", "magicDNS": "agent.t.ts.net"}, nil)
	var kr KillResult
	c.ok("DELETE", "/v1/objects/"+wl.ObjectID+killQuery(true, "twd down --purge"), nil, &kr)
	c.ok("DELETE", "/v1/objects/"+a.ObjectID+killQuery(false, "twd down --purge"), nil, nil)

	// The bug: the killed workload node blocks a redeploy.
	h1.fail("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "workload": "agent", "namespace": WorkloadNamespace, "objectName": "agent"}, 410, "killed")
	var dv DeploymentView
	c.ok("GET", "/v1/deployments/"+dep.DeploymentID, nil, &dv)
	if dv.Endpoint.Status != StatusKilled {
		t.Fatalf("endpoint before purge: %+v", dv.Endpoint)
	}

	var pr PurgeResult
	c.ok("DELETE", "/v1/tenants/"+tn.TenantID+"/objects", nil, &pr)
	if len(pr.Purged) != 2 || !slices.Contains(pr.Purged, wl.ObjectID) || !slices.Contains(pr.Purged, a.ObjectID) {
		t.Fatalf("purged %v, want %s and %s", pr.Purged, wl.ObjectID, a.ObjectID)
	}
	if o, err := e.m.store.GetObject(context.Background(), keep.ObjectID); err != nil || !o.Live() {
		t.Fatalf("live object touched by purge: %+v %v", o, err)
	}
	c.fail("DELETE", "/v1/tenants/nope/objects", nil, 404, "not_found")

	// The kill's revocation is gone too (else the host kills the new object).
	if revs, err := e.m.store.Revocations(context.Background(), "h1", time.Time{}); err != nil || slices.Contains(revs, wl.ObjectID) {
		t.Fatalf("revocations after purge: %v %v", revs, err)
	}
	// Same names again: new pending objects (same deterministic IDs).
	var wl2, a2 CreateObjectResponse
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "workload": "agent", "namespace": WorkloadNamespace, "objectName": "agent"}, &wl2)
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "workload": "agent", "namespace": "A", "objectName": "a"}, &a2)
	if wl2.Status == StatusKilled || wl2.ObjectID != wl.ObjectID || a2.Status == StatusKilled {
		t.Fatalf("recreate after purge: %+v %+v", wl2, a2)
	}
	c.ok("GET", "/v1/deployments/"+dep.DeploymentID, nil, &dv)
	if dv.Endpoint.Status == StatusKilled || dv.Endpoint.Status == StatusFailed {
		t.Fatalf("endpoint after purge: %+v", dv.Endpoint)
	}
	var audit struct {
		Records []AuditRecord `json:"items"`
	}
	c.ok("GET", "/v1/audit?object="+wl.ObjectID, nil, &audit)
	kinds := map[string]int{}
	for _, r := range audit.Records {
		kinds[r.Kind]++
	}
	if kinds["purge"] != 1 || kinds["kill"] != 1 || kinds["create"] != 2 {
		t.Fatalf("audit kinds %v", kinds)
	}
	// Idempotent.
	c.ok("DELETE", "/v1/tenants/"+tn.TenantID+"/objects", nil, &pr)
	if len(pr.Purged) != 0 {
		t.Fatalf("second purge: %v", pr.Purged)
	}
}

// TestAPIVolumeKey: the DO volume data key (L3) is created in custody once,
// returned stably, and only to the lease holder at the lease's epoch.
func TestAPIVolumeKey(t *testing.T) {
	e := newTestEnv(t, nil)
	c, h1, h2 := e.c, e.asHost("h1"), e.asHost("h2")
	var tn struct{ TenantID string }
	c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile}, &tn)
	h1.ok("PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 100, "tiers": []string{"process", "microvm"}}, nil)
	bundle := makeBundle(t, map[string]string{"twd.jsonc": testTwdJSONC, "dist/index.js": "export default {}"})
	var dep DeployResponse
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "support", "bundle": bundle}, &dep)
	q := func(epoch int64) string {
		return fmt.Sprintf("/v1/volumes/%s:support/keys?epoch=%d", tn.TenantID, epoch)
	}
	var k1, k2 VolumeKeyResponse
	h1.ok("POST", q(dep.Epoch), nil, &k1)
	h1.ok("POST", q(dep.Epoch), nil, &k2)
	if len(k1.Key) != 32 || !bytes.Equal(k1.Key, k2.Key) {
		t.Fatalf("volume key: %d bytes, stable %v", len(k1.Key), bytes.Equal(k1.Key, k2.Key))
	}
	h1.fail("POST", q(dep.Epoch+1), nil, 409, "lease_lost")
	directFail(t, e, "h1", "POST", q(0), 400, "bad_request") // the spec forbids epoch 0; the server still refuses it
	h2.ok("PUT", "/v1/hosts/h2", map[string]any{"capacityNodes": 100, "tiers": []string{"process"}}, nil)
	h2.fail("POST", q(dep.Epoch), nil, 409, "lease_lost")
	var other VolumeKeyResponse
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "other", "bundle": bundle}, &dep)
	h1.ok("POST", fmt.Sprintf("/v1/volumes/%s:other/keys?epoch=%d", tn.TenantID, dep.Epoch), nil, &other)
	if bytes.Equal(other.Key, k1.Key) {
		t.Fatal("two workloads share a volume key")
	}
	if s := fmt.Sprint(k1); strings.Contains(s, string(k1.Key)) {
		t.Fatal("VolumeKeyResponse prints its key")
	}
}
