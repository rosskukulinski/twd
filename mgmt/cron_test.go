package mgmt

import (
	"strings"
	"testing"
)

func TestBundleCrons(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		want  string // joined crons, or "error:<substring>"
	}{
		"none":         {map[string]string{"twd.jsonc": "{}"}, ""},
		"no triggers":  {map[string]string{"wrangler.jsonc": `{"name":"x"}`}, ""},
		"jsonc":        {map[string]string{"wrangler.jsonc": `{"triggers":{"crons":["*/5 * * * *", /* c */ "0 18 * * friL",]}}`}, "*/5 * * * *|0 18 * * friL"},
		"json":         {map[string]string{"wrangler.json": `{"triggers":{"crons":["0 0 * * *"]}}`}, "0 0 * * *"},
		"shallowest":   {map[string]string{"app/wrangler.jsonc": `{"triggers":{"crons":["0 0 * * *"]}}`, "app/sub/wrangler.jsonc": `{"triggers":{"crons":["bad"]}}`}, "0 0 * * *"},
		"node_modules": {map[string]string{"node_modules/x/wrangler.jsonc": `{"triggers":{"crons":["bad"]}}`}, ""},
		"bad expr":     {map[string]string{"wrangler.jsonc": `{"triggers":{"crons":["* * * *"]}}`}, "error:want 5 fields"},
		"sunday zero":  {map[string]string{"wrangler.jsonc": `{"triggers":{"crons":["0 0 * * 0"]}}`}, "error:1 = Sunday"},
		"never":        {map[string]string{"wrangler.jsonc": `{"triggers":{"crons":["0 0 30 2 *"]}}`}, "error:never fires"},
		"dup":          {map[string]string{"wrangler.jsonc": `{"triggers":{"crons":["* * * * *","* * * * *"]}}`}, "error:twice"},
		"too many":     {map[string]string{"wrangler.jsonc": `{"triggers":{"crons":["0 * * * *","1 * * * *","2 * * * *","3 * * * *","4 * * * *","5 * * * *","6 * * * *","7 * * * *","8 * * * *","9 * * * *","10 * * * *"]}}`}, "error:at most 10"},
		"wrong type":   {map[string]string{"wrangler.jsonc": `{"triggers":{"crons":"* * * * *"}}`}, "error:cannot unmarshal"},
		"bad jsonc":    {map[string]string{"wrangler.jsonc": `{"triggers":`}, "error:wrangler.jsonc"},
	} {
		got, err := BundleCrons(makeBundle(t, tc.files))
		if want, ok := strings.CutPrefix(tc.want, "error:"); ok {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: want error %q, got %v %q", name, want, err, got)
			}
			continue
		}
		if err != nil || strings.Join(got, "|") != tc.want {
			t.Errorf("%s: got %q %v, want %q", name, got, err, tc.want)
		}
	}
}

// TestAPIDeployRejectsBadCrons: an invalid cron trigger fails the deploy
// (and its preflight) with 400 bad_bundle before anything is written.
func TestAPIDeployRejectsBadCrons(t *testing.T) {
	e := newTestEnv(t, nil)
	c, h1 := e.c, e.asHost("h1")
	var tn struct{ TenantID string }
	c.ok("POST", "/v1/tenants", map[string]any{"tailnet": "t.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile}, &tn)
	h1.ok("PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 100, "tiers": []string{"process"}}, nil)
	bad := makeBundle(t, map[string]string{"twd.jsonc": testTwdJSONC, "dist/index.js": "export default {}",
		"wrangler.jsonc": `{"main":"dist/index.js","triggers":{"crons":["*/5 * * * * *"]}}`})
	body := map[string]any{"tenantID": tn.TenantID, "workload": "support", "bundle": bad}
	r := c.fail("POST", "/v1/deployments", body, 400, "bad_bundle")
	if m, _ := r["message"].(string); !strings.Contains(m, "triggers.crons") {
		t.Fatalf("message %v", r)
	}
	c.fail("POST", "/v1/deployments?dryRun=true", body, 400, "bad_bundle")
	var as []Assignment
	h1.items("/v1/hosts/h1/assignments", &as)
	if len(as) != 0 {
		t.Fatalf("a rejected deploy was assigned: %+v", as)
	}
	good := makeBundle(t, map[string]string{"twd.jsonc": testTwdJSONC, "dist/index.js": "export default {}",
		"wrangler.jsonc": `{"main":"dist/index.js","triggers":{"crons":["*/5 * * * *"]}}`})
	var dep DeployResponse
	c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": "support", "bundle": good}, &dep)
	if dep.Host != "h1" {
		t.Fatalf("deploy %+v", dep)
	}
}
