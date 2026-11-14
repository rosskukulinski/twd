// Package mgmtapi is twd-hostd's (and the twd CLI's) client for the mgmt
// /v1 API. The wire is the generated client in hostd/mgmtapi/v1 (from
// api/openapi.yaml, the contract's source of truth); the hostd-facing
// types here are the subset hostd works with, converted at the edge.
package mgmtapi

import (
	"encoding/json"

	"github.com/rosskukulinski/twd/internal/publicspec"
)

// Error is every non-2xx body (DX.md catalog shape).
type Error struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Missing json.RawMessage `json:"missing,omitempty"`
	Snippet string          `json:"snippet,omitempty"`
	DocURL  string          `json:"doc_url,omitempty"`
	TraceID string          `json:"trace_id,omitempty"`
	Status  int             `json:"-"`
}

func (e *Error) Error() string { return "mgmt: " + e.Code + ": " + e.Message }

// Error codes used by hostd.
const (
	CodeLeaseLost       = "lease_lost"
	CodeIdentityPending = "identity_pending"
	CodeNotFound        = "not_found"
	CodeKilled          = "killed"
)

type Tenant struct {
	TenantID               string `json:"tenantID,omitempty"`
	Tailnet                string `json:"tailnet"`
	ControlURL             string `json:"controlURL,omitempty"`
	MintMode               string `json:"mintMode"` // "apikey" | "wif" | "static" (v0.1, tests only)
	APIKeyFile             string `json:"apiKeyFile,omitempty"`
	OAuthClientFile        string `json:"oauthClientFile,omitempty"` // oauth mode: the 0600 client file (path only)
	WIFClientID            string `json:"wifClientID,omitempty"`
	RegistrationsPerMinute int    `json:"registrationsPerMinute,omitempty"`
	MaxObjects             int    `json:"maxObjects,omitempty"`
	// Tier (v0.1, optional) is the tenant's default isolation tier
	// ("process" | "microvm"); a deployment's twd.jsonc "tier" overrides it.
	Tier string `json:"tier,omitempty"`
	// OBOPublicKey (RT-01) is the base64 Ed25519 key that verifies this
	// tenant's node certificates. hostd pins it when it opens the tenant.
	OBOPublicKey string `json:"oboPublicKey,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
}

// NodeCert is GET /v1/certificates/{stableNodeID}?tenant= (RT-01): the sender's
// node certificate, signed by the tenant's OBO root key, and its payload.
type NodeCert struct {
	TenantID     string           `json:"tenantID"`
	ObjectID     string           `json:"objectID"`
	StableNodeID string           `json:"stableNodeID"`
	Status       string           `json:"status"`
	Cert         string           `json:"cert"`
	Payload      *NodeCertPayload `json:"payload,omitempty"`
	OBOPublicKey string           `json:"oboPublicKey,omitempty"`
}

// NodeCertPayload mirrors internal/nodecert.Payload.
type NodeCertPayload struct {
	V            int             `json:"v"`
	TenantID     string          `json:"tenant"`
	ObjectID     string          `json:"objectID"`
	StableNodeID string          `json:"stableNodeID"`
	Tags         []string        `json:"tags"`
	Lim          json.RawMessage `json:"lim,omitempty"`
	ParentID     string          `json:"parent,omitempty"`
	RootID       string          `json:"root"`
	IAT          int64           `json:"iat"`
	EXP          int64           `json:"exp"`
}

type RegisterRequest struct {
	HostID        string   `json:"hostID"`
	CapacityNodes int      `json:"capacityNodes"`
	Tiers         []string `json:"tiers"`
	// PushURL is where mgmt pushes kills and assignment changes on the
	// local-dev socket ("unix:///path/hostd.sock"); see hostd's push API.
	PushURL string `json:"pushURL,omitempty"`
	// PushPort is hostd's push listener on its own tsnet node; mgmt pushes
	// to the WhoIs node's address at this port (tsnet transport).
	PushPort int `json:"pushPort,omitempty"`
	// AuditPubKey: base64 ed25519 key for hostSig.
	AuditPubKey string `json:"auditPubKey,omitempty"`
	Pool        string `json:"pool,omitempty"`
}

type OK struct {
	OK bool `json:"ok"`
}

type Lease struct {
	Namespace string `json:"namespace"`
	Epoch     int64  `json:"epoch"`
}

type DeploymentStatus struct {
	DeploymentID string `json:"deploymentID"`
	Status       string `json:"status"` // "starting" | "active" | "draining" | "stopped" | "failed"
	Error        string `json:"error,omitempty"`
}

type HeartbeatRequest struct {
	LiveNodes int     `json:"liveNodes"`
	Leases    []Lease `json:"leases"`
	// Deployments (v0.1, optional) reports per-deployment status.
	Deployments []DeploymentStatus `json:"deployments,omitempty"`
}

type KillOrder struct {
	ObjectID string `json:"objectID"`
	Cascade  bool   `json:"cascade,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// HeartbeatResponse is PUT /v1/hosts/{id}/status's reply.
type HeartbeatResponse struct {
	Revoke []string `json:"revoke"`
	// RevokeObjects lists objects killed in the last 10 min in namespaces
	// this host holds; they repeat, so kills must be idempotent. This is
	// mgmt's kill delivery fallback (the push is the fast path).
	RevokeObjects []string `json:"revokeObjects"`
	// AssignmentsChanged hints that a push could not be delivered.
	AssignmentsChanged bool `json:"assignmentsChanged,omitempty"`
}

type DeployRequest struct {
	TenantID string `json:"tenantID"`
	Workload string `json:"workload"`
	Bundle   []byte `json:"bundle"` // base64 in JSON
}

type Endpoint struct {
	ObjectID string `json:"objectID,omitempty"`
	Node     string `json:"node"`
	MagicDNS string `json:"magicDNS"`
	Status   string `json:"status"`
}

// DeployResponse is the Deployment resource (POST /v1/deployments 202,
// GET /v1/deployments/{id}).
type DeployResponse struct {
	DeploymentID string          `json:"deploymentID"`
	TenantID     string          `json:"tenantID,omitempty"`
	Workload     string          `json:"workload,omitempty"`
	Host         string          `json:"host"`
	Epoch        int64           `json:"epoch"`
	Endpoint     Endpoint        `json:"endpoint"`
	Status       string          `json:"status,omitempty"`
	Error        string          `json:"error,omitempty"`
	Namespaces   []NamespaceSpec `json:"namespaces"`
	DeployedBy   string          `json:"deployedBy,omitempty"`
	CreatedAt    int64           `json:"createdAt"`
}

type NamespaceSpec struct {
	Class string   `json:"class"`
	Tags  []string `json:"tags"`
	Agent bool     `json:"agent,omitempty"`
}

type Assignment struct {
	DeploymentID string          `json:"deploymentID"`
	TenantID     string          `json:"tenantID"`
	Workload     string          `json:"workload"`
	BundleURL    string          `json:"bundleURL"`
	Namespaces   []NamespaceSpec `json:"namespaces"`
	Epoch        int64           `json:"epoch"`
	// WorkloadTags (v0.1, optional) are the workload entry node's tags.
	WorkloadTags []string `json:"workloadTags,omitempty"`
	Lease        string   `json:"lease"`
	Tailnet      string   `json:"tailnet"`
	ControlURL   string   `json:"controlURL,omitempty"`
	CreatedAt    int64    `json:"createdAt"`
	// Fence (F9, ADR-028 §3): stop serving this workload when mgmt is
	// unreachable for FenceAfter. nil (an older mgmt) means true.
	Fence *bool `json:"fence,omitempty"`
	// Public (ADR-029) is the workload's public ingress. nil: tailnet-only
	// (no twd.jsonc "public", or the tenant's publicIngress is not allow).
	Public *PublicAssignment `json:"public,omitempty"`
}

// PublicAssignment is a workload's public ingress as mgmt assigns it
// (ADR-029). hostd runs one extra node for it (namespace "_public", object
// name = the workload, tags Tags, hostname = the workload name) whose netd
// serves the Funnel listener.
type PublicAssignment struct {
	Routes  []string          `json:"routes"`
	Upgrade []string          `json:"upgrade,omitempty"`
	Limits  publicspec.Limits `json:"limits"` // effective (defaults filled)
	Tags    []string          `json:"tags"`
	// Exposed is false after `twd expose off`: the public node stays
	// joined (its certificate stays warm) but its Funnel listener is
	// closed and its flows reset. `twd expose on` reopens it.
	Exposed bool `json:"exposed"`
}

type ObjectRef struct {
	ObjectID string `json:"objectID"`
}

type CreateObjectRequest struct {
	TenantID string `json:"tenantID"`
	// Workload (M7) scopes the identity: (tenant, workload, namespace,
	// objectName). hostd always sends it.
	Workload   string          `json:"workload,omitempty"`
	Namespace  string          `json:"namespace"`
	ObjectName string          `json:"objectName"`
	Parent     *ObjectRef      `json:"parent,omitempty"`
	Caps       json.RawMessage `json:"caps,omitempty"`
	TTL        string          `json:"ttl,omitempty"`
	// Hostname (v0.1, optional) is the hostname hostd will join with.
	Hostname string `json:"hostname,omitempty"`
	// v0 clarifications: feed audit.
	ActingFor string `json:"actingFor,omitempty"`
	TraceID   string `json:"traceID,omitempty"`
}

// CreateObjectResponse is the part of the Object resource hostd uses
// after POST /v1/objects (202 new, 200 existing).
type CreateObjectResponse struct {
	ObjectID string   `json:"objectID"`
	Status   string   `json:"status"`
	Tags     []string `json:"tags"`
	Hostname string   `json:"hostname,omitempty"`
}

// Credential is POST /v1/objects/{id}/credentials (201): a single-use key.
type Credential struct {
	AuthKey    string   `json:"authKey"`
	ExpiresAt  int64    `json:"expiresAt"`
	Tags       []string `json:"tags"`
	Hostname   string   `json:"hostname"`
	Ephemeral  bool     `json:"ephemeral"`
	ControlURL string   `json:"controlURL,omitempty"`
	Reminted   bool     `json:"reminted,omitempty"`
}

// String never prints the key.
func (c Credential) String() string { return "mgmtapi.Credential{redacted}" }

// VolumeKey is POST /v1/volumes/{tenant}:{workload}/keys (201): a
// workload's DO volume data key grant.
type VolumeKey struct {
	VolumeID string `json:"volumeID,omitempty"`
	Epoch    int64  `json:"epoch,omitempty"`
	Key      []byte `json:"key"`
}

// String never prints the key.
func (VolumeKey) String() string { return "mgmtapi.VolumeKey{redacted}" }

// RemintRequest asks for a re-mint (POST /v1/objects/{id}/credentials
// ?reason=rejoin_rejected): the saved node state was rejected by control;
// mgmt deletes the old device, clears the certificate and mints again
// under the same hostname.
type RemintRequest struct {
	Epoch  int64  `json:"epoch,omitempty"`
	Reason string `json:"reason"`
}

type ReadyRequest struct {
	NodeID       string `json:"nodeID"`
	StableNodeID string `json:"stableNodeID"`
	MagicDNS     string `json:"magicDNS"`
	Epoch        int64  `json:"epoch,omitempty"`
}

type KillRequest struct {
	Cascade bool   `json:"cascade"`
	Reason  string `json:"reason"`
}

// KillResponse is DELETE /v1/objects/{id} (202 KillResult).
type KillResponse struct {
	Killed          []string          `json:"killed"`
	Pushed          []string          `json:"pushed,omitempty"`
	DevicesDeleted  []string          `json:"devicesDeleted"`
	DevicesDeleting []string          `json:"devicesDeleting,omitempty"`
	Errors          map[string]string `json:"errors,omitempty"`
}

// Object is the /v1 Object resource (the fields hostd and the fakes use).
type Object struct {
	ObjectID     string          `json:"objectID"`
	TenantID     string          `json:"tenantID"`
	Workload     string          `json:"workload"`
	Namespace    string          `json:"namespace"`
	ObjectName   string          `json:"objectName"`
	Hostname     string          `json:"hostname"`
	DeploymentID string          `json:"deploymentID"`
	Lease        string          `json:"lease"`
	Parent       string          `json:"parent,omitempty"`
	Root         string          `json:"root"`
	Depth        int             `json:"depth"`
	Ephemeral    bool            `json:"ephemeral"`
	Status       string          `json:"status"` // pending | minted | ready | killed | failed
	Tags         []string        `json:"tags"`
	Caps         json.RawMessage `json:"caps,omitempty"`
	NodeID       string          `json:"nodeID,omitempty"`
	StableNodeID string          `json:"stableNodeID,omitempty"`
	MagicDNS     string          `json:"magicDNS,omitempty"`
	Host         string          `json:"host,omitempty"`
	Lineage      []string        `json:"lineage"` // root first, self last
	Children     []string        `json:"children,omitempty"`
	KillReason   string          `json:"killReason,omitempty"`
	CreatedAt    int64           `json:"createdAt"`
	UpdatedAt    int64           `json:"updatedAt"`
}

type AuditRecord struct {
	TS       string `json:"ts"`
	ObjectID string `json:"objectID"`
	Kind     string `json:"kind"` // allow | deny | spawn | kill | egress | principal | tenant_note
	// Source (RT-02) is the record's provenance and is part of the hashed
	// body: "host" for netd/hostd security records, "tenant" for a
	// tenant_note submitted by tenant code (unverified content). mgmt
	// rejects a "tenant" source with any other kind.
	Source    string `json:"source,omitempty"`
	Dst       string `json:"dst,omitempty"`
	Grant     string `json:"grant,omitempty"`
	Code      string `json:"code,omitempty"`
	TraceID   string `json:"traceID"`
	ActingFor string `json:"actingFor,omitempty"`
	TenantID  string `json:"tenantID,omitempty"`
	// Detail is a JSON object (the contract's AuditRecord.detail).
	Detail   json.RawMessage `json:"detail,omitempty"`
	PrevHash string          `json:"prevHash"`
	Hash     string          `json:"hash"`
	HostSig  string          `json:"hostSig,omitempty"`
	// Chain and Seq are assigned by mgmt (read side).
	Chain string `json:"chain,omitempty"`
	Seq   int64  `json:"seq,omitempty"`
}

// Note wraps free text as an audit detail object.
func Note(s string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"note": s})
	return b
}

