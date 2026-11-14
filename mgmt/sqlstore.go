package mgmt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/internal/container"

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
	db    *sql.DB
	d     dialect
	clock TimeSource // the shared time source (ADR-028 §1)
	dbc   *dbClock   // Postgres: follows the server clock; nil for SQLite
}

var _ Store = (*SQLStore)(nil)

// OpenStore opens a store. dsn is "postgres://…" / "postgresql://…" for
// Postgres, otherwise a SQLite file path (optionally "sqlite:" prefixed).
func OpenStore(ctx context.Context, dsn string) (*SQLStore, error) {
	var s *SQLStore
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		dsn, maxConns := takeMaxConns(dsn)
		db, err := sql.Open("pgx", withSessionLimits(dsn))
		if err != nil {
			return nil, err
		}
		// Bounded pool (H19): a burst of requests waits for a connection
		// instead of opening hundreds and exhausting the server's
		// max_connections ("too many clients"), which fails every replica.
		db.SetMaxOpenConns(maxConns)
		db.SetMaxIdleConns(maxConns)
		db.SetConnMaxIdleTime(5 * time.Minute)
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
	s.clock = localClock{}
	if s.d == dialectPostgres {
		s.dbc = newDBClock(func(ctx context.Context) (time.Time, error) {
			var us int64
			err := s.db.QueryRowContext(ctx, "SELECT (extract(epoch from clock_timestamp())*1000000)::bigint").Scan(&us)
			return time.UnixMicro(us), err
		})
		if err := s.dbc.sync(ctx); err != nil {
			s.db.Close()
			return nil, fmt.Errorf("store clock: %w", err)
		}
		go s.dbc.run()
		s.clock = s.dbc
	}
	return s, nil
}

func (s *SQLStore) Close() error {
	if s.dbc != nil {
		s.dbc.close()
		s.dbc = nil
	}
	return s.db.Close()
}

// Now is the shared clock: the Postgres server clock, or the local clock for
// SQLite (single replica). See TimeSource.
func (s *SQLStore) Now() time.Time { return s.clock.Now() }

// ClockOffset reports the store clock minus this replica's wall clock at the
// last sync (Postgres), or 0 (SQLite / an overridden clock).
func (s *SQLStore) ClockOffset() (time.Duration, time.Time) {
	if s.dbc != nil && s.clock == TimeSource(s.dbc) {
		return s.dbc.ClockOffset()
	}
	return 0, time.Now()
}

