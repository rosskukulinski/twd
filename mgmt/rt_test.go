package mgmt

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/internal/nodecert"
)

// RT-01: mgmt signs a node certificate at ready, carrying the spawn lim,
// and serves it by stable node ID for receivers; the tenant's OBO public
// key verifies it.
func TestNodeCertificates(t *testing.T) {
	e := newTestEnv(t, nil)
	c, h1 := e.c, e.asHost("h1")
	var tn struct{ TenantID string }
	c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile}, &tn)
	h1.ok("PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 100, "tiers": []string{"process"}}, nil)
	bundle := makeBundle(t, map[string]string{"twd.jsonc": testTwdJSONC, "dist/index.js": "export default {}"})
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "support", "bundle": bundle}, nil)

	var root, child CreateObjectResponse
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "namespace": "SupportAgent", "objectName": "alice"}, &root)
	h1.ok("POST", "/v1/objects/"+root.ObjectID+"/credentials?epoch=1", nil, &Credential{})
	h1.ok("PATCH", "/v1/objects/"+root.ObjectID, map[string]any{"status": "ready", "nodeID": "1", "stableNodeID": "nROOT", "magicDNS": root.ObjectID + ".t.ts.net"}, nil)
	lim := map[string]any{"acme.com/cap/mcp": map[string]any{"tools": []string{"lookup_order"}}}
	h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "namespace": "LookupAgent", "objectName": "c1",
		"parent": map[string]string{"objectID": root.ObjectID}, "caps": lim}, &child)
	h1.ok("POST", "/v1/objects/"+child.ObjectID+"/credentials?epoch=1", nil, &Credential{})
	h1.ok("PATCH", "/v1/objects/"+child.ObjectID, map[string]any{"status": "ready", "nodeID": "2", "stableNodeID": "nCHILD"}, nil)

	var tv struct {
		OBOPublicKey string `json:"oboPublicKey"`
	}
	c.ok("GET", "/v1/tenants/"+tn.TenantID, nil, &tv)
	pub, err := base64.StdEncoding.DecodeString(tv.OBOPublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("tenant OBO public key: %q %v", tv.OBOPublicKey, err)
	}

	var cert NodeCertView
	h1.ok("GET", "/v1/certificates/nCHILD?tenant="+tn.TenantID, nil, &cert)
	if cert.ObjectID != child.ObjectID || cert.Payload == nil || cert.Payload.ParentID != root.ObjectID || cert.Payload.RootID != root.ObjectID ||
		cert.Payload.Tags[0] != "tag:twd-agent-support-sub" || !strings.Contains(string(cert.Payload.Lim), "lookup_order") {
		t.Fatalf("cert view: %+v payload %+v", cert, cert.Payload)
	}
	p, err := nodecert.Verify(ed25519.PublicKey(pub), cert.Cert, "nCHILD", time.Now())
	if err != nil || p.ObjectID != child.ObjectID || p.TenantID != tn.TenantID {
		t.Fatalf("verify against the tenant key: %v %+v", err, p)
	}
	if _, err := nodecert.Verify(ed25519.PublicKey(pub), cert.Cert, "nROOT", time.Now()); err == nil {
		t.Fatal("the child's certificate verified for the root's node")
	}
	// The root has no lim.
	var rc NodeCertView
	h1.ok("GET", "/v1/certificates/nROOT?tenant="+tn.TenantID, nil, &rc)
	if len(rc.Payload.Lim) != 0 || rc.Payload.ParentID != "" {
		t.Fatalf("root cert: %+v", rc.Payload)
	}
	// Unknown node / wrong tenant: 404 (the receiver then applies the plain
	// WhoIs caps: not a twd object). Killed: 410.
	h1.fail("GET", "/v1/certificates/nNOBODY?tenant="+tn.TenantID, nil, 404, "not_found")
	h1.fail("GET", "/v1/certificates/nCHILD?tenant=tn_other", nil, 404, "not_found")
	c.ok("DELETE", "/v1/objects/"+child.ObjectID+killQuery(false, "test"), nil, nil)
	h1.fail("GET", "/v1/certificates/nCHILD?tenant="+tn.TenantID, nil, 410, "killed")
	// The audit trail records the certificate and lim at ready.
	var aud struct {
		Records []AuditRecord `json:"items"`
	}
	c.ok("GET", "/v1/audit?object="+child.ObjectID, nil, &aud)
	sawReady := false
	for _, r := range aud.Records {
		if r.Kind == "ready" {
			sawReady = strings.Contains(string(r.Detail), `"cert":true`) && strings.Contains(string(r.Detail), "lookup_order")
		}
	}
	if !sawReady {
		t.Fatalf("ready record lacks cert/lim: %+v", aud.Records)
	}
}

