package mgmt

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rosskukulinski/twd/internal/tsapi"
)

// Config configures a Mgmt.
type Config struct {
	Store   Store
	Custody Custody
	Broker  *Broker
	// Policy fetches tenant policies; default: GetPolicy through the broker.
	Policy       PolicySource
	PolicyMaxAge time.Duration
	Clock        Clock
	Log          *slog.Logger
	// IDSecret keys the object-name HMAC (RFC §4.3). Required.
	IDSecret []byte
	// ReapInterval (default 10s); RevocationWindow is how long a kill is
	// repeated in heartbeats (default 10 min).
	ReapInterval     time.Duration
	RevocationWindow time.Duration
	// MintRetries before an object is marked failed (default 5).
	MintRetries int
	// DeviceDeleteDelay defers kill tier 2 (device delete) after the tier-1
	// push, so the killed node's tombstone on the host can answer NEW
	// connections with a typed object_killed instead of peers black-holing
	// once control drops it from their netmaps. 0 deletes immediately.
	// Pending deletes run at once on Close.
	DeviceDeleteDelay time.Duration
	// PushTimeout bounds one mgmt → hostd push (default 2 s).
	PushTimeout time.Duration
}

// Mgmt is twd-mgmt's core: tenants, deployments, objects, the mint broker
// and its per-tenant throttle, spawn checks, kill, the reaper and audit.
type Mgmt struct {
	cfg      Config
	store    Store
	custody  Custody
	broker   *Broker
	policies *PolicyCache
	clock    Clock
	log      *slog.Logger

	mu        sync.Mutex
	throttles map[string]*Throttle
	queues    map[string]chan string
	waiters   map[string]chan struct{}
	spawnMu   map[string]*sync.Mutex // per tenant: serialises budget checks
	depStatus map[string]DeploymentStatus
	pushURLs  map[string]string // hostID → unix:// push socket (in memory; hosts re-register)
	pending   map[string]*pendingDelete
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// New returns a Mgmt; call Start to run the mint workers and reaper.
func New(cfg Config) (*Mgmt, error) {
	if cfg.Store == nil || cfg.Custody == nil || len(cfg.IDSecret) < 16 {
		return nil, errors.New("mgmt: Store, Custody and a ≥16-byte IDSecret are required")
	}
	if cfg.Broker == nil {
		cfg.Broker = &Broker{}
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.ReapInterval == 0 {
		cfg.ReapInterval = 10 * time.Second
	}
	if cfg.RevocationWindow == 0 {
		cfg.RevocationWindow = 10 * time.Minute
	}
	if cfg.MintRetries == 0 {
		cfg.MintRetries = 5
	}
	m := &Mgmt{cfg: cfg, store: cfg.Store, custody: cfg.Custody, broker: cfg.Broker, clock: cfg.Clock, log: cfg.Log,
		throttles: map[string]*Throttle{}, queues: map[string]chan string{}, waiters: map[string]chan struct{}{},
		spawnMu: map[string]*sync.Mutex{}, depStatus: map[string]DeploymentStatus{}, pushURLs: map[string]string{},
		pending: map[string]*pendingDelete{}}
	if cfg.PushTimeout == 0 {
		m.cfg.PushTimeout = 2 * time.Second
	}
	src := cfg.Policy
	if src == nil {
		src = func(ctx context.Context, t Tenant, etag string) ([]byte, string, bool, error) {
			api, err := m.broker.API(ctx, t, purposePolicy)
			if err != nil {
				return nil, "", false, err
			}
			return api.GetPolicy(ctx, etag)
		}
	}
	m.policies = &PolicyCache{Fetch: src, MaxAge: cfg.PolicyMaxAge, Clock: cfg.Clock}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m, nil
}

// LoadOrCreateSecret reads a 32-byte secret file (0600), creating it.
func LoadOrCreateSecret(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b = make([]byte, 32)
		rand.Read(b)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return b, os.WriteFile(path, b, 0o600)
	}
	return b, err
}

// Start re-queues pending objects and runs the reaper until Close.
func (m *Mgmt) Start() error {
	pending, err := m.store.ListObjects(m.ctx, ObjectFilter{Status: []string{StatusPending}})
	if err != nil {
		return err
	}
	for _, o := range pending {
		m.enqueue(o.TenantID, o.ID)
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-m.clock.After(m.cfg.ReapInterval):
				if n, err := m.Reap(m.ctx); err != nil {
					m.log.Warn("reaper", "err", err)
				} else if n > 0 {
					m.log.Info("reaper killed objects", "n", n)
				}
			}
		}
	}()
	return nil
}

// Close stops the workers.
func (m *Mgmt) Close() {
	m.FlushDeletes() // never leave a killed object's device behind
	m.cancel()
	m.wg.Wait()
}

func (m *Mgmt) now() time.Time { return m.clock.Now() }

func newID(prefix string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func traceID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// audit appends to the mgmt chain; failures are logged (and surfaced by
// verify), never swallowed silently.
func (m *Mgmt) audit(ctx context.Context, r AuditRecord, detail map[string]any) {
	r.Chain = "mgmt"
	r.TS = m.now().UTC().Format(time.RFC3339Nano)
	if detail != nil {
		r.Detail, _ = json.Marshal(detail)
	}
	if _, err := m.store.AppendAudit(ctx, r); err != nil {
		m.log.Error("audit append failed", "kind", r.Kind, "object", r.ObjectID, "err", err)
	}
}

// ---- tenants ----

// TenantRequest is POST /v0/tenants.
type TenantRequest struct {
	ID                     string `json:"tenantID,omitempty"`
	Tailnet                string `json:"tailnet"`
	ControlURL             string `json:"controlURL,omitempty"`
	MintMode               string `json:"mintMode"`
	APIKeyFile             string `json:"apiKeyFile,omitempty"`
	WIFClientID            string `json:"wifClientID,omitempty"`
	WIFLifecycleClientID   string `json:"wifLifecycleClientID,omitempty"`
	RegistrationsPerMinute int    `json:"registrationsPerMinute,omitempty"`
	MaxObjects             int    `json:"maxObjects,omitempty"`
}

func (m *Mgmt) CreateTenant(ctx context.Context, r TenantRequest) (Tenant, error) {
	if r.Tailnet == "" || r.Tailnet == "-" {
		return Tenant{}, errf(400, "bad_request", "tailnet must be named explicitly")
	}
	switch r.MintMode {
	case MintAPIKey:
		if r.APIKeyFile == "" {
			return Tenant{}, errf(400, "bad_request", "mintMode apikey needs apiKeyFile")
		}
		if !filepath.IsAbs(r.APIKeyFile) {
			return Tenant{}, errf(400, "bad_request", "apiKeyFile must be an absolute path")
		}
		if _, err := os.Stat(r.APIKeyFile); err != nil {
			return Tenant{}, errf(400, "bad_request", "apiKeyFile: %v", err)
		}
	case MintWIF:
		if r.WIFClientID == "" {
			return Tenant{}, errf(400, "bad_request", "mintMode wif needs wifClientID")
		}
		if m.broker.Issuer == nil {
			return Tenant{}, errf(400, "bad_request", "mintMode wif needs twd-mgmt to run with an issuer (-issuer-url)")
		}
	default:
		return Tenant{}, errf(400, "bad_request", "mintMode must be apikey or wif")
	}
	t := Tenant{ID: r.ID, Tailnet: r.Tailnet, ControlURL: r.ControlURL, MintMode: r.MintMode, APIKeyFile: r.APIKeyFile,
		WIFClientID: r.WIFClientID, WIFLifecycleClientID: r.WIFLifecycleClientID,
		RegistrationsPerMinute: r.RegistrationsPerMinute, MaxObjects: r.MaxObjects, CreatedAt: m.now().UnixMilli()}
	if t.ID == "" {
		t.ID = newID("tn_")[:11]
	}
	if t.RegistrationsPerMinute <= 0 {
		t.RegistrationsPerMinute = DefaultRegistrationsPerMinute
	}
	if t.MaxObjects <= 0 {
		t.MaxObjects = DefaultMaxObjects
	}
	if err := m.store.CreateTenant(ctx, t); err != nil {
		return Tenant{}, err
	}
	m.audit(ctx, AuditRecord{TenantID: t.ID, Kind: "tenant"}, map[string]any{"tailnet": t.Tailnet, "mintMode": t.MintMode})
	return t, nil
}

// IssuerInfo is what a tenant admin needs to create the WIF trust credentials.
func (m *Mgmt) IssuerInfo(t Tenant) map[string]any {
	if m.broker.Issuer == nil {
		return nil
	}
	return map[string]any{"issuer": m.broker.Issuer.TenantIssuer(t.ID), "mintSubject": MintSubject(t.ID),
		"lifecycleSubject": LifecycleSubject(t.ID)}
}

// ---- hosts ----

// RegisterRequest is POST /v0/hosts/register.
type RegisterRequest struct {
	HostID        string   `json:"hostID"`
	CapacityNodes int      `json:"capacityNodes"`
	Tiers         []string `json:"tiers"`
	AuditPubKey   string   `json:"auditPubKey,omitempty"`
	// PushURL (hostd-additions) is where mgmt pushes kills and assignment
	// changes. Only unix:// sockets are accepted.
	PushURL string `json:"pushURL,omitempty"`
}

func (m *Mgmt) RegisterHost(ctx context.Context, r RegisterRequest, nodeID string) error {
	if r.HostID == "" {
		return errf(400, "bad_request", "hostID required")
	}
	if old, err := m.store.GetHost(ctx, r.HostID); err == nil && old.NodeID != "" && nodeID != "" && old.NodeID != nodeID {
		return errf(403, "host_identity_mismatch", "host %s is registered to another node", r.HostID)
	}
	m.mu.Lock()
	if strings.HasPrefix(r.PushURL, "unix:///") {
		m.pushURLs[r.HostID] = r.PushURL
	} else {
		delete(m.pushURLs, r.HostID)
	}
	m.mu.Unlock()
	return m.store.UpsertHost(ctx, Host{ID: r.HostID, CapacityNodes: r.CapacityNodes, Tiers: r.Tiers, NodeID: nodeID,
		AuditPubKey: r.AuditPubKey, LastSeen: m.now().UnixMilli()})
}

// HeartbeatLease is one lease a host believes it holds.
type HeartbeatLease struct {
	Namespace string `json:"namespace"`
	Epoch     int64  `json:"epoch"`
}

// HeartbeatResponse is the heartbeat reply. RevokeObjects is an addition to
// the v0 contract: object IDs killed in the last RevocationWindow that this
// host must kill (idempotent; repeated until the window passes).
type HeartbeatResponse struct {
	OK            bool     `json:"ok"`
	Revoke        []string `json:"revoke"`
	RevokeObjects []string `json:"revokeObjects"`
}

func (m *Mgmt) Heartbeat(ctx context.Context, hostID string, liveNodes int, leases []HeartbeatLease, deps ...DeploymentStatus) (HeartbeatResponse, error) {
	for _, d := range deps {
		if d.DeploymentID != "" {
			m.setDepStatus(d.DeploymentID, d)
		}
	}
	now := m.now()
	if err := m.store.TouchHost(ctx, hostID, liveNodes, now); err != nil {
		if errors.Is(err, ErrNotFound) {
			return HeartbeatResponse{}, errf(404, "host_not_registered", "host %s is not registered", hostID)
		}
		return HeartbeatResponse{}, err
	}
	resp := HeartbeatResponse{OK: true, Revoke: []string{}, RevokeObjects: []string{}}
	for _, hl := range leases {
		name := m.resolveLease(ctx, hostID, hl.Namespace)
		if _, err := m.store.RenewLease(ctx, name, hostID, hl.Epoch, NamespaceLeaseTTL, now); err != nil {
			resp.Revoke = append(resp.Revoke, hl.Namespace)
		}
	}
	objs, err := m.store.Revocations(ctx, hostID, now.Add(-m.cfg.RevocationWindow))
	if err != nil {
		return resp, err
	}
	resp.RevokeObjects = append(resp.RevokeObjects, objs...)
	return resp, nil
}

// resolveLease maps a heartbeat "namespace" to a lease name. Accepted forms:
// the lease name itself ("<tenant>/<workload>"), a workload name, or a DO
// class name of a deployment assigned to this host.
func (m *Mgmt) resolveLease(ctx context.Context, hostID, ns string) string {
	if strings.Contains(ns, "/") {
		return ns
	}
	as, _ := m.store.Assignments(ctx, hostID)
	for _, d := range as {
		if d.Workload == ns {
			return LeaseName(d.TenantID, d.Workload)
		}
		for _, c := range d.Namespaces {
			if c.Class == ns {
				return LeaseName(d.TenantID, d.Workload)
			}
		}
	}
	return ns
}

// ---- deployments ----

// DeployRequest is POST /v0/deployments. Namespaces and Host are additions
// to the v0 contract: explicit class config (instead of twd.jsonc in the
// bundle) and an explicit host choice.
type DeployRequest struct {
	TenantID   string        `json:"tenantID"`
	Workload   string        `json:"workload"`
	Bundle     []byte        `json:"bundle"` // base64 in JSON
	Namespaces []ClassConfig `json:"namespaces,omitempty"`
	Host       string        `json:"host,omitempty"`
}

// DeployResponse is the deploy reply.
type DeployResponse struct {
	DeploymentID string `json:"deploymentID"`
	Host         string `json:"host"`
	Epoch        int64  `json:"epoch"`
	Endpoint     struct {
		Node     string `json:"node"`
		MagicDNS string `json:"magicDNS"`
	} `json:"endpoint"`
}

func validName(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func (m *Mgmt) Deploy(ctx context.Context, r DeployRequest) (DeployResponse, error) {
	var resp DeployResponse
	t, err := m.store.GetTenant(ctx, r.TenantID)
	if err != nil {
		return resp, errf(404, "unknown_tenant", "tenant %q not found", r.TenantID)
	}
	if !validName(r.Workload) {
		return resp, errf(400, "bad_request", "workload must be a DNS label ([a-z0-9-], ≤63)")
	}
	classes := r.Namespaces
	maxLive, maxDepth := DefaultMaxLive, DefaultMaxDepth
	if len(r.Bundle) > 0 {
		cfg, err := BundleConfig(r.Bundle)
		if err != nil && len(classes) == 0 {
			return resp, errf(400, "bad_bundle", "%v", err)
		}
		if err == nil {
			if len(classes) == 0 {
				classes = cfg.Classes()
			} else if len(cfg.Tailnet.Tags) > 0 && !slices.ContainsFunc(classes, func(c ClassConfig) bool { return c.Class == WorkloadNamespace }) {
				classes = append(classes, ClassConfig{Class: WorkloadNamespace, Tags: cfg.Tailnet.Tags})
			}
			if cfg.Tailnet.Spawn.MaxLive > 0 {
				maxLive = min(cfg.Tailnet.Spawn.MaxLive, DefaultMaxLive)
			}
			if cfg.Tailnet.Spawn.MaxDepth > 0 {
				maxDepth = min(cfg.Tailnet.Spawn.MaxDepth, DefaultMaxDepth)
			}
		}
	}
	if len(classes) == 0 {
		return resp, errf(400, "bad_request", "no namespaces: the bundle's twd.jsonc declares neither tailnet.tags (the workload node) nor tailnet.objects")
	}
	for _, c := range classes {
		if c.Class == "" || len(c.Tags) == 0 {
			return resp, errf(400, "bad_request", "namespace %q needs tags", c.Class)
		}
		for _, tag := range c.Tags {
			if !strings.HasPrefix(tag, "tag:") {
				return resp, errf(400, "bad_request", "namespace %q: %q is not a tag", c.Class, tag)
			}
		}
	}
	host, err := m.pickHost(ctx, t, r.Workload, r.Host)
	if err != nil {
		return resp, err
	}
	lease, err := m.store.AcquireLease(ctx, LeaseName(t.ID, r.Workload), host, NamespaceLeaseTTL, m.now())
	if err != nil {
		return resp, err
	}
	d := Deployment{ID: newID("dp_"), TenantID: t.ID, Workload: r.Workload, HostID: host, Namespaces: classes,
		MaxLive: maxLive, MaxDepth: maxDepth, Epoch: lease.Epoch, Bundle: r.Bundle, CreatedAt: m.now().UnixMilli()}
	if err := m.store.CreateDeployment(ctx, d); err != nil {
		return resp, err
	}
	m.audit(ctx, AuditRecord{TenantID: t.ID, Kind: "deploy"}, map[string]any{"deploymentID": d.ID, "workload": d.Workload,
		"host": host, "epoch": lease.Epoch})
	resp.DeploymentID, resp.Host, resp.Epoch = d.ID, host, lease.Epoch
	resp.Endpoint.Node = r.Workload + "-" + host
	resp.Endpoint.MagicDNS = resp.Endpoint.Node + "." + t.Tailnet
	m.setDepStatus(d.ID, DeploymentStatus{Status: "assigned"})
	// Tell the host now instead of at its next heartbeat.
	go m.push(host, "/v0/push/assignments", map[string]any{})
	return resp, nil
}

// DeploymentStatus is what the host last reported for a deployment
// (heartbeat "deployments"); kept in memory, refreshed every heartbeat.
type DeploymentStatus struct {
	DeploymentID string `json:"deploymentID"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
}

func (m *Mgmt) setDepStatus(id string, st DeploymentStatus) {
	st.DeploymentID = id
	m.mu.Lock()
	m.depStatus[id] = st
	m.mu.Unlock()
}

// DeploymentView is GET /v0/deployments/{id}.
type DeploymentView struct {
	DeploymentID string        `json:"deploymentID"`
	TenantID     string        `json:"tenantID"`
	Workload     string        `json:"workload"`
	Host         string        `json:"host"`
	Epoch        int64         `json:"epoch"`
	Status       string        `json:"status"`
	Error        string        `json:"error,omitempty"`
	Namespaces   []ClassConfig `json:"namespaces"`
	CreatedAt    int64         `json:"createdAt"`
	Endpoint     struct {
		ObjectID string `json:"objectID,omitempty"`
		Node     string `json:"node"`
		MagicDNS string `json:"magicDNS"`
		Status   string `json:"status"`
	} `json:"endpoint"`
}

// GetDeployment returns a deployment with the host-reported status and its
// workload entry node. Status is "superseded" once a newer deploy of the
// same workload exists.
func (m *Mgmt) GetDeployment(ctx context.Context, id string) (DeploymentView, error) {
	var v DeploymentView
	d, err := m.store.GetDeployment(ctx, id)
	if err != nil {
		return v, err
	}
	v.DeploymentID, v.TenantID, v.Workload, v.Host, v.Epoch, v.Namespaces, v.CreatedAt = d.ID, d.TenantID, d.Workload, d.HostID, d.Epoch, d.Namespaces, d.CreatedAt
	m.mu.Lock()
	st, ok := m.depStatus[id]
	m.mu.Unlock()
	v.Status, v.Error = "unknown", ""
	if ok {
		v.Status, v.Error = st.Status, st.Error
	}
	if ds, err := m.store.LatestDeployments(ctx, d.TenantID); err == nil {
		for _, a := range ds {
			if a.Workload == d.Workload && a.ID != d.ID {
				v.Status = "superseded"
			}
		}
	}
	if o, err := m.store.GetObjectByName(ctx, d.TenantID, d.Workload, WorkloadNamespace, d.Workload); err == nil {
		v.Endpoint.ObjectID, v.Endpoint.MagicDNS, v.Endpoint.Status = o.ID, o.MagicDNS, o.Status
		v.Endpoint.Node, _, _ = strings.Cut(o.MagicDNS, ".")
	}
	return v, nil
}

// pickHost: MVP is a single host per tenant namespace. Keep the current
// live lease holder; otherwise the live host with the most free capacity.
func (m *Mgmt) pickHost(ctx context.Context, t Tenant, workload, want string) (string, error) {
	now := m.now()
	hosts, err := m.store.ListHosts(ctx)
	if err != nil {
		return "", err
	}
	live := map[string]Host{}
	for _, h := range hosts {
		if h.Live(now) {
			live[h.ID] = h
		}
	}
	if want != "" {
		if _, ok := live[want]; !ok {
			return "", errf(503, "no_host", "host %q is not live (no heartbeat within %v)", want, HostLeaseTTL)
		}
		return want, nil
	}
	if l, err := m.store.GetLease(ctx, LeaseName(t.ID, workload)); err == nil && l.Holder != "" {
		if _, ok := live[l.Holder]; ok {
			return l.Holder, nil
		}
	}
	best, bestFree := "", -1<<62
	for _, h := range hosts {
		if _, ok := live[h.ID]; !ok {
			continue
		}
		if free := h.CapacityNodes - h.LiveNodes; free > bestFree {
			best, bestFree = h.ID, free
		}
	}
	if best == "" {
		return "", errf(503, "no_host", "no live host (register twd-hostd and heartbeat first)")
	}
	return best, nil
}

// Assignment is one GET /v0/hosts/{id}/assignments entry. Tailnet,
// ControlURL and Lease are additions to the v0 contract.
type Assignment struct {
	DeploymentID string        `json:"deploymentID"`
	TenantID     string        `json:"tenantID"`
	Workload     string        `json:"workload"`
	BundleURL    string        `json:"bundleURL"`
	Namespaces   []ClassConfig `json:"namespaces"`
	Epoch        int64         `json:"epoch"`
	Lease        string        `json:"lease"`
	Tailnet      string        `json:"tailnet"`
	ControlURL   string        `json:"controlURL,omitempty"`
	WorkloadTags []string      `json:"workloadTags,omitempty"`
}

func (m *Mgmt) Assignments(ctx context.Context, hostID string) ([]Assignment, error) {
	ds, err := m.store.Assignments(ctx, hostID)
	if err != nil {
		return nil, err
	}
	out := []Assignment{}
	for _, d := range ds {
		t, _ := m.store.GetTenant(ctx, d.TenantID)
		a := Assignment{DeploymentID: d.ID, TenantID: d.TenantID, Workload: d.Workload,
			BundleURL: "/v0/deployments/" + d.ID + "/bundle", Epoch: d.Epoch,
			Lease: LeaseName(d.TenantID, d.Workload), Tailnet: t.Tailnet, ControlURL: t.ControlURL, Namespaces: []ClassConfig{}}
		for _, c := range d.Namespaces {
			if c.Class == WorkloadNamespace {
				a.WorkloadTags = c.Tags
				continue
			}
			a.Namespaces = append(a.Namespaces, c)
		}
		out = append(out, a)
	}
	return out, nil
}

// ---- objects ----

// CreateObjectRequest is POST /v0/objects. ActingFor is an addition (the
// human principal of the spawning request, for audit).
type CreateObjectRequest struct {
	TenantID string `json:"tenantID"`
	// Workload scopes the object's identity (M7): (tenant, workload,
	// namespace, objectName). Optional for compatibility: if empty, a
	// spawn takes its parent's workload, the "_workload" namespace takes
	// objectName, and otherwise exactly one workload may declare the class.
	Workload   string `json:"workload,omitempty"`
	Namespace  string `json:"namespace"`
	ObjectName string `json:"objectName"`
	Parent     *struct {
		ID string `json:"objectID"`
	} `json:"parent,omitempty"`
	Caps      json.RawMessage `json:"caps,omitempty"`
	TTL       json.RawMessage `json:"ttl,omitempty"`
	ActingFor string          `json:"actingFor,omitempty"`
	TraceID   string          `json:"traceID,omitempty"`
}

// CreateObjectResponse is the reply. Hostname is an addition: the node
// hostname hostd should use.
type CreateObjectResponse struct {
	ObjectID string   `json:"objectID"`
	Status   string   `json:"status"`
	Tags     []string `json:"tags"`
	Hostname string   `json:"hostname"`
}

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// objectID is <class-slug>-<b32(hmac(tenant/workload/ns/name))[:10]> (RFC
// §4.3); it doubles as the node hostname. Every part of the identity is in
// the MAC, so two workloads (or two tenants) with the same class and object
// name get distinct IDs, hostnames, nodes and state dirs.
func (m *Mgmt) objectID(tenantID, workload, ns, name string) string {
	mac := hmac.New(sha256.New, m.cfg.IDSecret)
	fmt.Fprintf(mac, "%s\x00%s\x00%s\x00%s", tenantID, workload, ns, name)
	var slug strings.Builder
	src := ns
	if ns == WorkloadNamespace {
		src = name // the workload entry node is named after the workload
	}
	for _, r := range strings.ToLower(src) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			slug.WriteRune(r)
		} else if slug.Len() > 0 {
			slug.WriteByte('-')
		}
	}
	s := strings.Trim(slug.String(), "-")
	if len(s) > 40 {
		s = s[:40]
	}
	if s == "" {
		s = "obj"
	}
	return s + "-" + b32.EncodeToString(mac.Sum(nil))[:10]
}

func (m *Mgmt) tenantLock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.spawnMu[id]
	if l == nil {
		l = &sync.Mutex{}
		m.spawnMu[id] = l
	}
	return l
}

func (m *Mgmt) CreateObject(ctx context.Context, r CreateObjectRequest) (CreateObjectResponse, error) {
	var resp CreateObjectResponse
	t, err := m.store.GetTenant(ctx, r.TenantID)
	if err != nil {
		return resp, errf(404, "unknown_tenant", "tenant %q not found", r.TenantID)
	}
	if r.ObjectName == "" || len(r.ObjectName) > 256 {
		return resp, errf(400, "bad_request", "objectName required (≤256 bytes)")
	}
	var parent *Object
	if r.Parent != nil && r.Parent.ID != "" {
		p, err := m.store.GetObject(ctx, r.Parent.ID)
		if err != nil || p.TenantID != t.ID {
			return resp, errf(404, "delegation_invalid", "parent %q not found in tenant %s", r.Parent.ID, t.ID)
		}
		parent = &p
	}
	workload := r.Workload
	switch {
	case r.Namespace == WorkloadNamespace:
		if workload != "" && workload != r.ObjectName {
			return resp, errf(400, "bad_request", "the %s object of workload %q must be named after it (got %q)", WorkloadNamespace, workload, r.ObjectName)
		}
		workload = r.ObjectName
	case workload == "" && parent != nil:
		workload = parent.Workload
	}
	var dep Deployment
	var class ClassConfig
	if r.Namespace == WorkloadNamespace {
		dep, class, err = m.findWorkloadClass(ctx, t.ID, workload)
	} else {
		dep, class, err = m.store.FindClass(ctx, t.ID, workload, r.Namespace)
	}
	if errors.Is(err, ErrAmbiguous) {
		return resp, errf(409, "namespace_ambiguous", "%v", err)
	}
	if err != nil {
		in := ""
		if workload != "" {
			in = " of workload " + workload
		}
		return resp, errf(404, "unknown_namespace", "no deployment%s of tenant %s declares namespace %q", in, t.ID, r.Namespace)
	}
	workload = dep.Workload
	ttl, err := ParseTTL(r.TTL)
	if err != nil {
		return resp, errf(400, "bad_request", "%v", err)
	}
	if ttl == 0 && class.TTL != "" {
		ttl, _ = time.ParseDuration(class.TTL)
	}
	l := m.tenantLock(t.ID)
	l.Lock()
	defer l.Unlock()

	if o, err := m.store.GetObjectByName(ctx, t.ID, workload, r.Namespace, r.ObjectName); err == nil {
		if !o.Live() {
			return resp, errf(410, "killed", "object %s (%s %s/%s) was %s", o.ID, workload, r.Namespace, r.ObjectName, o.Status)
		}
		return CreateObjectResponse{ObjectID: o.ID, Status: o.Status, Tags: o.Tags, Hostname: o.Hostname}, nil
	}
	if n, err := m.store.CountLive(ctx, t.ID, ""); err != nil {
		return resp, err
	} else if n >= t.MaxObjects {
		return resp, errf(429, "object_quota_exceeded", "tenant %s has %d live objects (maxObjects %d)", t.ID, n, t.MaxObjects)
	}
	id := m.objectID(t.ID, workload, r.Namespace, r.ObjectName)
	if _, err := m.store.GetObject(ctx, id); err == nil {
		// An ID collision with another identity (a 50-bit MAC prefix): never
		// hand out an existing object's node.
		return resp, errf(409, "identity_conflict", "object ID %s is already taken by another identity", id)
	}
	o := Object{ID: id, TenantID: t.ID, Workload: workload, Namespace: r.Namespace, Name: r.ObjectName, Hostname: id, DeploymentID: dep.ID,
		Lease: LeaseName(t.ID, dep.Workload), Tags: class.Tags, Caps: r.Caps, RootID: id, TTLSeconds: int64(ttl / time.Second),
		Status: StatusPending, CreatedAt: m.now().UnixMilli(), UpdatedAt: m.now().UnixMilli()}
	kind := "create"
	if parent != nil {
		kind = "spawn"
		p := *parent
		if !p.Live() {
			return resp, errf(410, "delegation_invalid", "parent %s is %s", p.ID, p.Status)
		}
		if err := m.checkSpawn(ctx, t, dep, p, o); err != nil {
			m.audit(ctx, AuditRecord{TenantID: t.ID, ObjectID: p.ID, Kind: "deny", Code: err.Code, TraceID: r.TraceID,
				ActingFor: r.ActingFor}, map[string]any{"spawn": r.Namespace + "/" + r.ObjectName, "message": err.Message})
			return resp, err
		}
		o.ParentID, o.RootID, o.Depth = p.ID, p.RootID, p.Depth+1
		o.Ephemeral = ttl > 0
	}
	if err := m.store.CreateObject(ctx, o); err != nil {
		return resp, err
	}
	detail := map[string]any{"workload": o.Workload, "namespace": o.Namespace, "name": o.Name, "tags": o.Tags, "root": o.RootID}
	if o.ParentID != "" {
		detail["parent"] = o.ParentID
	}
	if len(o.Caps) > 0 {
		detail["lim"] = o.Caps
	}
	if ttl > 0 {
		detail["ttl"] = ttl.String()
	}
	m.audit(ctx, AuditRecord{TenantID: t.ID, ObjectID: o.ID, Kind: kind, TraceID: r.TraceID, ActingFor: r.ActingFor}, detail)
	m.enqueue(t.ID, o.ID)
	return CreateObjectResponse{ObjectID: o.ID, Status: StatusPending, Tags: o.Tags, Hostname: o.Hostname}, nil
}

// findWorkloadClass finds the latest deployment of the workload named name
// and its "_workload" namespace (the entry node's tags).
func (m *Mgmt) findWorkloadClass(ctx context.Context, tenantID, name string) (Deployment, ClassConfig, error) {
	ds, err := m.store.LatestDeployments(ctx, tenantID)
	if err != nil {
		return Deployment{}, ClassConfig{}, err
	}
	for _, d := range ds {
		if d.Workload != name {
			continue
		}
		for _, c := range d.Namespaces {
			if c.Class == WorkloadNamespace {
				return d, c, nil
			}
		}
	}
	return Deployment{}, ClassConfig{}, fmt.Errorf("workload %q has no %s namespace (twd.jsonc tailnet.tags): %w", name, WorkloadNamespace, ErrNotFound)
}

// checkSpawn enforces depth ≤ maxDepth, live descendants of the root <
// maxLive, child-tag ownership, and caps ⊆ effective(parent) from policy.
func (m *Mgmt) checkSpawn(ctx context.Context, t Tenant, dep Deployment, p, child Object) *Error {
	maxDepth, maxLive := dep.MaxDepth, dep.MaxLive
	if maxDepth <= 0 {
		maxDepth = DefaultMaxDepth
	}
	if maxLive <= 0 {
		maxLive = DefaultMaxLive
	}
	if p.Depth+1 > maxDepth {
		return errf(429, "spawn_budget_exceeded", "depth %d would exceed maxDepth %d", p.Depth+1, maxDepth)
	}
	n, err := m.store.CountLive(ctx, t.ID, p.RootID)
	if err != nil {
		return asAPIError(err)
	}
	if n+1 > maxLive {
		return errf(429, "spawn_budget_exceeded", "root %s already has %d live descendants (maxLive %d)", p.RootID, n, maxLive)
	}
	pol, err := m.policies.Get(ctx, t)
	if err != nil {
		return errf(503, "policy_unavailable", "can't read tenant policy to check delegation: %v", err)
	}
	for _, tag := range child.Tags {
		if !pol.TagOwnedBy(tag, p.Tags) {
			e := errf(403, "delegation_invalid", "child tag %s is not owned by the parent's tags %v", tag, p.Tags)
			e.Snippet = fmt.Sprintf(`"tagOwners": { %q: ["tag:twd", %q] }`, tag, p.Tags[0])
			return e
		}
	}
	if e := pol.CheckSpawnCaps(p.Tags, child.Caps); e != nil {
		return e
	}
	return nil
}

func (m *Mgmt) GetObject(ctx context.Context, id string) (Object, error) {
	o, err := m.store.GetObject(ctx, id)
	if err != nil {
		return o, err
	}
	kids, _ := m.store.ListObjects(ctx, ObjectFilter{ParentID: id})
	for _, k := range kids {
		o.Children = append(o.Children, k.ID)
	}
	return o, nil
}

// ListObjects lists objects with lineage (children filled in).
func (m *Mgmt) ListObjects(ctx context.Context, tenant, workload, ns string) ([]Object, error) {
	objs, err := m.store.ListObjects(ctx, ObjectFilter{TenantID: tenant, Workload: workload, Namespace: ns})
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i := range objs {
		idx[objs[i].ID] = i
	}
	for _, o := range objs {
		if i, ok := idx[o.ParentID]; ok {
			objs[i].Children = append(objs[i].Children, o.ID)
		}
	}
	if objs == nil {
		objs = []Object{}
	}
	return objs, nil
}

// ---- mint queue ----

func (m *Mgmt) throttle(t Tenant) *Throttle {
	m.mu.Lock()
	defer m.mu.Unlock()
	th := m.throttles[t.ID]
	if th == nil {
		th = NewThrottle(t.RegistrationsPerMinute, m.clock)
		m.throttles[t.ID] = th
	}
	return th
}

func (m *Mgmt) enqueue(tenantID, objectID string) {
	m.mu.Lock()
	q := m.queues[tenantID]
	if q == nil {
		q = make(chan string, 100_000)
		m.queues[tenantID] = q
		m.wg.Add(1)
		go m.mintWorker(tenantID, q)
	}
	m.mu.Unlock()
	select {
	case q <- objectID:
	default:
		m.log.Error("mint queue full", "tenant", tenantID)
	}
}

// QueueDepth is twd_registration_queue for a tenant.
func (m *Mgmt) QueueDepth(tenantID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.queues[tenantID])
}

func (m *Mgmt) mintWorker(tenantID string, q chan string) {
	defer m.wg.Done()
	for {
		var id string
		select {
		case <-m.ctx.Done():
			return
		case id = <-q:
		}
		for attempt := 1; ; attempt++ {
			t, err := m.store.GetTenant(m.ctx, tenantID)
			if err != nil {
				break
			}
			th := m.throttle(t)
			if err := th.Wait(m.ctx); err != nil {
				return
			}
			err = m.mintOne(m.ctx, t, id)
			if err == nil {
				th.Success()
				break
			}
			var ae *tsapi.APIError
			if errors.As(err, &ae) && (ae.Status == http.StatusTooManyRequests || ae.Status >= 500) {
				d := th.Backoff(ae.RetryAfter)
				m.log.Warn("mint rate-limited; backing off", "tenant", tenantID, "status", ae.Status, "backoff", d)
				continue // retry the same object after the pause (not counted)
			}
			if attempt >= m.cfg.MintRetries || m.ctx.Err() != nil {
				m.log.Error("mint failed", "tenant", tenantID, "object", id, "err", err)
				m.store.UpdateObject(m.ctx, id, nil, func(o *Object) error {
					if o.Status == StatusPending {
						o.Status = StatusFailed
						o.UpdatedAt = m.now().UnixMilli()
					}
					return nil
				})
				m.audit(m.ctx, AuditRecord{TenantID: tenantID, ObjectID: id, Kind: "deny", Code: "bootstrap_failed"},
					map[string]any{"message": err.Error()})
				m.notify(id)
				break
			}
			m.log.Warn("mint failed; retrying", "object", id, "attempt", attempt, "err", err)
			select {
			case <-m.ctx.Done():
				return
			case <-m.clock.After(time.Duration(attempt) * time.Second):
			}
		}
	}
}

func custodyName(o Object) string { return "twd/" + o.TenantID + "/" + o.ID + "/authkey" }

// mintOne mints one single-use, preauthorized, ≤5-min key tagged with the
// object's tags, and puts it in custody. The key is never logged.
func (m *Mgmt) mintOne(ctx context.Context, t Tenant, id string) error {
	o, err := m.store.GetObject(ctx, id)
	if err != nil || o.Status != StatusPending {
		return nil // gone, killed or already minted
	}
	api, err := m.broker.API(ctx, t, purposeMint)
	if err != nil {
		return err
	}
	k, err := api.CreateKey(ctx, tsapi.KeyOptions{Tags: o.Tags, Ephemeral: o.Ephemeral, Preauthorized: true,
		ExpirySeconds: int(KeyExpiry / time.Second), Description: "twd " + o.ID})
	if err != nil {
		return err
	}
	if err := m.custody.Put(ctx, custodyName(o), []byte(k.Secret)); err != nil {
		api.DeleteKey(ctx, k.ID)
		return err
	}
	exp := m.now().Add(KeyExpiry - 15*time.Second).UnixMilli() // released only with ≥15 s of life left
	killed := false
	_, err = m.store.UpdateObject(ctx, id, nil, func(o *Object) error {
		if o.Status != StatusPending {
			killed = true
			return nil
		}
		o.Status, o.KeyID, o.KeyExpires, o.UpdatedAt = StatusMinted, k.ID, exp, m.now().UnixMilli()
		return nil
	})
	if err != nil || killed {
		api.DeleteKey(ctx, k.ID)
		m.custody.Delete(ctx, custodyName(o))
		return err
	}
	m.audit(ctx, AuditRecord{TenantID: t.ID, ObjectID: id, Kind: "mint"}, map[string]any{"keyID": k.ID, "tags": o.Tags,
		"mode": t.MintMode, "ephemeral": o.Ephemeral})
	m.notify(id)
	return nil
}

func (m *Mgmt) notify(id string) {
	m.mu.Lock()
	if ch, ok := m.waiters[id]; ok {
		close(ch)
		delete(m.waiters, id)
	}
	m.mu.Unlock()
}

func (m *Mgmt) changed(id string) <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.waiters[id]
	if !ok {
		ch = make(chan struct{})
		m.waiters[id] = ch
	}
	return ch
}

// Credential is GET /v0/objects/{id}/credential. ExpiresAt/Tags/Hostname/
// ControlURL are additions.
type Credential struct {
	AuthKey    string   `json:"authKey"`
	ExpiresAt  int64    `json:"expiresAt"`
	Tags       []string `json:"tags"`
	Hostname   string   `json:"hostname"`
	Ephemeral  bool     `json:"ephemeral"`
	ControlURL string   `json:"controlURL,omitempty"`
}

func (c Credential) String() string { return "mgmt.Credential{[REDACTED]}" }

// Credential releases the object's auth key to the namespace lease holder
// at epoch (fenced in the store). hostID "" (unix-socket callers that don't
// name themselves) fences on the epoch alone. It waits up to wait for a
// pending mint.
func (m *Mgmt) Credential(ctx context.Context, id, hostID string, epoch int64, wait time.Duration) (Credential, error) {
	deadline := m.now().Add(wait)
	for {
		ch := m.changed(id)
		o, err := m.store.GetObject(ctx, id)
		if err != nil {
			return Credential{}, err
		}
		switch o.Status {
		case StatusKilled:
			return Credential{}, errf(410, "killed", "object %s was killed (%s)", id, o.KillReason)
		case StatusFailed:
			return Credential{}, errf(502, "bootstrap_failed", "minting a key for %s failed", id)
		case StatusReady:
			return Credential{}, errf(409, "already_joined", "object %s already joined; rejoin from saved node state", id)
		case StatusMinted:
			if m.now().UnixMilli() >= o.KeyExpires {
				// Expired unused key: revoke it and mint again.
				m.store.UpdateObject(ctx, id, nil, func(o *Object) error {
					if o.Status == StatusMinted {
						o.Status, o.KeyID = StatusPending, ""
					}
					return nil
				})
				m.revokeKey(ctx, o)
				m.enqueue(o.TenantID, id)
				break
			}
			fence := &Fence{Lease: o.Lease, Holder: hostID, Epoch: epoch}
			if _, err := m.store.UpdateObject(ctx, id, fence, func(*Object) error { return nil }); err != nil {
				if errors.Is(err, ErrLeaseLost) {
					m.audit(ctx, AuditRecord{TenantID: o.TenantID, ObjectID: id, Kind: "deny", Code: "lease_lost"},
						map[string]any{"host": hostID, "epoch": epoch})
				}
				return Credential{}, err
			}
			secret, err := m.custody.Get(ctx, custodyName(o))
			if err != nil {
				return Credential{}, fmt.Errorf("custody: %w", err)
			}
			t, _ := m.store.GetTenant(ctx, o.TenantID)
			m.audit(ctx, AuditRecord{TenantID: o.TenantID, ObjectID: id, Kind: "credential"},
				map[string]any{"host": hostID, "epoch": epoch, "keyID": o.KeyID})
			return Credential{AuthKey: string(secret), ExpiresAt: o.KeyExpires, Tags: o.Tags, Hostname: o.Hostname,
				Ephemeral: o.Ephemeral, ControlURL: t.ControlURL}, nil
		}
		left := deadline.Sub(m.now())
		if left <= 0 {
			e := errf(503, "identity_pending", "object %s is waiting on the registration queue (depth %d)", id, m.QueueDepth(o.TenantID))
			return Credential{}, e
		}
		select {
		case <-ch:
		case <-time.After(left):
		case <-ctx.Done():
			return Credential{}, ctx.Err()
		}
	}
}

// ReadyRequest is POST /v0/objects/{id}/ready. Epoch is optional (fences
// the commit when set).
type ReadyRequest struct {
	NodeID       string `json:"nodeID"`
	StableNodeID string `json:"stableNodeID"`
	MagicDNS     string `json:"magicDNS"`
	Epoch        int64  `json:"epoch,omitempty"`
}

func (m *Mgmt) Ready(ctx context.Context, id, hostID string, r ReadyRequest) (Object, error) {
	o, err := m.store.GetObject(ctx, id)
	if err != nil {
		return o, err
	}
	var fence *Fence
	if r.Epoch > 0 {
		fence = &Fence{Lease: o.Lease, Holder: hostID, Epoch: r.Epoch}
	}
	wasKilled := false
	o, err = m.store.UpdateObject(ctx, id, fence, func(o *Object) error {
		if !o.Live() {
			wasKilled = true
			return nil
		}
		o.Status, o.NodeID, o.StableNodeID, o.MagicDNS, o.UpdatedAt = StatusReady, r.NodeID, r.StableNodeID, r.MagicDNS, m.now().UnixMilli()
		return nil
	})
	if err != nil {
		return o, err
	}
	m.custody.Delete(ctx, custodyName(o))
	if wasKilled {
		// Killed while joining: the device that just appeared must go too.
		o.NodeID, o.StableNodeID = r.NodeID, r.StableNodeID
		m.deleteDevice(ctx, o)
		return o, errf(410, "killed", "object %s was killed while joining; device deleted", id)
	}
	m.audit(ctx, AuditRecord{TenantID: o.TenantID, ObjectID: id, Kind: "ready"},
		map[string]any{"nodeID": r.NodeID, "stableNodeID": r.StableNodeID, "magicDNS": r.MagicDNS, "host": hostID})
	m.notify(id)
	return o, nil
}

// ---- kill ----

// KillRequest is POST /v0/objects/{id}/kill.
type KillRequest struct {
	Cascade bool   `json:"cascade"`
	Reason  string `json:"reason"`
	By      string `json:"-"`
}

// KillResult reports what a kill did. Pushed lists the objects whose host
// acknowledged the tier-1 push; DevicesDeleting those whose device delete
// is scheduled (Config.DeviceDeleteDelay).
type KillResult struct {
	Killed          []string          `json:"killed"`
	DevicesDeleted  []string          `json:"devicesDeleted"`
	DevicesDeleting []string          `json:"devicesDeleting,omitempty"`
	Pushed          []string          `json:"pushed,omitempty"`
	PushMS          float64           `json:"pushMs,omitempty"`
	Errors          map[string]string `json:"errors,omitempty"`
}

// Kill marks the object (and with cascade, every descendant) killed, pushes
// the kill to the lease-holding host (tier 1, immediate; the heartbeat's
// revokeObjects is the fallback), deletes devices via the tenant API (tier
// 2; deferred by DeviceDeleteDelay), revokes unused keys, wipes custody, and
// records audit.
func (m *Mgmt) Kill(ctx context.Context, id string, r KillRequest) (KillResult, error) {
	res := KillResult{Killed: []string{}, DevicesDeleted: []string{}, Errors: map[string]string{}}
	root, err := m.store.GetObject(ctx, id)
	if err != nil {
		return res, err
	}
	targets := []Object{root}
	if r.Cascade {
		seen := map[string]bool{root.ID: true}
		for i := 0; i < len(targets); i++ {
			kids, err := m.store.ListObjects(ctx, ObjectFilter{ParentID: targets[i].ID})
			if err != nil {
				return res, err
			}
			for _, k := range kids {
				if !seen[k.ID] {
					seen[k.ID] = true
					targets = append(targets, k)
				}
			}
		}
	}
	now := m.now()
	type killedObj struct {
		o    Object
		prev string
	}
	var done []killedObj
	byHost := map[string][]string{}
	for _, o := range targets {
		if !o.Live() {
			continue
		}
		prev := o.Status
		killed, err := m.store.UpdateObject(ctx, o.ID, nil, func(x *Object) error {
			x.Status, x.KillReason, x.UpdatedAt = StatusKilled, r.Reason, now.UnixMilli()
			return nil
		})
		if err != nil {
			res.Errors[o.ID] = err.Error()
			continue
		}
		res.Killed = append(res.Killed, o.ID)
		m.notify(o.ID)
		if l, err := m.store.GetLease(ctx, o.Lease); err == nil && l.Holder != "" {
			m.store.AddRevocation(ctx, l.Holder, o.ID, now)
			byHost[l.Holder] = append(byHost[l.Holder], o.ID)
		}
		done = append(done, killedObj{killed, prev})
	}
	// Tier 1: push to the hosts, all objects in parallel, before any device
	// delete (the RST must leave while the node is still in peers' netmaps).
	ps := time.Now()
	var pmu sync.Mutex
	var wg sync.WaitGroup
	for host, ids := range byHost {
		for _, oid := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := m.push(host, "/v0/push/kill", map[string]any{"objectID": oid, "reason": r.Reason}); err == nil {
					pmu.Lock()
					res.Pushed = append(res.Pushed, oid)
					pmu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	if len(byHost) > 0 {
		res.PushMS = float64(time.Since(ps).Microseconds()) / 1000
	}
	for _, k := range done {
		o, killed, prev := k.o, k.o, k.prev
		m.custody.Delete(ctx, custodyName(o))
		if prev != StatusReady {
			m.revokeKey(ctx, killed)
		}
		deleted, scheduled := false, false
		if killed.StableNodeID != "" || killed.NodeID != "" {
			if m.cfg.DeviceDeleteDelay > 0 {
				m.scheduleDelete(killed)
				scheduled = true
				res.DevicesDeleting = append(res.DevicesDeleting, o.ID)
			} else if err := m.deleteDevice(ctx, killed); err != nil {
				res.Errors[o.ID] = "device delete: " + err.Error()
			} else {
				deleted = true
				res.DevicesDeleted = append(res.DevicesDeleted, o.ID)
			}
		}
		detail := map[string]any{"reason": r.Reason, "cascade": r.Cascade, "requested": id, "root": o.RootID,
			"parent": o.ParentID, "deviceDeleted": deleted, "prevStatus": prev, "pushed": slices.Contains(res.Pushed, o.ID)}
		if scheduled {
			detail["deviceDeleteIn"] = m.cfg.DeviceDeleteDelay.String()
		}
		m.audit(ctx, AuditRecord{TenantID: o.TenantID, ObjectID: o.ID, Kind: "kill", ActingFor: r.By}, detail)
	}
	if len(res.Errors) == 0 {
		res.Errors = nil
	}
	return res, nil
}

// push POSTs body to the host's push socket (best effort; bounded by
// PushTimeout). Hosts without a push URL get nothing (heartbeat fallback).
func (m *Mgmt) push(hostID, path string, body any) error {
	m.mu.Lock()
	u := m.pushURLs[hostID]
	m.mu.Unlock()
	if u == "" {
		return errors.New("host has no push URL")
	}
	sock := strings.TrimPrefix(u, "unix://")
	hc := &http.Client{Timeout: m.cfg.PushTimeout, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	defer hc.CloseIdleConnections()
	b, _ := json.Marshal(body)
	resp, err := hc.Post("http://hostd"+path, "application/json", bytes.NewReader(b))
	if err != nil {
		m.log.Debug("push failed", "host", hostID, "path", path, "err", err)
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("push %s: %s", path, resp.Status)
	}
	return nil
}

type pendingDelete struct {
	o     Object
	timer *time.Timer
}

func (m *Mgmt) scheduleDelete(o Object) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pending[o.ID]; ok {
		return
	}
	pd := &pendingDelete{o: o}
	pd.timer = time.AfterFunc(m.cfg.DeviceDeleteDelay, func() { m.runDelete(o.ID) })
	m.pending[o.ID] = pd
}

// runDelete performs a scheduled device delete (once).
func (m *Mgmt) runDelete(id string) {
	m.mu.Lock()
	pd := m.pending[id]
	delete(m.pending, id)
	m.mu.Unlock()
	if pd == nil {
		return
	}
	pd.timer.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st := time.Now()
	err := m.deleteDevice(ctx, pd.o)
	detail := map[string]any{"nodeID": pd.o.StableNodeID, "ms": time.Since(st).Milliseconds()}
	if err != nil {
		detail["error"] = err.Error()
		m.log.Warn("device delete failed", "object", id, "err", err)
	}
	m.audit(ctx, AuditRecord{TenantID: pd.o.TenantID, ObjectID: id, Kind: "device_delete"}, detail)
}

// PendingDeletes is how many device deletes are scheduled.
func (m *Mgmt) PendingDeletes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}

// FlushDeletes runs every scheduled device delete now (in parallel).
func (m *Mgmt) FlushDeletes() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.pending))
	for id := range m.pending {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() { defer wg.Done(); m.runDelete(id) }()
	}
	wg.Wait()
}

func (m *Mgmt) revokeKey(ctx context.Context, o Object) {
	if o.KeyID == "" {
		return
	}
	t, err := m.store.GetTenant(ctx, o.TenantID)
	if err != nil {
		return
	}
	if api, err := m.broker.API(ctx, t, purposeMint); err == nil {
		if err := api.DeleteKey(ctx, o.KeyID); err != nil {
			m.log.Warn("key revoke failed", "object", o.ID, "key_id", o.KeyID, "err", err)
		}
	}
}

func (m *Mgmt) deleteDevice(ctx context.Context, o Object) error {
	dev := o.StableNodeID
	if dev == "" {
		dev = o.NodeID
	}
	if dev == "" {
		return nil
	}
	t, err := m.store.GetTenant(ctx, o.TenantID)
	if err != nil {
		return err
	}
	api, err := m.broker.API(ctx, t, purposeLifecycle)
	if err != nil {
		return err
	}
	return api.DeleteDevice(ctx, dev)
}

// ---- reaper ----

// Reap kills (cascade) every live object past its TTL and every live
// sub-agent whose parent is dead. It returns how many it killed.
func (m *Mgmt) Reap(ctx context.Context) (int, error) {
	live, err := m.store.ListObjects(ctx, ObjectFilter{Status: []string{StatusPending, StatusMinted, StatusReady}})
	if err != nil {
		return 0, err
	}
	now := m.now().UnixMilli()
	n := 0
	for _, o := range live {
		reason := ""
		if o.TTLSeconds > 0 && now >= o.CreatedAt+o.TTLSeconds*1000 {
			reason = "ttl_expired"
		} else if o.ParentID != "" {
			p, err := m.store.GetObject(ctx, o.ParentID)
			if errors.Is(err, ErrNotFound) || (err == nil && !p.Live()) {
				reason = "parent_dead"
			}
		}
		if reason == "" {
			continue
		}
		cur, err := m.store.GetObject(ctx, o.ID)
		if err != nil || !cur.Live() {
			continue // killed by an earlier cascade in this pass
		}
		res, err := m.Kill(ctx, o.ID, KillRequest{Cascade: true, Reason: reason, By: "twd-reaper"})
		if err != nil {
			return n, err
		}
		n += len(res.Killed)
	}
	return n, nil
}

// ---- audit ingest / query ----

// IngestAudit stores host-submitted records on the host's chain. Each must
// carry hash (and prevHash); the response reports whether they link.
func (m *Mgmt) IngestAudit(ctx context.Context, hostID string, recs []json.RawMessage) (int, ChainStatus, error) {
	chain := "host:" + hostID
	for i, raw := range recs {
		var r struct {
			TS        any    `json:"ts"`
			ObjectID  string `json:"objectID"`
			Kind      string `json:"kind"`
			TraceID   string `json:"traceID"`
			ActingFor string `json:"actingFor"`
			PrevHash  string `json:"prevHash"`
			Hash      string `json:"hash"`
			HostSig   string `json:"hostSig"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return i, ChainStatus{}, errf(400, "bad_request", "record %d: %v", i, err)
		}
		if r.Hash == "" || r.Kind == "" {
			return i, ChainStatus{}, errf(400, "bad_request", "record %d: kind and hash are required", i)
		}
		canon, err := Canonicalize(raw)
		if err != nil {
			return i, ChainStatus{}, errf(400, "bad_request", "record %d: %v", i, err)
		}
		tenant := ""
		if o, err := m.store.GetObject(ctx, r.ObjectID); err == nil {
			tenant = o.TenantID
		}
		if _, err := m.store.AppendAudit(ctx, AuditRecord{Chain: chain, TS: fmt.Sprint(r.TS), TenantID: tenant, ObjectID: r.ObjectID,
			Kind: r.Kind, TraceID: r.TraceID, ActingFor: r.ActingFor, PrevHash: r.PrevHash, Hash: r.Hash, HostSig: r.HostSig,
			Raw: canon}); err != nil {
			return i, ChainStatus{}, err
		}
	}
	return len(recs), m.VerifyChain(ctx, chain), nil
}

