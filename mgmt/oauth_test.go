package mgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/internal/tsapi"
)

func getPath(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// fakeClientSecret builds an OAuth client secret at runtime (no
// key-shaped literal is ever committed).
func fakeClientSecret(id string) string {
	return "tskey-" + "client-" + id + "-" + strings.Repeat("Q", 24)
}

// writeOAuthFile registers an OAuth client with the fake API and writes its
// credential file with mode perm.
func (e *testEnv) writeOAuthFile(t *testing.T, id string, perm os.FileMode, scopes ...string) string {
	t.Helper()
	sec := fakeClientSecret(id)
	e.ts.mu.Lock()
	e.ts.oauthClients[id] = fakeOAuthClient{secret: sec, scopes: scopes}
	e.ts.mu.Unlock()
	p := filepath.Join(filepath.Dir(e.keyFile), id+".oauth.env")
	if err := os.WriteFile(p, []byte("TS_OAUTH_CLIENT_ID="+id+"\nTS_OAUTH_CLIENT_SECRET="+sec+"\n"), perm); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, perm)
	return p
}

func (e *testEnv) oauthExchanges() int {
	e.ts.mu.Lock()
	defer e.ts.mu.Unlock()
	return e.ts.oauthExchanges
}

// TestOAuthMintPath is mint mode oauth (M12) end to end through the /v1
// API against a fake Tailscale API: 0600 enforcement, the client ID taken
// from the file, the token exchanged once and cached, a 401 (token revoked
// early) retried once with a fresh exchange, host join keys and object
// keys minted with the client, kill with and without devices:core
// (device_delete_unavailable in the kill result and the audit), a tag
// refusal typed as policy_missing_tag_owners, and no secret or token in
// any response or log.
func TestOAuthMintPath(t *testing.T) {
	e := newTestEnv(t, nil) // no issuer: oauth needs none
	c, h1 := e.c, e.asHost("h1")
	full := e.writeOAuthFile(t, "kFULL1CNTRL", 0o600, "auth_keys", "devices:core", "policy_file:read")
	loose := e.writeOAuthFile(t, "kLOOSE1CNTRL", 0o644, "auth_keys")
	mintOnly := e.writeOAuthFile(t, "kMINT1CNTRL", 0o600, "auth_keys")

	// 0600 enforcement and validation at create.
	c.fail("POST", "/v1/tenants", map[string]any{"tailnet": "o.ts.net", "mintMode": "oauth", "oauthClientFile": loose}, 400, "bad_request")
	c.fail("POST", "/v1/tenants", map[string]any{"tailnet": "o.ts.net", "mintMode": "oauth", "oauthClientFile": "rel/oauth.env"}, 400, "bad_request")
	c.fail("POST", "/v1/tenants", map[string]any{"tailnet": "o.ts.net", "mintMode": "oauth"}, 400, "bad_request")
	c.fail("POST", "/v1/tenants", map[string]any{"tailnet": "o.ts.net", "mintMode": "oauth", "oauthClientFile": full, "oauthClientID": "kOTHER"}, 400, "bad_request")
	var tn struct {
		TenantID, MintMode, OAuthClientID, OAuthClientFile string
	}
	st, raw, _ := c.doRaw("POST", "/v1/tenants", map[string]any{"tailnet": "o.ts.net", "mintMode": "oauth", "oauthClientFile": full}, &tn)
	if st != 201 || tn.MintMode != "oauth" || tn.OAuthClientID != "kFULL1CNTRL" || tn.OAuthClientFile != full {
		t.Fatalf("create oauth tenant: %d %+v %v", st, tn, raw)
	}
	b, _ := json.Marshal(raw)
	if strings.Contains(string(b), "tskey-") {
		t.Fatal("the tenant response carries a secret")
	}

	// Host join key and object keys through the OAuth client.
	var hj struct{ AuthKey string }
	c.ok("POST", "/v1/hosts", map[string]any{"hostID": "h-o", "tenantID": tn.TenantID}, &hj)
	if !strings.HasPrefix(hj.AuthKey, "tskey-auth-") {
		t.Fatal("no host join key via oauth")
	}
	if got := getPath(e.ts.createdKey(1), "capabilities", "devices", "create", "tags"); len(got.([]any)) != 1 || got.([]any)[0] != HostTag {
		t.Fatalf("host key tags %v", got)
	}
	h1.ok("PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 10, "tiers": []string{"process"}}, nil)
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "w", "bundle": noConfigBundle(t), "namespaces": []ClassConfig{{Class: "A", Tags: []string{"tag:ttest"}}}}, nil)
	mintObj := func(tenant, name string) string {
		t.Helper()
		var a CreateObjectResponse
		h1.ok("POST", "/v1/objects", map[string]any{"tenantID": tenant, "namespace": "A", "objectName": name}, &a)
		var cred Credential
		h1.ok("POST", "/v1/objects/"+a.ObjectID+"/credentials?epoch=1", nil, &cred)
		if !strings.HasPrefix(cred.AuthKey, "tskey-auth-") {
			t.Fatalf("no object key via oauth for %s", name)
		}
		h1.ok("PATCH", "/v1/objects/"+a.ObjectID, map[string]any{"status": "ready", "stableNodeID": "n-" + name}, nil)
		return a.ObjectID
	}
	a := mintObj(tn.TenantID, "a")
	if n := e.oauthExchanges(); n != 1 {
		t.Fatalf("token exchanges %d, want 1 (cached)", n)
	}
	// The access token is revoked early: the next call gets a 401, mgmt
	// exchanges once more and retries.
	e.ts.mu.Lock()
	for tok := range e.ts.oauthTokens {
		delete(e.ts.oauthTokens, tok)
	}
	e.ts.mu.Unlock()
	mintObj(tn.TenantID, "b")
	if n := e.oauthExchanges(); n != 2 {
		t.Fatalf("token exchanges after a 401: %d, want 2", n)
	}

	// Kill with devices:core: the device is deleted, no warning.
	var kr KillResult
	c.ok("DELETE", "/v1/objects/"+a+killQuery(false, "t"), nil, &kr)
	if len(kr.DevicesDeleted) != 1 || len(kr.Warnings) != 0 {
		t.Fatalf("oauth kill with devices:core: %+v", kr)
	}

	// A client without devices:core (PATCH re-keys the tenant): the kill
	// succeeds and reports device_delete_unavailable with the fix.
	c.ok("PATCH", "/v1/tenants/"+tn.TenantID, map[string]any{"oauthClientFile": mintOnly, "oauthClientID": "kMINT1CNTRL"}, nil)
	x := mintObj(tn.TenantID, "x")
	kr = KillResult{}
	c.ok("DELETE", "/v1/objects/"+x+killQuery(false, "t"), nil, &kr)
	if len(kr.Killed) != 1 || len(kr.DevicesDeleted) != 0 || len(kr.Errors) != 0 || len(kr.Warnings) != 1 ||
		kr.Warnings[0].Code != CodeDeviceDeleteUnavailable || kr.Warnings[0].Objects[0] != x || !strings.Contains(kr.Warnings[0].Fix, "Devices › Core") {
		t.Fatalf("oauth kill without devices:core: %+v", kr)
	}
	var recs struct{ Items []AuditRecord }
	c.ok("GET", "/v1/audit?object="+x, nil, &recs)
	found := false
	for _, r := range recs.Items {
		var d map[string]any
		json.Unmarshal(r.Detail, &d)
		if r.Kind == "kill" && d["deviceDelete"] == CodeDeviceDeleteUnavailable && d["fix"] != nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("kill audit lacks device_delete_unavailable: %+v", recs.Items)
	}
	if st, body := c.do("GET", "/v1/objects/"+x, nil, nil); st != 410 || getPath(body, "tombstone", "deviceDeleted") != false {
		t.Fatalf("object after kill: %d %v", st, body)
	}

	// Deferred deletes (the default -device-delete-delay): the kill warns
	// up front (learned from the 403 above), and the scheduled delete's
	// audit record carries the code and the fix.
	e.m.cfg.DeviceDeleteDelay = time.Hour
	z := mintObj(tn.TenantID, "z")
	kr = KillResult{}
	c.ok("DELETE", "/v1/objects/"+z+killQuery(false, "t"), nil, &kr)
	if len(kr.DevicesDeleting) != 1 || len(kr.Warnings) != 1 || kr.Warnings[0].Code != CodeDeviceDeleteUnavailable {
		t.Fatalf("deferred oauth kill without devices:core: %+v", kr)
	}
	c.ok("POST", "/v1/device-delete-runs", nil, nil)
	recs.Items = nil
	c.ok("GET", "/v1/audit?object="+z, nil, &recs)
	found = false
	for _, r := range recs.Items {
		var d map[string]any
		json.Unmarshal(r.Detail, &d)
		if r.Kind == "device_delete" && d["code"] == CodeDeviceDeleteUnavailable && strings.Contains(fmt.Sprint(d["fix"]), "devices:core") {
			found = true
		}
	}
	if !found {
		t.Fatalf("device_delete audit lacks device_delete_unavailable: %+v", recs.Items)
	}
	e.m.cfg.DeviceDeleteDelay = 0

	// A tag the control plane refuses: typed, with the snippet.
	e.ts.mu.Lock()
	e.ts.refuseTags[HostTag] = true
	e.ts.mu.Unlock()
	body := c.fail("POST", "/v1/hosts", map[string]any{"hostID": "h-p", "tenantID": tn.TenantID}, 403, "policy_missing_tag_owners")
	if sn, _ := body["snippet"].(string); !strings.Contains(sn, `"`+HostTag+`": ["autogroup:admin", "tag:twd"]`) {
		t.Fatalf("policy_missing_tag_owners without the snippet: %v", body)
	}

	// The file loosened after create: mgmt refuses to read it.
	os.Chmod(mintOnly, 0o640)
	e.m.broker.Forget(tn.TenantID)
	cur, _ := e.store.GetTenant(context.Background(), tn.TenantID)
	api, err := e.m.broker.API(context.Background(), cur, purposeMint)
	if err == nil {
		_, err = api.CreateKey(context.Background(), tsapi.KeyOptions{Tags: []string{"tag:ttest"}})
	}
	if !tsapi.IsInsecureFile(err) {
		t.Fatalf("minting with a group-readable OAuth client file: %v", err)
	}

	// Never a secret or an access token in the logs.
	logs := e.logs.String()
	if strings.Contains(logs, "tskey-client-") || strings.Contains(logs, "oauth-access-") || strings.Contains(logs, "QQQQQQQQ") {
		t.Fatalf("secret or token in mgmt logs:\n%s", logs)
	}
}
