package mgmt

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Mint modes.
const (
	MintAPIKey = "apikey"
	MintWIF    = "wif"
	// MintOAuth (M12): a Tailscale OAuth client whose secret stays in a
	// 0600 file on the mgmt host; the default for self-hosted installs.
	MintOAuth = "oauth"
)

// Object statuses.
const (
	StatusPending = "pending" // recorded, waiting on the mint queue/throttle
	StatusMinted  = "minted"  // a credential is in custody, waiting for the lease holder
	StatusReady   = "ready"   // node joined (hostd reported ready)
	StatusKilled  = "killed"  // killed (by operator, cascade, or reaper); terminal
	StatusFailed  = "failed"  // minting failed permanently
)

// Defaults (RFC §4.6, §4.9, ADR-017).
const (
	DefaultRegistrationsPerMinute = 200
	DefaultMaxObjects             = 1000
	DefaultMaxLive                = 64
	DefaultMaxDepth               = 3
	NamespaceLeaseTTL             = 45 * time.Second // > hostd's FenceAfter (30 s) + margin (ADR-028 §3)
	HostLeaseTTL                  = 10 * time.Second
	KeyExpiry                     = 5 * time.Minute
)

// Tenant is one customer tailnet.
type Tenant struct {
	ID                   string `json:"tenantID"`
	Tailnet              string `json:"tailnet"`
	ControlURL           string `json:"controlURL,omitempty"`
	MintMode             string `json:"mintMode"`
	APIKeyFile           string `json:"apiKeyFile,omitempty"` // path only; the key is read at use and never stored
	WIFClientID          string `json:"wifClientID,omitempty"`
	WIFLifecycleClientID string `json:"wifLifecycleClientID,omitempty"` // optional (RFC §4.4 credential 2)
	// OAuthClientID is the OAuth client's public ID; OAuthClientFile the
	// absolute path of its 0600 credential file (the secret is read at
	// use and never stored).
	OAuthClientID          string `json:"oauthClientID,omitempty"`
	OAuthClientFile        string `json:"oauthClientFile,omitempty"`
	RegistrationsPerMinute int    `json:"registrationsPerMinute"`
	MaxObjects             int    `json:"maxObjects"`
	// Pool places the tenant's deployments on hosts with the same pool
	// ("" = any host). Tier is the default isolation tier.
	Pool      string `json:"pool,omitempty"`
	Tier      string `json:"tier,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// Host is a twd-hostd instance.
type Host struct {
	ID            string   `json:"hostID"`
	CapacityNodes int      `json:"capacityNodes"`
	Tiers         []string `json:"tiers"`
	NodeID        string   `json:"nodeID,omitempty"`      // operator-tailnet stable node ID (tsnet mode)
	AuditPubKey   string   `json:"auditPubKey,omitempty"` // base64 ed25519, verifies hostSig
	Pool          string   `json:"pool,omitempty"`
	// PushPort is hostd's push listener on its own tsnet node (0 = none;
	// local-dev hosts register a unix:// push URL instead).
	PushPort int `json:"pushPort,omitempty"`
	// PushTarget is where mgmt pushes kills and assignments: unix:///… or
	// http://<node addr>:<port>. Persisted so every replica can push, not
	// only the one that served the registration (mm-loop F8, H06).
	PushTarget string `json:"-"`
	LiveNodes  int    `json:"liveNodes"`
	LastSeen   int64  `json:"lastSeen"` // unix ms
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  int64  `json:"updatedAt"`
}

// Pending reports a host created by `twd host add` that never registered.
func (h Host) Pending() bool { return h.LastSeen == 0 }

// Live reports whether the host heartbeated within the host lease TTL.
func (h Host) Live(now time.Time) bool {
	return now.UnixMilli()-h.LastSeen <= HostLeaseTTL.Milliseconds()
}

// ClassConfig is one DO namespace from twd.jsonc tailnet.objects.
type ClassConfig struct {
	Class string   `json:"class"`
	Tags  []string `json:"tags"`
	Agent bool     `json:"agent,omitempty"`
	TTL   string   `json:"ttl,omitempty"`
}

// Deployment is one deploy of a workload.
type Deployment struct {
	ID         string        `json:"deploymentID"`
	TenantID   string        `json:"tenantID"`
	Workload   string        `json:"workload"`
	HostID     string        `json:"host"`
	Namespaces []ClassConfig `json:"namespaces"`
	MaxLive    int           `json:"maxLive"`
	MaxDepth   int           `json:"maxDepth"`
	Epoch      int64         `json:"epoch"`
	Bundle     []byte        `json:"-"`
	// DeployedBy is who ran the deploy: the WhoIs login of a tailnet
	// caller (`twd link`), or the local actor on the unix socket.
	DeployedBy string `json:"deployedBy,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	// StoppedAt is set by undeploy (DELETE /v1/deployments/{id}).
	StoppedAt int64 `json:"stoppedAt,omitempty"`
	// Fence: the workload has Durable Objects that don't tolerate two
	// instances (BundleStateful), so its host stops serving it when it loses
	// mgmt (ADR-028 §3). Stateless workloads keep serving.
	Fence bool `json:"fence"`
	// HostStatus/HostError: what the host (or mgmt, for assigned, draining
	// and stopped) last reported; shared by every replica (F13, H08).
	HostStatus string `json:"-"`
	HostError  string `json:"-"`
}

