package mgmt

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Object identity is (tenant, workload, namespace = class, objectName):
// the same class + name in two workloads, or in two tenants, are distinct
// objects (M7; M5 found two workloads sharing one node).
func TestStoreIdentityScope(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		for _, id := range []string{"tA", "tB"} {
			if err := s.CreateTenant(ctx, Tenant{ID: id, Tailnet: id + ".ts.net", MintMode: MintAPIKey, RegistrationsPerMinute: 200, MaxObjects: 10}); err != nil {
				t.Fatal(err)
			}
		}
		for i, w := range []string{"one", "two"} {
			if err := s.CreateDeployment(ctx, Deployment{ID: "d-" + w, TenantID: "tA", Workload: w, HostID: "h",
				Namespaces: []ClassConfig{{Class: "Agent", Tags: []string{"tag:" + w}}}, CreatedAt: int64(i + 1)}); err != nil {
				t.Fatal(err)
			}
		}
		// The class is in two workloads: a lookup without one is ambiguous.
		if _, _, err := s.FindClass(ctx, "tA", "", "Agent"); !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("FindClass without workload: %v", err)
		}
		if d, c, err := s.FindClass(ctx, "tA", "two", "Agent"); err != nil || d.ID != "d-two" || c.Tags[0] != "tag:two" {
			t.Fatalf("FindClass two: %+v %+v %v", d, c, err)
		}
		if _, _, err := s.FindClass(ctx, "tB", "one", "Agent"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("FindClass across tenants: %v", err)
		}
		mk := func(id, tenant, w string) error {
			return s.CreateObject(ctx, Object{ID: id, TenantID: tenant, Workload: w, Namespace: "Agent", Name: "alice", Hostname: id, RootID: id, Status: StatusPending})
		}
		for _, c := range []struct{ id, tenant, w string }{{"a1", "tA", "one"}, {"a2", "tA", "two"}, {"b1", "tB", "one"}} {
			if err := mk(c.id, c.tenant, c.w); err != nil {
				t.Fatalf("%s: %v", c.id, err)
			}
		}
		if err := mk("a1-dup", "tA", "one"); !errors.Is(err, ErrExists) {
			t.Fatalf("same (tenant, workload, class, name): %v", err)
		}
		for _, c := range []struct{ tenant, w, want string }{{"tA", "one", "a1"}, {"tA", "two", "a2"}, {"tB", "one", "b1"}} {
			if o, err := s.GetObjectByName(ctx, c.tenant, c.w, "Agent", "alice"); err != nil || o.ID != c.want || o.Workload != c.w {
				t.Fatalf("GetObjectByName %v: %+v %v", c, o, err)
			}
		}
		if objs, _ := s.ListObjects(ctx, ObjectFilter{TenantID: "tA", Workload: "two"}); len(objs) != 1 || objs[0].ID != "a2" {
			t.Fatalf("ListObjects by workload: %+v", objs)
		}
	})
}

