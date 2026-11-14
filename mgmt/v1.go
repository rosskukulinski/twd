package mgmt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "github.com/rosskukulinski/twd/mgmt/api/v1"
)

// v1api implements the generated v1.ServerInterface (api/openapi.yaml is
// the source of truth; mgmt/api/v1 is generated from it). Routing and
// parameter parsing are generated; this file holds the glue to Mgmt and
// the /v1 conventions: pages, ETags, merge patches, tombstones.
type v1api struct {
	s *Server
}

var _ v1.ServerInterface = (*v1api)(nil)

// ---- conventions ----

// etagOf is the strong validator of a representation.
func etagOf(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// writeResource writes v with its ETag, honouring If-None-Match.
func writeResource(w http.ResponseWriter, r *http.Request, status int, v any) {
	et := etagOf(v)
	w.Header().Set("ETag", et)
	if inm := r.Header.Get("If-None-Match"); inm != "" && status == 200 && etagMatches(inm, et) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, status, v)
}

func etagMatches(header, et string) bool {
	for _, p := range strings.Split(header, ",") {
		p = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p), "W/"))
		if p == "*" || p == et {
			return true
		}
	}
	return false
}

// checkIfMatch enforces If-Match against the current representation.
func checkIfMatch(r *http.Request, current any) error {
	im := r.Header.Get("If-Match")
	if im == "" {
		return nil
	}
	et := etagOf(current)
	if !etagMatches(im, et) {
		e := errf(412, "precondition_failed", "If-Match %s does not match the current ETag %s", im, et)
		return e
	}
	return nil
}

// pageParams turns limit/cursor into a store Page that fetches one extra
// row (to know whether there is a next page).
func pageParams(limit *int, cursor *string) (Page, int, error) {
	n := PageDefaultLimit
	if limit != nil {
		if *limit <= 0 || *limit > PageMaxLimit {
			return Page{}, 0, errf(400, "bad_request", "limit must be between 1 and %d", PageMaxLimit)
		}
		n = *limit
	}
	pg := Page{Limit: n + 1}
	if cursor != nil {
		k, err := DecodeCursor(*cursor)
		if err != nil {
			return pg, n, errf(400, "bad_cursor", "cursor does not decode; start again without it")
		}
		pg.After = k
	}
	return pg, n, nil
}

type page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

func mkPage[T any](rows []T, limit int, key func(T) PageKey) page[T] {
	items, next := pageOf(rows, limit, key)
	if items == nil {
		items = []T{}
	}
	return page[T]{Items: items, NextCursor: next}
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func boolOf(p *bool) bool { return p != nil && *p }

func location(w http.ResponseWriter, path string) { w.Header().Set("Location", path) }

// ---- views (what the spec's schemas require beyond the store types) ----

type hostView struct {
	Host
	Status  string `json:"status"`
	DNSName string `json:"dnsName,omitempty"`
	Push    *struct {
		Port int    `json:"port,omitempty"`
		URL  string `json:"url,omitempty"`
	} `json:"push,omitempty"`
	Leases []Lease `json:"leases,omitempty"`
}

func (a *v1api) hostView(r *http.Request, h Host) hostView {
	v := hostView{Host: h}
	switch {
	case h.Pending():
		v.Status = "pending"
	case h.Live(a.s.M.now()):
		v.Status = "live"
	default:
		v.Status = "stale"
	}
	if h.NodeID != "" && a.s.Self.Tailnet != "" {
		v.DNSName = HostNodeName(h.ID) + "." + a.s.Self.Tailnet
	}
	if u := a.s.M.pushTarget(h.ID); u != "" || h.PushPort > 0 {
		v.Push = &struct {
			Port int    `json:"port,omitempty"`
			URL  string `json:"url,omitempty"`
		}{Port: h.PushPort}
		if strings.HasPrefix(u, "unix://") {
			v.Push.URL = u
		}
	}
	if ls, err := a.s.M.HostLeases(r.Context(), h.ID); err == nil && len(ls) > 0 {
		v.Leases = ls
	}
	return v
}

type tombstone struct {
	ObjectID       string `json:"objectID"`
	TenantID       string `json:"tenantID"`
	Workload       string `json:"workload"`
	Namespace      string `json:"namespace"`
	ObjectName     string `json:"objectName"`
	KilledAt       int64  `json:"killedAt"`
	Reason         string `json:"reason,omitempty"`
	By             string `json:"by"`
	Cascade        bool   `json:"cascade"`
	Root           string `json:"root"`
	DeviceDeleted  bool   `json:"deviceDeleted"`
	DeviceDeleteAt int64  `json:"deviceDeleteAt,omitempty"`
}

func (a *v1api) tombstoneOf(o Object) tombstone {
	t := tombstone{ObjectID: o.ID, TenantID: o.TenantID, Workload: o.Workload, Namespace: o.Namespace, ObjectName: o.Name,
		KilledAt: o.KilledAt, Reason: o.KillReason, By: o.KilledBy, Cascade: o.KillCascade, Root: o.KillRoot}
	if t.KilledAt == 0 {
		t.KilledAt = o.UpdatedAt
	}
	if t.Root == "" {
		t.Root = o.ID
	}
	if t.By == "" {
		t.By = "mgmt"
	}
	if at, pending := a.s.M.pendingDeleteAt(o.ID); pending {
		t.DeviceDeleteAt = at
	} else {
		a.s.M.mu.Lock()
		kept := a.s.M.deviceKept[o.ID]
		a.s.M.mu.Unlock()
		t.DeviceDeleted = (o.StableNodeID != "" || o.NodeID != "") && !kept
	}
	return t
}

type objectView struct {
	Object
	Lineage   []string   `json:"lineage"`
	Tombstone *tombstone `json:"tombstone,omitempty"`
}

// objectView fills lineage (root → self) and the tombstone of a killed
// object. idx (optional) resolves parents from an already listed set.
func (a *v1api) objectView(r *http.Request, o Object, idx map[string]Object) objectView {
	v := objectView{Object: o, Lineage: []string{o.ID}}
	seen := map[string]bool{o.ID: true}
	for cur := o; cur.ParentID != "" && !seen[cur.ParentID]; {
		seen[cur.ParentID] = true
		p, ok := idx[cur.ParentID]
		if !ok {
			var err error
			if p, err = a.s.M.store.GetObject(r.Context(), cur.ParentID); err != nil {
				break
			}
		}
		v.Lineage = append([]string{p.ID}, v.Lineage...)
		cur = p
	}
	if o.Status == StatusKilled {
		t := a.tombstoneOf(o)
		v.Tombstone = &t
	}
	return v
}

// goneError is the 410 body: the typed error plus the kill record.
type goneError struct {
	Error
	Tombstone tombstone `json:"tombstone"`
}

func (a *v1api) writeGone(w http.ResponseWriter, r *http.Request, o Object) {
	e := *errf(410, "killed", "object %s was killed (%s)", o.ID, o.KillReason)
	e.TraceID = r.Header.Get("Twd-Trace-ID")
	if e.TraceID == "" {
		e.TraceID = traceID()
	}
	w.Header().Set("Twd-Error", "killed")
	writeJSON(w, 410, goneError{Error: e, Tombstone: a.tombstoneOf(o)})
}

// ---- operational ----

func (a *v1api) GetHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *v1api) WhoAmI(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r.Context())
	if p.Kind == "" {
		writeErr(w, r, errf(401, "no_identity", "unauthenticated"))
		return
	}
	kind := p.Kind
	if kind == "user" && p.NodeID != "" && len(p.Tags) > 0 {
		kind = "node"
	}
	self := a.s.Self
	self.Transport = "tsnet"
	if p.Kind == "local" {
		self.Transport = "local-dev"
	}
	writeJSON(w, 200, WhoAmI{Kind: kind, Login: p.Login, NodeID: p.NodeID, Tags: p.Tags, HostID: p.HostID,
		Admin: a.s.allowed(p, roleAdmin), Actor: p.Actor(r), Mgmt: self})
}

