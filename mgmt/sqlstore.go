package mgmt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // Postgres driver "pgx"
	_ "modernc.org/sqlite"             // pure-Go SQLite driver "sqlite" (no cgo)
)

type dialect int

const (
	dialectSQLite dialect = iota
	dialectPostgres
)

// SQLStore implements Store on SQLite or Postgres with one set of queries.
type SQLStore struct {
	db *sql.DB
	d  dialect
}

var _ Store = (*SQLStore)(nil)

// OpenStore opens a store. dsn is "postgres://…" / "postgresql://…" for
// Postgres, otherwise a SQLite file path (optionally "sqlite:" prefixed).
func OpenStore(ctx context.Context, dsn string) (*SQLStore, error) {
	var s *SQLStore
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, err
		}
		s = &SQLStore{db: db, d: dialectPostgres}
	} else {
		path := strings.TrimPrefix(dsn, "sqlite:")
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err == nil {
			f.Close() // create 0600 up front; SQLite gives -wal/-shm the same mode
		}
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate")
		if err != nil {
			return nil, err
		}
		// One connection: SQLite has a single writer, and this serialises
		// every read-modify-write transaction without SQLITE_BUSY.
		db.SetMaxOpenConns(1)
		s = &SQLStore{db: db, d: dialectSQLite}
	}
	if err := s.db.PingContext(ctx); err != nil {
		s.db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		s.db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *SQLStore) Close() error { return s.db.Close() }

// q rewrites ? placeholders to $n for Postgres.
func (s *SQLStore) q(query string) string {
	if s.d != dialectPostgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *SQLStore) forUpdate() string {
	if s.d == dialectPostgres {
		return " FOR UPDATE"
	}
	return ""
}

const schemaCommon = `
CREATE TABLE IF NOT EXISTS tenants (
  id TEXT PRIMARY KEY, tailnet TEXT NOT NULL, control_url TEXT NOT NULL DEFAULT '',
  mint_mode TEXT NOT NULL, api_key_file TEXT NOT NULL DEFAULT '', wif_client_id TEXT NOT NULL DEFAULT '',
  wif_lifecycle_client_id TEXT NOT NULL DEFAULT '', rpm BIGINT NOT NULL, max_objects BIGINT NOT NULL,
  created_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS hosts (
  id TEXT PRIMARY KEY, capacity BIGINT NOT NULL, tiers TEXT NOT NULL, node_id TEXT NOT NULL DEFAULT '',
  audit_pubkey TEXT NOT NULL DEFAULT '', live_nodes BIGINT NOT NULL DEFAULT 0, last_seen BIGINT NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS deployments (
  id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, workload TEXT NOT NULL, host_id TEXT NOT NULL,
  namespaces TEXT NOT NULL, max_live BIGINT NOT NULL, max_depth BIGINT NOT NULL, epoch BIGINT NOT NULL,
  bundle %s, created_at BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS deployments_tw ON deployments (tenant_id, workload, created_at);
CREATE TABLE IF NOT EXISTS objects (
  id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, workload TEXT NOT NULL DEFAULT '', namespace TEXT NOT NULL, name TEXT NOT NULL,
  hostname TEXT NOT NULL, deployment_id TEXT NOT NULL, lease TEXT NOT NULL, tags TEXT NOT NULL,
  caps TEXT NOT NULL DEFAULT '', parent_id TEXT NOT NULL DEFAULT '', root_id TEXT NOT NULL,
  depth BIGINT NOT NULL, ttl_seconds BIGINT NOT NULL DEFAULT 0, ephemeral BIGINT NOT NULL DEFAULT 0,
  status TEXT NOT NULL, node_id TEXT NOT NULL DEFAULT '', stable_node_id TEXT NOT NULL DEFAULT '',
  magic_dns TEXT NOT NULL DEFAULT '', key_id TEXT NOT NULL DEFAULT '', key_expires BIGINT NOT NULL DEFAULT 0,
  kill_reason TEXT NOT NULL DEFAULT '', created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL,
  cert TEXT NOT NULL DEFAULT '',
  UNIQUE (tenant_id, workload, namespace, name));
CREATE INDEX IF NOT EXISTS objects_parent ON objects (parent_id);
CREATE INDEX IF NOT EXISTS objects_root ON objects (root_id, status);
CREATE TABLE IF NOT EXISTS leases (
  name TEXT PRIMARY KEY, holder TEXT NOT NULL, epoch BIGINT NOT NULL, expires_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS audit (
  chain TEXT NOT NULL, seq BIGINT NOT NULL, ts TEXT NOT NULL, tenant_id TEXT NOT NULL DEFAULT '',
  object_id TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL, trace_id TEXT NOT NULL DEFAULT '',
  acting_for TEXT NOT NULL DEFAULT '', prev_hash TEXT NOT NULL, hash TEXT NOT NULL,
  host_sig TEXT NOT NULL DEFAULT '', raw TEXT NOT NULL, PRIMARY KEY (chain, seq));
CREATE INDEX IF NOT EXISTS audit_object ON audit (object_id);
CREATE INDEX IF NOT EXISTS audit_trace ON audit (trace_id);
CREATE TABLE IF NOT EXISTS audit_heads (chain TEXT PRIMARY KEY, seq BIGINT NOT NULL, hash TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS revocations (
  host_id TEXT NOT NULL, object_id TEXT NOT NULL, created_at BIGINT NOT NULL, PRIMARY KEY (host_id, object_id));
`

