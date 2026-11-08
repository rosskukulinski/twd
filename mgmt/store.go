package mgmt

import (
	"context"
	"time"
)

// Store is twd-mgmt's durable state (ADR-011: leases are conditional writes
// on a durable store; mgmt replicas are stateless). Backends: SQLite
// (homelab / `twd up`) and Postgres.
//
// Fencing: every lease-holder commit carries (lease, holder, epoch); a stale
// epoch fails with ErrLeaseLost inside the same transaction as the write.
type Store interface {
	CreateTenant(ctx context.Context, t Tenant) error
	GetTenant(ctx context.Context, id string) (Tenant, error)
	ListTenants(ctx context.Context) ([]Tenant, error)

	UpsertHost(ctx context.Context, h Host) error
	GetHost(ctx context.Context, id string) (Host, error)
	ListHosts(ctx context.Context) ([]Host, error)
	TouchHost(ctx context.Context, id string, liveNodes int, now time.Time) error

	CreateDeployment(ctx context.Context, d Deployment) error
	GetDeployment(ctx context.Context, id string) (Deployment, error)
	// Assignments returns the latest deployment of every (tenant, workload)
	// whose namespace lease this host currently holds.
	Assignments(ctx context.Context, hostID string) ([]Deployment, error)
	// FindClass returns the latest deployment of tenantID that declares the
	// DO class (namespace).
	FindClass(ctx context.Context, tenantID, class string) (Deployment, ClassConfig, error)

	CreateObject(ctx context.Context, o Object) error // ErrExists on (tenant, namespace, name)
	GetObject(ctx context.Context, id string) (Object, error)
	GetObjectByName(ctx context.Context, tenantID, namespace, name string) (Object, error)
	ListObjects(ctx context.Context, f ObjectFilter) ([]Object, error)
	// UpdateObject is a read-modify-write in one transaction. With a non-nil
	// fence, the lease must still be held at fence.Epoch (else ErrLeaseLost).
	UpdateObject(ctx context.Context, id string, fence *Fence, fn func(*Object) error) (Object, error)
	// CountLive counts live objects of a tenant, or (rootID != "") the live
	// descendants of rootID.
	CountLive(ctx context.Context, tenantID, rootID string) (int, error)

	AcquireLease(ctx context.Context, name, holder string, ttl time.Duration, now time.Time) (Lease, error)
	RenewLease(ctx context.Context, name, holder string, epoch int64, ttl time.Duration, now time.Time) (Lease, error)
	ReleaseLease(ctx context.Context, name, holder string, epoch int64) error
	GetLease(ctx context.Context, name string) (Lease, error)

	// AppendAudit appends to rec.Chain. If rec.Hash is empty the store
	// links (PrevHash = chain head) and hashes it; otherwise the record is
	// stored as submitted (host chains) and verified on read.
	AppendAudit(ctx context.Context, rec AuditRecord) (AuditRecord, error)
	ListAudit(ctx context.Context, f AuditFilter) ([]AuditRecord, error)
	AuditChains(ctx context.Context) ([]string, error)
	AuditHead(ctx context.Context, chain string) (seq int64, hash string, err error)

	AddRevocation(ctx context.Context, hostID, objectID string, now time.Time) error
	Revocations(ctx context.Context, hostID string, since time.Time) ([]string, error)

	Close() error
}

// Fence is the lease a commit is conditional on. An empty Holder matches any
// holder at that epoch (the epoch alone is the fencing token).
type Fence struct {
	Lease  string
	Holder string
	Epoch  int64
}

// ObjectFilter selects objects; empty fields match everything.
type ObjectFilter struct {
	TenantID  string
	Namespace string
	ParentID  string
	RootID    string
	Status    []string
	HasParent bool
}

// AuditFilter selects audit records; empty fields match everything.
type AuditFilter struct {
	Chain     string
	ObjectID  string
	ActingFor string
	TraceID   string
	TenantID  string
	Limit     int
}