func (a *v1api) GetStatus(w http.ResponseWriter, r *http.Request) {
	st, err := a.s.M.Status(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	transport := "tsnet"
	if principalOf(r.Context()).Kind == "local" {
		transport = "local-dev"
	}
	listeners := a.s.Listeners
	if listeners == nil {
		listeners = []string{}
	}
	writeJSON(w, 200, map[string]any{"ok": st.Clock.OK && st.Replicas.OK, "transport": transport, "listeners": listeners,
		"pendingDeviceDeletes": st.PendingDeletes, "hosts": map[string]int{"total": st.Hosts, "live": st.LiveHosts},
		"tenants": st.Tenants, "objects": st.Objects, "version": a.s.Version, "clock": st.Clock, "replicas": st.Replicas})
}

func (a *v1api) CreateDeviceDeleteRun(w http.ResponseWriter, r *http.Request) {
	ids := a.s.M.pendingIDs()
	a.s.M.FlushDeletes()
	writeJSON(w, 201, map[string]any{"deleted": ids})
}

// ---- OIDC (served here too so the spec is complete; Funnel is the public path) ----

func (a *v1api) oidc(w http.ResponseWriter, r *http.Request) {
	if is := a.s.M.broker.Issuer; is != nil {
		is.Handler().ServeHTTP(w, r)
		return
	}
	writeErr(w, r, errf(404, "not_found", "no OIDC issuer configured"))
}
func (a *v1api) GetOIDCConfiguration(w http.ResponseWriter, r *http.Request) { a.oidc(w, r) }
func (a *v1api) GetJWKS(w http.ResponseWriter, r *http.Request)              { a.oidc(w, r) }
func (a *v1api) GetTenantOIDCConfiguration(w http.ResponseWriter, r *http.Request, _ v1.TenantPath) {
	a.oidc(w, r)
}
func (a *v1api) GetTenantJWKS(w http.ResponseWriter, r *http.Request, _ v1.TenantPath) { a.oidc(w, r) }

// ---- tenants ----

type tenantView struct {
	Tenant
	WIF           map[string]any `json:"wif,omitempty"`
	OBOPublicKey  string         `json:"oboPublicKey"`
	MintQueue     int            `json:"mintQueue"`
	EffectiveRate float64        `json:"effectiveRegistrationsPerMinute"`
}

func (a *v1api) tenantView(r *http.Request, t Tenant) (tenantView, error) {
	m := a.s.M
	pub, err := m.OBOPublicKey(r.Context(), t.ID)
	if err != nil {
		return tenantView{}, err
	}
	v := tenantView{Tenant: t, OBOPublicKey: pub, MintQueue: m.QueueDepth(t.ID), EffectiveRate: m.throttle(t).Rate()}
	if t.MintMode == MintWIF {
		v.WIF = m.IssuerInfo(t)
	}
	return v, nil
}

func (a *v1api) CreateTenant(w http.ResponseWriter, r *http.Request) {
	var req TenantRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, r, err)
		return
	}
	t, err := a.s.M.CreateTenant(r.Context(), req)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	v, err := a.tenantView(r, t)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	location(w, "/v1/tenants/"+t.ID)
	writeResource(w, r, 201, v)
}