// Append-only audit: UPDATE/DELETE on audit rows abort (defence in depth;
// the hash chain is what detects tampering).
const sqliteTriggers = `
CREATE TRIGGER IF NOT EXISTS audit_no_update BEFORE UPDATE ON audit BEGIN SELECT RAISE(ABORT, 'audit is append-only'); END;
CREATE TRIGGER IF NOT EXISTS audit_no_delete BEFORE DELETE ON audit BEGIN SELECT RAISE(ABORT, 'audit is append-only'); END;
`

const pgTriggers = `
CREATE OR REPLACE FUNCTION twd_audit_append_only() RETURNS trigger AS $$
BEGIN RAISE EXCEPTION 'audit is append-only'; END; $$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS audit_append_only ON audit;
CREATE TRIGGER audit_append_only BEFORE UPDATE OR DELETE ON audit FOR EACH ROW EXECUTE FUNCTION twd_audit_append_only();
`

func (s *SQLStore) migrate(ctx context.Context) error {
	blob, trig := "BLOB", sqliteTriggers
	if s.d == dialectPostgres {
		blob, trig = "BYTEA", pgTriggers
		// Serialise concurrent migrations from several replicas.
		if _, err := s.db.ExecContext(ctx, "SELECT pg_advisory_lock(7464)"); err != nil {
			return err
		}
		defer s.db.ExecContext(context.Background(), "SELECT pg_advisory_unlock(7464)")
	}
	legacy, err := s.renameLegacyObjects(ctx)
	if err != nil {
		return err
	}
	ddl := fmt.Sprintf(schemaCommon, blob)
	if s.d == dialectPostgres {
		// pgx runs multi-statement strings as one simple-protocol batch.
		if _, err := s.db.ExecContext(ctx, ddl+trig); err != nil {
			return err
		}
	} else {
		for _, stmt := range splitSQL(ddl) {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("%w: %s", err, stmt)
			}
		}
		for _, stmt := range strings.Split(strings.TrimSpace(trig), "\n") {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
	}
	if err := s.copyLegacyObjects(ctx, legacy); err != nil {
		return err
	}
	return s.addColumns(ctx)
}

