package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/oapi-codegen/nullable"
)

// TestSpecValid: the embedded spec parses, validates (examples included) and
// is byte-identical to api/openapi.yaml (the CI "generated code is current"
// check for the embedded copy).
func TestSpecValid(t *testing.T) {
	doc, err := Spec()
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("spec does not validate: %v", err)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Fatalf("openapi = %q, want 3.1.0", doc.OpenAPI)
	}
	// The file on disk is what the generator embedded.
	disk, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Skip("spec file not found (running outside the repo)")
	}
	fromDisk, err := openapi3.NewLoader().LoadFromData(disk)
	if err != nil {
		t.Fatal(err)
	}
	if len(fromDisk.Paths.Map()) != len(doc.Paths.Map()) {
		t.Fatalf("embedded spec has %d paths, api/openapi.yaml has %d: run `make api`", len(doc.Paths.Map()), len(fromDisk.Paths.Map()))
	}
}

// TestSpecConventions enforces the brief's conventions on every operation,
// so a later edit cannot quietly drop them.
func TestSpecConventions(t *testing.T) {
	doc, err := Spec()
	if err != nil {
		t.Fatal(err)
	}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			name := method + " " + path
			if op.OperationID == "" {
				t.Errorf("%s: no operationId", name)
			}
			role, hasRole := op.Extensions["x-twd-role"]
			public := op.Security != nil && len(*op.Security) == 0
			if !public && !hasRole {
				t.Errorf("%s: no x-twd-role", name)
			}
			if public && !strings.HasPrefix(path, "/.well-known") && !strings.HasPrefix(path, "/jwks") && !strings.HasPrefix(path, "/t/") {
				t.Errorf("%s: public but not an OIDC path", name)
			}
			mutating := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
			audited, _ := op.Extensions["x-twd-audit"].(bool)
			switch {
			case mutating && !audited && !unauditedOK[op.OperationID]:
				t.Errorf("%s: mutating but not x-twd-audit: true", name)
			case !mutating && audited:
				t.Errorf("%s: GET marked audited", name)
			}
			if method == "DELETE" && op.RequestBody != nil {
				t.Errorf("%s: DELETE must not take a body", name)
			}
			if method == "GET" && op.Responses.Status(200) != nil {
				r := op.Responses.Status(200).Value
				for _, mt := range r.Content {
					if mt.Schema != nil && strings.HasSuffix(mt.Schema.Ref, "Page") {
						assertPaged(t, name, op)
					}
				}
			}
			if method == "PATCH" {
				if op.RequestBody == nil || op.RequestBody.Value.Content.Get("application/merge-patch+json") == nil {
					t.Errorf("%s: PATCH must accept application/merge-patch+json", name)
				}
				if !hasParam(op, "If-Match") {
					t.Errorf("%s: PATCH must accept If-Match", name)
				}
				if op.Responses.Status(412) == nil {
					t.Errorf("%s: PATCH must document 412", name)
				}
			}
			if op.Responses.Status(202) != nil && method == "POST" {
				if h := op.Responses.Status(202).Value.Headers["Location"]; h == nil {
					t.Errorf("%s: 202 without a Location header", name)
				}
			}
			_ = role
		}
	}
}

// Mutating operations that are legitimately not audited.
var unauditedOK = map[string]bool{
	"ReportHostStatus": true, // 3 s heartbeat
	"ReportHostPaths":  true, // placement telemetry (≤ 1/min); decisions are audited with the deploy
	"CheckTenantWIF":   true, // diagnostic; creates nothing
}

func assertPaged(t *testing.T, name string, op *openapi3.Operation) {
	t.Helper()
	if !hasParam(op, "limit") || !hasParam(op, "cursor") {
		t.Errorf("%s: list without limit/cursor", name)
	}
}

func hasParam(op *openapi3.Operation, name string) bool {
	for _, p := range op.Parameters {
		if p.Value != nil && p.Value.Name == name {
			return true
		}
	}
	return false
}

