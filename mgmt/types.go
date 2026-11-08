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
	NamespaceLeaseTTL             = 15 * time.Second
	HostLeaseTTL                  = 10 * time.Second
	KeyExpiry                     = 5 * time.Minute
)

// Tenant is one customer tailnet.
type Tenant struct {
	ID                     string `json:"tenantID"`
	Tailnet                string `json:"tailnet"`
	ControlURL             string `json:"controlURL,omitempty"`
	MintMode               string `json:"mintMode"`
	APIKeyFile             string `json:"apiKeyFile,omitempty"` // path only; the key is read at use and never stored
	WIFClientID            string `json:"wifClientID,omitempty"`
	WIFLifecycleClientID   string `json:"wifLifecycleClientID,omitempty"` // optional (RFC §4.4 credential 2)
	RegistrationsPerMinute int    `json:"registrationsPerMinute"`
	MaxObjects             int    `json:"maxObjects"`
	CreatedAt              int64  `json:"createdAt"`
}

// Host is a twd-hostd instance.
type Host struct {
	ID            string   `json:"hostID"`
	CapacityNodes int      `json:"capacityNodes"`
	Tiers         []string `json:"tiers"`
	NodeID        string   `json:"nodeID,omitempty"`      // operator-tailnet stable node ID (tsnet mode)
	AuditPubKey   string   `json:"auditPubKey,omitempty"` // base64 ed25519, verifies hostSig
	LiveNodes     int      `json:"liveNodes"`
	LastSeen      int64    `json:"lastSeen"` // unix ms
}

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
	CreatedAt  int64         `json:"createdAt"`
}

// LeaseName is the namespace lease key for a deployment. MVP: one lease per
// (tenant, workload) covers every DO class of the workload, so one epoch
// fences the whole deployment (the assignment carries a single epoch).
func LeaseName(tenantID, workload string) string { return tenantID + "/" + workload }

// Object is one DO instance / agent / sub-agent.
type Object struct {
	ID           string          `json:"objectID"`
	TenantID     string          `json:"tenantID"`
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
	KillReason   string          `json:"killReason,omitempty"`
	CreatedAt    int64           `json:"createdAt"`
	UpdatedAt    int64           `json:"updatedAt"`
	Children     []string        `json:"children,omitempty"` // filled by list endpoints
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
	Seq       int64           `json:"seq"`
	Chain     string          `json:"chain"` // "mgmt" or "host:<id>"
	TS        string          `json:"ts"`
	TenantID  string          `json:"tenantID,omitempty"`
	ObjectID  string          `json:"objectID,omitempty"`
	Kind      string          `json:"kind"`
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
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, a...),
		DocURL: "https://twd.dev/errors/" + code}
}

// Store errors.
var (
	ErrNotFound  = errors.New("not found")
	ErrExists    = errors.New("already exists")
	ErrLeaseHeld = errors.New("lease held by another holder")
	ErrLeaseLost = errors.New("lease_lost")
	ErrConflict  = errors.New("conflict")
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
		return errf(http.StatusConflict, "lease_lost", "the caller does not hold the namespace lease at this epoch")
	case errors.Is(err, ErrLeaseHeld):
		return errf(http.StatusConflict, "lease_held", "%v", err)
	}
	return errf(http.StatusInternalServerError, "internal", "%v", err)
}