// LeaseName is the namespace lease key for a deployment. MVP: one lease per
// (tenant, workload) covers every DO class of the workload, so one epoch
// fences the whole deployment (the assignment carries a single epoch).
func LeaseName(tenantID, workload string) string { return tenantID + "/" + workload }

// Object is one DO instance / agent / sub-agent.
type Object struct {
	ID           string          `json:"objectID"`
	TenantID     string          `json:"tenantID"`
	Workload     string          `json:"workload"`
	Namespace    string          `json:"namespace"`
	Name         string          `json:"objectName"`
	Hostname     string          `json:"hostname"`
	DeploymentID string          `json:"deploymentID"`
	Lease        string          `json:"lease"`
	Tags         []string        `json:"tags"`
	Caps         json.RawMessage `json:"caps,omitempty"`
	ParentID     string          `json:"parent,omitempty"`
	RootID       string          `json:"root"`
	Depth        int             `json:"depth"`
	TTLSeconds   int64           `json:"ttl,omitempty"`
	Ephemeral    bool            `json:"ephemeral"`
	Status       string          `json:"status"`
	NodeID       string          `json:"nodeID,omitempty"`
	StableNodeID string          `json:"stableNodeID,omitempty"`
	MagicDNS     string          `json:"magicDNS,omitempty"`
	KeyID        string          `json:"-"`
	KeyExpires   int64           `json:"-"`
	// DeviceDeleteAt: 0 none; >0 the device delete is due (unix ms, or a
	// claim's lease end); -1 done. Set in the same write that kills the
	// object, so the kill and its device cleanup commit atomically (F21).
	DeviceDeleteAt int64  `json:"-"`
	KillReason     string `json:"killReason,omitempty"`
	// Kill record (the /v1 tombstone): when, by whom, whether as part of a
	// cascade and which object the kill was issued on.
	KilledAt    int64  `json:"killedAt,omitempty"`
	KilledBy    string `json:"killedBy,omitempty"`
	KillCascade bool   `json:"killCascade,omitempty"`
	KillRoot    string `json:"killRoot,omitempty"`
	// ActingFor is the principal the object was created for (audit head).
	ActingFor string `json:"actingFor,omitempty"`
	// Cert is the node certificate (internal/nodecert) signed at ready with
	// the tenant's OBO root key; it binds Caps (lim) to StableNodeID.
	Cert      string   `json:"cert,omitempty"`
	CreatedAt int64    `json:"createdAt"`
	UpdatedAt int64    `json:"updatedAt"`
	Children  []string `json:"children,omitempty"` // filled by list endpoints
}

