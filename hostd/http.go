package hostd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/hostd/gen"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/netd"
)

// errStatus maps catalog codes to HTTP statuses for hostd-originated errors.
var errStatus = map[string]int{
	"bad_request": 400, "no_identity": 401, "unknown_object": 404, "killed": 410,
	"identity_pending": 503, "upstream_unavailable": 503, "hostd_unavailable": 503,
	"lease_lost": 409, "namespace_conflict": 409, "spawn_budget_exceeded": 429, "bootstrap_failed": 502,
	"cap_missing": 403, "delegation_exceeds_parent": 403, "delegation_invalid": 403,
	"forbidden_destination": 403, "peer_not_visible": 403, "acl_denied": 403,
}

func writeAPIError(w http.ResponseWriter, e *apiError) {
	st := errStatus[e.Code]
	if st == 0 {
		st = 502
	}
	body := map[string]any{"code": e.Code, "message": netd.Redact(e.Detail), "detail": netd.Redact(e.Detail), "doc_url": netd.DocBase + e.Code}
	for k, v := range e.Extra {
		body[k] = v
	}
	w.Header().Set("content-type", "application/json")
	w.Header().Set(netd.HeaderError, e.Code)
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(st)
	json.NewEncoder(w).Encode(body)
}

// ingressHandler is the tenant registry's netd Upstream: netd has done
// WhoIs and set the cf blob; hostd adds cf.twd routing (only hostd can, as
// only it reaches the ingress socket) and forwards to the active workerd.
func (h *Host) ingressHandler(t *tenant) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(netd.HeaderObject)
		h.mu.Lock()
		o := h.byNetd[id]
		var wl *workload
		if o != nil {
			wl = h.workloads[o.TenantID+"/"+o.Workload]
		}
		h.mu.Unlock()
		if o == nil || wl == nil || o.TenantID != t.id {
			writeAPIError(w, &apiError{Code: "unknown_object", Detail: "node " + id + " is not hosted here"})
			return
		}
		var blob map[string]any
		if err := json.Unmarshal([]byte(r.Header.Get(netd.HeaderCF)), &blob); err != nil || blob == nil {
			writeAPIError(w, &apiError{Code: "no_identity", Detail: "missing cf blob"})
			return
		}
		route := map[string]any{"kind": "workload", "workload": o.Workload, "objectID": o.ID}
		if o.Class != "" {
			route = map[string]any{"kind": "object", "class": o.Class, "name": o.Name, "workload": o.Workload, "objectID": o.ID}
		}
		blob["twd"] = route
		blob["traceId"] = newTraceID()
		b, _ := json.Marshal(blob)
		r.Header.Set(netd.HeaderCF, string(b))
		ctx, cancel := context.WithTimeout(r.Context(), h.cfg.DrainTimeout+5*time.Second)
		inst, release := wl.acquire(ctx)
		cancel()
		if inst == nil {
			writeAPIError(w, &apiError{Code: "upstream_unavailable", Detail: "workload " + o.Workload + " has no running instance"})
			return
		}
		defer release()
		inst.ingress.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status and twd-error of a proxied response.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) {
	s.code = c
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// egressObjHandler serves a facet's egress (platform Egress entrypoint):
// verify the MAC, map class+name to the object (creating it on first use),
// and hand the request to netd, which dials from the object's own node.
func (h *Host) egressObjHandler(inst *instance) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cls, name, ok := gen.VerifyEgress(inst.res.MACKey, r.Header, r.Method, r.RequestURI, time.Now())
		if !ok {
			writeAPIError(w, &apiError{Code: "unknown_object", Detail: "egress request failed platform authentication"})
			return
		}
		c := inst.app.Classes[cls]
		if c == nil || !c.Facet() {
			writeAPIError(w, &apiError{Code: "unknown_object", Detail: "no facet class " + cls})
			return
		}
		o, err := h.ensure(r.Context(), inst.w, cls, name, nil, nil, "")
		if err != nil {
			writeAPIError(w, asAPIError(err))
			return
		}
		if o.Status == statusKilled {
			writeAPIError(w, &apiError{Code: "killed", Detail: "object " + o.ID + " was killed"})
			return
		}
		gen.StripPlatformHeaders(r.Header)
		h.serveEgress(w, r, inst, o)
	})
}