// VerifyChain verifies one chain (host chains with the host's key, if registered).
func (m *Mgmt) VerifyChain(ctx context.Context, chain string) ChainStatus {
	var pub []byte
	if h, ok := strings.CutPrefix(chain, "host:"); ok {
		if host, err := m.store.GetHost(ctx, h); err == nil && host.AuditPubKey != "" {
			pub, _ = decodeB64(host.AuditPubKey)
		}
	}
	return VerifyChain(ctx, m.store, chain, pub)
}

// VerifyAll verifies every chain.
func (m *Mgmt) VerifyAll(ctx context.Context) ([]ChainStatus, bool, error) {
	chains, err := m.store.AuditChains(ctx)
	if err != nil {
		return nil, false, err
	}
	out, ok := []ChainStatus{}, true
	for _, c := range chains {
		st := m.VerifyChain(ctx, c)
		ok = ok && st.OK
		out = append(out, st)
	}
	return out, ok, nil
}

// QueryAudit returns matching records plus the status of every chain they are on.
func (m *Mgmt) QueryAudit(ctx context.Context, f AuditFilter) ([]AuditRecord, []ChainStatus, error) {
	recs, err := m.store.ListAudit(ctx, f)
	if err != nil {
		return nil, nil, err
	}
	var chains []string
	for _, r := range recs {
		if !slices.Contains(chains, r.Chain) {
			chains = append(chains, r.Chain)
		}
	}
	sts := []ChainStatus{}
	for _, c := range chains {
		sts = append(sts, m.VerifyChain(ctx, c))
	}
	if recs == nil {
		recs = []AuditRecord{}
	}
	return recs, sts, nil
}