func (a *v1api) ListTenants(w http.ResponseWriter, r *http.Request, params v1.ListTenantsParams) {
	pg, n, err := pageParams(params.Limit, params.Cursor)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	ts, err := a.s.M.store.ListTenants(r.Context(), pg)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, mkPage(ts, n, func(t Tenant) PageKey { return PageKey{Created: t.CreatedAt, ID: t.ID} }))
}

func (a *v1api) GetTenant(w http.ResponseWriter, r *http.Request, id v1.TenantID, _ v1.GetTenantParams) {
	t, err := a.s.M.store.GetTenant(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	v, err := a.tenantView(r, t)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeResource(w, r, 200, v)
}

func (a *v1api) UpdateTenant(w http.ResponseWriter, r *http.Request, id v1.TenantID, _ v1.UpdateTenantParams) {
	cur, err := a.s.M.store.GetTenant(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	curView, err := a.tenantView(r, cur)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := checkIfMatch(r, curView); err != nil {
		w.Header().Set("ETag", etagOf(curView))
		writeErr(w, r, err)
		return
	}
	var body map[string]json.RawMessage
	if err := readJSON(r, &body); err != nil {
		writeErr(w, r, err)
		return
	}
	var p TenantPatch
	p.By = principalOf(r.Context()).Actor(r)
	for k, raw := range body {
		null := string(raw) == "null"
		var sp *string
		var ip *int
		switch k {
		case "mintMode", "apiKeyFile", "wifClientID", "wifLifecycleClientID", "oauthClientID", "oauthClientFile", "pool", "tier":
			var s string
			if !null {
				if err := json.Unmarshal(raw, &s); err != nil {
					writeErr(w, r, errf(400, "bad_request", "%s must be a string", k))
					return
				}
			}
			sp = &s
		case "maxContainerVCPU":
			var f float64
			if null {
				writeErr(w, r, errf(400, "bad_request", "%s cannot be null", k))
				return
			}
			if err := json.Unmarshal(raw, &f); err != nil {
				writeErr(w, r, errf(400, "bad_request", "%s must be a number", k))
				return
			}
			p.MaxContainerVCPU = &f
			continue
		case "registrationsPerMinute", "maxObjects", "maxContainers", "maxContainerMemMiB":
			var n int
			if null {
				writeErr(w, r, errf(400, "bad_request", "%s cannot be null", k))
				return
			}
			if err := json.Unmarshal(raw, &n); err != nil {
				writeErr(w, r, errf(400, "bad_request", "%s must be an integer", k))
				return
			}
			ip = &n
		case "tailnet", "controlURL", "tenantID", "createdAt", "updatedAt", "oboPublicKey", "wif":
			e := errf(400, "immutable_field", "%s cannot be changed; create a new tenant", k)
			e.Missing = map[string]any{"field": k}
			writeErr(w, r, e)
			return
		default:
			writeErr(w, r, errf(400, "bad_request", "unknown field %q", k))
			return
		}
		switch k {
		case "mintMode":
			if null {
				writeErr(w, r, errf(400, "bad_request", "mintMode cannot be null"))
				return
			}
			p.MintMode = sp
		case "apiKeyFile":
			p.APIKeyFile = sp
		case "wifClientID":
			p.WIFClientID = sp
		case "wifLifecycleClientID":
			p.WIFLifecycleClientID = sp
		case "oauthClientID":
			p.OAuthClientID = sp
		case "oauthClientFile":
			p.OAuthClientFile = sp
		case "pool":
			p.Pool = sp
		case "tier":
			p.Tier = sp
		case "registrationsPerMinute":
			p.RegistrationsPerMinute = ip
		case "maxObjects":
			p.MaxObjects = ip
		case "maxContainers":
			p.MaxContainers = ip
		case "maxContainerMemMiB":
			p.MaxContainerMemMiB = ip
		}
	}
	t, err := a.s.M.UpdateTenant(r.Context(), id, p)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	v, err := a.tenantView(r, t)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeResource(w, r, 200, v)
}

func (a *v1api) DeleteTenant(w http.ResponseWriter, r *http.Request, id v1.TenantID, params v1.DeleteTenantParams) {
	cur, err := a.s.M.store.GetTenant(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if curView, err := a.tenantView(r, cur); err == nil {
		if err := checkIfMatch(r, curView); err != nil {
			writeErr(w, r, err)
			return
		}
	}
	res, err := a.s.M.DeleteTenant(r.Context(), id, boolOf(params.Force), principalOf(r.Context()).Actor(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if len(res.Killed) == 0 && len(res.DeploymentsStopped) == 0 {
		w.WriteHeader(204)
		return
	}
	writeJSON(w, 202, res)
}

func (a *v1api) PurgeTenantObjects(w http.ResponseWriter, r *http.Request, id v1.TenantID, params v1.PurgeTenantObjectsParams) {
	var states []string
	if params.State != nil {
		for _, s := range *params.State {
			states = append(states, string(s))
		}
	}
	res, err := a.s.M.Purge(r.Context(), id, states...)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, res)
}

func (a *v1api) CheckTenantWIF(w http.ResponseWriter, r *http.Request, id v1.TenantID) {
	res, err := a.s.M.WIFCheck(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, res)
}

// ---- hosts ----

func (a *v1api) CreateHost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HostID   string   `json:"hostID"`
		TenantID string   `json:"tenantID"`
		Pool     string   `json:"pool"`
		Tiers    []string `json:"tiers"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, r, err)
		return
	}
	hj, created, err := a.s.M.CreateHost(r.Context(), HostCreate{HostID: req.HostID, TenantID: req.TenantID, Pool: req.Pool,
		Tiers: req.Tiers, By: principalOf(r.Context()).Actor(r)})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	out := struct {
		Host       hostView `json:"host"`
		AuthKey    string   `json:"authKey"`
		ExpiresAt  int64    `json:"expiresAt"`
		Tags       []string `json:"tags"`
		Hostname   string   `json:"hostname"`
		ControlURL string   `json:"controlURL,omitempty"`
		Tailnet    string   `json:"tailnet,omitempty"`
	}{a.hostView(r, hj.Host), hj.AuthKey, hj.ExpiresAt, hj.Tags, hj.Hostname, hj.ControlURL, hj.Tailnet}
	if created {
		location(w, "/v1/hosts/"+hj.Host.ID)
		writeJSON(w, 201, out)
		return
	}
	writeJSON(w, 200, out)
}

func (a *v1api) ListHosts(w http.ResponseWriter, r *http.Request, params v1.ListHostsParams) {
	pg, n, err := pageParams(params.Limit, params.Cursor)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if params.Live != nil {
		pg.Limit = 0 // filter in memory, then page
	}
	hs, err := a.s.M.store.ListHosts(r.Context(), pg)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	views := make([]hostView, 0, len(hs))
	for _, h := range hs {
		if params.Live != nil && h.Live(a.s.M.now()) != *params.Live {
			continue
		}
		views = append(views, a.hostView(r, h))
	}
	if params.Live != nil && len(views) > n+1 {
		views = views[:n+1]
	}
	writeJSON(w, 200, mkPage(views, n, func(h hostView) PageKey { return PageKey{Created: h.CreatedAt, ID: h.ID} }))
}

func (a *v1api) GetHost(w http.ResponseWriter, r *http.Request, id v1.HostID, _ v1.GetHostParams) {
	h, err := a.s.M.store.GetHost(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeResource(w, r, 200, a.hostView(r, h))
}

func (a *v1api) RegisterHost(w http.ResponseWriter, r *http.Request, id v1.HostID) {
	var body struct {
		RegisterRequest
		Push *struct {
			Port int    `json:"port"`
			URL  string `json:"url"`
		} `json:"push"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, r, err)
		return
	}
	req := body.RegisterRequest
	if body.Push != nil { // the spec's HostRegistration.push {url | port}
		req.PushURL, req.PushPort = body.Push.URL, body.Push.Port
	}
	req.HostID = id
	p := principalOf(r.Context())
	if p.Kind == "host" && p.HostID != "" && p.HostID != id {
		writeErr(w, r, errf(403, "host_identity_mismatch", "this node is registered as host %s", p.HostID))
		return
	}
	nodeAddr := ""
	if p.Kind == "host" {
		if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			nodeAddr = h
		}
	}
	h, created, err := a.s.M.RegisterHost(r.Context(), req, p.NodeID, nodeAddr)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	status := 200
	if created {
		status = 201
	}
	writeResource(w, r, status, a.hostView(r, h))
}