// addColumns adds columns introduced after a store was created (additive
// migrations; both dialects accept ADD COLUMN with a default).
func (s *SQLStore) addColumns(ctx context.Context) error {
	for _, c := range []struct{ table, col, ddl string }{
		{"objects", "cert", "ALTER TABLE objects ADD COLUMN cert TEXT NOT NULL DEFAULT ''"}, // RT-01 node certificates
	} {
		rows, err := s.db.QueryContext(ctx, "SELECT "+c.col+" FROM "+c.table+" WHERE 1=0")
		if err == nil {
			rows.Close()
			continue
		}
		if _, err := s.db.ExecContext(ctx, c.ddl); err != nil {
			return fmt.Errorf("%w: %s", err, c.ddl)
		}
	}
	return nil
}

// Object identity is (tenant, workload, namespace = class, name) since M7.
// A store created before that has objects keyed by (tenant, namespace,
// name) and no workload column: renameLegacyObjects moves that table
// aside (with its indexes) before the DDL creates the new one, and
// copyLegacyObjects copies the rows back with the workload taken from each
// object's deployment. Object IDs are kept, so existing nodes keep their
// identity.
func (s *SQLStore) renameLegacyObjects(ctx context.Context) (bool, error) {
	var exists bool
	if s.d == dialectPostgres {
		if err := s.db.QueryRowContext(ctx, "SELECT to_regclass('objects') IS NOT NULL").Scan(&exists); err != nil {
			return false, err
		}
	} else {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='objects'").Scan(&n); err != nil {
			return false, err
		}
		exists = n > 0
	}
	if !exists {
		return false, nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT workload FROM objects WHERE 1=0")
	if err == nil {
		rows.Close()
		return false, nil // already has the workload column
	}
	stmts := []string{"DROP INDEX IF EXISTS objects_parent", "DROP INDEX IF EXISTS objects_root",
		"ALTER TABLE objects RENAME TO objects_v0"}
	if s.d == dialectPostgres {
		// Postgres keeps index names on rename; free the new table's PK name.
		stmts = append(stmts, "ALTER INDEX IF EXISTS objects_pkey RENAME TO objects_v0_pkey")
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return false, fmt.Errorf("%w: %s", err, stmt)
		}
	}
	return true, nil
}

func (s *SQLStore) copyLegacyObjects(ctx context.Context, legacy bool) error {
	if !legacy {
		return nil
	}
	old := strings.Replace(objectColsV1, "workload, ", "", 1)
	ins := "INSERT INTO objects (" + objectColsV1 + ") SELECT " +
		strings.Replace(prefixCols("o.", old), "o.tenant_id, ", "o.tenant_id, COALESCE((SELECT d.workload FROM deployments d WHERE d.id=o.deployment_id), ''), ", 1) +
		" FROM objects_v0 o"
	if _, err := s.db.ExecContext(ctx, ins); err != nil {
		return fmt.Errorf("migrate objects: %w", err)
	}
	// The workload entry node of a workload is named after it.
	if _, err := s.db.ExecContext(ctx, s.q("UPDATE objects SET workload=name WHERE workload='' AND namespace=?"), WorkloadNamespace); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "DROP TABLE objects_v0")
	return err
}

func splitSQL(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *SQLStore) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func isUnique(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "UNIQUE constraint") || strings.Contains(m, "duplicate key") || strings.Contains(m, "23505")
}

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---- tenants ----

const tenantCols = "id, tailnet, control_url, mint_mode, api_key_file, wif_client_id, wif_lifecycle_client_id, rpm, max_objects, created_at"

func scanTenant(sc interface{ Scan(...any) error }) (Tenant, error) {
	var t Tenant
	var rpm, mo int64
	err := sc.Scan(&t.ID, &t.Tailnet, &t.ControlURL, &t.MintMode, &t.APIKeyFile, &t.WIFClientID, &t.WIFLifecycleClientID, &rpm, &mo, &t.CreatedAt)
	t.RegistrationsPerMinute, t.MaxObjects = int(rpm), int(mo)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return t, err
}

