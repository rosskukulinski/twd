// Package fakemgmt is an in-memory twd-mgmt implementing the subset of the
// mgmt /v1 contract (api/openapi.yaml) that hostd uses, for hostd tests
// and the e2e-real suite. Tests wrap it with the spec validator
// (mgmt/api/v1.Validator), so its shapes must match the spec. It is not production code: no persistence, no peer-cred
// checks, one host per deployment.
//
// Mint modes per tenant:
//   - "static": every credential is Config.StaticKey (testcontrol accepts any key);
//   - "apikey": single-use, pre-authorized, 5-minute tagged keys minted via
//     the Tailscale API (internal/tsapi) with the key in Tenant.APIKeyFile.
package fakemgmt

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/internal/nodecert"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/netd"
)

// Config configures a Server.
type Config struct {
	// StaticKey is returned by "static" tenants (tests against testcontrol).
	StaticKey string
	// ForceTags, if set, replaces every object's tags at mint time (e2e-real
	// uses tag:ttest only).
	ForceTags []string
	// NonEphemeral mints non-ephemeral keys (default: ephemeral).
	NonEphemeral bool
	// PolicyCaps are the caps "computed from policy" per tag, used as a
	// root object's effective caps for spawn checks ({cap: value}).
	PolicyCaps map[string]map[string]any
	// MaxLive and MaxDepth bound spawns (defaults 64 and 3).
	MaxLive, MaxDepth int
	// APIBaseURL overrides the Tailscale API base (tests).
	APIBaseURL string
	// NoPush disables the v0.1 push extension, so kills reach hosts only via
	// heartbeat revokeObjects (what M2's twd-mgmt does).
	NoPush bool
	// MaxContainers is every tenant's container quota (default 10).
	MaxContainers int
	Logger        *slog.Logger
}

// Server is the fake mgmt.
type Server struct {
	cfg Config
	log *slog.Logger
	mux *http.ServeMux

	mu          sync.Mutex
	tenants     map[string]*tenant
	hosts       map[string]*host
	deployments map[string]*deployment
	leases      map[string]*lease // tenantID + "|" + namespace
	objects     map[string]*object
	byName      map[string]string // tenant|workload|ns|name -> objectID
	audit       map[string][]mgmtapi.AuditRecord
	nodeIDs     map[string]string // stable node ID -> objectID (every node that reported ready)
	remints     int               // POST /remint calls (rejoins rejected by control)
	pushes      []string
	oboKey      ed25519.PrivateKey // one OBO root key for every tenant (node certs, RT-01)
	certDenied  map[string]bool    // stable node IDs whose cert lookup fails (tests: fail closed)
	volKeys     map[string][]byte  // tenant/workload -> DO volume data key
	// VolumeKeyCalls counts GET /v0/volumes/key (tests).
	VolumeKeyCalls       int
	leaseLostCredentials int // next N credential calls fail with lease_lost
	credentialCalls      int
	reservations         map[string]*mgmtapi.ContainerReservation // container reservations by ID
	reservationsDown     bool
}

// SetLeaseLostCredentials makes the next n credential calls fail with
// lease_lost (tests: a host whose lease epoch went stale mid-join).
func (s *Server) SetLeaseLostCredentials(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaseLostCredentials = n
}

// CredentialCalls counts GET /v0/objects/{id}/credential.
func (s *Server) CredentialCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credentialCalls
}

type tenant struct {
	mgmtapi.Tenant
	api    *tsapi.Client
	minter *tsapi.Minter
	bucket *bucket
}

type host struct {
	mgmtapi.RegisterRequest
	created     time.Time
	lastSeen    time.Time
	liveNodes   int
	usage       *mgmtapi.HostUsage
	running     []mgmtapi.RunningContainer // the last heartbeat's containers
	leases      []mgmtapi.Lease
	deployments map[string]mgmtapi.DeploymentStatus
	kills       []mgmtapi.KillOrder
}

type deployment struct {
	mgmtapi.DeployResponse
	tenantID   string
	workload   string
	bundle     []byte
	namespaces []mgmtapi.NamespaceSpec
	wlTags     []string
	epoch      int64
	superseded bool
	created    time.Time
}

type lease struct {
	host  string
	epoch int64
}

type object struct {
	mgmtapi.Object
	caps     map[string]any
	created  time.Time
	workload string
	killedAt time.Time
}

// New returns a Server.
func New(cfg Config) *Server {
	if cfg.MaxLive == 0 {
		cfg.MaxLive = 64
	}
	if cfg.MaxDepth == 0 {
		cfg.MaxDepth = 3
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := &Server{cfg: cfg, log: cfg.Logger, mux: http.NewServeMux(),
		tenants: map[string]*tenant{}, hosts: map[string]*host{}, deployments: map[string]*deployment{},
		leases: map[string]*lease{}, objects: map[string]*object{}, byName: map[string]string{},
		audit: map[string][]mgmtapi.AuditRecord{}, nodeIDs: map[string]string{}, volKeys: map[string][]byte{}, oboKey: priv, certDenied: map[string]bool{},
		reservations: map[string]*mgmtapi.ContainerReservation{}}
	m := s.mux
	m.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	m.HandleFunc("GET /v1/certificates/{nodeID}", s.nodeCert)
	m.HandleFunc("POST /v1/tenants", s.createTenant)
	m.HandleFunc("GET /v1/tenants", s.listTenants)
	m.HandleFunc("GET /v1/tenants/{id}", s.getTenant)
	m.HandleFunc("PUT /v1/hosts/{id}", s.register)
	m.HandleFunc("PUT /v1/hosts/{id}/status", s.heartbeat)
	m.HandleFunc("GET /v1/hosts/{id}/assignments", s.assignments)
	m.HandleFunc("POST /v1/deployments", s.deploy)
	m.HandleFunc("GET /v1/deployments", s.listDeployments)
	m.HandleFunc("GET /v1/deployments/{id}", s.getDeployment)
	m.HandleFunc("GET /v1/deployments/{id}/bundle", s.getBundle)
	m.HandleFunc("POST /v1/objects", s.createObject)
	m.HandleFunc("GET /v1/objects", s.listObjects)
	m.HandleFunc("GET /v1/objects/{id}", s.getObject)
	m.HandleFunc("POST /v1/objects/{id}/credentials", s.credential)
	m.HandleFunc("PATCH /v1/objects/{id}", s.ready)
	m.HandleFunc("POST /v1/volumes/{id}/keys", s.volumeKey)
	m.HandleFunc("POST /v1/container-reservations", s.reserveContainer)
	m.HandleFunc("DELETE /v1/container-reservations/{id}", s.releaseContainer)
	m.HandleFunc("DELETE /v1/objects/{id}", s.kill)
	m.HandleFunc("POST /v1/audit", s.postAudit)
	m.HandleFunc("GET /v1/audit", s.getAudit)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, 404, mgmtapi.CodeNotFound, r.Method+" "+r.URL.Path+" is not a /v1 route")
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// ---- helpers ----

func newID(prefix string) string {
	b := make([]byte, 6)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// page is the /v1 list envelope (fakemgmt never pages: one page, no cursor).
func page[T any](items []T) mgmtapi.Page[T] {
	if items == nil {
		items = []T{}
	}
	return mgmtapi.Page[T]{Items: items}
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Twd-Error", code)
	writeJSON(w, status, map[string]any{"code": code, "message": netd.Redact(msg), "trace_id": newID(""),
		"doc_url": netd.DocBase + code})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 128<<20)).Decode(v)
}