func (a *v1api) DeleteHost(w http.ResponseWriter, r *http.Request, id v1.HostID, params v1.DeleteHostParams) {
	cur, err := a.s.M.store.GetHost(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := checkIfMatch(r, a.hostView(r, cur)); err != nil {
		writeErr(w, r, err)
		return
	}
	res, err := a.s.M.DeleteHost(r.Context(), id, boolOf(params.Drain), principalOf(r.Context()).Actor(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if len(res.Killed) == 0 && len(res.LeasesReleased) == 0 {
		w.WriteHeader(204)
		return
	}
	writeJSON(w, 202, res)
}

func (a *v1api) ReportHostStatus(w http.ResponseWriter, r *http.Request, id v1.HostID) {
	var req HostReport
	if err := readJSON(r, &req); err != nil {
		writeErr(w, r, err)
		return
	}
	resp, err := a.s.M.ReportHostStatus(r.Context(), id, req)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, resp)
}

type assignmentView struct {
	Assignment
	CreatedAt int64 `json:"createdAt"`
}

func (a *v1api) ListHostAssignments(w http.ResponseWriter, r *http.Request, id v1.HostID, params v1.ListHostAssignmentsParams) {
	pg, n, err := pageParams(params.Limit, params.Cursor)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if _, err := a.s.M.store.GetHost(r.Context(), id); err != nil {
		writeErr(w, r, err)
		return
	}
	as, err := a.s.M.Assignments(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	views := make([]assignmentView, 0, len(as))
	for _, x := range as {
		d, _ := a.s.M.store.GetDeployment(r.Context(), x.DeploymentID)
		k := PageKey{Created: d.CreatedAt, ID: d.ID}
		if pg.After != nil && !(k.Created > pg.After.Created || (k.Created == pg.After.Created && k.ID > pg.After.ID)) {
			continue
		}
		views = append(views, assignmentView{x, d.CreatedAt})
	}
	if len(views) > n+1 {
		views = views[:n+1]
	}
	writeJSON(w, 200, mkPage(views, n, func(v assignmentView) PageKey { return PageKey{Created: v.CreatedAt, ID: v.DeploymentID} }))
}

// ---- deployments ----

type deploymentView struct {
	DeploymentView
	UpdatedAt int64 `json:"updatedAt,omitempty"`
}

func (a *v1api) deploymentView(r *http.Request, id string) (deploymentView, error) {
	v, err := a.s.M.GetDeployment(r.Context(), id)
	if err != nil {
		return deploymentView{}, err
	}
	if v.Endpoint.Status == "" {
		v.Endpoint.Status = StatusPending
	}
	if v.Endpoint.Node == "" {
		d, _ := a.s.M.store.GetDeployment(r.Context(), id)
		t, _ := a.s.M.store.GetTenant(r.Context(), d.TenantID)
		v.Endpoint.Node = d.Workload + "-" + d.HostID
		v.Endpoint.MagicDNS = v.Endpoint.Node + "." + t.Tailnet
	}
	if v.Namespaces == nil {
		v.Namespaces = []ClassConfig{}
	}
	return deploymentView{DeploymentView: v}, nil
}

func (a *v1api) CreateDeployment(w http.ResponseWriter, r *http.Request, params v1.CreateDeploymentParams) {
	var req DeployRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, r, err)
		return
	}
	req.By = principalOf(r.Context()).Actor(r)
	if boolOf(params.DryRun) {
		res, err := a.s.M.PreflightBundle(r.Context(), req)
		if err != nil {
			writeErr(w, r, err)
			return
		}
		writeJSON(w, 200, res)
		return
	}
	resp, err := a.s.M.Deploy(r.Context(), req)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	v, err := a.deploymentView(r, resp.DeploymentID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	location(w, "/v1/deployments/"+resp.DeploymentID)
	writeJSON(w, 202, v)
}

func (a *v1api) ListDeployments(w http.ResponseWriter, r *http.Request, params v1.ListDeploymentsParams) {
	pg, n, err := pageParams(params.Limit, params.Cursor)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	ds, err := a.s.M.store.ListDeployments(r.Context(), DeploymentFilter{TenantID: str(params.Tenant), Workload: str(params.Workload),
		HostID: str(params.Host), Latest: boolOf(params.Latest)}, pg)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	views := make([]deploymentView, 0, len(ds))
	for _, d := range ds {
		if v, err := a.deploymentView(r, d.ID); err == nil {
			views = append(views, v)
		}
	}
	writeJSON(w, 200, mkPage(views, n, func(v deploymentView) PageKey { return PageKey{Created: v.CreatedAt, ID: v.DeploymentID} }))
}

func (a *v1api) GetDeployment(w http.ResponseWriter, r *http.Request, id v1.DeploymentID, _ v1.GetDeploymentParams) {
	v, err := a.deploymentView(r, id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if p := principalOf(r.Context()); p.Kind == "host" && v.Host != p.HostID {
		writeErr(w, r, errf(403, "forbidden", "deployment %s is not assigned to host %s", id, p.HostID))
		return
	}
	writeResource(w, r, 200, v)
}

func (a *v1api) DeleteDeployment(w http.ResponseWriter, r *http.Request, id v1.DeploymentID, params v1.DeleteDeploymentParams) {
	cur, err := a.deploymentView(r, id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := checkIfMatch(r, cur); err != nil {
		writeErr(w, r, err)
		return
	}
	res, err := a.s.M.Undeploy(r.Context(), id, str(params.Reason), principalOf(r.Context()).Actor(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 202, res)
}

func (a *v1api) GetDeploymentBundle(w http.ResponseWriter, r *http.Request, id v1.DeploymentID) {
	d, err := a.s.M.store.GetDeployment(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if p := principalOf(r.Context()); p.Kind == "host" && d.HostID != p.HostID {
		writeErr(w, r, errf(403, "forbidden", "deployment %s is not assigned to host %s", d.ID, p.HostID))
		return
	}
	sum := sha256.Sum256(d.Bundle)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)
	w.Header().Set("Content-Type", "application/x-tar")
	w.Write(d.Bundle)
}

// ---- objects ----

func (a *v1api) CreateObject(w http.ResponseWriter, r *http.Request) {
	var req CreateObjectRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, r, err)
		return
	}
	if req.TraceID == "" {
		req.TraceID = r.Header.Get("Twd-Trace-ID")
	}
	// A killed identity answers 410 with its tombstone.
	if req.Namespace != "" && req.ObjectName != "" {
		wl := req.Workload
		if req.Namespace == WorkloadNamespace {
			wl = req.ObjectName
		}
		if wl != "" {
			if o, err := a.s.M.store.GetObjectByName(r.Context(), req.TenantID, wl, req.Namespace, req.ObjectName); err == nil && o.Status == StatusKilled {
				a.writeGone(w, r, o)
				return
			}
		}
	}
	resp, err := a.s.M.CreateObject(r.Context(), req)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Code == "killed" {
			if objs, lerr := a.s.M.store.ListObjects(r.Context(), ObjectFilter{TenantID: req.TenantID, Namespace: req.Namespace, Status: []string{StatusKilled}}); lerr == nil {
				for _, o := range objs {
					if o.Name == req.ObjectName && (req.Workload == "" || o.Workload == req.Workload) {
						a.writeGone(w, r, o)
						return
					}
				}
			}
		}
		writeErr(w, r, err)
		return
	}
	o, err := a.s.M.GetObject(r.Context(), resp.ObjectID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if resp.Created {
		// Accepted: the mint is queued. The body is the resource as created
		// (the mint worker may already have moved it on; poll Location).
		if o.Status == StatusMinted {
			o.Status = StatusPending
		}
		location(w, "/v1/objects/"+o.ID)
		writeJSON(w, 202, a.objectView(r, o, nil))
		return
	}
	writeResource(w, r, 200, a.objectView(r, o, nil))
}

func (a *v1api) ListObjects(w http.ResponseWriter, r *http.Request, params v1.ListObjectsParams) {
	pg, n, err := pageParams(params.Limit, params.Cursor)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	f := ObjectFilter{TenantID: str(params.Tenant), Workload: str(params.Workload), Namespace: str(params.Namespace),
		ParentID: str(params.Parent), RootID: str(params.Root), Page: pg}
	if params.State != nil {
		for _, s := range *params.State {
			f.Status = append(f.Status, string(s))
		}
	}
	objs, err := a.s.M.store.ListObjects(r.Context(), f)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	idx := map[string]Object{}
	for _, o := range objs {
		idx[o.ID] = o
	}
	views := make([]objectView, 0, len(objs))
	for _, o := range objs {
		v := a.objectView(r, o, idx)
		for _, k := range objs {
			if k.ParentID == o.ID {
				v.Children = append(v.Children, k.ID)
			}
		}
		views = append(views, v)
	}
	writeJSON(w, 200, mkPage(views, n, func(v objectView) PageKey { return PageKey{Created: v.CreatedAt, ID: v.ID} }))
}

func (a *v1api) GetObject(w http.ResponseWriter, r *http.Request, id v1.ObjectID, _ v1.GetObjectParams) {
	o, err := a.s.M.GetObject(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if o.Status == StatusKilled {
		a.writeGone(w, r, o)
		return
	}
	writeResource(w, r, 200, a.objectView(r, o, nil))
}

func (a *v1api) UpdateObject(w http.ResponseWriter, r *http.Request, id v1.ObjectID, _ v1.UpdateObjectParams) {
	cur, err := a.s.M.GetObject(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := checkIfMatch(r, a.objectView(r, cur, nil)); err != nil {
		writeErr(w, r, err)
		return
	}
	var body map[string]json.RawMessage
	if err := readJSON(r, &body); err != nil {
		writeErr(w, r, err)
		return
	}
	p := principalOf(r.Context())
	_, ready := body["status"]
	_, ttl := body["ttl"]
	if cur.Status == StatusKilled && !ready {
		// A ready report still goes through Ready: it deletes the device
		// that joined after the kill, then answers 410.
		a.writeGone(w, r, cur)
		return
	}
	for k := range body {
		switch k {
		case "status", "nodeID", "stableNodeID", "magicDNS", "epoch", "ttl":
		default:
			e := errf(400, "immutable_field", "%s cannot be changed", k)
			e.Missing = map[string]any{"field": k}
			writeErr(w, r, e)
			return
		}
	}
	switch {
	case ready && ttl:
		writeErr(w, r, errf(400, "immutable_field", "a patch is either the host's {status: ready, …} or the admin's {ttl}"))
		return
	case ready:
		if p.Kind == "user" {
			writeErr(w, r, errf(400, "immutable_field", "only the lease-holding host reports status"))
			return
		}
		var req struct {
			Status string `json:"status"`
			ReadyRequest
		}
		raw, _ := json.Marshal(body)
		if err := json.Unmarshal(raw, &req); err != nil || req.Status != StatusReady {
			writeErr(w, r, errf(400, "bad_request", "status must be \"ready\" with nodeID, stableNodeID, magicDNS and epoch"))
			return
		}
		o, err := a.s.M.Ready(r.Context(), id, hostOf(r), req.ReadyRequest)
		if err != nil {
			var e *Error
			if errors.As(err, &e) && e.Code == "killed" {
				if k, gerr := a.s.M.store.GetObject(r.Context(), id); gerr == nil {
					a.writeGone(w, r, k)
					return
				}
			}
			writeErr(w, r, err)
			return
		}
		writeResource(w, r, 200, a.objectView(r, o, nil))
	case ttl:
		if p.Kind == "host" {
			writeErr(w, r, errf(400, "immutable_field", "hosts cannot change ttl"))
			return
		}
		var secs *int64
		if string(body["ttl"]) != "null" {
			var n int64
			if err := json.Unmarshal(body["ttl"], &n); err != nil || n <= 0 {
				writeErr(w, r, errf(400, "bad_request", "ttl must be a positive number of seconds or null"))
				return
			}
			secs = &n
		}
		o, err := a.s.M.SetObjectTTL(r.Context(), id, secs, p.Actor(r))
		if err != nil {
			writeErr(w, r, err)
			return
		}
		writeResource(w, r, 200, a.objectView(r, o, nil))
	default:
		writeErr(w, r, errf(400, "bad_request", "empty patch"))
	}
}

func (a *v1api) DeleteObject(w http.ResponseWriter, r *http.Request, id v1.ObjectID, params v1.DeleteObjectParams) {
	cur, err := a.s.M.GetObject(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if cur.Status == StatusKilled {
		a.writeGone(w, r, cur)
		return
	}
	if err := checkIfMatch(r, a.objectView(r, cur, nil)); err != nil {
		writeErr(w, r, err)
		return
	}
	p := principalOf(r.Context())
	if p.Kind == "host" {
		if l, err := a.s.M.store.GetLease(r.Context(), cur.Lease); err != nil || l.Holder != p.HostID {
			writeErr(w, r, errf(403, "forbidden", "host %s does not hold %s", p.HostID, cur.Lease))
			return
		}
	}
	res, err := a.s.M.Kill(r.Context(), id, KillRequest{Cascade: boolOf(params.Cascade), Reason: str(params.Reason), By: p.Actor(r)})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 202, res)
}

func (a *v1api) CreateObjectCredential(w http.ResponseWriter, r *http.Request, id v1.ObjectID, params v1.CreateObjectCredentialParams) {
	if params.Epoch <= 0 {
		writeErr(w, r, errf(400, "bad_request", "epoch query parameter must be positive"))
		return
	}
	wait := 10 * time.Second
	if params.Wait != nil && *params.Wait >= 0 && *params.Wait <= 30 {
		wait = time.Duration(*params.Wait) * time.Second
	}
	w.Header().Set("Cache-Control", "no-store")
	host := hostOf(r)
	reminted := false
	if params.Reason != nil && *params.Reason == "rejoin_rejected" {
		if _, err := a.s.M.Remint(r.Context(), id, host, RemintRequest{Epoch: params.Epoch, Reason: "saved node state rejected by control (rejoin_rejected)"}); err != nil {
			var e *Error
			if errors.As(err, &e) && e.Code == "killed" {
				if k, gerr := a.s.M.store.GetObject(r.Context(), id); gerr == nil {
					a.writeGone(w, r, k)
					return
				}
			}
			writeErr(w, r, err)
			return
		}
		reminted = true
	}
	c, err := a.s.M.Credential(r.Context(), id, host, params.Epoch, wait)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Code == "killed" {
			if k, gerr := a.s.M.store.GetObject(r.Context(), id); gerr == nil {
				a.writeGone(w, r, k)
				return
			}
		}
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 201, struct {
		Credential
		Reminted bool `json:"reminted,omitempty"`
	}{c, reminted})
}

func (a *v1api) GetObjectCertificate(w http.ResponseWriter, r *http.Request, id v1.ObjectID) {
	o, err := a.s.M.store.GetObject(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if o.Status == StatusKilled {
		a.writeGone(w, r, o)
		return
	}
	if o.StableNodeID == "" {
		writeErr(w, r, errf(409, "not_ready", "object %s has no node yet (%s)", id, o.Status))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	nc, err := a.s.M.NodeCert(r.Context(), o.TenantID, o.StableNodeID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeResource(w, r, 200, nc)
}

func (a *v1api) GetCertificate(w http.ResponseWriter, r *http.Request, stableNodeID string, params v1.GetCertificateParams) {
	w.Header().Set("Cache-Control", "no-store")
	nc, err := a.s.M.NodeCert(r.Context(), params.Tenant, stableNodeID)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Code == "killed" {
			if objs, lerr := a.s.M.store.ListObjects(r.Context(), ObjectFilter{TenantID: params.Tenant, StableNodeID: stableNodeID}); lerr == nil && len(objs) > 0 {
				a.writeGone(w, r, objs[len(objs)-1])
				return
			}
		}
		writeErr(w, r, err)
		return
	}
	writeResource(w, r, 200, nc)
}

// ---- audit ----

func (a *v1api) AppendAudit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Records []json.RawMessage `json:"records"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, r, err)
		return
	}
	host := hostOf(r)
	if host == "" {
		writeErr(w, r, errf(400, "bad_request", "audit records need a host (Twd-Host-ID header on the local-dev socket)"))
		return
	}
	n, st, err := a.s.M.IngestAudit(r.Context(), host, req.Records)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"accepted": n, "chain": st})
}

func (a *v1api) ListAudit(w http.ResponseWriter, r *http.Request, params v1.ListAuditParams) {
	pg, n, err := pageParams(params.Limit, params.Cursor)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	f := AuditFilter{ObjectID: str(params.Object), ActingFor: str(params.Human), TraceID: str(params.Trace), TenantID: str(params.Tenant),
		Chain: str(params.Chain), Since: normTS(str(params.Since)), Until: normTS(str(params.Until)), Page: pg}
	if params.Kind != nil {
		for _, k := range *params.Kind {
			f.Kinds = append(f.Kinds, string(k))
		}
	}
	recs, chains, err := a.s.M.QueryAudit(r.Context(), f)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	pageRecs := mkPage(recs, n, func(x AuditRecord) PageKey { return PageKey{TS: x.TS, Chain: x.Chain, Seq: x.Seq} })
	ok := true
	for _, c := range chains {
		ok = ok && c.OK
	}
	if chains == nil {
		chains = []ChainStatus{}
	}
	writeJSON(w, 200, struct {
		page[AuditRecord]
		Chains   []ChainStatus `json:"chains"`
		Verified bool          `json:"verified"`
	}{pageRecs, chains, ok})
}

// normTS accepts RFC 3339 or unix ms and returns RFC 3339 (the stored form).
func normTS(s string) string {
	if s == "" {
		return ""
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
	}
	return s
}

func (a *v1api) GetAuditVerification(w http.ResponseWriter, r *http.Request) {
	chains, ok, err := a.s.M.VerifyAll(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": ok, "chains": chains, "verifiedAt": a.s.M.now().UnixMilli()})
}

// ---- volumes ----

func (a *v1api) CreateVolumeKeyGrant(w http.ResponseWriter, r *http.Request, id v1.VolumeID, params v1.CreateVolumeKeyGrantParams) {
	tenantID, workload, ok := strings.Cut(id, ":")
	if !ok || tenantID == "" || workload == "" {
		writeErr(w, r, errf(400, "bad_request", "volume ID must be <tenantID>:<workload>"))
		return
	}
	if params.Epoch <= 0 {
		writeErr(w, r, errf(400, "bad_request", "epoch query parameter must be positive"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	host := hostOf(r)
	key, err := a.s.M.VolumeKey(r.Context(), tenantID, workload, host, params.Epoch)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 201, struct {
		VolumeID  string `json:"volumeID"`
		TenantID  string `json:"tenantID"`
		Workload  string `json:"workload"`
		Epoch     int64  `json:"epoch"`
		Key       []byte `json:"key"`
		GrantedAt int64  `json:"grantedAt"`
	}{id, tenantID, workload, params.Epoch, key, a.s.M.now().UnixMilli()})
}

// ---- container reservations (ADR-032 D9) ----

func (a *v1api) CreateContainerReservation(w http.ResponseWriter, r *http.Request) {
	var req ReserveRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, r, err)
		return
	}
	res, created, err := a.s.M.ReserveContainer(r.Context(), hostOf(r), req)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if created {
		location(w, "/v1/container-reservations/"+res.ID)
		writeJSON(w, 201, res)
		return
	}
	writeJSON(w, 200, res)
}

func (a *v1api) ListContainerReservations(w http.ResponseWriter, r *http.Request, params v1.ListContainerReservationsParams) {
	pg, n, err := pageParams(params.Limit, params.Cursor)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	f := ContainerFilter{TenantID: str(params.Tenant), HostID: str(params.Host)}
	if p := principalOf(r.Context()); p.Kind == "host" || r.Header.Get("Twd-Host-ID") != "" {
		f.HostID = hostOf(r) // a host sees its own reservations only
	}
	rs, err := a.s.M.store.ListContainerReservations(r.Context(), f, pg)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, mkPage(rs, n, func(c ContainerReservation) PageKey { return PageKey{Created: c.CreatedAt, ID: c.ID} }))
}

func (a *v1api) DeleteContainerReservation(w http.ResponseWriter, r *http.Request, id v1.ReservationID) {
	p := principalOf(r.Context())
	host := ""
	if p.Kind == "host" || p.Kind == "local" {
		host = hostOf(r) // "" for a local admin (twd CLI): any host's
	}
	if err := a.s.M.ReleaseContainer(r.Context(), id, host, p.Actor(r)); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(204)
}

// ---- role middleware ----

// roleAllowed decides whether principal p may call an operation whose
// x-twd-role is role ("admin", "host", "admin|host", "any", "public").
func (s *Server) roleAllowed(p Principal, role string) bool {
	if role == "public" {
		return true
	}
	if p.Kind == "" {
		return false
	}
	if role == "any" || p.Kind == "local" {
		return true
	}
	roles := 0
	for _, part := range strings.Split(role, "|") {
		switch strings.TrimSpace(part) {
		case "admin":
			roles |= roleAdmin
		case "host":
			roles |= roleHost
		}
	}
	return s.allowed(p, roles)
}

// roleGate gates every generated route by the spec's x-twd-role and binds
// tsnet hosts to their own /v1/hosts/{id} records. It runs before the
// generated parameter binding (mux.Handler resolves the pattern without
// serving), so a malformed request from the wrong role is a 403, not a 400.
func (s *Server) roleGate(roles map[string]string, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalOf(r.Context())
		_, pattern := mux.Handler(r)
		role, known := roles[pattern]
		if !known {
			if p.Kind == "" {
				writeErr(w, r, errf(401, "no_identity", "unauthenticated"))
				return
			}
			writeErr(w, r, errf(404, "not_found", "%s %s is not a /v1 route (see api/openapi.yaml)", r.Method, r.URL.Path))
			return
		}
		if !s.roleAllowed(p, role) {
			writeErr(w, r, errf(403, "forbidden", "caller (%s) may not call %s %s", p.Kind, r.Method, r.URL.Path))
			return
		}
		if p.Kind == "host" && p.HostID != "" && strings.HasPrefix(pattern[strings.Index(pattern, " ")+1:], "/v1/hosts/{id}") {
			// /v1/hosts/{id}[/...]: the id is the third path element.
			if parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 4); len(parts) >= 3 && parts[2] != p.HostID {
				writeErr(w, r, errf(403, "forbidden", "host %s may not act as host %s", p.HostID, parts[2]))
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// Handler returns the /v1 API mux: generated routing + parameter parsing
// over v1api, with the role gate from the spec.
func (s *Server) Handler() http.Handler {
	roles, err := v1.OperationRoles()
	if err != nil {
		panic("mgmt: embedded spec: " + err.Error())
	}
	mux := http.NewServeMux()
	v1.HandlerWithOptions(&v1api{s: s}, v1.StdHTTPServerOptions{
		BaseRouter: mux,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeErr(w, r, errf(400, "bad_request", "%v", err))
		},
	})
	// Anything else (there is no /v0) is a typed 404 from the gate.
	gate := s.roleGate(roles, mux)
	// The OCI distribution API (/v2/, ADR-032 D3) is an external spec:
	// api/openapi.yaml documents its routes and roles (tag "registry",
	// excluded from code generation); the registry enforces them.
	reg := s.M.Registry()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/") {
			if reg == nil {
				writeErr(w, r, errf(404, "registry_unavailable", "this twd-mgmt serves no registry (start it with -registry-dir)"))
				return
			}
			if principalOf(r.Context()).Kind == "" {
				writeErr(w, r, errf(401, "no_identity", "unauthenticated"))
				return
			}
			reg.ServeHTTP(w, r)
			return
		}
		gate.ServeHTTP(w, r)
	})
}

// pendingIDs lists objects whose device delete is scheduled.
func (m *Mgmt) pendingIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.pending))
	for id := range m.pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// pendingDeleteAt reports when an object's scheduled device delete runs.
func (m *Mgmt) pendingDeleteAt(id string) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pending[id]
	if !ok {
		return 0, false
	}
	return p.at, true
}

var _ = fmt.Sprintf