func (s *SQLStore) CreateTenant(ctx context.Context, t Tenant) error {
	_, err := s.db.ExecContext(ctx, s.q("INSERT INTO tenants ("+tenantCols+") VALUES (?,?,?,?,?,?,?,?,?,?)"),
		t.ID, t.Tailnet, t.ControlURL, t.MintMode, t.APIKeyFile, t.WIFClientID, t.WIFLifecycleClientID,
		t.RegistrationsPerMinute, t.MaxObjects, t.CreatedAt)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func (s *SQLStore) GetTenant(ctx context.Context, id string) (Tenant, error) {
	return scanTenant(s.db.QueryRowContext(ctx, s.q("SELECT "+tenantCols+" FROM tenants WHERE id=?"), id))
}

func (s *SQLStore) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+tenantCols+" FROM tenants ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- hosts ----

const hostCols = "id, capacity, tiers, node_id, audit_pubkey, live_nodes, last_seen"

func scanHost(sc interface{ Scan(...any) error }) (Host, error) {
	var h Host
	var capacity, live int64
	var tiers string
	err := sc.Scan(&h.ID, &capacity, &tiers, &h.NodeID, &h.AuditPubKey, &live, &h.LastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	h.CapacityNodes, h.LiveNodes = int(capacity), int(live)
	json.Unmarshal([]byte(tiers), &h.Tiers)
	return h, err
}

func (s *SQLStore) UpsertHost(ctx context.Context, h Host) error {
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO hosts (`+hostCols+`) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (id) DO UPDATE SET capacity=excluded.capacity, tiers=excluded.tiers, node_id=excluded.node_id,
		audit_pubkey=excluded.audit_pubkey, last_seen=excluded.last_seen`),
		h.ID, h.CapacityNodes, jsonStr(h.Tiers), h.NodeID, h.AuditPubKey, h.LiveNodes, h.LastSeen)
	return err
}

func (s *SQLStore) GetHost(ctx context.Context, id string) (Host, error) {
	return scanHost(s.db.QueryRowContext(ctx, s.q("SELECT "+hostCols+" FROM hosts WHERE id=?"), id))
}

func (s *SQLStore) ListHosts(ctx context.Context) ([]Host, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+hostCols+" FROM hosts ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *SQLStore) TouchHost(ctx context.Context, id string, liveNodes int, now time.Time) error {
	r, err := s.db.ExecContext(ctx, s.q("UPDATE hosts SET live_nodes=?, last_seen=? WHERE id=?"), liveNodes, now.UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- deployments ----

const deployCols = "id, tenant_id, workload, host_id, namespaces, max_live, max_depth, epoch, created_at"

func scanDeployment(sc interface{ Scan(...any) error }) (Deployment, error) {
	var d Deployment
	var ns string
	var ml, md int64
	err := sc.Scan(&d.ID, &d.TenantID, &d.Workload, &d.HostID, &ns, &ml, &md, &d.Epoch, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	d.MaxLive, d.MaxDepth = int(ml), int(md)
	json.Unmarshal([]byte(ns), &d.Namespaces)
	return d, err
}

func (s *SQLStore) CreateDeployment(ctx context.Context, d Deployment) error {
	_, err := s.db.ExecContext(ctx, s.q("INSERT INTO deployments ("+deployCols+", bundle) VALUES (?,?,?,?,?,?,?,?,?,?)"),
		d.ID, d.TenantID, d.Workload, d.HostID, jsonStr(d.Namespaces), d.MaxLive, d.MaxDepth, d.Epoch, d.CreatedAt, d.Bundle)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func (s *SQLStore) GetDeployment(ctx context.Context, id string) (Deployment, error) {
	row := s.db.QueryRowContext(ctx, s.q("SELECT "+deployCols+", bundle FROM deployments WHERE id=?"), id)
	var d Deployment
	var ns string
	var ml, md int64
	err := row.Scan(&d.ID, &d.TenantID, &d.Workload, &d.HostID, &ns, &ml, &md, &d.Epoch, &d.CreatedAt, &d.Bundle)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	d.MaxLive, d.MaxDepth = int(ml), int(md)
	json.Unmarshal([]byte(ns), &d.Namespaces)
	return d, err
}

// latestDeployments returns the newest deployment per (tenant, workload).
func (s *SQLStore) latestDeployments(ctx context.Context, where string, args ...any) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+prefixCols("d.", deployCols)+` FROM deployments d
		WHERE NOT EXISTS (SELECT 1 FROM deployments n WHERE n.tenant_id=d.tenant_id AND n.workload=d.workload
		  AND (n.created_at > d.created_at OR (n.created_at = d.created_at AND n.id > d.id))) `+where+` ORDER BY d.created_at, d.id`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func prefixCols(p, cols string) string {
	parts := strings.Split(cols, ", ")
	for i := range parts {
		parts[i] = p + parts[i]
	}
	return strings.Join(parts, ", ")
}

func (s *SQLStore) Assignments(ctx context.Context, hostID string) ([]Deployment, error) {
	ds, err := s.latestDeployments(ctx, "AND d.host_id=?", hostID)
	if err != nil {
		return nil, err
	}
	// Report the current epoch of the lease (it may have been renewed/re-acquired).
	out := ds[:0]
	for _, d := range ds {
		l, err := s.GetLease(ctx, LeaseName(d.TenantID, d.Workload))
		if err != nil || l.Holder != hostID {
			continue
		}
		d.Epoch = l.Epoch
		out = append(out, d)
	}
	return out, nil
}

func (s *SQLStore) LatestDeployments(ctx context.Context, tenantID string) ([]Deployment, error) {
	return s.latestDeployments(ctx, "AND d.tenant_id=?", tenantID)
}

func (s *SQLStore) FindClass(ctx context.Context, tenantID, workload, class string) (Deployment, ClassConfig, error) {
	ds, err := s.latestDeployments(ctx, "AND d.tenant_id=?", tenantID)
	if err != nil {
		return Deployment{}, ClassConfig{}, err
	}
	var hits []Deployment
	var cfg ClassConfig
	for i := len(ds) - 1; i >= 0; i-- {
		if workload != "" && ds[i].Workload != workload {
			continue
		}
		for _, c := range ds[i].Namespaces {
			if c.Class == class {
				hits, cfg = append(hits, ds[i]), c
				break
			}
		}
	}
	switch {
	case len(hits) == 1:
		return hits[0], cfg, nil
	case len(hits) > 1:
		var ws []string
		for _, d := range hits {
			ws = append(ws, d.Workload)
		}
		return Deployment{}, ClassConfig{}, fmt.Errorf("namespace %q is declared by workloads %v; name the workload: %w", class, ws, ErrAmbiguous)
	}
	return Deployment{}, ClassConfig{}, fmt.Errorf("namespace %q: %w", class, ErrNotFound)
}

// ---- objects ----

// objectColsV1 are the columns of the M7 schema; cert (RT-01) was added later
// and is filled by addColumns on older stores.
const objectColsV1 = "id, tenant_id, workload, namespace, name, hostname, deployment_id, lease, tags, caps, parent_id, root_id, depth, ttl_seconds, ephemeral, status, node_id, stable_node_id, magic_dns, key_id, key_expires, kill_reason, created_at, updated_at"
const objectCols = objectColsV1 + ", cert"

func scanObject(sc interface{ Scan(...any) error }) (Object, error) {
	var o Object
	var tags, caps string
	var depth, eph int64
	err := sc.Scan(&o.ID, &o.TenantID, &o.Workload, &o.Namespace, &o.Name, &o.Hostname, &o.DeploymentID, &o.Lease, &tags, &caps,
		&o.ParentID, &o.RootID, &depth, &o.TTLSeconds, &eph, &o.Status, &o.NodeID, &o.StableNodeID, &o.MagicDNS,
		&o.KeyID, &o.KeyExpires, &o.KillReason, &o.CreatedAt, &o.UpdatedAt, &o.Cert)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	o.Depth, o.Ephemeral = int(depth), eph != 0
	json.Unmarshal([]byte(tags), &o.Tags)
	if caps != "" {
		o.Caps = json.RawMessage(caps)
	}
	return o, err
}

func objectArgs(o Object) []any {
	eph := 0
	if o.Ephemeral {
		eph = 1
	}
	return []any{o.ID, o.TenantID, o.Workload, o.Namespace, o.Name, o.Hostname, o.DeploymentID, o.Lease, jsonStr(o.Tags), string(o.Caps),
		o.ParentID, o.RootID, o.Depth, o.TTLSeconds, eph, o.Status, o.NodeID, o.StableNodeID, o.MagicDNS,
		o.KeyID, o.KeyExpires, o.KillReason, o.CreatedAt, o.UpdatedAt, o.Cert}
}

func (s *SQLStore) CreateObject(ctx context.Context, o Object) error {
	_, err := s.db.ExecContext(ctx, s.q("INSERT INTO objects ("+objectCols+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"), objectArgs(o)...)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func (s *SQLStore) GetObject(ctx context.Context, id string) (Object, error) {
	return scanObject(s.db.QueryRowContext(ctx, s.q("SELECT "+objectCols+" FROM objects WHERE id=?"), id))
}

func (s *SQLStore) GetObjectByName(ctx context.Context, tenantID, workload, ns, name string) (Object, error) {
	return scanObject(s.db.QueryRowContext(ctx, s.q("SELECT "+objectCols+" FROM objects WHERE tenant_id=? AND workload=? AND namespace=? AND name=?"), tenantID, workload, ns, name))
}

func (s *SQLStore) ListObjects(ctx context.Context, f ObjectFilter) ([]Object, error) {
	var where []string
	var args []any
	add := func(cond string, v any) { where = append(where, cond); args = append(args, v) }
	if f.TenantID != "" {
		add("tenant_id=?", f.TenantID)
	}
	if f.Workload != "" {
		add("workload=?", f.Workload)
	}
	if f.Namespace != "" {
		add("namespace=?", f.Namespace)
	}
	if f.ParentID != "" {
		add("parent_id=?", f.ParentID)
	}
	if f.RootID != "" {
		add("root_id=?", f.RootID)
	}
	if f.StableNodeID != "" {
		add("stable_node_id=?", f.StableNodeID)
	}
	if f.HasParent {
		where = append(where, "parent_id<>''")
	}
	if len(f.Status) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(f.Status)), ",")
		where = append(where, "status IN ("+ph+")")
		for _, st := range f.Status {
			args = append(args, st)
		}
	}
	q := "SELECT " + objectCols + " FROM objects"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.db.QueryContext(ctx, s.q(q+" ORDER BY created_at, id"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Object
	for rows.Next() {
		o, err := scanObject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// checkFence verifies, inside tx, that the lease is held at the epoch. On
// Postgres the lease row is locked FOR UPDATE, so a concurrent Acquire
// (epoch+1) serialises behind this commit or fails it.
func (s *SQLStore) checkFence(ctx context.Context, tx *sql.Tx, f *Fence) error {
	var holder string
	var epoch int64
	err := tx.QueryRowContext(ctx, s.q("SELECT holder, epoch FROM leases WHERE name=?"+s.forUpdate()), f.Lease).Scan(&holder, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	if epoch != f.Epoch || holder == "" || (f.Holder != "" && holder != f.Holder) {
		return ErrLeaseLost
	}
	return nil
}

func (s *SQLStore) UpdateObject(ctx context.Context, id string, fence *Fence, fn func(*Object) error) (Object, error) {
	var out Object
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if fence != nil {
			if err := s.checkFence(ctx, tx, fence); err != nil {
				return err
			}
		}
		o, err := scanObject(tx.QueryRowContext(ctx, s.q("SELECT "+objectCols+" FROM objects WHERE id=?"+s.forUpdate()), id))
		if err != nil {
			return err
		}
		if err := fn(&o); err != nil {
			return err
		}
		o.ID = id
		args := objectArgs(o)[1:]
		sets := strings.Split(objectCols, ", ")[1:]
		for i := range sets {
			sets[i] += "=?"
		}
		_, err = tx.ExecContext(ctx, s.q("UPDATE objects SET "+strings.Join(sets, ", ")+" WHERE id=?"), append(args, id)...)
		out = o
		return err
	})
	return out, err
}

func (s *SQLStore) CountLive(ctx context.Context, tenantID, rootID string) (int, error) {
	var n int64
	var err error
	live := "status NOT IN ('" + StatusKilled + "','" + StatusFailed + "')"
	if rootID != "" {
		err = s.db.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM objects WHERE root_id=? AND id<>? AND "+live), rootID, rootID).Scan(&n)
	} else {
		err = s.db.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM objects WHERE tenant_id=? AND "+live), tenantID).Scan(&n)
	}
	return int(n), err
}

// ---- leases ----

func (s *SQLStore) AcquireLease(ctx context.Context, name, holder string, ttl time.Duration, now time.Time) (Lease, error) {
	if holder == "" {
		return Lease{}, errors.New("lease holder required")
	}
	l := Lease{Name: name}
	// One conditional write: insert epoch 1, or take over an expired /
	// released / own lease with epoch+1. A lease held by someone else and
	// unexpired matches no row.
	err := s.db.QueryRowContext(ctx, s.q(`INSERT INTO leases (name, holder, epoch, expires_at) VALUES (?, ?, 1, ?)
		ON CONFLICT (name) DO UPDATE SET holder=excluded.holder, epoch=leases.epoch+1, expires_at=excluded.expires_at
		WHERE leases.expires_at < ? OR leases.holder = excluded.holder OR leases.holder = ''
		RETURNING holder, epoch, expires_at`), name, holder, now.Add(ttl).UnixMilli(), now.UnixMilli()).Scan(&l.Holder, &l.Epoch, &l.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrLeaseHeld
	}
	return l, err
}

// RenewLease extends the lease iff it is still (holder, epoch). It succeeds
// after expiry if nobody else acquired it in between (the epoch is the
// fencing token; expiry only lets another holder take over).
func (s *SQLStore) RenewLease(ctx context.Context, name, holder string, epoch int64, ttl time.Duration, now time.Time) (Lease, error) {
	l := Lease{Name: name}
	err := s.db.QueryRowContext(ctx, s.q(`UPDATE leases SET expires_at=? WHERE name=? AND holder=? AND epoch=? AND holder<>''
		RETURNING holder, epoch, expires_at`), now.Add(ttl).UnixMilli(), name, holder, epoch).Scan(&l.Holder, &l.Epoch, &l.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrLeaseLost
	}
	return l, err
}

// ReleaseLease frees the lease but keeps the row, so epochs stay monotonic.
func (s *SQLStore) ReleaseLease(ctx context.Context, name, holder string, epoch int64) error {
	r, err := s.db.ExecContext(ctx, s.q("UPDATE leases SET holder='', expires_at=0 WHERE name=? AND holder=? AND epoch=?"), name, holder, epoch)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrLeaseLost
	}
	return nil
}

func (s *SQLStore) GetLease(ctx context.Context, name string) (Lease, error) {
	l := Lease{Name: name}
	err := s.db.QueryRowContext(ctx, s.q("SELECT holder, epoch, expires_at FROM leases WHERE name=?"), name).Scan(&l.Holder, &l.Epoch, &l.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

// ---- audit ----

func (s *SQLStore) AppendAudit(ctx context.Context, rec AuditRecord) (AuditRecord, error) {
	if rec.Chain == "" {
		return rec, errors.New("audit chain required")
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.q("INSERT INTO audit_heads (chain, seq, hash) VALUES (?, 0, '') ON CONFLICT (chain) DO NOTHING"), rec.Chain); err != nil {
			return err
		}
		var seq int64
		var head string
		if err := tx.QueryRowContext(ctx, s.q("SELECT seq, hash FROM audit_heads WHERE chain=?"+s.forUpdate()), rec.Chain).Scan(&seq, &head); err != nil {
			return err
		}
		rec.Seq = seq + 1
		if rec.Hash == "" {
			rec.PrevHash = head
			body, err := mgmtRecordBody(rec)
			if err != nil {
				return err
			}
			rec.Raw = body
			if rec.Hash, err = HashRecord(body); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO audit (chain, seq, ts, tenant_id, object_id, kind, trace_id, acting_for, prev_hash, hash, host_sig, raw)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`), rec.Chain, rec.Seq, rec.TS, rec.TenantID, rec.ObjectID, rec.Kind, rec.TraceID,
			rec.ActingFor, rec.PrevHash, rec.Hash, rec.HostSig, string(rec.Raw)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.q("UPDATE audit_heads SET seq=?, hash=? WHERE chain=? AND seq=?"), rec.Seq, rec.Hash, rec.Chain, seq)
		return err
	})
	return rec, err
}

