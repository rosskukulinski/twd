package mgmt

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// Tailscale Services for workloads (twd.jsonc tailnet.service): deploy
// creates the Service definition under twd's ownership marker, the
// assignment and deployment carry it, a Service owned by anyone else (or
// named like a device) is refused, and preflight checks autoApprovers.

const svcPolicy = `{
  "tagOwners": { "tag:twd": ["autogroup:admin"], "tag:twd-web": ["tag:twd"] },
  "autoApprovers": { "services": { "svc:web": ["tag:twd-web"], "svc:shop": ["tag:twd-web"] } },
  "grants": [ { "src": ["tag:ttest"], "dst": ["svc:web"], "ip": ["tcp:80"] } ],
}`

func svcBundle(t *testing.T, service string) string {
	t.Helper()
	jsonc := `{ "tailnet": { "tags": ["tag:twd-web"]` + service + ` } }`
	return base64.StdEncoding.EncodeToString(makeBundle(t, map[string]string{"twd.jsonc": jsonc, "dist/index.js": "export default {}"}))
}

func TestServiceDeploy(t *testing.T) {
	e := newTestEnv(t, nil)
	e.ts.policy = svcPolicy
	tn := v1Tenant(t, e, "ttest.ts.net", nil)
	h := v1Host(t, e, "h1")
	c := e.c

	t.Run("TrueMeansWorkloadName", func(t *testing.T) {
		var dep map[string]any
		c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "web", "bundle": svcBundle(t, `, "service": true`)}, &dep)
		sv := e.ts.services["svc:web"]
		if sv == nil || sv["comment"] != serviceOwner(tn, "web") || !strings.Contains(js(sv["ports"]), ServicePort) {
			t.Fatalf("service definition %v", sv)
		}
		ep := dep["endpoint"].(map[string]any)["service"].(map[string]any)
		if ep["name"] != "svc:web" || ep["magicDNS"] != "web.ttest.ts.net" {
			t.Fatalf("endpoint.service %v", ep)
		}
		var as []Assignment
		h.items("/v1/hosts/h1/assignments", &as)
		if len(as) != 1 || as[0].Service != "svc:web" || len(as[0].WorkloadTags) != 1 {
			t.Fatalf("assignments %+v", as)
		}
		// A redeploy adopts its own definition (no new PUT).
		puts := e.ts.svcPuts
		c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "web", "bundle": svcBundle(t, `, "service": true`)}, &dep)
		if e.ts.svcPuts != puts {
			t.Fatal("redeploy rewrote an existing, owned Service")
		}
	})

	t.Run("ExplicitName", func(t *testing.T) {
		c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "store", "bundle": svcBundle(t, `, "service": "shop"`)}, nil)
		if e.ts.services["svc:shop"]["comment"] != serviceOwner(tn, "store") {
			t.Fatalf("svc:shop %v", e.ts.services["svc:shop"])
		}
	})

	t.Run("OtherWorkloadsServiceRefused", func(t *testing.T) {
		// svc:web belongs to workload web: another workload can't take it.
		eb := c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "evil", "bundle": svcBundle(t, `, "service": "svc:web"`)}, 409, "service_conflict")
		if !strings.Contains(eb["message"].(string), "not managed by twd") {
			t.Fatalf("message %v", eb["message"])
		}
		// Nothing was recorded for the refused deploy.
		var as []Assignment
		h.items("/v1/hosts/h1/assignments", &as)
		for _, a := range as {
			if a.Workload == "evil" {
				t.Fatal("refused deploy produced an assignment")
			}
		}
	})

	t.Run("UnmanagedServiceRefused", func(t *testing.T) {
		e.ts.services["svc:admin-made"] = map[string]any{"name": "svc:admin-made", "comment": "", "ports": []string{"tcp:443"}}
		c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "x", "bundle": svcBundle(t, `, "service": "admin-made"`)}, 409, "service_conflict")
		// Handing it over: the admin sets the ownership comment; twd adds its port.
		e.ts.services["svc:admin-made"]["comment"] = serviceOwner(tn, "x")
		c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "x", "bundle": svcBundle(t, `, "service": "admin-made"`)}, nil)
		if p := js(e.ts.services["svc:admin-made"]["ports"]); !strings.Contains(p, "tcp:443") || !strings.Contains(p, ServicePort) {
			t.Fatalf("ports %s", p)
		}
	})

	t.Run("DeviceNameCollision", func(t *testing.T) {
		e.ts.devices = []map[string]any{{"id": "d1", "name": "printer.ttest.ts.net", "hostname": "printer"}}
		eb := c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "p", "bundle": svcBundle(t, `, "service": "printer"`)}, 409, "service_conflict")
		if !strings.Contains(eb["message"].(string), "printer.ttest.ts.net") {
			t.Fatalf("message %v", eb["message"])
		}
		if _, ok := e.ts.services["svc:printer"]; ok {
			t.Fatal("colliding Service was created")
		}
	})

	t.Run("BadConfig", func(t *testing.T) {
		noTags := base64.StdEncoding.EncodeToString(makeBundle(t, map[string]string{
			"twd.jsonc": `{ "tailnet": { "service": true, "objects": { "A": { "tags": ["tag:twd-web"] } } } }`, "dist/index.js": ""}))
		eb := c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "q", "bundle": noTags}, 400, "bad_bundle")
		if !strings.Contains(eb["message"].(string), "tailnet.tags") {
			t.Fatalf("message %v", eb["message"])
		}
		c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "q", "bundle": svcBundle(t, `, "service": "Not_A_Label"`)}, 400, "bad_bundle")
		c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "q", "bundle": svcBundle(t, `, "service": 7`)}, 400, "bad_bundle")
		// false and absent: no Service.
		c.ok("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "plain", "bundle": svcBundle(t, `, "service": false`)}, nil)
		if _, ok := e.ts.services["svc:plain"]; ok {
			t.Fatal("service: false created a Service")
		}
	})

	t.Run("NoServicesScope", func(t *testing.T) {
		e.ts.svcDenied = true
		defer func() { e.ts.svcDenied = false }()
		eb := c.fail("POST", "/v1/deployments", map[string]any{"tenantID": tn, "workload": "denied", "bundle": svcBundle(t, `, "service": true`)}, 502, "service_api_failed")
		if !strings.Contains(eb["message"].(string), "Services") {
			t.Fatalf("message %v", eb["message"])
		}
	})

	t.Run("Preflight", func(t *testing.T) {
		var ok PreflightResult
		c.ok("POST", "/v1/deployments?dryRun=true", map[string]any{"tenantID": tn, "workload": "web", "bundle": svcBundle(t, `, "service": true`)}, &ok)
		if !ok.OK || ok.Service != "svc:web" || ok.ServiceUnapproved {
			t.Fatalf("approved preflight %+v", ok)
		}
		var bad PreflightResult
		c.ok("POST", "/v1/deployments?dryRun=true", map[string]any{"tenantID": tn, "workload": "other", "bundle": svcBundle(t, `, "service": true`)}, &bad)
		if bad.OK || !bad.ServiceUnapproved || !strings.Contains(bad.ServiceSnippet, `"svc:other": ["tag:twd-web"]`) {
			t.Fatalf("unapproved preflight %+v", bad)
		}
		if _, ok := e.ts.services["svc:other"]; ok {
			t.Fatal("dry run created a Service")
		}
	})
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }
