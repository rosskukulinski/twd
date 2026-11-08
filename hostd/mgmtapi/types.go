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
}

type RegisterRequest struct {
	HostID        string   `json:"hostID"`
	CapacityNodes int      `json:"capacityNodes"`
	Tiers         []string `json:"tiers"`
	// PushURL (v0.1, optional) is where mgmt may push kills and assignment
	// changes ("unix:///path/hostd.sock" or "http://..."); see hostd push API.
	PushURL string `json:"pushURL,omitempty"`
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
}

type ObjectRef struct {
	ObjectID string `json:"objectID"`
}

type CreateObjectRequest struct {
	TenantID   string          `json:"tenantID"`
	Namespace  string          `json:"namespace"`
	ObjectName string          `json:"objectName"`
	Parent     *ObjectRef      `json:"parent,omitempty"`
	Caps       json.RawMessage `json:"caps,omitempty"`
	TTL        string          `json:"ttl,omitempty"`
	// Hostname (v0.1, optional) is the hostname hostd will join with.
	Hostname string `json:"hostname,omitempty"`
}

type CreateObjectResponse struct {
	ObjectID string   `json:"objectID"`
	Status   string   `json:"status"`
	Tags     []string `json:"tags"`
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
	TS        string `json:"ts"`
	ObjectID  string `json:"objectID"`
	Kind      string `json:"kind"` // allow | deny | spawn | kill | egress
	Dst       string `json:"dst,omitempty"`
	Grant     string `json:"grant,omitempty"`
	Code      string `json:"code,omitempty"`
	TraceID   string `json:"traceID"`
	ActingFor string `json:"actingFor,omitempty"`
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

// Namespace returns the lease namespace of a DO class in a workload
// ("<workload>/<Class>"), or of the workload entry node itself (class "").
func Namespace(workload, class string) string {
	if class == "" {
		return workload
	}
	return workload + "/" + class
}
