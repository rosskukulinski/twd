// Package mgmtapi holds the wire types of the mgmt ⇄ hostd contract
// (docs/contracts/mgmt-api.md, v0 plus the backward-compatible v0.1
// additions marked "v0.1"), and a small client used by twd-hostd.
package mgmtapi

import "encoding/json"

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
	WIFClientID            string `json:"wifClientID,omitempty"`
	RegistrationsPerMinute int    `json:"registrationsPerMinute,omitempty"`
	MaxObjects             int    `json:"maxObjects,omitempty"`
	// Tier (v0.1, optional) is the tenant's default isolation tier
	// ("process" | "microvm"); a deployment's twd.jsonc "tier" overrides it.
	Tier string `json:"tier,omitempty"`
	// OBOPublicKey (RT-01) is the base64 Ed25519 key that verifies this
	// tenant's node certificates. hostd pins it when it opens the tenant.
	OBOPublicKey string `json:"oboPublicKey,omitempty"`
}

// NodeCert is GET /v0/certs/{stableNodeID}?tenant= (RT-01): the sender's
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
	// PushURL (v0.1, optional) is where mgmt may push kills and assignment
	// changes ("unix:///path/hostd.sock" or "http://..."); see hostd push API.
	PushURL string `json:"pushURL,omitempty"`
	// AuditPubKey (v0 clarification): base64 ed25519 key for hostSig.
	AuditPubKey string `json:"auditPubKey,omitempty"`
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

type HeartbeatResponse struct {
	OK     bool     `json:"ok"`
	Revoke []string `json:"revoke"`
	// Kill (v0.1, optional) lists objects mgmt killed since the last
	// heartbeat (the fallback when a push could not be delivered).
	Kill []KillOrder `json:"kill,omitempty"`
	// RevokeObjects (v0 clarification) lists objects killed in the last
	// 10 min in namespaces this host holds; they repeat, so kills must be
	// idempotent. This is mgmt's kill delivery path.
	RevokeObjects []string `json:"revokeObjects,omitempty"`
}

type DeployRequest struct {
	TenantID string `json:"tenantID"`
	Workload string `json:"workload"`
	Bundle   []byte `json:"bundle"` // base64 in JSON
}

type Endpoint struct {
	Node     string `json:"node"`
	MagicDNS string `json:"magicDNS"`
}

type DeployResponse struct {
	DeploymentID string   `json:"deploymentID"`
	Host         string   `json:"host"`
	Endpoint     Endpoint `json:"endpoint"`
	// v0.1 (GET /v0/deployments/{id})
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
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
	// v0 clarifications (M2): the lease name, tailnet and control URL.
	Lease      string `json:"lease,omitempty"`
	Tailnet    string `json:"tailnet,omitempty"`
	ControlURL string `json:"controlURL,omitempty"`
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

type CreateObjectResponse struct {
	ObjectID string   `json:"objectID"`
	Status   string   `json:"status"`
	Tags     []string `json:"tags"`
	Hostname string   `json:"hostname,omitempty"` // v0 clarification
}

type Credential struct {
	AuthKey string `json:"authKey"`
}

// String never prints the key.
func (c Credential) String() string { return "mgmtapi.Credential{redacted}" }

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

type KillResponse struct {
	Killed []string `json:"killed"`
}

type Object struct {
	ObjectID   string          `json:"objectID"`
	TenantID   string          `json:"tenantID"`
	Workload   string          `json:"workload,omitempty"`
	Namespace  string          `json:"namespace"`
	ObjectName string          `json:"objectName"`
	Parent     string          `json:"parent,omitempty"`
	Status     string          `json:"status"` // pending | ready | killed
	Tags       []string        `json:"tags"`
	Caps       json.RawMessage `json:"caps,omitempty"`
	NodeID     string          `json:"nodeID,omitempty"`
	MagicDNS   string          `json:"magicDNS,omitempty"`
	Host       string          `json:"host,omitempty"`
	Lineage    []string        `json:"lineage,omitempty"` // root first
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
	Detail    string `json:"detail,omitempty"` // v0.1, optional
	PrevHash  string `json:"prevHash"`
	Hash      string `json:"hash"`
	HostSig   string `json:"hostSig"`
}

type AuditBatch struct {
	Records []AuditRecord `json:"records"`
}

type AuditResponse struct {
	Records  []AuditRecord `json:"records"`
	Verified bool          `json:"verified"`
	Problems []string      `json:"problems,omitempty"`
}

// WorkloadNamespace is the object namespace of a workload's entry node
// (contract gap, see STATUS.md M3: mgmt must declare it like a class, with
// the twd.jsonc tailnet.tags).
const WorkloadNamespace = "_workload"

// Namespace returns the object namespace for POST /v0/objects: the DO
// class name, or WorkloadNamespace for the workload entry node (class "").
func Namespace(workload, class string) string {
	if class == "" {
		return WorkloadNamespace
	}
	return class
}

// LeaseName is the namespace lease of a workload: "<tenantID>/<workload>".
func LeaseName(tenantID, workload string) string { return tenantID + "/" + workload }