// egressWLHandler serves the tenant service's egress (the Worker and
// plain-DO objects): it leaves as the workload node.
func (h *Host) egressWLHandler(inst *instance) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gen.StripPlatformHeaders(r.Header)
		inst.w.mu.Lock()
		o := inst.w.wl
		inst.w.mu.Unlock()
		if o == nil {
			writeAPIError(w, &apiError{Code: "identity_pending", Detail: "workload node not created yet"})
			return
		}
		h.serveEgress(w, r, inst, o)
	})
}

func (h *Host) serveEgress(w http.ResponseWriter, r *http.Request, inst *instance, o *object) {
	r.Header.Set(netd.HeaderObject, o.NetdID)
	t := inst.w.tenant
	rec := &statusRecorder{ResponseWriter: w, code: 200}
	t.egress.ServeHTTP(rec, r)
	if code := rec.Header().Get(netd.HeaderError); code != "" {
		h.audit.add(mgmtapi.AuditRecord{ObjectID: o.ID, Kind: "deny", Dst: r.URL.Host, Code: code, TraceID: newTraceID()})
	}
}

// ---- local host API (platform ⇄ hostd) ----

type apiReq struct {
	As *struct {
		Class string `json:"class"`
		Name  string `json:"name"`
	} `json:"as"`
	Class     string                        `json:"class"`
	Name      string                        `json:"name"`
	Caps      json.RawMessage               `json:"caps"`
	TTL       string                        `json:"ttl"`
	Wake      bool                          `json:"wake"`
	TimeoutMS int                           `json:"timeoutMs"`
	Reason    string                        `json:"reason"`
	Cascade   bool                          `json:"cascade"`
	Target    *struct{ Class, Name string } `json:"target"`
	Records   []map[string]any              `json:"records"`
	ActingFor string                        `json:"actingFor"`
	Principal string                        `json:"principal"`
}

func objView(o *object) map[string]any {
	return map[string]any{"objectID": o.ID, "objectId": o.ID, "status": o.Status, "hostname": o.Hostname, "magicDNS": o.MagicDNS,
		"kind": map[bool]string{true: "workload", false: "object"}[o.Class == ""], "class": o.Class, "name": o.Name}
}