func (s *SQLStore) ClaimMint(ctx context.Context, id, claimer string, now, staleBefore time.Time) (bool, error) {
	r, err := s.db.ExecContext(ctx, s.q(`UPDATE objects SET mint_claim=?, mint_claimed_at=?
		WHERE id=? AND status=? AND key_id='' AND (mint_claim='' OR mint_claim=? OR mint_claimed_at<?)`),
		claimer, now.UnixMilli(), id, StatusPending, claimer, staleBefore.UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

// UpdateThrottle applies fn to the tenant's shared throttle state in one
// transaction (Postgres: row locked FOR UPDATE; SQLite: the immediate
// transaction holds the write lock). A missing row reaches fn as the zero
// state (Last == 0), which the throttle initialises to a full bucket.
func (s *SQLStore) UpdateThrottle(ctx context.Context, tenantID string, fn func(*ThrottleState)) error {
	if _, err := s.db.ExecContext(ctx, s.q(`INSERT INTO tenant_throttle (tenant_id, tokens, rate, last, pause_till, failures)
		VALUES (?, 0, 0, 0, 0, 0) ON CONFLICT (tenant_id) DO NOTHING`), tenantID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var st ThrottleState
	var failures int64
	if err := tx.QueryRowContext(ctx, s.q("SELECT tokens, rate, last, pause_till, failures FROM tenant_throttle WHERE tenant_id=?"+s.forUpdate()),
		tenantID).Scan(&st.Tokens, &st.Rate, &st.Last, &st.PauseTill, &failures); err != nil {
		return err
	}
	st.Failures = int(failures)
	fn(&st)
	if _, err := tx.ExecContext(ctx, s.q("UPDATE tenant_throttle SET tokens=?, rate=?, last=?, pause_till=?, failures=? WHERE tenant_id=?"),
		st.Tokens, st.Rate, st.Last, st.PauseTill, int64(st.Failures), tenantID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLStore) SetDeploymentStatus(ctx context.Context, id, status, errText string) error {
	_, err := s.db.ExecContext(ctx, s.q("UPDATE deployments SET host_status=?, host_error=? WHERE id=? AND (host_status<>? OR host_error<>?)"),
		status, errText, id, status, errText)
	return err
}

func (s *SQLStore) SetDeviceDelete(ctx context.Context, id string, due int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error { // a tx: a device_delete audit intent commits with it (H22)
		_, err := tx.ExecContext(ctx, s.q("UPDATE objects SET device_delete_at=? WHERE id=?"), due, id)
		return err
	})
}

// ClaimDeviceDelete takes a due delete by moving its due time to now +
// DeviceDeleteClaimTTL: a claim is a lease, not a terminal state, so a
// claimer that dies or can't record the outcome (store blip) leaves the
// delete due again (H21, soak 101). Callers mark success with
// SetDeviceDelete(id, -1).
func (s *SQLStore) ClaimDeviceDelete(ctx context.Context, id string, now int64) (bool, error) {
	claimUntil := now + DeviceDeleteClaimTTL.Milliseconds()
	if now == math.MaxInt64 { // forced (FlushDeletes)
		claimUntil = time.Now().Add(DeviceDeleteClaimTTL).UnixMilli()
	}
	r, err := s.db.ExecContext(ctx, s.q("UPDATE objects SET device_delete_at=? WHERE id=? AND device_delete_at>0 AND device_delete_at<=?"), claimUntil, id, now)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (s *SQLStore) DueDeviceDeletes(ctx context.Context, now int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.q("SELECT id FROM objects WHERE device_delete_at>0 AND device_delete_at<=? ORDER BY device_delete_at"), now)
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

// ChooseSecret records keyID as the secret for name unless another key was
// chosen first, and returns the chosen key ID (mm-loop F11, H13).
func (s *SQLStore) ChooseSecret(ctx context.Context, name, keyID string, now int64) (string, error) {
	if _, err := s.db.ExecContext(ctx, s.q("INSERT INTO secret_choice (name, key_id, created_at) VALUES (?, ?, ?) ON CONFLICT (name) DO NOTHING"),
		name, keyID, now); err != nil {
		return "", err
	}
	return s.SecretChoice(ctx, name)
}

// SecretChoice returns the chosen key ID for name (ErrNotFound if none).
func (s *SQLStore) SecretChoice(ctx context.Context, name string) (string, error) {
	var k string
	err := s.db.QueryRowContext(ctx, s.q("SELECT key_id FROM secret_choice WHERE name=?"), name).Scan(&k)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return k, err
}

func (s *SQLStore) DeleteSecretChoice(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, s.q("DELETE FROM secret_choice WHERE name=?"), name)
	return err
}

func (s *SQLStore) UpsertReplica(ctx context.Context, r Replica) error {
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO replicas (id, custody, started_at, last_seen) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET custody=excluded.custody, started_at=excluded.started_at, last_seen=excluded.last_seen`),
		r.ID, r.Custody, r.StartedAt, r.LastSeen)
	return err
}

func (s *SQLStore) ListReplicas(ctx context.Context) ([]Replica, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, custody, started_at, last_seen FROM replicas ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Replica
	for rows.Next() {
		var r Replica
		if err := rows.Scan(&r.ID, &r.Custody, &r.StartedAt, &r.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLStore) ReleaseMint(ctx context.Context, id, claimer string) error {
	_, err := s.db.ExecContext(ctx, s.q("UPDATE objects SET mint_claim='', mint_claimed_at=0 WHERE id=? AND mint_claim=?"), id, claimer)
	return err
}

// ClockSource reports "postgres" while the store follows the server clock,
// else "local".
func (s *SQLStore) ClockSource() string {
	if s.dbc != nil && s.clock == TimeSource(s.dbc) {
		return "postgres"
	}
	return "local"
}

// SetClock overrides the shared clock (tests: simulated time shared by
// several Mgmt replicas on this store).
func (s *SQLStore) SetClock(c TimeSource) { s.clock = c }

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

// Session limits for mgmt's Postgres connections (S6): a replica whose link
// to the store is slow must not stall the healthy ones. lock_timeout bounds
// how long anyone waits for a row lock; idle_in_transaction_session_timeout
// aborts a transaction that sits idle between its statements (the slow
// replica), releasing its locks. Both surface as StoreUnavailable (503).
const (
	pgLockTimeoutMs     = 3000
	pgIdleInTxTimeoutMs = 3000
)

// DefaultMaxConns bounds each replica's Postgres pool; N replicas use at
// most N×DefaultMaxConns connections. Override with ?twd_max_conns=K.
const DefaultMaxConns = 16

// takeMaxConns removes twd's own twd_max_conns parameter from dsn (pgx would
// send it to the server) and returns the pool size.
func takeMaxConns(dsn string) (string, int) {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn, DefaultMaxConns
	}
	q := u.Query()
	n := DefaultMaxConns
	if v := q.Get("twd_max_conns"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k > 0 {
			n = k
		}
		q.Del("twd_max_conns")
		u.RawQuery = q.Encode()
	}
	return u.String(), n
}

// withSessionLimits adds the session limits as startup parameters unless the
// DSN already sets them.
func withSessionLimits(dsn string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	for k, v := range map[string]int{"lock_timeout": pgLockTimeoutMs, "idle_in_transaction_session_timeout": pgIdleInTxTimeoutMs} {
		if !strings.Contains(dsn, k+"=") {
			dsn += fmt.Sprintf("%s%s=%d", sep, k, v)
			sep = "&"
		}
	}
	return dsn
}

// forShare locks a row against changes but not against other sharers
// (fence reads: concurrent fenced commits don't serialise; H17).
func (s *SQLStore) forShare() string {
	if s.d == dialectPostgres {
		return " FOR SHARE"
	}
	return ""
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
CREATE TABLE IF NOT EXISTS tenant_throttle (
  tenant_id TEXT PRIMARY KEY, tokens REAL NOT NULL, rate REAL NOT NULL, last BIGINT NOT NULL,
  pause_till BIGINT NOT NULL, failures BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS audit_outbox (
  id TEXT PRIMARY KEY, created_at BIGINT NOT NULL, record TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS secret_choice (
  name TEXT PRIMARY KEY, key_id TEXT NOT NULL, created_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS replicas (
  id TEXT PRIMARY KEY, custody TEXT NOT NULL, started_at BIGINT NOT NULL, last_seen BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS container_reservations (
  id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, workload TEXT NOT NULL, class TEXT NOT NULL, object TEXT NOT NULL,
  host_id TEXT NOT NULL, uvcpu BIGINT NOT NULL, mem_mib BIGINT NOT NULL, disk_mib BIGINT NOT NULL,
  created_at BIGINT NOT NULL, renewed_at BIGINT NOT NULL,
  UNIQUE (tenant_id, workload, class, object));
CREATE INDEX IF NOT EXISTS container_reservations_tenant ON container_reservations (tenant_id, renewed_at);
CREATE INDEX IF NOT EXISTS container_reservations_host ON container_reservations (host_id);
CREATE TABLE IF NOT EXISTS registry_links (
  repo TEXT NOT NULL, digest TEXT NOT NULL, kind TEXT NOT NULL, media_type TEXT NOT NULL, size BIGINT NOT NULL, created_at BIGINT NOT NULL,
  PRIMARY KEY (repo, digest, kind));
CREATE TABLE IF NOT EXISTS registry_tags (
  repo TEXT NOT NULL, tag TEXT NOT NULL, digest TEXT NOT NULL, updated_at BIGINT NOT NULL,
  PRIMARY KEY (repo, tag));
CREATE TABLE IF NOT EXISTS public_images (
  id TEXT PRIMARY KEY, ref TEXT NOT NULL, arch TEXT NOT NULL, repo TEXT NOT NULL, digest TEXT NOT NULL,
  source_digest TEXT NOT NULL, created_by TEXT NOT NULL, created_at BIGINT NOT NULL,
  UNIQUE (ref, arch));
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
	if err := s.addColumns(ctx); err != nil {
		return err
	}
	return s.addIndexes(ctx)
}

// addIndexes creates the cursor-pagination indexes (/v1 lists are ordered
// by (created_at, id); audit by (ts, chain, seq)). Idempotent on both
// dialects; hosts.created_at is backfilled from last_seen for rows that
// predate the column.
func (s *SQLStore) addIndexes(ctx context.Context) error {
	for _, ddl := range []string{
		"UPDATE hosts SET created_at=last_seen WHERE created_at=0",
		"CREATE INDEX IF NOT EXISTS tenants_created ON tenants (created_at, id)",
		"CREATE INDEX IF NOT EXISTS hosts_created ON hosts (created_at, id)",
		"CREATE INDEX IF NOT EXISTS deployments_created ON deployments (created_at, id)",
		"CREATE INDEX IF NOT EXISTS objects_created ON objects (created_at, id)",
		"CREATE INDEX IF NOT EXISTS objects_tenant_created ON objects (tenant_id, created_at, id)",
		"CREATE INDEX IF NOT EXISTS audit_ts ON audit (ts, chain, seq)",
		"CREATE INDEX IF NOT EXISTS audit_tenant ON audit (tenant_id, ts)",
		"CREATE INDEX IF NOT EXISTS audit_acting_for ON audit (acting_for)",
	} {
		if _, err := s.db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("%w: %s", err, ddl)
		}
	}
	return nil
}

// afterKey is the keyset condition for a page: rows strictly after
// (created_at, id). cols names the two columns (possibly prefixed).
func afterKey(pg Page, created, id string) (string, []any) {
	if pg.After == nil {
		return "", nil
	}
	return "(" + created + " > ? OR (" + created + " = ? AND " + id + " > ?))", []any{pg.After.Created, pg.After.Created, pg.After.ID}
}

func limitClause(pg Page) string {
	if pg.Limit > 0 {
		return fmt.Sprintf(" LIMIT %d", pg.Limit)
	}
	return ""
}

// addColumns adds columns introduced after a store was created (additive
// migrations; both dialects accept ADD COLUMN with a default).
func (s *SQLStore) addColumns(ctx context.Context) error {
	for _, c := range []struct{ table, col, ddl string }{
		{"objects", "cert", "ALTER TABLE objects ADD COLUMN cert TEXT NOT NULL DEFAULT ''"},                       // RT-01 node certificates
		{"deployments", "deployed_by", "ALTER TABLE deployments ADD COLUMN deployed_by TEXT NOT NULL DEFAULT ''"}, // M10: WhoIs login / local actor
		// M11 (/v1): tenant pool/tier/updated_at; host pool, push port,
		// created/updated; deployment stopped_at (undeploy); the object
		// kill record (tombstone) and acting_for.
		{"tenants", "pool", "ALTER TABLE tenants ADD COLUMN pool TEXT NOT NULL DEFAULT ''"},
		{"tenants", "tier", "ALTER TABLE tenants ADD COLUMN tier TEXT NOT NULL DEFAULT ''"},
		{"tenants", "updated_at", "ALTER TABLE tenants ADD COLUMN updated_at BIGINT NOT NULL DEFAULT 0"},
		{"hosts", "pool", "ALTER TABLE hosts ADD COLUMN pool TEXT NOT NULL DEFAULT ''"},
		{"hosts", "push_port", "ALTER TABLE hosts ADD COLUMN push_port BIGINT NOT NULL DEFAULT 0"},
		{"hosts", "created_at", "ALTER TABLE hosts ADD COLUMN created_at BIGINT NOT NULL DEFAULT 0"},
		{"hosts", "updated_at", "ALTER TABLE hosts ADD COLUMN updated_at BIGINT NOT NULL DEFAULT 0"},
		{"deployments", "stopped_at", "ALTER TABLE deployments ADD COLUMN stopped_at BIGINT NOT NULL DEFAULT 0"},
		{"objects", "killed_at", "ALTER TABLE objects ADD COLUMN killed_at BIGINT NOT NULL DEFAULT 0"},
		{"objects", "killed_by", "ALTER TABLE objects ADD COLUMN killed_by TEXT NOT NULL DEFAULT ''"},
		{"objects", "kill_cascade", "ALTER TABLE objects ADD COLUMN kill_cascade BIGINT NOT NULL DEFAULT 0"},
		{"objects", "kill_root", "ALTER TABLE objects ADD COLUMN kill_root TEXT NOT NULL DEFAULT ''"},
		{"objects", "acting_for", "ALTER TABLE objects ADD COLUMN acting_for TEXT NOT NULL DEFAULT ''"},
		// M12: oauth mint mode (client ID + credential file path; never the secret).
		{"tenants", "oauth_client_id", "ALTER TABLE tenants ADD COLUMN oauth_client_id TEXT NOT NULL DEFAULT ''"},
		{"tenants", "oauth_client_file", "ALTER TABLE tenants ADD COLUMN oauth_client_file TEXT NOT NULL DEFAULT ''"},
		// mm-loop F13 (H08): the host-reported deployment status, shared by replicas.
		{"deployments", "host_status", "ALTER TABLE deployments ADD COLUMN host_status TEXT NOT NULL DEFAULT ''"},
		{"deployments", "host_error", "ALTER TABLE deployments ADD COLUMN host_error TEXT NOT NULL DEFAULT ''"},
		// mm-loop F10 (H07): durable device deletes (0 none, >0 due unix ms, -1 claimed/done).
		{"objects", "device_delete_at", "ALTER TABLE objects ADD COLUMN device_delete_at BIGINT NOT NULL DEFAULT 0"},
		// mm-loop F9 (ADR-028 §3): hosts self-fence only stateful workloads.
		// Existing rows default to 1 (fence), the safe value.
		{"deployments", "fence", "ALTER TABLE deployments ADD COLUMN fence BIGINT NOT NULL DEFAULT 1"},
		// mm-loop F8 (H06): every replica can push to a host.
		{"hosts", "push_target", "ALTER TABLE hosts ADD COLUMN push_target TEXT NOT NULL DEFAULT ''"},
		// mm-loop F3 (H02): one replica mints each object.
		{"objects", "mint_claim", "ALTER TABLE objects ADD COLUMN mint_claim TEXT NOT NULL DEFAULT ''"},
		{"objects", "mint_claimed_at", "ALTER TABLE objects ADD COLUMN mint_claimed_at BIGINT NOT NULL DEFAULT 0"},
		// ADR-032: container quotas (0 = the default), host arch and
		// capacity/usage vectors (JSON), a deployment's container classes.
		{"tenants", "max_containers", "ALTER TABLE tenants ADD COLUMN max_containers BIGINT NOT NULL DEFAULT 0"},
		{"tenants", "max_container_mem", "ALTER TABLE tenants ADD COLUMN max_container_mem BIGINT NOT NULL DEFAULT 0"},
		{"tenants", "max_container_uvcpu", "ALTER TABLE tenants ADD COLUMN max_container_uvcpu BIGINT NOT NULL DEFAULT 0"},
		{"hosts", "arch", "ALTER TABLE hosts ADD COLUMN arch TEXT NOT NULL DEFAULT ''"},
		{"hosts", "capacity_vec", "ALTER TABLE hosts ADD COLUMN capacity_vec TEXT NOT NULL DEFAULT ''"},
		{"hosts", "usage_vec", "ALTER TABLE hosts ADD COLUMN usage_vec TEXT NOT NULL DEFAULT ''"},
		{"deployments", "containers", "ALTER TABLE deployments ADD COLUMN containers TEXT NOT NULL DEFAULT ''"},
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
	return s.commitWithIntents(ctx, tx)
}

// commitWithIntents writes the audit intents carried by ctx into
// audit_outbox inside tx, then commits: the change and its audit record
// commit together or not at all (H22). An outbox write failure rolls the
// change back.
func (s *SQLStore) commitWithIntents(ctx context.Context, tx *sql.Tx) error {
	pend := pendingIntents(ctx)
	for _, ai := range pend {
		b, err := json.Marshal(ai.Rec)
		if err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, s.q("INSERT INTO audit_outbox (id, created_at, record) VALUES (?, ?, ?)"), ai.ID, s.Now().UnixMilli(), string(b)); err != nil {
			tx.Rollback()
			return fmt.Errorf("audit outbox: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, ai := range pend {
		ai.mu.Lock()
		ai.written = true
		ai.mu.Unlock()
	}
	return nil
}

// PutAuditIntent writes a standalone intent (an audit record not bound to a
// state change of its own).
func (s *SQLStore) PutAuditIntent(ctx context.Context, id string, rec AuditRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, s.q("INSERT INTO audit_outbox (id, created_at, record) VALUES (?, ?, ?)"), id, s.Now().UnixMilli(), string(b))
	return err
}

// AmendAuditIntent replaces a pending intent's record (detail known only
// after the change). No-op if it was already drained.
func (s *SQLStore) AmendAuditIntent(ctx context.Context, id string, rec AuditRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, s.q("UPDATE audit_outbox SET record=? WHERE id=?"), string(b), id)
	return err
}

// DrainAuditIntent appends intent id to its chain and removes it from the
// outbox in one transaction; false if another replica already did.
func (s *SQLStore) DrainAuditIntent(ctx context.Context, id string) (bool, error) {
	done := false
	err := s.tx(withoutIntents(ctx), func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRowContext(ctx, s.q("DELETE FROM audit_outbox WHERE id=? RETURNING record"), id).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var rec AuditRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return err
		}
		if _, err := s.appendAuditTx(ctx, tx, rec); err != nil {
			return err
		}
		done = true
		return nil
	})
	return done, err
}

// ListAuditIntents returns outbox intents created before olderThan (unix ms).
func (s *SQLStore) ListAuditIntents(ctx context.Context, olderThan int64, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.q("SELECT id FROM audit_outbox WHERE created_at<? ORDER BY created_at, id LIMIT ?"), olderThan, limit)
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

const tenantCols = "id, tailnet, control_url, mint_mode, api_key_file, wif_client_id, wif_lifecycle_client_id, rpm, max_objects, created_at, pool, tier, updated_at, oauth_client_id, oauth_client_file, max_containers, max_container_mem, max_container_uvcpu"

func scanTenant(sc interface{ Scan(...any) error }) (Tenant, error) {
	var t Tenant
	var rpm, mo, mc, mm, mv int64
	err := sc.Scan(&t.ID, &t.Tailnet, &t.ControlURL, &t.MintMode, &t.APIKeyFile, &t.WIFClientID, &t.WIFLifecycleClientID, &rpm, &mo, &t.CreatedAt, &t.Pool, &t.Tier, &t.UpdatedAt, &t.OAuthClientID, &t.OAuthClientFile,
		&mc, &mm, &mv)
	t.RegistrationsPerMinute, t.MaxObjects = int(rpm), int(mo)
	t.MaxContainers, t.MaxContainerMemMiB, t.MaxContainerVCPU = int(mc), int(mm), float64(mv)/1e6
	t.containerDefaults() // rows from before the columns
	if t.UpdatedAt == 0 {
		t.UpdatedAt = t.CreatedAt
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return t, err
}

// micro is a fractional vCPU count in millionths (the stored unit).
func micro(v float64) int64 { return int64(math.Round(v * 1e6)) }

func (s *SQLStore) CreateTenant(ctx context.Context, t Tenant) error {
	if t.UpdatedAt == 0 {
		t.UpdatedAt = t.CreatedAt
	}
	_, err := s.db.ExecContext(ctx, s.q("INSERT INTO tenants ("+tenantCols+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"),
		t.ID, t.Tailnet, t.ControlURL, t.MintMode, t.APIKeyFile, t.WIFClientID, t.WIFLifecycleClientID,
		t.RegistrationsPerMinute, t.MaxObjects, t.CreatedAt, t.Pool, t.Tier, t.UpdatedAt, t.OAuthClientID, t.OAuthClientFile,
		t.MaxContainers, t.MaxContainerMemMiB, micro(t.MaxContainerVCPU))
	if isUnique(err) {
		return ErrExists
	}
	return err
}

// UpdateTenant rewrites a tenant's mint configuration (mode, key file,
// WIF client IDs) and limits; ID, tailnet and createdAt are immutable.
func (s *SQLStore) UpdateTenant(ctx context.Context, t Tenant) error {
	res, err := s.db.ExecContext(ctx, s.q("UPDATE tenants SET control_url=?, mint_mode=?, api_key_file=?, wif_client_id=?, wif_lifecycle_client_id=?, rpm=?, max_objects=?, pool=?, tier=?, updated_at=?, oauth_client_id=?, oauth_client_file=?, max_containers=?, max_container_mem=?, max_container_uvcpu=? WHERE id=?"),
		t.ControlURL, t.MintMode, t.APIKeyFile, t.WIFClientID, t.WIFLifecycleClientID, t.RegistrationsPerMinute, t.MaxObjects, t.Pool, t.Tier, t.UpdatedAt, t.OAuthClientID, t.OAuthClientFile,
		t.MaxContainers, t.MaxContainerMemMiB, micro(t.MaxContainerVCPU), t.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) GetTenant(ctx context.Context, id string) (Tenant, error) {
	return scanTenant(s.db.QueryRowContext(ctx, s.q("SELECT "+tenantCols+" FROM tenants WHERE id=?"), id))
}

func (s *SQLStore) DeleteTenant(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{"DELETE FROM objects WHERE tenant_id=?", "DELETE FROM deployments WHERE tenant_id=?",
			"DELETE FROM leases WHERE name LIKE ? || '/%'", "DELETE FROM container_reservations WHERE tenant_id=?",
			// The tenant's registry namespace (ADR-032 D3): its links and
			// tags go; blobs stay content-addressed on disk.
			"DELETE FROM registry_links WHERE repo LIKE ? || '/%'", "DELETE FROM registry_tags WHERE repo LIKE ? || '/%'"} {
			if _, err := tx.ExecContext(ctx, s.q(q), id); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, s.q("DELETE FROM tenants WHERE id=?"), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *SQLStore) ListTenants(ctx context.Context, pg Page) ([]Tenant, error) {
	q, args := "SELECT "+tenantCols+" FROM tenants", []any{}
	if cond, a := afterKey(pg, "created_at", "id"); cond != "" {
		q, args = q+" WHERE "+cond, a
	}
	rows, err := s.db.QueryContext(ctx, s.q(q+" ORDER BY created_at, id"+limitClause(pg)), args...)
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

const hostCols = "id, capacity, tiers, node_id, audit_pubkey, live_nodes, last_seen, pool, push_port, created_at, updated_at, push_target, arch, capacity_vec, usage_vec"

func scanHost(sc interface{ Scan(...any) error }) (Host, error) {
	var h Host
	var capacity, live int64
	var tiers, capVec, usageVec string
	var port int64
	err := sc.Scan(&h.ID, &capacity, &tiers, &h.NodeID, &h.AuditPubKey, &live, &h.LastSeen, &h.Pool, &port, &h.CreatedAt, &h.UpdatedAt, &h.PushTarget,
		&h.Arch, &capVec, &usageVec)
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	h.CapacityNodes, h.LiveNodes, h.PushPort = int(capacity), int(live), int(port)
	if h.UpdatedAt == 0 {
		h.UpdatedAt = h.LastSeen
	}
	json.Unmarshal([]byte(tiers), &h.Tiers)
	if capVec != "" {
		h.Capacity = &HostCapacity{}
		json.Unmarshal([]byte(capVec), h.Capacity)
	}
	if usageVec != "" {
		h.Usage = &HostUsage{}
		json.Unmarshal([]byte(usageVec), h.Usage)
	}
	return h, err
}

// vecStr is a capacity/usage vector column ("" = none reported).
func vecStr[T any](v *T) string {
	if v == nil {
		return ""
	}
	return jsonStr(v)
}

func (s *SQLStore) UpsertHost(ctx context.Context, h Host) error {
	if h.UpdatedAt == 0 {
		h.UpdatedAt = h.LastSeen
	}
	if h.CreatedAt == 0 {
		h.CreatedAt = h.UpdatedAt
	}
	// created_at is kept from the first insert (the page key must be stable).
	// usage_vec is the heartbeat's (TouchHost), kept across re-registration.
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO hosts (`+hostCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (id) DO UPDATE SET capacity=excluded.capacity, tiers=excluded.tiers, node_id=excluded.node_id,
		audit_pubkey=excluded.audit_pubkey, last_seen=excluded.last_seen, pool=excluded.pool, push_port=excluded.push_port, updated_at=excluded.updated_at,
		push_target=excluded.push_target, arch=excluded.arch, capacity_vec=excluded.capacity_vec`),
		h.ID, h.CapacityNodes, jsonStr(h.Tiers), h.NodeID, h.AuditPubKey, h.LiveNodes, h.LastSeen, h.Pool, h.PushPort, h.CreatedAt, h.UpdatedAt, h.PushTarget,
		h.Arch, vecStr(h.Capacity), vecStr(h.Usage))
	return err
}

func (s *SQLStore) GetHost(ctx context.Context, id string) (Host, error) {
	return scanHost(s.db.QueryRowContext(ctx, s.q("SELECT "+hostCols+" FROM hosts WHERE id=?"), id))
}

// DeleteHost removes the host and its container reservations (its
// containers die with it: ADR-032 D2).
func (s *SQLStore) DeleteHost(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.q("DELETE FROM hosts WHERE id=?"), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, s.q("DELETE FROM container_reservations WHERE host_id=?"), id)
		return err
	})
}

func (s *SQLStore) ListHosts(ctx context.Context, pg Page) ([]Host, error) {
	q, args := "SELECT "+hostCols+" FROM hosts", []any{}
	if cond, a := afterKey(pg, "created_at", "id"); cond != "" {
		q, args = q+" WHERE "+cond, a
	}
	rows, err := s.db.QueryContext(ctx, s.q(q+" ORDER BY created_at, id"+limitClause(pg)), args...)
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

func (s *SQLStore) TouchHost(ctx context.Context, id string, liveNodes int, usage *HostUsage, now time.Time) error {
	q, args := "UPDATE hosts SET live_nodes=?, last_seen=? WHERE id=?", []any{liveNodes, now.UnixMilli(), id}
	if usage != nil {
		q, args = "UPDATE hosts SET live_nodes=?, last_seen=?, usage_vec=? WHERE id=?", []any{liveNodes, now.UnixMilli(), jsonStr(usage), id}
	}
	r, err := s.db.ExecContext(ctx, s.q(q), args...)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- deployments ----

const deployCols = "id, tenant_id, workload, host_id, namespaces, max_live, max_depth, epoch, created_at, deployed_by, stopped_at, fence, host_status, host_error, containers"

func scanDeployment(sc interface{ Scan(...any) error }) (Deployment, error) {
	var d Deployment
	var ns, cs string
	var ml, md, fence int64
	err := sc.Scan(&d.ID, &d.TenantID, &d.Workload, &d.HostID, &ns, &ml, &md, &d.Epoch, &d.CreatedAt, &d.DeployedBy, &d.StoppedAt, &fence, &d.HostStatus, &d.HostError, &cs)
	d.Fence = fence != 0
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	d.MaxLive, d.MaxDepth = int(ml), int(md)
	json.Unmarshal([]byte(ns), &d.Namespaces)
	if cs != "" {
		json.Unmarshal([]byte(cs), &d.Containers)
	}
	return d, err
}

// containersStr is the deployments.containers column ("" = none).
func containersStr(cs []container.Class) string {
	if len(cs) == 0 {
		return ""
	}
	return jsonStr(cs)
}

func (s *SQLStore) CreateDeployment(ctx context.Context, d Deployment) error {
	fence := int64(0)
	if d.Fence {
		fence = 1
	}
	err := s.tx(ctx, func(tx *sql.Tx) error { // a tx so a deploy's audit intent commits with it (H22)
		_, err := tx.ExecContext(ctx, s.q("INSERT INTO deployments ("+deployCols+", bundle) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"),
			d.ID, d.TenantID, d.Workload, d.HostID, jsonStr(d.Namespaces), d.MaxLive, d.MaxDepth, d.Epoch, d.CreatedAt, d.DeployedBy, d.StoppedAt, fence,
			d.HostStatus, d.HostError, containersStr(d.Containers), d.Bundle)
		return err
	})
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func (s *SQLStore) GetDeployment(ctx context.Context, id string) (Deployment, error) {
	row := s.db.QueryRowContext(ctx, s.q("SELECT "+deployCols+", bundle FROM deployments WHERE id=?"), id)
	var d Deployment
	var ns, cs string
	var ml, md, fence int64
	err := row.Scan(&d.ID, &d.TenantID, &d.Workload, &d.HostID, &ns, &ml, &md, &d.Epoch, &d.CreatedAt, &d.DeployedBy, &d.StoppedAt, &fence, &d.HostStatus, &d.HostError, &cs, &d.Bundle)
	d.Fence = fence != 0
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	d.MaxLive, d.MaxDepth = int(ml), int(md)
	json.Unmarshal([]byte(ns), &d.Namespaces)
	if cs != "" {
		json.Unmarshal([]byte(cs), &d.Containers)
	}
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

func (s *SQLStore) StopDeployment(ctx context.Context, id string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, s.q("UPDATE deployments SET stopped_at=? WHERE id=?"), now.UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) ListDeployments(ctx context.Context, f DeploymentFilter, pg Page) ([]Deployment, error) {
	var where []string
	var args []any
	add := func(cond string, v any) { where = append(where, cond); args = append(args, v) }
	if f.TenantID != "" {
		add("d.tenant_id=?", f.TenantID)
	}
	if f.Workload != "" {
		add("d.workload=?", f.Workload)
	}
	if f.HostID != "" {
		add("d.host_id=?", f.HostID)
	}
	if f.Latest {
		where = append(where, `NOT EXISTS (SELECT 1 FROM deployments n WHERE n.tenant_id=d.tenant_id AND n.workload=d.workload
		  AND (n.created_at > d.created_at OR (n.created_at = d.created_at AND n.id > d.id)))`)
	}
	if cond, a := afterKey(pg, "d.created_at", "d.id"); cond != "" {
		where = append(where, cond)
		args = append(args, a...)
	}
	q := "SELECT " + prefixCols("d.", deployCols) + " FROM deployments d"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.db.QueryContext(ctx, s.q(q+" ORDER BY d.created_at, d.id"+limitClause(pg)), args...)
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
const objectCols = objectColsV1 + ", cert, killed_at, killed_by, kill_cascade, kill_root, acting_for, device_delete_at"

// objectPlaceholders is "?,?,…" for objectCols.
var objectPlaceholders = strings.TrimSuffix(strings.Repeat("?,", len(strings.Split(objectCols, ","))), ",")

func scanObject(sc interface{ Scan(...any) error }) (Object, error) {
	var o Object
	var tags, caps string
	var depth, eph, casc int64
	err := sc.Scan(&o.ID, &o.TenantID, &o.Workload, &o.Namespace, &o.Name, &o.Hostname, &o.DeploymentID, &o.Lease, &tags, &caps,
		&o.ParentID, &o.RootID, &depth, &o.TTLSeconds, &eph, &o.Status, &o.NodeID, &o.StableNodeID, &o.MagicDNS,
		&o.KeyID, &o.KeyExpires, &o.KillReason, &o.CreatedAt, &o.UpdatedAt, &o.Cert, &o.KilledAt, &o.KilledBy, &casc, &o.KillRoot, &o.ActingFor, &o.DeviceDeleteAt)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	o.Depth, o.Ephemeral, o.KillCascade = int(depth), eph != 0, casc != 0
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
	casc := 0
	if o.KillCascade {
		casc = 1
	}
	return []any{o.ID, o.TenantID, o.Workload, o.Namespace, o.Name, o.Hostname, o.DeploymentID, o.Lease, jsonStr(o.Tags), string(o.Caps),
		o.ParentID, o.RootID, o.Depth, o.TTLSeconds, eph, o.Status, o.NodeID, o.StableNodeID, o.MagicDNS,
		o.KeyID, o.KeyExpires, o.KillReason, o.CreatedAt, o.UpdatedAt, o.Cert, o.KilledAt, o.KilledBy, casc, o.KillRoot, o.ActingFor, o.DeviceDeleteAt}
}

func (s *SQLStore) CreateObject(ctx context.Context, o Object) error {
	_, err := s.db.ExecContext(ctx, s.q("INSERT INTO objects ("+objectCols+") VALUES ("+objectPlaceholders+")"), objectArgs(o)...)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

// CreateChild inserts a spawned object iff its root stays within maxLive
// live descendants (CreateObjectChecked without a tenant quota).
func (s *SQLStore) CreateChild(ctx context.Context, o Object, maxLive int) error {
	return s.CreateObjectChecked(ctx, o, 0, maxLive)
}

// CreateObjectChecked inserts o in one transaction with its limits, so
// concurrent creates through different replicas can't overshoot them:
// maxObjects > 0 locks the tenant row and refuses (ErrQuota) when the tenant
// already has maxObjects live objects (H16); maxLive > 0 (a spawn) locks the
// root row and refuses (ErrSpawnBudget) when the root already has maxLive
// live descendants (H04). Locks are taken tenant first, then root. Postgres
// uses FOR UPDATE; SQLite's immediate transaction holds the write lock.
// ErrExists on a duplicate.
func (s *SQLStore) CreateObjectChecked(ctx context.Context, o Object, maxObjects, maxLive int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	live := "status NOT IN ('" + StatusKilled + "','" + StatusFailed + "')"
	lock := func(q string, arg string) error {
		var x string
		if err := tx.QueryRowContext(ctx, s.q(q+s.forUpdate()), arg).Scan(&x); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		return nil
	}
	if maxObjects > 0 {
		if err := lock("SELECT id FROM tenants WHERE id=?", o.TenantID); err != nil {
			return err
		}
		var n int64
		if err := tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM objects WHERE tenant_id=? AND "+live), o.TenantID).Scan(&n); err != nil {
			return err
		}
		if int(n) >= maxObjects {
			return ErrQuota
		}
	}
	if maxLive > 0 {
		if err := lock("SELECT id FROM objects WHERE id=?", o.RootID); err != nil {
			return err
		}
		var n int64
		if err := tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM objects WHERE root_id=? AND id<>? AND "+live), o.RootID, o.RootID).Scan(&n); err != nil {
			return err
		}
		if int(n)+1 > maxLive {
			return ErrSpawnBudget
		}
	}
	if _, err := tx.ExecContext(ctx, s.q("INSERT INTO objects ("+objectCols+") VALUES ("+objectPlaceholders+")"), objectArgs(o)...); err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	return s.commitWithIntents(ctx, tx)
}

func (s *SQLStore) GetObject(ctx context.Context, id string) (Object, error) {
	return scanObject(s.db.QueryRowContext(ctx, s.q("SELECT "+objectCols+" FROM objects WHERE id=?"), id))
}

func (s *SQLStore) DeleteDeadObjects(ctx context.Context, tenantID string, statuses []string) ([]Object, error) {
	if len(statuses) == 0 {
		statuses = []string{StatusKilled, StatusFailed}
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",")
	args := []any{tenantID}
	for _, st := range statuses {
		if st != StatusKilled && st != StatusFailed {
			return nil, fmt.Errorf("purge: %q is not a terminal status", st)
		}
		args = append(args, st)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, s.q("SELECT "+objectCols+" FROM objects WHERE tenant_id=? AND status IN ("+ph+")"), args...)
	if err != nil {
		return nil, err
	}
	var dead []Object
	for rows.Next() {
		o, err := scanObject(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		dead = append(dead, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Their kill revocations go too: a host syncing after the purge must
	// not kill a new object that reuses the (deterministic) ID.
	for _, o := range dead {
		if _, err := tx.ExecContext(ctx, s.q("DELETE FROM revocations WHERE object_id=?"), o.ID); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, s.q("DELETE FROM objects WHERE tenant_id=? AND status IN ("+ph+")"), args...); err != nil {
		return nil, err
	}
	return dead, tx.Commit()
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
	if cond, a := afterKey(f.Page, "created_at", "id"); cond != "" {
		where = append(where, cond)
		args = append(args, a...)
	}
	q := "SELECT " + objectCols + " FROM objects"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.db.QueryContext(ctx, s.q(q+" ORDER BY created_at, id"+limitClause(f.Page)), args...)
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
	var expires int64
	err := tx.QueryRowContext(ctx, s.q("SELECT holder, epoch, expires_at FROM leases WHERE name=?"+s.forShare()), f.Lease).Scan(&holder, &epoch, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no lease %q (caller %q asked for epoch %d)", ErrLeaseLost, f.Lease, f.Holder, f.Epoch)
	}
	if err != nil {
		return err
	}
	if epoch != f.Epoch || holder == "" || (f.Holder != "" && holder != f.Holder) {
		return fmt.Errorf("%w: lease %q is held by %q at epoch %d (expires %s); caller %q asked for epoch %d",
			ErrLeaseLost, f.Lease, holder, epoch, time.UnixMilli(expires).UTC().Format(time.RFC3339), f.Holder, f.Epoch)
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

func (s *SQLStore) CountObjects(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT status, COUNT(*) FROM objects GROUP BY status")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = int(n)
	}
	return out, rows.Err()
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
	var out AuditRecord
	err := s.tx(withoutIntents(ctx), func(tx *sql.Tx) error {
		var err error
		out, err = s.appendAuditTx(ctx, tx, rec)
		return err
	})
	return out, err
}

// appendAuditTx appends rec to its chain inside tx.
func (s *SQLStore) appendAuditTx(ctx context.Context, tx *sql.Tx, rec AuditRecord) (AuditRecord, error) {
	if rec.Chain == "" {
		return rec, errors.New("audit chain required")
	}
	err := func() error {
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
	}()
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
	if len(f.Kinds) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(f.Kinds)), ",")
		where = append(where, "kind IN ("+ph+")")
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	if f.Since != "" {
		add("ts>=?", f.Since)
	}
	if f.Until != "" {
		add("ts<=?", f.Until)
	}
	// Order: a single chain is read in chain order (verification needs it);
	// cross-chain queries are ordered by (ts, chain, seq), the page key.
	order := " ORDER BY ts, chain, seq"
	if f.Chain != "" && f.Page.After == nil {
		order = " ORDER BY chain, seq"
	}
	if k := f.Page.After; k != nil {
		where = append(where, "(ts > ? OR (ts = ? AND chain > ?) OR (ts = ? AND chain = ? AND seq > ?))")
		args = append(args, k.TS, k.TS, k.Chain, k.TS, k.Chain, k.Seq)
	}
	q := "SELECT chain, seq, ts, tenant_id, object_id, kind, trace_id, acting_for, prev_hash, hash, host_sig, raw FROM audit"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += order
	if lim := max(f.Limit, f.Page.Limit); lim > 0 {
		q += fmt.Sprintf(" LIMIT %d", lim)
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
