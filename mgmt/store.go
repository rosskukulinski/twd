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
	// UpdateTenant rewrites mint configuration and limits (not ID/tailnet).
	UpdateTenant(ctx context.Context, t Tenant) error
	ListTenants(ctx context.Context, pg Page) ([]Tenant, error)
	// DeleteTenant removes the tenant row (objects, deployments and leases
	// of the tenant must already be gone or are removed with it).
	DeleteTenant(ctx context.Context, id string) error

	UpsertHost(ctx context.Context, h Host) error
	GetHost(ctx context.Context, id string) (Host, error)
	ListHosts(ctx context.Context, pg Page) ([]Host, error)
	TouchHost(ctx context.Context, id string, liveNodes int, now time.Time) error
	DeleteHost(ctx context.Context, id string) error

	CreateDeployment(ctx context.Context, d Deployment) error
	GetDeployment(ctx context.Context, id string) (Deployment, error)
	// Assignments returns the latest deployment of every (tenant, workload)
	// whose namespace lease this host currently holds.
	Assignments(ctx context.Context, hostID string) ([]Deployment, error)
	// FindClass returns the latest deployment of the tenant's workload that
	// declares the DO class (namespace). With workload "", the class must be
	// declared by exactly one workload (else ErrAmbiguous).
	FindClass(ctx context.Context, tenantID, workload, class string) (Deployment, ClassConfig, error)
	// LatestDeployments returns the latest deployment of every workload of a tenant.
	LatestDeployments(ctx context.Context, tenantID string) ([]Deployment, error)
	// ListDeployments lists deployments (Latest: only the newest per
	// (tenant, workload)) ordered by (created_at, id).
	ListDeployments(ctx context.Context, f DeploymentFilter, pg Page) ([]Deployment, error)
	// StopDeployment marks a deployment stopped (undeploy).
	StopDeployment(ctx context.Context, id string, now time.Time) error

	// Object identity is (tenant, workload, namespace = class, name).
	CreateObject(ctx context.Context, o Object) error // ErrExists on (tenant, workload, namespace, name)
	GetObject(ctx context.Context, id string) (Object, error)
	GetObjectByName(ctx context.Context, tenantID, workload, namespace, name string) (Object, error)
	ListObjects(ctx context.Context, f ObjectFilter) ([]Object, error)
	// UpdateObject is a read-modify-write in one transaction. With a non-nil
	// fence, the lease must still be held at fence.Epoch (else ErrLeaseLost).
	UpdateObject(ctx context.Context, id string, fence *Fence, fn func(*Object) error) (Object, error)
	// ClaimMint takes (or refreshes) claimer's claim on minting a pending
	// object. It succeeds iff the object is pending without a key and is
	// unclaimed, claimed by claimer, or claimed before staleBefore (H02).
	ClaimMint(ctx context.Context, id, claimer string, now, staleBefore time.Time) (bool, error)
	// Durable device deletes (F10, H07): SetDeviceDelete records when a
	// killed object's device is due for deletion (unix ms); ClaimDeviceDelete
	// takes a due one (at most one replica wins); DueDeviceDeletes lists them.
	SetDeviceDelete(ctx context.Context, id string, due int64) error
	ClaimDeviceDelete(ctx context.Context, id string, now int64) (bool, error)
	DueDeviceDeletes(ctx context.Context, now int64) ([]string, error)
	// UpsertReplica records a mgmt replica (registry: F6/H11); ListReplicas
	// returns every recorded replica, live or not.
	UpsertReplica(ctx context.Context, r Replica) error
	ListReplicas(ctx context.Context) ([]Replica, error)
	// ReleaseMint drops claimer's claim (no-op if someone else holds it).
	ReleaseMint(ctx context.Context, id, claimer string) error
	// CountLive counts live objects of a tenant, or (rootID != "") the live
	// descendants of rootID.
	CountLive(ctx context.Context, tenantID, rootID string) (int, error)
	// DeleteDeadObjects removes a tenant's killed and failed objects (twd
	// down --purge), so their names and IDs can be used again. It returns
	// the removed objects; audit records are untouched.
	DeleteDeadObjects(ctx context.Context, tenantID string, statuses []string) ([]Object, error)

	AcquireLease(ctx context.Context, name, holder string, ttl time.Duration, now time.Time) (Lease, error)
	RenewLease(ctx context.Context, name, holder string, epoch int64, ttl time.Duration, now time.Time) (Lease, error)
	ReleaseLease(ctx context.Context, name, holder string, epoch int64) error
	GetLease(ctx context.Context, name string) (Lease, error)

	// AppendAudit appends to rec.Chain. If rec.Hash is empty the store
	// links (PrevHash = chain head) and hashes it; otherwise the record is
	// stored as submitted (host chains) and verified on read.
	AppendAudit(ctx context.Context, rec AuditRecord) (AuditRecord, error)
	ListAudit(ctx context.Context, f AuditFilter) ([]AuditRecord, error)
	// CountObjects counts objects per status (GET /v1/status).
	CountObjects(ctx context.Context) (map[string]int, error)
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
	Workload  string
	Namespace string
	ParentID  string
	RootID    string
	// StableNodeID selects the object whose node this is (node certs).
	StableNodeID string
	Status       []string
	HasParent    bool
	Page         Page
}

// DeploymentFilter selects deployments; empty fields match everything.
type DeploymentFilter struct {
	TenantID string
	Workload string
	HostID   string
	Latest   bool
}

// AuditFilter selects audit records; empty fields match everything.
type AuditFilter struct {
	Chain     string
	ObjectID  string
	ActingFor string
	TraceID   string
	TenantID  string
	Kinds     []string
	Since     string // RFC 3339; records with ts >= Since
	Until     string
	Limit     int // deprecated: use Page.Limit
	Page      Page
}