// hostAPI serves the platform's HOSTD binding (only the trusted platform
// worker can reach it). `as` is the calling object, as attested by the
// platform (TailnetApi props); nil means the workload itself.
func (h *Host) hostAPI(inst *instance) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req apiReq
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, &apiError{Code: "bad_request", Detail: err.Error()})
			return
		}
		ctx := r.Context()
		wl := inst.w
		self := func() (*object, error) {
			if req.As == nil {
				wl.mu.Lock()
				defer wl.mu.Unlock()
				if wl.wl == nil {
					return nil, &apiError{Code: "identity_pending", Detail: "workload node not created yet"}
				}
				return wl.wl, nil
			}
			c := inst.app.Classes[req.As.Class]
			if c == nil || req.As.Name == "" {
				return nil, &apiError{Code: "bad_request", Detail: "unknown class " + req.As.Class}
			}
			if !c.Node {
				return nil, &apiError{Code: "bad_request", Detail: "class " + req.As.Class + " has no tailnet identity (\"node\": false)"}
			}
			return h.ensure(ctx, wl, req.As.Class, req.As.Name, nil, nil, "", req.ActingFor)
		}
		fail := func(err error) { writeAPIError(w, asAPIError(err)) }
		switch strings.TrimPrefix(r.URL.Path, "/v1/") {
		case "ensure":
			o, err := self()
			if err != nil {
				fail(err)
				return
			}
			h.mu.Lock()
			if req.Wake {
				o.Wakes++
			}
			late := req.ActingFor != "" && o.ActingFor == "" && o.Class != ""
			if late {
				o.ActingFor = req.ActingFor
			}
			h.mu.Unlock()
			if late {
				h.audit.add(mgmtapi.AuditRecord{ObjectID: o.ID, Kind: "principal", ActingFor: req.ActingFor, Detail: "acting for " + req.ActingFor, TraceID: newTraceID()})
				h.saveObjects()
			}
			writeJSON(w, 200, objView(o))
		case "self":
			o, err := self()
			if err != nil {
				fail(err)
				return
			}
			writeJSON(w, 200, objView(o))
		case "spawn":
			c := inst.app.Classes[req.Class]
			if c == nil || req.Name == "" || !c.Node {
				fail(&apiError{Code: "bad_request", Detail: "spawn: unknown class " + req.Class + " (or it has no node)"})
				return
			}
			parent, err := self()
			if err != nil {
				fail(err)
				return
			}
			if len(req.Caps) == 0 || string(req.Caps) == "null" {
				req.Caps = nil
			}
			ttl := req.TTL
			if ttl == "" {
				ttl = c.TTL
			}
			af := req.Principal
			if af == "" {
				af = parent.ActingFor
			}
			o, err := h.ensure(ctx, wl, req.Class, req.Name, parent, req.Caps, ttl, af)
			if err != nil {
				fail(err)
				return
			}
			writeJSON(w, 200, objView(o))
		case "ready":
			c := inst.app.Classes[req.Class]
			if c == nil || !c.Node {
				fail(&apiError{Code: "bad_request", Detail: "ready: unknown class " + req.Class})
				return
			}
			o, err := h.ensure(ctx, wl, req.Class, req.Name, nil, nil, "")
			if err != nil {
				fail(err)
				return
			}
			to := time.Duration(req.TimeoutMS) * time.Millisecond
			if to <= 0 || to > 5*time.Minute {
				to = h.cfg.ReadyTimeout
			}
			if err := h.waitReady(ctx, o, to); err != nil {
				fail(err)
				return
			}
			writeJSON(w, 200, objView(o))
		case "kill":
			caller, err := self()
			if err != nil {
				fail(err)
				return
			}
			target := caller
			if req.Target != nil {
				h.mu.Lock()
				target = h.byKey[objKey(wl.tenantID, wl.name, req.Target.Class, req.Target.Name)]
				h.mu.Unlock()
				if target == nil {
					fail(&apiError{Code: "unknown_object", Detail: "no live object " + req.Target.Class + "/" + req.Target.Name})
					return
				}
				// Self or own descendants only (the workload may kill its objects).
				if req.As != nil && target != caller && !h.isDescendant(target, caller) {
					fail(&apiError{Code: "delegation_invalid", Detail: "an object may kill only itself or its descendants"})
					return
				}
			}
			reason := req.Reason
			if reason == "" {
				reason = "killed by " + caller.Hostname
			}
			resp, err := h.mgmt.Kill(ctx, target.ID, mgmtapi.KillRequest{Cascade: req.Cascade, Reason: reason})
			if err != nil {
				fail(err)
				return
			}
			// mgmt pushes the kill too; do it here as well so it is immediate.
			go func() {
				for _, id := range resp.Killed {
					h.killLocal(id, reason)
				}
			}()
			writeJSON(w, 200, map[string]any{"killed": resp.Killed})
		case "audit":
			o, err := self()
			if err != nil {
				fail(err)
				return
			}
			for _, rec := range req.Records {
				kind, _ := rec["kind"].(string)
				if kind != "allow" && kind != "deny" && kind != "egress" {
					continue
				}
				ar := mgmtapi.AuditRecord{ObjectID: o.ID, Kind: kind, TraceID: newTraceID()}
				ar.Dst, _ = rec["dst"].(string)
				ar.Grant, _ = rec["grant"].(string)
				ar.Code, _ = rec["code"].(string)
				ar.Detail, _ = rec["detail"].(string)
				ar.ActingFor, _ = rec["actingFor"].(string)
				if tid, _ := rec["traceID"].(string); tid != "" {
					ar.TraceID = tid
				}
				h.audit.add(ar)
			}
			writeJSON(w, 200, map[string]any{"ok": true})
		default:
			fail(&apiError{Code: "bad_request", Detail: "unknown op"})
		}
	})
}

// ---- push API (mgmt → hostd) and status ----