// RT-03: the unix socket admits nobody without the per-host token, not even
// for /v1/healthz, and the token file is minted 0600 and rotatable.
func TestHostTokenOnUnixSocket(t *testing.T) {
	e := newTestEnv(t, nil)
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", e.sock)
	}}}
	get := func(path, token string) (int, string) {
		req, _ := http.NewRequest("GET", "http://twd-mgmt"+path, nil)
		if token != "" {
			req.Header.Set(HostTokenHeader, token)
		}
		res, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var eb struct {
			Code string `json:"code"`
		}
		json.NewDecoder(res.Body).Decode(&eb)
		return res.StatusCode, eb.Code
	}
	for _, path := range []string{"/v1/healthz", "/v1/tenants", "/v1/hosts", "/v1/audit"} {
		if st, code := get(path, ""); st != 401 || code != "host_token_required" {
			t.Fatalf("%s without token: %d %s", path, st, code)
		}
		if st, code := get(path, NewHostToken()); st != 403 || code != "host_token_invalid" {
			t.Fatalf("%s with a wrong token: %d %s", path, st, code)
		}
	}
	if st, _ := get("/v1/healthz", e.token); st != 200 {
		t.Fatalf("healthz with the token: %d", st)
	}
	// Token file: minted 0600, stable across loads, rotated on demand.
	dir := t.TempDir()
	path := dir + "/hostd/mgmt.token"
	tok1, err := LoadOrCreateHostToken(path, false)
	if err != nil || len(tok1) != 64 {
		t.Fatal(tok1, err)
	}
	if ReadHostToken(path) != tok1 {
		t.Fatal("read back")
	}
	tok2, _ := LoadOrCreateHostToken(path, false)
	tok3, _ := LoadOrCreateHostToken(path, true)
	if tok2 != tok1 || tok3 == tok1 || ReadHostToken(path) != tok3 {
		t.Fatalf("stable=%v rotated=%v", tok2 == tok1, tok3 != tok1)
	}
}

// RT-02: host-submitted records carry their provenance; a tenant source
// may only be a tenant_note and is reported as such.
func TestAuditIngestProvenance(t *testing.T) {
	e := newTestEnv(t, nil)
	h1 := e.asHost("h1")
	e.c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile}, nil) // a mgmt-chain record
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	h1.ok("PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 10, "tiers": []string{"process"}, "auditPubKey": base64.StdEncoding.EncodeToString(pub)}, nil)
	prev := ""
	mk := func(kind, source string) map[string]any {
		r := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "objectID": "o1", "kind": kind, "traceID": "tr-1", "prevHash": prev,
			"detail": map[string]any{"claimedKind": "allow", "grant": "example.com/cap/mcp: refund"}}
		if source != "" {
			r["source"] = source
		}
		b, _ := json.Marshal(r)
		h, _ := HashRecord(b)
		r["hash"] = h
		r["hostSig"] = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(h)))
		return r
	}
	// Refused before anything is appended: the chain head stays put.
	for _, bad := range [][2]string{{"allow", "tenant"}, {"deny", "tenant"}, {"kill", "tenant"}, {"tenant_note", "host"}, {"tenant_note", ""}, {"allow", "mgmt"}} {
		h1.fail("POST", "/v1/audit", map[string]any{"records": []any{mk(bad[0], bad[1])}}, 400, "bad_request")
	}
	var resp struct {
		Accepted int
		Chain    ChainStatus
	}
	good := []any{}
	for _, g := range [][2]string{{"deny", "host"}, {"allow", ""}, {"tenant_note", "tenant"}} {
		r := mk(g[0], g[1])
		prev = r["hash"].(string)
		good = append(good, r)
	}
	h1.ok("POST", "/v1/audit", map[string]any{"records": good}, &resp)
	if resp.Accepted != 3 || !resp.Chain.OK {
		t.Fatalf("ingest %+v", resp)
	}
	var aud struct {
		Records  []AuditRecord `json:"items"`
		Verified bool
	}
	e.c.ok("GET", "/v1/audit?chain=host:h1", nil, &aud)
	if len(aud.Records) != 3 || !aud.Verified {
		t.Fatalf("query: %+v", aud)
	}
	for i, want := range []string{SourceHost, SourceHost, SourceTenant} {
		if aud.Records[i].Source != want {
			t.Fatalf("record %d source %q want %q", i, aud.Records[i].Source, want)
		}
	}
	if aud.Records[2].Kind != KindTenantNote {
		t.Fatalf("tenant record kind %q", aud.Records[2].Kind)
	}
	e.c.ok("GET", "/v1/audit?chain=mgmt&limit=1", nil, &aud)
	if len(aud.Records) == 0 || aud.Records[0].Source != SourceMgmt {
		t.Fatalf("mgmt chain source: %+v", aud.Records)
	}
}