type AuditBatch struct {
	Records []AuditRecord `json:"records"`
}

// ChainStatus is one chain's verification.
type ChainStatus struct {
	Chain    string `json:"chain"`
	OK       bool   `json:"ok"`
	Records  int    `json:"records"`
	BrokenAt int64  `json:"brokenAt,omitempty"`
	Error    string `json:"error,omitempty"`
}

// AuditResponse is one page of GET /v1/audit.
type AuditResponse struct {
	Items      []AuditRecord `json:"items"`
	NextCursor *string       `json:"nextCursor"`
	Chains     []ChainStatus `json:"chains"`
	Verified   bool          `json:"verified"`
}

// Page is the generic /v1 list envelope.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// WorkloadNamespace is the object namespace of a workload's entry node
// (contract gap, see STATUS.md M3: mgmt must declare it like a class, with
// the twd.jsonc tailnet.tags).
const WorkloadNamespace = "_workload"

// PublicNamespace is the object namespace of a workload's public node
// (ADR-029); the object name is the workload name.
const PublicNamespace = publicspec.Namespace

// Namespace returns the object namespace for POST /v1/objects: the DO
// class name, or WorkloadNamespace for the workload entry node (class "").
func Namespace(workload, class string) string {
	if class == "" {
		return WorkloadNamespace
	}
	return class
}

// LeaseName is the namespace lease of a workload: "<tenantID>/<workload>".
func LeaseName(tenantID, workload string) string { return tenantID + "/" + workload }