// Live reports whether the object is not terminal.
func (o Object) Live() bool { return o.Status != StatusKilled && o.Status != StatusFailed }

// Lease is a namespace lease.
type Lease struct {
	Name      string `json:"namespace"`
	Holder    string `json:"holder"`
	Epoch     int64  `json:"epoch"`
	ExpiresAt int64  `json:"expiresAt"`
}

// AuditRecord is one hash-chained audit entry. Fields are those of the
// contract's POST /v0/audit plus the chain/seq the server assigns.
type AuditRecord struct {
	Seq      int64  `json:"seq"`
	Chain    string `json:"chain"` // "mgmt" or "host:<id>"
	TS       string `json:"ts"`
	TenantID string `json:"tenantID,omitempty"`
	ObjectID string `json:"objectID,omitempty"`
	Kind     string `json:"kind"`
	// Source is who produced the record's content: "mgmt" (mgmt chain),
	// "host" (netd/hostd security records) or "tenant" (a tenant_note
	// submitted by tenant code via ctx.tailnet.audit; its content is
	// unverified). Host records carry it in the hashed body; mgmt fills it
	// on read for older records.
	Source    string          `json:"source,omitempty"`
	Dst       string          `json:"dst,omitempty"`
	Grant     string          `json:"grant,omitempty"`
	Code      string          `json:"code,omitempty"`
	TraceID   string          `json:"traceID,omitempty"`
	ActingFor string          `json:"actingFor,omitempty"`
	Detail    json.RawMessage `json:"detail,omitempty"`
	PrevHash  string          `json:"prevHash"`
	Hash      string          `json:"hash"`
	HostSig   string          `json:"hostSig,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"` // host records: the record exactly as submitted (hash input)
}

// Error is a typed API error (DX.md catalog shape).
type Error struct {
	Status  int            `json:"-"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Missing map[string]any `json:"missing,omitempty"`
	Snippet string         `json:"snippet,omitempty"`
	DocURL  string         `json:"doc_url,omitempty"`
	TraceID string         `json:"trace_id"`
	// RetryAfter (seconds), when set, is sent as the Retry-After header.
	RetryAfter int `json:"-"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, a...),
		DocURL: "https://twd.dev/errors/" + code}
}

// Store errors.
var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	// ErrSpawnBudget: the root already has its maximum of live descendants
	// (checked in the insert transaction: mm-loop F14, H04).
	ErrSpawnBudget = errors.New("spawn budget exceeded")
	// ErrQuota: the tenant already has maxObjects live objects (checked in
	// the insert transaction: mm-loop F17, H16).
	ErrQuota     = errors.New("object quota exceeded")
	ErrLeaseHeld = errors.New("lease held by another holder")
	ErrLeaseLost = errors.New("lease_lost")
	ErrConflict  = errors.New("conflict")
	ErrAmbiguous = errors.New("ambiguous")
)

// asAPIError maps store errors to API errors.
func asAPIError(err error) *Error {
	var e *Error
	switch {
	case errors.As(err, &e):
		return e
	case errors.Is(err, ErrNotFound):
		return errf(http.StatusNotFound, "not_found", "%v", err)
	case errors.Is(err, ErrExists):
		return errf(http.StatusConflict, "exists", "%v", err)
	case errors.Is(err, ErrLeaseLost):
		return errf(http.StatusConflict, "lease_lost", "the caller does not hold the namespace lease at this epoch (%v)", err)
	case errors.Is(err, ErrLeaseHeld):
		return errf(http.StatusConflict, "lease_held", "%v", err)
	case isStoreUnavailable(err):
		e := errf(http.StatusServiceUnavailable, "store_unavailable", "the store is unavailable or contended; retry (%v)", err)
		e.RetryAfter = 1
		return e
	}
	return errf(http.StatusInternalServerError, "internal", "%v", err)
}