func (h *Host) pushAPI() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /v0/push/kill", func(w http.ResponseWriter, r *http.Request) {
		var k mgmtapi.KillOrder
		if err := json.NewDecoder(r.Body).Decode(&k); err != nil || k.ObjectID == "" {
			writeAPIError(w, &apiError{Code: "bad_request", Detail: "objectID required"})
			return
		}
		h.killLocal(k.ObjectID, k.Reason)
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	m.HandleFunc("POST /v0/push/assignments", func(w http.ResponseWriter, r *http.Request) {
		h.TriggerSync()
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	m.HandleFunc("GET /v0/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, h.Status())
	})
	return m
}

// ObjectStatus is a status view of one object.
type ObjectStatus struct {
	ID, NetdID, TenantID, Workload, Class, Name, Hostname, Status, NodeID, MagicDNS, IP, Error, Parent string
	Wakes                                                                                              int
}

// WorkloadStatus is a status view of one workload.
type WorkloadStatus struct {
	TenantID, Workload, DeploymentID, Status, Error string
	Epoch                                           int64
	Requests                                        int64
	InstanceDir                                     string
	Tier                                            string
	VM                                              *sandbox.Info `json:",omitempty"`
	// Process tier: the workerd pid, its sandbox ("none" | "seatbelt"),
	// the Seatbelt profile, and its DO storage dir.
	PID            int    `json:",omitempty"`
	Sandbox        string `json:",omitempty"`
	SandboxProfile string `json:",omitempty"`
	DOStorage      string `json:",omitempty"`
}

// StatusView is GET /v0/status.
type StatusView struct {
	HostID    string
	Objects   []ObjectStatus
	Workloads []WorkloadStatus
}

// Status returns a snapshot.
func (h *Host) Status() StatusView {
	h.mu.Lock()
	v := StatusView{HostID: h.cfg.HostID}
	for _, o := range h.objects {
		v.Objects = append(v.Objects, ObjectStatus{o.ID, o.NetdID, o.TenantID, o.Workload, o.Class, o.Name, o.Hostname, o.Status, o.NodeID, o.MagicDNS, o.IP, o.Err, o.Parent, o.Wakes})
	}
	wls := make([]*workload, 0, len(h.workloads))
	for _, w := range h.workloads {
		wls = append(wls, w)
	}
	h.mu.Unlock()
	for _, w := range wls {
		w.mu.Lock()
		ws := WorkloadStatus{TenantID: w.tenantID, Workload: w.name, DeploymentID: w.activeDep, Status: w.status, Error: w.err, Epoch: w.epoch}
		if w.active != nil {
			ws.Requests = w.active.reqs.Load()
			ws.InstanceDir = w.active.dir
			ws.Tier = w.active.tier
			ws.DOStorage = w.active.doStorage
			w.active.mu.Lock()
			if w.active.cmd != nil && w.active.cmd.Process != nil {
				ws.PID = w.active.cmd.Process.Pid
			}
			if w.active.tier != "microvm" {
				ws.Sandbox, ws.SandboxProfile = SandboxNone, w.active.profile
				if w.active.profile != "" {
					ws.Sandbox = SandboxSeatbelt
				}
			}
			w.active.mu.Unlock()
			if vm := w.active.currentVM(); vm != nil {
				info := vm.Info()
				ws.VM = &info
			}
		}
		w.mu.Unlock()
		v.Workloads = append(v.Workloads, ws)
	}
	sort.Slice(v.Objects, func(i, j int) bool { return v.Objects[i].Hostname < v.Objects[j].Hostname })
	sort.Slice(v.Workloads, func(i, j int) bool { return v.Workloads[i].Workload < v.Workloads[j].Workload })
	return v
}

// Sandbox returns the running microVM of a workload's active instance (nil
// in the process tier).
func (h *Host) Sandbox(tenantID, workload string) sandbox.VM {
	h.mu.Lock()
	w := h.workloads[tenantID+"/"+workload]
	h.mu.Unlock()
	if w == nil {
		return nil
	}
	inst := w.current()
	if inst == nil {
		return nil
	}
	return inst.currentVM()
}

// Object returns the status of the object (class, name) of a workload.
func (h *Host) Object(tenantID, workload, class, name string) (ObjectStatus, bool) {
	for _, o := range h.Status().Objects {
		if o.TenantID == tenantID && o.Workload == workload && o.Class == class && o.Name == name && o.Status != statusKilled {
			return o, true
		}
	}
	return ObjectStatus{}, false
}