// ---- deploy preflight ----

// PreflightRequest asks whether the tenant policy lets a workload's tags
// reach its declared dependencies (twd.jsonc tailnet.connects) and whether
// every tag it will mint is in tagOwners.
type PreflightRequest struct {
	SrcTags  []string `json:"srcTags"`  // workload + object tags
	Connects []string `json:"connects"` // tags ("tag:db") or "*"; names are reported unchecked
	Port     string   `json:"port,omitempty"`
}

// PreflightMissing is one missing grant.
type PreflightMissing struct {
	Src     string `json:"src"`
	Dst     string `json:"dst"`
	Snippet string `json:"snippet"`
}

// PreflightResult is the preflight verdict.
type PreflightResult struct {
	OK               bool               `json:"ok"`
	Missing          []PreflightMissing `json:"missing"`
	MissingTagOwners []string           `json:"missingTagOwners"`
	TagOwnersSnippet string             `json:"tagOwnersSnippet,omitempty"`
	Unchecked        []string           `json:"unchecked,omitempty"`
}

// Preflight checks the request against the tenant's current policy (read
// through the policy cache; never written).
func (m *Mgmt) Preflight(ctx context.Context, tenantID string, r PreflightRequest) (PreflightResult, error) {
	res := PreflightResult{Missing: []PreflightMissing{}, MissingTagOwners: []string{}}
	t, err := m.store.GetTenant(ctx, tenantID)
	if err != nil {
		return res, errf(404, "unknown_tenant", "tenant %q not found", tenantID)
	}
	pol, err := m.policies.Get(ctx, t)
	if err != nil {
		return res, errf(503, "policy_unavailable", "can't read the tenant policy: %v", err)
	}
	port := r.Port
	if port == "" {
		port = "tcp:80"
	}
	var owners []string
	for _, tag := range r.SrcTags {
		if _, ok := pol.TagOwners[tag]; !ok && !slices.Contains(res.MissingTagOwners, tag) {
			res.MissingTagOwners = append(res.MissingTagOwners, tag)
			owners = append(owners, fmt.Sprintf("%q: [\"autogroup:admin\", \"tag:twd\"]", tag))
		}
	}
	if len(owners) > 0 {
		res.TagOwnersSnippet = `"tagOwners": { ` + strings.Join(owners, ", ") + ` }`
	}
	for _, dst := range r.Connects {
		if dst != "*" && !strings.HasPrefix(dst, "tag:") && !strings.HasPrefix(dst, "autogroup:") {
			res.Unchecked = append(res.Unchecked, dst)
			continue
		}
		for _, src := range r.SrcTags {
			if !pol.HasIPGrant(src, dst) {
				res.Missing = append(res.Missing, PreflightMissing{Src: src, Dst: dst,
					Snippet: fmt.Sprintf(`{ "src": [%q], "dst": [%q], "ip": [%q] }`, src, dst, port)})
			}
		}
	}
	res.OK = len(res.Missing) == 0 && len(res.MissingTagOwners) == 0
	return res, nil
}