func (s *SQLStore) ListAudit(ctx context.Context, f AuditFilter) ([]AuditRecord, error) {
	var where []string
	var args []any
	add := func(cond string, v any) { where = append(where, cond); args = append(args, v) }
	if f.Chain != "" {
		add("chain=?", f.Chain)
	}
	if f.ObjectID != "" {
		add("object_id=?", f.ObjectID)
	}
	if f.ActingFor != "" {
		add("acting_for=?", f.ActingFor)
	}
	if f.TraceID != "" {
		add("trace_id=?", f.TraceID)
	}
	if f.TenantID != "" {
		add("tenant_id=?", f.TenantID)
	}
	q := "SELECT chain, seq, ts, tenant_id, object_id, kind, trace_id, acting_for, prev_hash, hash, host_sig, raw FROM audit"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY chain, seq"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, s.q(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRecord
	for rows.Next() {
		var r AuditRecord
		var raw string
		if err := rows.Scan(&r.Chain, &r.Seq, &r.TS, &r.TenantID, &r.ObjectID, &r.Kind, &r.TraceID, &r.ActingFor,
			&r.PrevHash, &r.Hash, &r.HostSig, &raw); err != nil {
			return nil, err
		}
		r.Raw = json.RawMessage(raw)
		var extra struct {
			Dst    string          `json:"dst"`
			Grant  string          `json:"grant"`
			Code   string          `json:"code"`
			Detail json.RawMessage `json:"detail"`
		}
		json.Unmarshal(r.Raw, &extra)
		r.Dst, r.Grant, r.Code, r.Detail = extra.Dst, extra.Grant, extra.Code, extra.Detail
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLStore) AuditChains(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT chain FROM audit_heads ORDER BY chain")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *SQLStore) AuditHead(ctx context.Context, chain string) (int64, string, error) {
	var seq int64
	var h string
	err := s.db.QueryRowContext(ctx, s.q("SELECT seq, hash FROM audit_heads WHERE chain=?"), chain).Scan(&seq, &h)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return seq, h, err
}

// ---- revocations ----

func (s *SQLStore) AddRevocation(ctx context.Context, hostID, objectID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, s.q("INSERT INTO revocations (host_id, object_id, created_at) VALUES (?,?,?) ON CONFLICT (host_id, object_id) DO NOTHING"),
		hostID, objectID, now.UnixMilli())
	return err
}

func (s *SQLStore) Revocations(ctx context.Context, hostID string, since time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.q("SELECT object_id FROM revocations WHERE host_id=? AND created_at>=? ORDER BY created_at, object_id"), hostID, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// rawExec is for tests (e.g. simulating tampering).
func (s *SQLStore) rawExec(ctx context.Context, q string, args ...any) error {
	_, err := s.db.ExecContext(ctx, s.q(q), args...)
	return err
}