// ---- tenants ----

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	var t mgmtapi.Tenant
	if err := decode(r, &t); err != nil || t.Tailnet == "" {
		writeErr(w, 400, "bad_request", "tailnet required")
		return
	}
	tn := &tenant{Tenant: t}
	switch t.MintMode {
	case "static":
		if s.cfg.StaticKey == "" {
			writeErr(w, 400, "bad_request", "static mint mode needs Config.StaticKey")
			return
		}
	case "apikey":
		if t.APIKeyFile == "" {
			writeErr(w, 400, "bad_request", "apikey mode needs apiKeyFile")
			return
		}
		key, err := tsapi.LoadEnvFile(t.APIKeyFile, "TS_API_KEY")
		if err != nil {
			writeErr(w, 400, "bad_request", "apiKeyFile: "+err.Error())
			return
		}
		tn.api = &tsapi.Client{Tailnet: t.Tailnet, APIKey: key, BaseURL: s.cfg.APIBaseURL}
		tn.minter = tsapi.MintSource(tn.api, s.log)
	default:
		writeErr(w, 400, "bad_request", "mintMode "+t.MintMode+" is not supported by fakemgmt")
		return
	}
	if tn.RegistrationsPerMinute == 0 {
		tn.RegistrationsPerMinute = 200
	}
	if tn.MaxObjects == 0 {
		tn.MaxObjects = 1000
	}
	tn.bucket = newBucket(tn.RegistrationsPerMinute)
	tn.TenantID = newID("tn")
	tn.CreatedAt, tn.UpdatedAt = ms(time.Now()), ms(time.Now())
	s.mu.Lock()
	s.tenants[tn.TenantID] = tn
	pub := s.public(tn)
	s.mu.Unlock()
	w.Header().Set("Location", "/v1/tenants/"+tn.TenantID)
	writeJSON(w, 201, pub)
}

func (s *Server) public(t *tenant) mgmtapi.Tenant {
	p := t.Tenant
	p.APIKeyFile = ""
	p.OBOPublicKey = base64.StdEncoding.EncodeToString(s.oboKey.Public().(ed25519.PublicKey))
	return p
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := []mgmtapi.Tenant{}
	for _, t := range s.tenants {
		out = append(out, s.public(t))
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].TenantID < out[j].TenantID })
	writeJSON(w, 200, page(out))
}

func (s *Server) getTenant(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	t := s.tenants[r.PathValue("id")]
	s.mu.Unlock()
	if t == nil {
		writeErr(w, 404, mgmtapi.CodeNotFound, "no such tenant")
		return
	}
	writeJSON(w, 200, s.public(t))
}

// ---- hosts ----