// A store from before M7 (objects unique on (tenant, namespace, name), no
// workload column) is migrated in place: rows keep their IDs and get the
// workload of their deployment. SQLite always; Postgres with
// TWD_TEST_POSTGRES_DSN (make test-pg).
func TestStoreMigratesLegacyObjects(t *testing.T) {
	ctx := context.Background()
	type backend struct {
		dsn, driver, open, blob string
		ph                      func(string) string
	}
	path := filepath.Join(t.TempDir(), "mgmt.db")
	bs := map[string]backend{"sqlite": {dsn: path, driver: "sqlite", open: "file:" + path, blob: "BLOB", ph: func(q string) string { return q }}}
	if dsn := os.Getenv("TWD_TEST_POSTGRES_DSN"); dsn != "" {
		bs["postgres"] = backend{dsn: dsn, driver: "pgx", open: dsn, blob: "BYTEA", ph: (&SQLStore{d: dialectPostgres}).q}
	}
	for name, be := range bs {
		t.Run(name, func(t *testing.T) {
			db, err := sql.Open(be.driver, be.open)
			if err != nil {
				t.Fatal(err)
			}
			for _, tbl := range []string{"tenants", "hosts", "deployments", "objects", "objects_v0", "leases", "audit", "audit_heads", "revocations"} {
				db.Exec("DROP TABLE IF EXISTS " + tbl + map[string]string{"sqlite": "", "postgres": " CASCADE"}[name])
			}
			legacy := strings.Replace(strings.Replace(schemaCommon, "workload TEXT NOT NULL DEFAULT '', namespace", "namespace", 1),
				"UNIQUE (tenant_id, workload, namespace, name)", "UNIQUE (tenant_id, namespace, name)", 1)
			for _, stmt := range splitSQL(strings.Replace(legacy, "%s", be.blob, 1)) {
				if _, err := db.Exec(stmt); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`INSERT INTO deployments (id, tenant_id, workload, host_id, namespaces, max_live, max_depth, epoch, created_at) VALUES ('d1','tn','support','h','[]',0,0,1,1)`); err != nil {
				t.Fatal(err)
			}
			ins := be.ph(`INSERT INTO objects (id, tenant_id, namespace, name, hostname, deployment_id, lease, tags, root_id, depth, status, created_at, updated_at)
			        VALUES (?, 'tn', ?, ?, ?, ?, 'tn/support', '[]', ?, 0, 'ready', 1, 1)`)
			for _, r := range [][]any{{"supportagent-x", "SupportAgent", "alice", "supportagent-x", "d1", "supportagent-x"},
				{"support-y", WorkloadNamespace, "support", "support-y", "d1", "support-y"},
				{"orphan-z", WorkloadNamespace, "gone", "orphan-z", "nope", "orphan-z"}} {
				if _, err := db.Exec(ins, r...); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()

			s, err := OpenStore(ctx, be.dsn)
			if err != nil {
				t.Fatalf("open legacy store: %v", err)
			}
			for _, c := range []struct{ id, w string }{{"supportagent-x", "support"}, {"support-y", "support"}, {"orphan-z", "gone"}} {
				o, err := s.GetObject(ctx, c.id)
				if err != nil || o.Workload != c.w || o.Status != StatusReady {
					t.Fatalf("migrated %s: %+v %v", c.id, o, err)
				}
			}
			if o, err := s.GetObjectByName(ctx, "tn", "support", "SupportAgent", "alice"); err != nil || o.ID != "supportagent-x" {
				t.Fatalf("by name after migration: %+v %v", o, err)
			}
			// The new uniqueness holds: same class/name in another workload is fine.
			if err := s.CreateObject(ctx, Object{ID: "n1", TenantID: "tn", Workload: "other", Namespace: "SupportAgent", Name: "alice", RootID: "n1", Status: StatusPending}); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateObject(ctx, Object{ID: "n2", TenantID: "tn", Workload: "support", Namespace: "SupportAgent", Name: "alice", RootID: "n2", Status: StatusPending}); !errors.Is(err, ErrExists) {
				t.Fatalf("dup after migration: %v", err)
			}
			if err := s.CreateObject(ctx, Object{ID: "n1", TenantID: "tn", Workload: "x", Namespace: "Y", Name: "z", RootID: "n1", Status: StatusPending}); !errors.Is(err, ErrExists) {
				t.Fatalf("primary key after migration: %v", err)
			}
			s.Close()
			// Re-opening is a no-op.
			s2, err := OpenStore(ctx, be.dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer s2.Close()
			if objs, _ := s2.ListObjects(ctx, ObjectFilter{TenantID: "tn"}); len(objs) != 4 {
				t.Fatalf("after reopen: %d objects", len(objs))
			}
		})
	}
}

// The API: two workloads of one tenant declaring the same class, and a
// second tenant with the same workload, class and object name, get four
// distinct objects (IDs = node hostnames); the wire request without a
// workload is refused as ambiguous instead of guessing; spawns inherit the
// parent's workload; and the throttles are per tenant.
func TestAPIIdentityScope(t *testing.T) {
	e := newTestEnv(t, nil)
	c, h1 := e.c, e.asHost("h1")
	h1.ok("POST", "/v0/hosts/register", map[string]any{"hostID": "h1", "capacityNodes": 100, "tiers": []string{"process"}}, nil)
	var ta, tb struct{ TenantID string }
	c.ok("POST", "/v0/tenants", map[string]any{"tailnet": "a.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile, "registrationsPerMinute": 10}, &ta)
	c.ok("POST", "/v0/tenants", map[string]any{"tailnet": "b.ts.net", "mintMode": "apikey", "apiKeyFile": e.keyFile}, &tb)
	if ta.TenantID == tb.TenantID {
		t.Fatal("tenant IDs collide")
	}
	bundle := makeBundle(t, map[string]string{"twd.jsonc": testTwdJSONC, "dist/index.js": "export default {}"})
	for _, d := range []struct{ tenant, w string }{{ta.TenantID, "one"}, {ta.TenantID, "two"}, {tb.TenantID, "one"}} {
		c.ok("POST", "/v0/deployments", map[string]any{"tenantID": d.tenant, "workload": d.w, "bundle": bundle}, nil)
	}
	create := func(tenant, w, name string, parent string) CreateObjectResponse {
		t.Helper()
		body := map[string]any{"tenantID": tenant, "workload": w, "namespace": "SupportAgent", "objectName": name}
		if parent != "" {
			body["namespace"], body["parent"] = "LookupAgent", map[string]string{"objectID": parent}
		}
		var r CreateObjectResponse
		h1.ok("POST", "/v0/objects", body, &r)
		return r
	}
	a1 := create(ta.TenantID, "one", "alice", "")
	a2 := create(ta.TenantID, "two", "alice", "")
	b1 := create(tb.TenantID, "one", "alice", "")
	ids := map[string]bool{a1.ObjectID: true, a2.ObjectID: true, b1.ObjectID: true}
	if len(ids) != 3 || a1.Hostname == a2.Hostname || a1.Hostname == b1.Hostname {
		t.Fatalf("identities collide: %+v %+v %+v", a1, a2, b1)
	}
	if again := create(ta.TenantID, "two", "alice", ""); again.ObjectID != a2.ObjectID {
		t.Fatal("create is not idempotent per (tenant, workload, class, name)")
	}
	h1.fail("POST", "/v0/objects", map[string]any{"tenantID": ta.TenantID, "namespace": "SupportAgent", "objectName": "alice"}, 409, "namespace_ambiguous")
	// Unambiguous without a workload (compat): tenant B has one workload.
	var bc CreateObjectResponse
	h1.ok("POST", "/v0/objects", map[string]any{"tenantID": tb.TenantID, "namespace": "SupportAgent", "objectName": "alice"}, &bc)
	if bc.ObjectID != b1.ObjectID {
		t.Fatalf("compat create: %s, want %s", bc.ObjectID, b1.ObjectID)
	}
	h1.fail("POST", "/v0/objects", map[string]any{"tenantID": ta.TenantID, "workload": "nope", "namespace": "SupportAgent", "objectName": "alice"}, 404, "unknown_namespace")
	h1.fail("POST", "/v0/objects", map[string]any{"tenantID": ta.TenantID, "workload": "two", "namespace": WorkloadNamespace, "objectName": "one"}, 400, "bad_request")

	// Spawn without a workload inherits the parent's.
	var kid CreateObjectResponse
	h1.ok("POST", "/v0/objects", map[string]any{"tenantID": ta.TenantID, "namespace": "LookupAgent", "objectName": "c1",
		"parent": map[string]string{"objectID": a2.ObjectID}}, &kid)
	if o, err := e.m.GetObject(context.Background(), kid.ObjectID); err != nil || o.Workload != "two" || o.ParentID != a2.ObjectID {
		t.Fatalf("spawned child: %+v %v", o, err)
	}
	// A parent from another tenant is refused.
	h1.fail("POST", "/v0/objects", map[string]any{"tenantID": tb.TenantID, "namespace": "LookupAgent", "objectName": "x",
		"parent": map[string]string{"objectID": a1.ObjectID}}, 404, "delegation_invalid")

	var objs []Object
	c.ok("GET", "/v0/objects?tenant="+ta.TenantID+"&workload=two", nil, &objs)
	if len(objs) != 2 {
		t.Fatalf("list tenant A workload two: %+v", objs)
	}
	for _, o := range objs {
		if o.TenantID != ta.TenantID || o.Workload != "two" {
			t.Fatalf("list leaked %+v", o)
		}
	}
	c.ok("GET", "/v0/objects?tenant="+tb.TenantID, nil, &objs)
	for _, o := range objs {
		if o.TenantID != tb.TenantID {
			t.Fatalf("tenant B list has %+v", o)
		}
	}

	// Per-tenant throttles: separate buckets, separate limits.
	tA, _ := e.store.GetTenant(context.Background(), ta.TenantID)
	tB, _ := e.store.GetTenant(context.Background(), tb.TenantID)
	thA, thB := e.m.throttle(tA), e.m.throttle(tB)
	if thA == thB || thA.Rate() != 10 || thB.Rate() != DefaultRegistrationsPerMinute {
		t.Fatalf("throttles shared or wrong: A %v B %v", thA.Rate(), thB.Rate())
	}
	for thA.Reserve() == 0 {
	}
	thA.Backoff(time.Minute) // tenant A's control plane said 429
	if thB.Reserve() != 0 || thB.Rate() != DefaultRegistrationsPerMinute {
		t.Fatal("tenant A's drained, backed-off bucket slowed tenant B")
	}

	// Audit by tenant has only that tenant's records.
	var ar struct {
		Records []AuditRecord `json:"records"`
	}
	c.ok("GET", "/v0/audit?tenant="+tb.TenantID, nil, &ar)
	if len(ar.Records) == 0 {
		t.Fatal("no tenant B audit")
	}
	for _, r := range ar.Records {
		if r.TenantID != tb.TenantID {
			t.Fatalf("tenant B audit has %+v", r)
		}
	}
}