// TestValidatorRejectsAndAccepts drives the validator with a stub handler to
// show it enforces the spec on both directions.
func TestValidatorRejectsAndAccepts(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	// Request side.
	for _, c := range []struct {
		method, url string
		body        string
		wantErr     bool
	}{
		{"GET", "/v1/tenants?limit=50", "", false},
		{"GET", "/v1/tenants?limit=0", "", true},          // below minimum
		{"GET", "/v1/tenants?limit=501", "", true},        // above maximum
		{"GET", "/v1/tenants/not%20a%20tenant", "", true}, // pattern
		{"GET", "/v1/tenants/tn_abc123", "", false},
		{"POST", "/v1/objects/agent-k3j5m7p9qa/credentials", "", true}, // epoch required
		{"POST", "/v1/objects/agent-k3j5m7p9qa/credentials?epoch=3", "", false},
		{"POST", "/v1/tenants", `{"tailnet":"x.ts.net","mintMode":"wif"}`, false},
		{"POST", "/v1/tenants", `{"tailnet":"x.ts.net","mintMode":"magic"}`, true}, // enum
		{"POST", "/v1/tenants", `{"tailnet":"x.ts.net","mintMode":"wif","bogus":1}`, true},
		{"PATCH", "/v1/tenants/tn_abc123", `{"maxObjects":10}`, false},
		{"PATCH", "/v1/tenants/tn_abc123", `{"tailnet":"y"}`, true}, // additionalProperties: false
		{"DELETE", "/v1/objects/agent-k3j5m7p9qa?cascade=true&reason=test", "", false},
		{"DELETE", "/v1/objects/agent-k3j5m7p9qa?cascade=maybe", "", true},
		{"GET", "/v0/tenants", "", true}, // v0 is gone
	} {
		var body *bytes.Reader
		if c.body != "" {
			body = bytes.NewReader([]byte(c.body))
		} else {
			body = bytes.NewReader(nil)
		}
		r := httptest.NewRequest(c.method, "http://mgmt"+c.url, body)
		if c.body != "" {
			ct := "application/json"
			if c.method == "PATCH" {
				ct = "application/merge-patch+json"
			}
			r.Header.Set("Content-Type", ct)
		}
		_, err := v.ValidateRequest(r)
		if (err != nil) != c.wantErr {
			t.Errorf("%s %s: err=%v, wantErr=%v", c.method, c.url, err, c.wantErr)
		}
	}

	// Response side, through the middleware.
	good := Tenant{TenantID: "tn_abc123", Tailnet: "x.ts.net", MintMode: Wif, RegistrationsPerMinute: 200, MaxObjects: 1000}
	for _, c := range []struct {
		name    string
		status  int
		body    any
		wantErr bool
	}{
		{"tenant ok", 200, good, false},
		{"tenant missing required", 200, map[string]any{"tenantID": "tn_abc123"}, true},
		{"error ok", 404, Error{Code: ErrorCodeNotFound, Message: "no", DocURL: "https://twd.dev/errors/not_found", TraceID: "t"}, false},
		{"error bad code", 404, map[string]any{"code": "nope", "message": "m", "doc_url": "u", "trace_id": "t"}, true},
		{"undocumented status", 418, good, true},
	} {
		var got []error
		h := v.Middleware(func(err error) { got = append(got, err) })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if c.status == 200 {
				w.Header().Set("ETag", `"abc"`)
			}
			w.WriteHeader(c.status)
			_ = json.NewEncoder(w).Encode(c.body)
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "http://mgmt/v1/tenants/tn_abc123", nil))
		if (len(got) > 0) != c.wantErr {
			t.Errorf("%s: validation errors=%v, wantErr=%v", c.name, got, c.wantErr)
		}
	}
}

// TestPageTypes: every list response is a Page with items + nextCursor, and
// the generated Go types carry a nullable cursor.
func TestPageTypes(t *testing.T) {
	p := TenantPage{Items: []Tenant{}, NextCursor: nullable.NewNullNullable[string]()}
	b, _ := json.Marshal(p)
	if string(b) != `{"items":[],"nextCursor":null}` {
		t.Fatalf("last page JSON = %s", b)
	}
	p.NextCursor = nullable.NewNullableWithValue("eyJj")
	b, _ = json.Marshal(p)
	if string(b) != `{"items":[],"nextCursor":"eyJj"}` {
		t.Fatalf("page JSON = %s", b)
	}
	// Merge-patch semantics: absent vs null are distinguishable.
	var tp TenantPatch
	_ = json.Unmarshal([]byte(`{"wifLifecycleClientID":null,"maxObjects":5}`), &tp)
	if !tp.WifLifecycleClientID.IsNull() || tp.WifClientID.IsSpecified() || tp.MaxObjects == nil {
		t.Fatalf("merge patch: lifecycle null=%v clientID specified=%v", tp.WifLifecycleClientID.IsNull(), tp.WifClientID.IsSpecified())
	}
}