// hostView is the /v1 Host resource.
func (s *Server) hostView(h *host) map[string]any {
	st := "live"
	if time.Since(h.lastSeen) > 10*time.Second {
		st = "stale"
	}
	v := map[string]any{"hostID": h.HostID, "status": st, "capacityNodes": h.CapacityNodes, "tiers": h.Tiers,
		"liveNodes": h.liveNodes, "lastSeen": ms(h.lastSeen), "createdAt": ms(h.created), "updatedAt": ms(h.lastSeen)}
	if h.AuditPubKey != "" {
		v["auditPubKey"] = h.AuditPubKey
	}
	if h.Arch != "" {
		v["arch"] = h.Arch
	}
	if h.Capacity != nil {
		v["capacity"] = h.Capacity
	}
	if h.usage != nil {
		v["usage"] = h.usage
	}
	return v
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		mgmtapi.RegisterRequest
		Push *struct {
			Port int    `json:"port"`
			URL  string `json:"url"`
		} `json:"push"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	req.HostID = r.PathValue("id")
	if req.Push != nil {
		req.PushURL, req.PushPort = req.Push.URL, req.Push.Port
	}
	s.mu.Lock()
	h := s.hosts[req.HostID]
	status := 200
	if h == nil {
		h = &host{deployments: map[string]mgmtapi.DeploymentStatus{}, created: time.Now()}
		s.hosts[req.HostID] = h
		status = 201
	}
	h.RegisterRequest, h.lastSeen = req.RegisterRequest, time.Now()
	v := s.hostView(h)
	s.mu.Unlock()
	writeJSON(w, status, v)
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req mgmtapi.HeartbeatRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hosts[id]
	if h == nil {
		writeErr(w, 404, mgmtapi.CodeNotFound, "host not registered")
		return
	}
	h.lastSeen, h.liveNodes, h.leases = time.Now(), req.LiveNodes, req.Leases
	h.usage, h.running = req.Usage, req.Containers
	for _, d := range req.Deployments {
		h.deployments[d.DeploymentID] = d
		if dep := s.deployments[d.DeploymentID]; dep != nil && !dep.superseded {
			dep.Status, dep.Error = d.Status, d.Error
		}
	}
	resp := mgmtapi.HeartbeatResponse{Revoke: []string{}, RevokeObjects: []string{}}
	h.kills = nil // kills reach hosts through revokeObjects below (and pushes)
	held := map[string]bool{}
	for _, l := range req.Leases {
		cur := s.leases[l.Namespace]
		if cur == nil || cur.host != id || cur.epoch != l.Epoch {
			resp.Revoke = append(resp.Revoke, l.Namespace)
		} else {
			held[l.Namespace] = true
		}
	}
	for _, o := range s.objects {
		if o.Status == "killed" && time.Since(o.killedAt) < 10*time.Minute && held[mgmtapi.LeaseName(o.TenantID, o.workload)] {
			resp.RevokeObjects = append(resp.RevokeObjects, o.ObjectID)
		}
	}
	writeJSON(w, 200, resp)
}

func (s *Server) assignments(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	out := []mgmtapi.Assignment{}
	for _, d := range s.sortedDeployments() {
		if d.Host != id || d.superseded {
			continue
		}
		out = append(out, mgmtapi.Assignment{DeploymentID: d.DeploymentID, TenantID: d.tenantID, Workload: d.workload,
			BundleURL: "/v1/deployments/" + d.DeploymentID + "/bundle", Namespaces: d.namespaces, Epoch: d.epoch,
			WorkloadTags: d.wlTags, Lease: mgmtapi.LeaseName(d.tenantID, d.workload),
			Tailnet: s.tenants[d.tenantID].Tailnet, ControlURL: s.tenants[d.tenantID].ControlURL, CreatedAt: ms(d.created)})
	}
	s.mu.Unlock()
	writeJSON(w, 200, page(out))
}

func (s *Server) sortedDeployments() []*deployment {
	ds := make([]*deployment, 0, len(s.deployments))
	for _, d := range s.deployments {
		ds = append(ds, d)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].created.Before(ds[j].created) })
	return ds
}

// ---- deployments ----

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	var req mgmtapi.DeployRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	files, err := bundle.Unpack(req.Bundle)
	if err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	app, err := bundle.Parse(files)
	if err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	if req.Workload == "" {
		req.Workload = app.Name
	}
	if !bundle.ValidWorkload(req.Workload) {
		writeErr(w, 400, "bad_request", "invalid workload name")
		return
	}
	s.mu.Lock()
	if s.tenants[req.TenantID] == nil {
		s.mu.Unlock()
		writeErr(w, 404, mgmtapi.CodeNotFound, "no such tenant")
		return
	}
	var hostID string
	ids := make([]string, 0, len(s.hosts))
	for id := range s.hosts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// MVP: the single host per tenant namespace; keep a workload where it is.
	for _, d := range s.deployments {
		if d.tenantID == req.TenantID && d.workload == req.Workload && !d.superseded {
			hostID = d.Host
		}
	}
	if hostID == "" && len(ids) > 0 {
		hostID = ids[0]
	}
	if hostID == "" {
		s.mu.Unlock()
		writeErr(w, 503, "no_host", "no host registered")
		return
	}
	// Acquire the namespace leases: epoch+1 for the workload and its classes.
	// One lease per (tenant, workload), named "<tenantID>/<workload>".
	var epoch int64
	wlKey := mgmtapi.LeaseName(req.TenantID, req.Workload)
	if l := s.leases[wlKey]; l != nil {
		epoch = l.epoch
	}
	epoch++
	nss := []mgmtapi.NamespaceSpec{}
	s.leases[wlKey] = &lease{hostID, epoch}
	for _, c := range app.ClassNames() {
		cl := app.Classes[c]
		tags := cl.Tags
		if tags == nil {
			tags = []string{}
		}
		nss = append(nss, mgmtapi.NamespaceSpec{Class: c, Tags: tags, Agent: cl.Agent})
	}
	for _, d := range s.deployments {
		if d.tenantID == req.TenantID && d.workload == req.Workload {
			d.superseded = true
		}
	}
	d := &deployment{tenantID: req.TenantID, workload: req.Workload, bundle: req.Bundle, namespaces: nss,
		wlTags: app.WorkloadTags, epoch: epoch, created: time.Now()}
	d.DeploymentID, d.Host, d.Status = newID("dp"), hostID, "assigned"
	s.deployments[d.DeploymentID] = d
	push := s.hosts[hostID].PushURL
	if s.cfg.NoPush {
		push = ""
	}
	resp := s.deploymentView(d)
	s.mu.Unlock()
	if push != "" {
		go s.push(push, "/v1/push/assignments", map[string]any{})
	}
	w.Header().Set("Location", "/v1/deployments/"+d.DeploymentID)
	writeJSON(w, 202, resp)
}

// deploymentView is the /v1 Deployment resource (s.mu held).
func (s *Server) deploymentView(d *deployment) mgmtapi.DeployResponse {
	resp := d.DeployResponse
	resp.TenantID, resp.Workload, resp.Epoch, resp.CreatedAt = d.tenantID, d.workload, d.epoch, ms(d.created)
	resp.Namespaces = d.namespaces
	if resp.Namespaces == nil {
		resp.Namespaces = []mgmtapi.NamespaceSpec{}
	}
	if d.superseded {
		resp.Status = "superseded"
	}
	tailnet := ""
	if t := s.tenants[d.tenantID]; t != nil {
		tailnet = t.Tailnet
	}
	resp.Endpoint = mgmtapi.Endpoint{Node: d.workload + "-" + d.Host, MagicDNS: d.workload + "-" + d.Host + "." + tailnet, Status: "pending"}
	if id, ok := s.byName[objKey(d.tenantID, d.workload, mgmtapi.WorkloadNamespace, d.workload)]; ok {
		if o := s.objects[id]; o != nil {
			resp.Endpoint.ObjectID, resp.Endpoint.Status = o.ObjectID, o.Status
			if o.MagicDNS != "" {
				resp.Endpoint.Node, resp.Endpoint.MagicDNS = strings.Split(o.MagicDNS, ".")[0], o.MagicDNS
			}
		}
	}
	return resp
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	out := []mgmtapi.DeployResponse{}
	for _, d := range s.sortedDeployments() {
		if tn := q.Get("tenant"); tn != "" && d.tenantID != tn {
			continue
		}
		if wl := q.Get("workload"); wl != "" && d.workload != wl {
			continue
		}
		if q.Get("latest") == "true" && d.superseded {
			continue
		}
		out = append(out, s.deploymentView(d))
	}
	s.mu.Unlock()
	writeJSON(w, 200, page(out))
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.deployments[r.PathValue("id")]
	if d == nil {
		writeErr(w, 404, mgmtapi.CodeNotFound, "no such deployment")
		return
	}
	writeJSON(w, 200, s.deploymentView(d))
}

func (s *Server) getBundle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	d := s.deployments[r.PathValue("id")]
	s.mu.Unlock()
	if d == nil {
		writeErr(w, 404, mgmtapi.CodeNotFound, "no such deployment")
		return
	}
	w.Header().Set("content-type", "application/x-tar")
	w.Write(d.bundle)
}

// ---- objects ----

// objKey is the object identity: (tenant, workload, namespace, name).
func objKey(tenant, workload, ns, name string) string {
	return tenant + "|" + workload + "|" + ns + "|" + name
}

func (s *Server) createObject(w http.ResponseWriter, r *http.Request) {
	var req mgmtapi.CreateObjectRequest
	if err := decode(r, &req); err != nil || req.TenantID == "" || req.Namespace == "" || req.ObjectName == "" {
		writeErr(w, 400, "bad_request", "tenantID, namespace and objectName are required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tenants[req.TenantID]
	if t == nil {
		writeErr(w, 404, mgmtapi.CodeNotFound, "no such tenant")
		return
	}
	wl := req.Workload
	if wl == "" && req.Parent != nil {
		if p := s.objects[req.Parent.ObjectID]; p != nil {
			wl = p.workload
		}
	}
	workload := s.workloadOf(req.TenantID, wl, req.Namespace, req.ObjectName)
	k := objKey(req.TenantID, workload, req.Namespace, req.ObjectName)
	if id, ok := s.byName[k]; ok && workload != "" {
		o := s.objects[id]
		if o.Status == "killed" {
			s.writeGone(w, o)
			return
		}
		writeJSON(w, 200, s.view(o))
		return
	}
	l := s.leases[mgmtapi.LeaseName(req.TenantID, workload)]
	if workload == "" || l == nil {
		writeErr(w, 404, "unknown_namespace", "no deployment of this tenant declares namespace "+req.Namespace)
		return
	}
	live := 0
	for _, o := range s.objects {
		if o.TenantID == req.TenantID && o.Status != "killed" {
			live++
		}
	}
	if live >= t.MaxObjects {
		writeErr(w, 429, "object_quota_exceeded", fmt.Sprintf("tenant has %d live objects (maxObjects %d)", live, t.MaxObjects))
		return
	}
	tags := s.namespaceTags(req.TenantID, workload, req.Namespace)
	now := time.Now()
	hn := req.Hostname
	if hn == "" {
		hn = newID("o")
	}
	o := &object{Object: mgmtapi.Object{ObjectID: newID("o"), TenantID: req.TenantID, Workload: workload, Namespace: req.Namespace,
		ObjectName: req.ObjectName, Hostname: hn, Lease: mgmtapi.LeaseName(req.TenantID, workload), Status: "pending", Tags: tags, Host: l.host,
		CreatedAt: ms(now), UpdatedAt: ms(now)}, created: now, workload: workload}
	for _, d := range s.deployments {
		if d.tenantID == req.TenantID && d.workload == workload && !d.superseded {
			o.DeploymentID = d.DeploymentID
		}
	}
	if len(req.Caps) > 0 && string(req.Caps) != "null" {
		if err := json.Unmarshal(req.Caps, &o.caps); err != nil {
			writeErr(w, 400, "bad_request", "caps must be a JSON object")
			return
		}
		o.Caps = req.Caps
	}
	if req.Parent != nil && req.Parent.ObjectID != "" {
		p := s.objects[req.Parent.ObjectID]
		if p == nil || p.Status == "killed" {
			writeErr(w, 403, "delegation_invalid", "parent object is unknown or killed")
			return
		}
		lineage := append(append([]string{}, p.Lineage...), p.ObjectID)
		if len(lineage) > s.cfg.MaxDepth {
			writeErr(w, 429, "spawn_budget_exceeded", fmt.Sprintf("spawn depth %d exceeds maxDepth %d", len(lineage), s.cfg.MaxDepth))
			return
		}
		root := lineage[0]
		desc := 0
		for _, x := range s.objects {
			if x.Status != "killed" && len(x.Lineage) > 0 && x.Lineage[0] == root {
				desc++
			}
		}
		if desc >= s.cfg.MaxLive {
			writeErr(w, 429, "spawn_budget_exceeded", fmt.Sprintf("%d live descendants of %s (maxLive %d)", desc, root, s.cfg.MaxLive))
			return
		}
		if pc := s.effectiveCaps(p); o.caps != nil && pc != nil {
			if bad := subset(o.caps, pc, ""); bad != "" {
				writeErr(w, 403, "delegation_invalid", "spawn caps exceed the parent's: "+bad)
				return
			}
		} else if pc := s.effectiveCaps(p); pc != nil {
			o.caps = pc // inherit
		}
		o.Parent, o.Lineage = p.ObjectID, lineage
		o.Ephemeral = req.TTL != ""
	}
	s.objects[o.ObjectID] = o
	s.byName[k] = o.ObjectID
	w.Header().Set("Location", "/v1/objects/"+o.ObjectID)
	writeJSON(w, 202, s.view(o))
}

// view is the /v1 Object resource: lineage is root → self (the internal
// Lineage holds the ancestors only).
func (s *Server) view(o *object) mgmtapi.Object {
	out := o.Object
	out.Lineage = append(append([]string{}, o.Lineage...), o.ObjectID)
	out.Root = out.Lineage[0]
	out.Depth = len(o.Lineage)
	if out.Tags == nil {
		out.Tags = []string{}
	}
	for _, x := range s.objects {
		if x.Parent == o.ObjectID {
			out.Children = append(out.Children, x.ObjectID)
		}
	}
	sort.Strings(out.Children)
	return out
}

// writeGone is the 410 tombstone (s.mu held).
func (s *Server) writeGone(w http.ResponseWriter, o *object) {
	root := o.ObjectID
	if len(o.Lineage) > 0 {
		root = o.Lineage[0]
	}
	w.Header().Set("Twd-Error", mgmtapi.CodeKilled)
	writeJSON(w, 410, map[string]any{"code": mgmtapi.CodeKilled, "message": "object " + o.ObjectID + " was killed", "trace_id": newID(""),
		"doc_url": netd.DocBase + mgmtapi.CodeKilled,
		"tombstone": map[string]any{"objectID": o.ObjectID, "tenantID": o.TenantID, "workload": o.workload, "namespace": o.Namespace,
			"objectName": o.ObjectName, "killedAt": ms(o.killedAt), "reason": o.KillReason, "by": "mgmt", "cascade": false, "root": root,
			"deviceDeleted": o.NodeID != ""}})
}

// workloadOf finds the (live) deployment declaring namespace: a DO class,
// or "_workload" whose object name is the workload. With workload set,
// only that workload's deployment counts.
func (s *Server) workloadOf(tenantID, workload, ns, name string) string {
	for _, d := range s.sortedDeployments() {
		if d.tenantID != tenantID || d.superseded || (workload != "" && d.workload != workload) {
			continue
		}
		if ns == mgmtapi.WorkloadNamespace && name == d.workload {
			return d.workload
		}
		for _, n := range d.namespaces {
			if n.Class == ns {
				return d.workload
			}
		}
	}
	return ""
}

// effectiveCaps: spawn-time caps if set, else caps computed from policy for
// its tags; nil means unrestricted (no policy information).
func (s *Server) effectiveCaps(o *object) map[string]any {
	if o.caps != nil {
		return o.caps
	}
	var out map[string]any
	for _, t := range o.Tags {
		for k, v := range s.cfg.PolicyCaps[t] {
			if out == nil {
				out = map[string]any{}
			}
			out[k] = v
		}
	}
	return out
}

// subset returns "" if child ⊆ parent: objects key-wise, arrays element-wise,
// numbers ≤, other scalars equal. parent nil = unrestricted.
func subset(child, parent any, path string) string {
	if parent == nil {
		return ""
	}
	switch c := child.(type) {
	case map[string]any:
		p, ok := parent.(map[string]any)
		if !ok {
			return path + " (parent is not an object)"
		}
		for k, v := range c {
			pv, ok := p[k]
			if !ok {
				return path + "." + k + " (parent has none)"
			}
			if bad := subset(v, pv, path+"."+k); bad != "" {
				return bad
			}
		}
	case []any:
		p, ok := parent.([]any)
		if !ok {
			return path + " (parent is not a list)"
		}
		for _, v := range c {
			found := false
			for _, pv := range p {
				if subset(v, pv, path) == "" {
					found = true
					break
				}
			}
			if !found {
				b, _ := json.Marshal(v)
				return path + " ∋ " + string(b)
			}
		}
	case float64:
		if p, ok := parent.(float64); !ok || c > p {
			return fmt.Sprintf("%s = %v > %v", path, c, parent)
		}
	default:
		cb, _ := json.Marshal(child)
		pb, _ := json.Marshal(parent)
		if !bytes.Equal(cb, pb) {
			return path + " = " + string(cb)
		}
	}
	return ""
}

func (s *Server) namespaceTags(tenantID, workload, ns string) []string {
	var tags []string
	for _, d := range s.deployments {
		if d.tenantID != tenantID || d.workload != workload || d.superseded {
			continue
		}
		if ns == mgmtapi.WorkloadNamespace {
			tags = d.wlTags
		}
		for _, n := range d.namespaces {
			if ns == n.Class {
				tags = n.Tags
			}
		}
	}
	if len(s.cfg.ForceTags) > 0 {
		tags = s.cfg.ForceTags
	}
	if tags == nil {
		tags = []string{}
	}
	return tags
}

func (s *Server) listObjects(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tn, ns, wl := q.Get("tenant"), q.Get("namespace"), q.Get("workload")
	s.mu.Lock()
	out := []mgmtapi.Object{}
	for _, o := range s.objects {
		if (tn == "" || o.TenantID == tn) && (ns == "" || o.Namespace == ns) && (wl == "" || o.workload == wl) {
			out = append(out, s.view(o))
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ObjectID < out[j].ObjectID
	})
	writeJSON(w, 200, page(out))
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.objects[r.PathValue("id")]
	if o == nil {
		writeErr(w, 404, mgmtapi.CodeNotFound, "no such object")
		return
	}
	if o.Status == "killed" {
		s.writeGone(w, o)
		return
	}
	writeJSON(w, 200, s.view(o))
}

// volumeKey releases a workload's DO volume key to the lease holder at
// the lease's epoch (mgmt.VolumeKey's contract).
// ---- container reservations (ADR-032 D9; the fake checks the lease and
// maxContainers only, without the store clock) ----

// SetReservationsDown makes the reservation API answer 503 (tests: mgmt
// unreachable for containers).
func (s *Server) SetReservationsDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reservationsDown = down
}

// HostReport is what host id last registered and reported: its
// registration (arch, tiers, capacity), usage and running containers.
func (s *Server) HostReport(id string) (mgmtapi.RegisterRequest, *mgmtapi.HostUsage, []mgmtapi.RunningContainer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hosts[id]
	if h == nil {
		return mgmtapi.RegisterRequest{}, nil, nil, false
	}
	return h.RegisterRequest, h.usage, append([]mgmtapi.RunningContainer(nil), h.running...), true
}

// Reservations returns the reservations held, by ID.
func (s *Server) Reservations() map[string]mgmtapi.ContainerReservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]mgmtapi.ContainerReservation{}
	for id, r := range s.reservations {
		out[id] = *r
	}
	return out
}

func (s *Server) reserveContainer(w http.ResponseWriter, r *http.Request) {
	var req mgmtapi.ContainerReservationRequest
	if err := decode(r, &req); err != nil || req.TenantID == "" || req.Workload == "" || req.Class == "" || req.Object == "" {
		writeErr(w, 400, "bad_request", "tenantID, workload, class and object are required")
		return
	}
	host := r.Header.Get("Twd-Host-ID")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reservationsDown {
		writeErr(w, 503, "store_unavailable", "reservations are down (test)")
		return
	}
	if l := s.leases[mgmtapi.LeaseName(req.TenantID, req.Workload)]; l == nil || l.epoch != req.Epoch || l.host != host {
		writeErr(w, 409, mgmtapi.CodeLeaseLost, "the workload's lease is not held by "+host)
		return
	}
	n := 0
	for _, x := range s.reservations {
		if x.TenantID == req.TenantID && x.Workload == req.Workload && x.Class == req.Class && x.Object == req.Object {
			x.Host, x.RenewedAt = host, ms(time.Now())
			writeJSON(w, 200, x)
			return
		}
		if x.TenantID == req.TenantID {
			n++
		}
	}
	max := s.cfg.MaxContainers
	if max == 0 {
		max = 10
	}
	if n+1 > max {
		w.Header().Set("Twd-Error", "container_quota_exceeded")
		writeJSON(w, 429, map[string]any{"code": "container_quota_exceeded", "message": fmt.Sprintf("tenant %s runs %d containers (maxContainers %d)", req.TenantID, n, max),
			"missing": map[string]any{"quota": "maxContainers", "limit": max, "used": n, "requested": 1}, "doc_url": "https://twd.dev/errors/container_quota_exceeded", "trace_id": "fake"})
		return
	}
	b := make([]byte, 6)
	rand.Read(b)
	x := &mgmtapi.ContainerReservation{ReservationID: "cr_" + hex.EncodeToString(b), TenantID: req.TenantID, Workload: req.Workload, Class: req.Class,
		Object: req.Object, Host: host, Resources: req.Resources, CreatedAt: ms(time.Now()), RenewedAt: ms(time.Now())}
	s.reservations[x.ReservationID] = x
	w.Header().Set("Location", "/v1/container-reservations/"+x.ReservationID)
	writeJSON(w, 201, x)
}

func (s *Server) releaseContainer(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reservationsDown {
		writeErr(w, 503, "store_unavailable", "reservations are down (test)")
		return
	}
	x := s.reservations[r.PathValue("id")]
	if x == nil {
		writeErr(w, 404, mgmtapi.CodeNotFound, "no such reservation")
		return
	}
	if host := r.Header.Get("Twd-Host-ID"); host != "" && host != x.Host {
		writeErr(w, 403, "forbidden", "held by another host")
		return
	}
	delete(s.reservations, x.ReservationID)
	w.WriteHeader(204)
}

func (s *Server) volumeKey(w http.ResponseWriter, r *http.Request) {
	epoch, _ := strconv.ParseInt(r.URL.Query().Get("epoch"), 10, 64)
	tenantID, workload, ok := strings.Cut(r.PathValue("id"), ":")
	if !ok {
		writeErr(w, 400, "bad_request", "volume ID must be <tenantID>:<workload>")
		return
	}
	name := mgmtapi.LeaseName(tenantID, workload)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.VolumeKeyCalls++
	l := s.leases[name]
	if l == nil || l.epoch != epoch {
		writeErr(w, 409, mgmtapi.CodeLeaseLost, fmt.Sprintf("lease %s is not held at epoch %d", name, epoch))
		return
	}
	if hid := r.Header.Get("Twd-Host-ID"); hid != "" && hid != l.host {
		writeErr(w, 409, mgmtapi.CodeLeaseLost, "host "+hid+" does not hold the lease")
		return
	}
	k := s.volKeys[name]
	if k == nil {
		k = make([]byte, 32)
		rand.Read(k)
		s.volKeys[name] = k
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 201, map[string]any{"volumeID": r.PathValue("id"), "tenantID": tenantID, "workload": workload, "epoch": epoch,
		"key": k, "grantedAt": ms(time.Now())})
}

// VolumeKeyOf returns the volume key fakemgmt holds for (tenant, workload)
// (tests: the no-keys-in-guest scan's needle).
func (s *Server) VolumeKeyOf(tenantID, workload string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.volKeys[mgmtapi.LeaseName(tenantID, workload)]
}

func (s *Server) credential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	epoch, _ := strconv.ParseInt(r.URL.Query().Get("epoch"), 10, 64)
	remint := r.URL.Query().Get("reason") == "rejoin_rejected"
	s.mu.Lock()
	o := s.objects[id]
	if o == nil {
		s.mu.Unlock()
		writeErr(w, 404, mgmtapi.CodeNotFound, "no such object")
		return
	}
	if o.Status == "killed" {
		s.writeGone(w, o)
		s.mu.Unlock()
		return
	}
	if remint {
		// The host's rejoin was rejected by control: back to pending, same
		// hostname, a fresh key below.
		delete(s.nodeIDs, o.NodeID)
		o.Status, o.NodeID, o.StableNodeID, o.MagicDNS = "pending", "", "", ""
		s.remints++
	}
	s.credentialCalls++
	if s.leaseLostCredentials > 0 {
		s.leaseLostCredentials--
		s.mu.Unlock()
		writeErr(w, 409, mgmtapi.CodeLeaseLost, "the caller does not hold the namespace lease at this epoch (test knob)")
		return
	}
	l := s.leases[mgmtapi.LeaseName(o.TenantID, o.workload)]
	if hid := r.Header.Get("Twd-Host-ID"); l != nil && hid != "" && hid != l.host {
		s.mu.Unlock()
		writeErr(w, 409, mgmtapi.CodeLeaseLost, "host "+hid+" does not hold the lease")
		return
	}
	if l == nil || l.epoch != epoch {
		s.mu.Unlock()
		writeErr(w, 409, mgmtapi.CodeLeaseLost, fmt.Sprintf("namespace %s lease is not held at epoch %d", o.Namespace, epoch))
		return
	}
	t := s.tenants[o.TenantID]
	tags, hostname := o.Tags, o.Hostname
	s.mu.Unlock()
	if err := t.bucket.wait(r.Context()); err != nil {
		w.Header().Set("Retry-After", "1")
		writeErr(w, 503, mgmtapi.CodeIdentityPending, "registration throttle: "+err.Error())
		return
	}
	cred := mgmtapi.Credential{ExpiresAt: ms(time.Now().Add(5 * time.Minute)), Tags: tags, Hostname: hostname,
		Ephemeral: !s.cfg.NonEphemeral, ControlURL: t.ControlURL, Reminted: remint}
	if cred.Tags == nil {
		cred.Tags = []string{}
	}
	switch t.MintMode {
	case "static":
		cred.AuthKey = s.cfg.StaticKey
	case "apikey":
		k, err := t.minter.AuthKey(r.Context(), netd.KeyRequest{ObjectID: id, Tags: tags, Ephemeral: !s.cfg.NonEphemeral})
		if err != nil {
			writeErr(w, 502, "bootstrap_failed", "mint: "+err.Error())
			return
		}
		cred.AuthKey = k
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 201, cred)
}

// ready is PATCH /v1/objects/{id} {"status":"ready", …} (the host's
// merge patch; the admin's {ttl} is not modelled by the fake).
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status string `json:"status"`
		mgmtapi.ReadyRequest
	}
	if err := decode(r, &req); err != nil || req.Status != "ready" {
		writeErr(w, 400, "bad_request", "fakemgmt: the patch must be {status: ready, nodeID, stableNodeID, magicDNS, epoch}")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.objects[r.PathValue("id")]
	if o == nil {
		writeErr(w, 404, mgmtapi.CodeNotFound, "no such object")
		return
	}
	if o.Status == "killed" {
		s.writeGone(w, o)
		return
	}
	o.Status, o.NodeID, o.StableNodeID, o.MagicDNS, o.UpdatedAt = "ready", req.StableNodeID, req.StableNodeID, req.MagicDNS, ms(time.Now())
	if req.StableNodeID != "" {
		s.nodeIDs[req.StableNodeID] = o.ObjectID
	}
	writeJSON(w, 200, s.view(o))
}

// kill is DELETE /v1/objects/{id}?cascade=&reason= (202 KillResult).
func (s *Server) kill(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	o := s.objects[r.PathValue("id")]
	if o != nil && o.Status == "killed" {
		s.writeGone(w, o)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	killed, err := s.Kill(r.Context(), r.PathValue("id"), q.Get("cascade") == "true", q.Get("reason"))
	if err != nil {
		writeErr(w, 404, mgmtapi.CodeNotFound, err.Error())
		return
	}
	if killed == nil {
		killed = []string{}
	}
	writeJSON(w, 202, mgmtapi.KillResponse{Killed: killed, DevicesDeleted: []string{}})
}

// Kill marks an object (and with cascade, its descendants) killed, pushes
// the kill to its host (and queues it for the next heartbeat), deletes the
// devices of apikey tenants, and records audit.
func (s *Server) Kill(ctx context.Context, id string, cascade bool, reason string) ([]string, error) {
	s.mu.Lock()
	o := s.objects[id]
	if o == nil {
		s.mu.Unlock()
		return nil, errors.New("no such object")
	}
	targets := []*object{o}
	if cascade {
		for _, x := range s.objects {
			for _, a := range x.Lineage {
				if a == id {
					targets = append(targets, x)
				}
			}
		}
	}
	type del struct {
		t    *tenant
		node string
	}
	var dels []del
	var killed []string
	pushes := map[string][]mgmtapi.KillOrder{}
	for _, t := range targets {
		if t.Status == "killed" {
			continue
		}
		t.Status, t.killedAt, t.KillReason, t.UpdatedAt = "killed", time.Now(), reason, ms(time.Now())
		killed = append(killed, t.ObjectID)
		ko := mgmtapi.KillOrder{ObjectID: t.ObjectID, Reason: reason}
		if h := s.hosts[t.Host]; h != nil {
			h.kills = append(h.kills, ko)
			if h.PushURL != "" && !s.cfg.NoPush {
				pushes[h.PushURL] = append(pushes[h.PushURL], ko)
			}
		}
		if tn := s.tenants[t.TenantID]; tn != nil && tn.api != nil && t.NodeID != "" {
			dels = append(dels, del{tn, t.NodeID})
		}
		rec := mgmtapi.AuditRecord{TS: time.Now().UTC().Format(time.RFC3339Nano), ObjectID: t.ObjectID, Kind: "kill",
			Detail: mgmtapi.Note(reason), TraceID: newID(""), Source: "mgmt"}
		s.appendAudit("mgmt", rec)
	}
	s.mu.Unlock()
	// Push first (tier 1 on the host is the fast path), then delete devices.
	var wg sync.WaitGroup
	for u, kos := range pushes {
		for _, ko := range kos {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.push(u, "/v1/push/kill", ko)
			}()
		}
	}
	wg.Wait()
	for _, d := range dels {
		if err := d.t.api.DeleteDevice(ctx, d.node); err != nil {
			s.log.Warn("device delete failed", "node", d.node, "err", netd.RedactError(err))
		}
	}
	return killed, nil
}

// ---- audit ----

func (s *Server) appendAudit(chain string, r mgmtapi.AuditRecord) {
	recs := s.audit[chain]
	prev := ""
	if len(recs) > 0 {
		prev = recs[len(recs)-1].Hash
	}
	r.PrevHash = prev
	r.Hash = mgmtapi.ChainHash(prev, r)
	s.audit[chain] = append(recs, r)
}

func (s *Server) postAudit(w http.ResponseWriter, r *http.Request) {
	var b mgmtapi.AuditBatch
	if err := decode(r, &b); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	// RT-02 provenance contract, as twd-mgmt enforces it.
	for i, rec := range b.Records {
		if (rec.Source == "tenant") != (rec.Kind == "tenant_note") || rec.Source == "mgmt" {
			writeErr(w, 400, "bad_request", fmt.Sprintf("record %d: source %q with kind %q violates the audit provenance contract", i, rec.Source, rec.Kind))
			return
		}
	}
	host := "host:" + r.Header.Get("Twd-Host-ID")
	s.mu.Lock()
	s.audit[host] = append(s.audit[host], b.Records...)
	n := len(s.audit[host])
	ok, _ := mgmtapi.VerifyChain(s.audit[host])
	s.mu.Unlock()
	writeJSON(w, 201, map[string]any{"accepted": len(b.Records), "chain": mgmtapi.ChainStatus{Chain: host, OK: ok, Records: n}})
}

func (s *Server) getAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := mgmtapi.AuditResponse{Items: []mgmtapi.AuditRecord{}, Chains: []mgmtapi.ChainStatus{}, Verified: true}
	chains := make([]string, 0, len(s.audit))
	for c := range s.audit {
		chains = append(chains, c)
	}
	sort.Strings(chains)
	for _, c := range chains {
		recs := s.audit[c]
		ok, probs := mgmtapi.VerifyChain(recs)
		st := mgmtapi.ChainStatus{Chain: c, OK: ok, Records: len(recs)}
		if !ok {
			resp.Verified = false
			st.Error = strings.Join(probs, "; ")
		}
		resp.Chains = append(resp.Chains, st)
		for i, rec := range recs {
			rec.Chain, rec.Seq = c, int64(i+1)
			if rec.Source == "" {
				rec.Source = "host"
			}
			if obj := q.Get("object"); obj != "" && rec.ObjectID != obj {
				o := s.objects[rec.ObjectID]
				if o == nil || !contains(o.Lineage, obj) {
					continue
				}
			}
			if h := q.Get("human"); h != "" && rec.ActingFor != h {
				continue
			}
			if tr := q.Get("trace"); tr != "" && rec.TraceID != tr {
				continue
			}
			resp.Items = append(resp.Items, rec)
		}
	}
	sort.SliceStable(resp.Items, func(i, j int) bool { return resp.Items[i].TS < resp.Items[j].TS })
	writeJSON(w, 200, resp)
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ---- push ----

func (s *Server) push(pushURL, path string, body any) {
	hc := &http.Client{Timeout: 5 * time.Second}
	base := pushURL
	if strings.HasPrefix(pushURL, "unix://") {
		sock := strings.TrimPrefix(pushURL, "unix://")
		hc.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}
		base = "http://hostd"
	}
	b, _ := json.Marshal(body)
	resp, err := hc.Post(base+path, "application/json", bytes.NewReader(b))
	if err != nil {
		s.log.Warn("push failed", "url", pushURL, "path", path, "err", err)
		return
	}
	resp.Body.Close()
	s.mu.Lock()
	s.pushes = append(s.pushes, path)
	s.mu.Unlock()
}

// ---- test accessors ----

// NodeIDs returns every stable node ID that reported ready (for ID-based
// cleanup in e2e-real).
func (s *Server) NodeIDs() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.nodeIDs {
		out[k] = v
	}
	return out
}

// HostTiers returns the tiers each host registered with.
func (s *Server) HostTiers() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]string{}
	for id, h := range s.hosts {
		out[id] = append([]string(nil), h.Tiers...)
	}
	return out
}

// Minters returns the apikey tenants' minters (for revoking keys at cleanup).
func (s *Server) Minters() []*tsapi.Minter {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*tsapi.Minter
	for _, t := range s.tenants {
		if t.minter != nil {
			out = append(out, t.minter)
		}
	}
	return out
}

// Object returns a copy of an object record.
func (s *Server) Object(id string) (mgmtapi.Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.objects[id]
	if o == nil {
		return mgmtapi.Object{}, false
	}
	return o.Object, true
}

// Objects returns all object records.
func (s *Server) Objects() []mgmtapi.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mgmtapi.Object, 0, len(s.objects))
	for _, o := range s.objects {
		out = append(out, o.Object)
	}
	return out
}

// ---- registration throttle ----

type bucket struct {
	mu     sync.Mutex
	tokens float64
	max    float64
	rate   float64 // per second
	last   time.Time
}

func newBucket(perMinute int) *bucket {
	return &bucket{tokens: float64(perMinute), max: float64(perMinute), rate: float64(perMinute) / 60, last: time.Now()}
}

func (b *bucket) wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		now := time.Now()
		b.tokens = min(b.max, b.tokens+now.Sub(b.last).Seconds()*b.rate)
		b.last = now
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		need := time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(need):
		}
	}
}

// URLQuery is a small helper for tests.
func URLQuery(kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v.Encode()
}

// ---- node certificates (RT-01) ----

// DenyCert makes the certificate lookup of stableNodeID fail (a 500), so
// the receiving host must fail closed (tests).
func (s *Server) DenyCert(stableNodeID string, deny bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if deny {
		s.certDenied[stableNodeID] = true
	} else {
		delete(s.certDenied, stableNodeID)
	}
}

func (s *Server) nodeCert(w http.ResponseWriter, r *http.Request) {
	nodeID, tenantID := r.PathValue("nodeID"), r.URL.Query().Get("tenant")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.certDenied[nodeID] {
		writeErr(w, 500, "internal", "certificate lookup failed (test)")
		return
	}
	o := s.objects[s.nodeIDs[nodeID]]
	if o == nil || o.TenantID != tenantID {
		writeErr(w, 404, mgmtapi.CodeNotFound, "no object of that tenant has node "+nodeID)
		return
	}
	if o.Status == "killed" {
		writeErr(w, 410, mgmtapi.CodeKilled, "object "+o.ObjectID+" was killed")
		return
	}
	now := time.Now()
	root := o.ObjectID
	if len(o.Lineage) > 0 {
		root = o.Lineage[0]
	}
	p := nodecert.Payload{TenantID: o.TenantID, ObjectID: o.ObjectID, StableNodeID: nodeID, Tags: o.Tags, Lim: o.Caps,
		ParentID: o.Parent, RootID: root, IAT: now.Unix(), EXP: now.Add(nodecert.TTL).Unix()}
	c, err := nodecert.Sign(s.oboKey, p)
	if err != nil {
		writeErr(w, 500, "internal", err.Error())
		return
	}
	writeJSON(w, 200, mgmtapi.NodeCert{TenantID: o.TenantID, ObjectID: o.ObjectID, StableNodeID: nodeID, Status: o.Status, Cert: c,
		Payload:      &mgmtapi.NodeCertPayload{V: 1, TenantID: p.TenantID, ObjectID: p.ObjectID, StableNodeID: p.StableNodeID, Tags: p.Tags, Lim: p.Lim, ParentID: p.ParentID, RootID: p.RootID, IAT: p.IAT, EXP: p.EXP},
		OBOPublicKey: base64.StdEncoding.EncodeToString(s.oboKey.Public().(ed25519.PublicKey))})
}
