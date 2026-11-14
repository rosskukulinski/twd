package hostd

// Public ingress on the host (ADR-029).
//
//   - A workload whose assignment carries Public gets one extra object, the
//     public node: namespace "_public", object name = the workload, tags
//     and hostname chosen by mgmt (tag:twd-public, the workload name). Its
//     netd node listens only on Funnel and delivers every request to the
//     workload's entry node (netd PublicOptions.DeliverTo), so public
//     traffic reaches the app's default export, never an object directly.
//   - `twd expose off` (Assignment.Public.Exposed=false) closes the Funnel
//     listener and resets its flows; the node stays joined so its
//     certificate stays warm. An assignment without Public closes it too
//     (mgmt deletes the node when the routes are removed).
//   - Public requests may reach existing objects only. The platform asks
//     /v1/admit before a public request touches an object it has not seen;
//     a class with "public": {"create": true, "maxObjects": N} may admit up
//     to N new names, at most publicspec.CreatesPerMinute per tenant.
//   - Every public request is audited (D7); requests the limits refused
//     are summarized per minute.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/internal/publicspec"
	"github.com/rosskukulinski/twd/netd"
)

// OriginPublic is ActingFor for objects a public request created.
const OriginPublic = "public"

// publicState is one workload's public ingress (h.mu).
type publicState struct {
	spec *mgmtapi.PublicAssignment // nil: not public (close any listener)
	opts *netd.PublicOptions       // what the live node was started with
}

// publicCreates is the persisted record of names admitted by public
// requests, per tenant|workload|class (so maxObjects survives restarts and
// counts "node": false classes, which have no hostd object).
type publicCreates struct {
	mu    sync.Mutex
	path  string
	Names map[string][]string `json:"names"`
	lane  map[string]*laneBucket
}

type laneBucket struct {
	tokens float64
	last   time.Time
}

func newPublicCreates(path string) *publicCreates {
	pc := &publicCreates{path: path, Names: map[string][]string{}, lane: map[string]*laneBucket{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, pc)
		if pc.Names == nil {
			pc.Names = map[string][]string{}
		}
	}
	return pc
}

func (pc *publicCreates) saveLocked() {
	b, _ := json.Marshal(pc)
	tmp := pc.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, pc.path)
	}
}

func pcKey(tenantID, workload, class string) string { return tenantID + "|" + workload + "|" + class }

func (pc *publicCreates) has(tenantID, workload, class, name string) bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for _, n := range pc.Names[pcKey(tenantID, workload, class)] {
		if n == name {
			return true
		}
	}
	return false
}

// admit records a new public-created name if the class's cap and the
// tenant's lane allow it.
func (pc *publicCreates) admit(tenantID, workload, class, name string, max int, now time.Time) (bool, string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	k := pcKey(tenantID, workload, class)
	for _, n := range pc.Names[k] {
		if n == name {
			return true, ""
		}
	}
	if len(pc.Names[k]) >= max {
		return false, "class " + class + " already has its maximum of " + itoa(max) + " publicly created objects (public.maxObjects)"
	}
	b := pc.lane[tenantID]
	if b == nil {
		b = &laneBucket{tokens: publicspec.CreatesPerMinute, last: now}
		pc.lane[tenantID] = b
	}
	b.tokens += now.Sub(b.last).Minutes() * publicspec.CreatesPerMinute
	if b.tokens > publicspec.CreatesPerMinute {
		b.tokens = publicspec.CreatesPerMinute
	}
	b.last = now
	if b.tokens < 1 {
		return false, "the tenant's public-create lane is exhausted (" + itoa(publicspec.CreatesPerMinute) + "/min); try again later"
	}
	b.tokens--
	pc.Names[k] = append(pc.Names[k], name)
	pc.saveLocked()
	return true, ""
}

// forget drops a name (its object was killed), freeing a maxObjects slot.
func (pc *publicCreates) forget(tenantID, workload, class, name string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	k := pcKey(tenantID, workload, class)
	ns := pc.Names[k]
	for i, n := range ns {
		if n == name {
			pc.Names[k] = append(ns[:i:i], ns[i+1:]...)
			pc.saveLocked()
			return
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// isPublicObject reports whether o is a workload's public node.
func (o *object) isPublicNode() bool { return o.Class == mgmtapi.PublicNamespace }

// publicOptionsLocked builds the netd options for w's public node (h.mu).
func (h *Host) publicOptionsLocked(tenantID, workload string) *netd.PublicOptions {
	ps := h.public[tenantID+"/"+workload]
	if ps == nil || ps.spec == nil {
		return nil
	}
	wl := h.byKey[objKey(tenantID, workload, "", workload)]
	if wl == nil {
		return nil
	}
	sp := ps.spec
	return &netd.PublicOptions{Routes: append([]string(nil), sp.Routes...), Upgrade: append([]string(nil), sp.Upgrade...),
		Limits: sp.Limits, DeliverTo: wl.NetdID, Enabled: sp.Exposed}
}

// applyPublic reconciles w's public node with its assignment. It runs
// after every assignment sync and after a deploy becomes active.
func (h *Host) applyPublic(w *workload, spec *mgmtapi.PublicAssignment) {
	key := w.tenantID + "/" + w.name
	h.mu.Lock()
	ps := h.public[key]
	if ps == nil {
		ps = &publicState{}
		h.public[key] = ps
	}
	ps.spec = spec
	po := h.byKey[objKey(w.tenantID, w.name, mgmtapi.PublicNamespace, w.name)]
	t := h.tenants[w.tenantID]
	h.mu.Unlock()

	if spec == nil {
		if po != nil && t != nil {
			if err := t.reg.SetPublicEnabled(po.NetdID, false); err == nil {
				h.log.Info("public ingress closed (no public routes assigned)", "workload", w.name)
			}
		}
		return
	}
	if !w.isActive() || t == nil {
		return // the deploy calls applyPublic again once active
	}
	ctx, cancel := context.WithTimeout(h.ctx, 45*time.Second)
	defer cancel()
	if po == nil {
		o, err := h.ensure(ctx, w, mgmtapi.PublicNamespace, w.name, nil, nil, "")
		if err != nil {
			h.log.Warn("public node: create failed", "workload", w.name, "err", err)
			return
		}
		h.log.Info("public node created", "workload", w.name, "object", o.ID, "hostname", o.Hostname, "exposed", spec.Exposed)
		h.mu.Lock()
		ps.opts = h.publicOptionsLocked(w.tenantID, w.name)
		h.mu.Unlock()
		return
	}
	h.mu.Lock()
	want := h.publicOptionsLocked(w.tenantID, w.name)
	have := ps.opts
	live := po.live
	taken := strings.HasPrefix(po.Err, "public_name_taken")
	h.mu.Unlock()
	if want == nil {
		return
	}
	if taken {
		want.Enabled = false // never serve a de-duplicated name
	}
	if live && have != nil && sameRouting(have, want) {
		// netd reports a listener that failed to open (no HTTPS certs or no
		// funnel attribute yet): retry it on every sync while exposed.
		if want.Enabled && have.Enabled {
			if no := t.reg.Get(po.NetdID); no != nil && no.State() == netd.StateReady && !no.PublicListening() {
				if err := t.reg.SetPublicEnabled(po.NetdID, true); err != nil {
					h.log.Warn("public listener still not open", "workload", w.name, "err", netd.RedactError(err))
				}
			}
		}
		if have.Enabled != want.Enabled {
			st := time.Now()
			if err := t.reg.SetPublicEnabled(po.NetdID, want.Enabled); err != nil {
				h.log.Warn("public node: toggle failed", "workload", w.name, "err", err)
				return
			}
			h.mu.Lock()
			ps.opts = want
			h.mu.Unlock()
			kind := "expose_off"
			if want.Enabled {
				kind = "expose_on"
			}
			h.audit.add(mgmtapi.AuditRecord{ObjectID: po.ID, Kind: kind, Detail: mgmtapi.Note("public listener " + map[bool]string{true: "opened", false: "closed"}[want.Enabled]), TraceID: newTraceID()})
			h.log.Info("public listener toggled", "workload", w.name, "exposed", want.Enabled, "ms", time.Since(st).Milliseconds())
		}
		return
	}
	// Routes, limits or the entry node changed (a redeploy): restart the
	// public node with the new options; its state (and certificate) is kept.
	h.mu.Lock()
	wasLive := po.live
	po.live = false
	if po.Status == statusReady || po.Status == statusPending {
		po.Status = statusPending
		po.readyCh = make(chan struct{})
	}
	h.mu.Unlock()
	if wasLive {
		t.reg.Kill(context.Background(), po.NetdID, netd.KillOptions{Reason: "public routes changed", KeepState: true, SkipDelete: true})
	}
	h.mu.Lock()
	ps.opts = want
	if !po.terminal() {
		h.startNodeLocked(po)
	}
	h.mu.Unlock()
	h.log.Info("public node restarted with new options", "workload", w.name, "routes", want.Routes, "exposed", want.Enabled)
}

func sameRouting(a, b *netd.PublicOptions) bool {
	return reflect.DeepEqual(a.Routes, b.Routes) && reflect.DeepEqual(a.Upgrade, b.Upgrade) &&
		a.Limits == b.Limits && a.DeliverTo == b.DeliverTo
}

// admitPublic answers the platform's /v1/admit: may a public request reach
// (class, name) of w? Existing objects: yes. New ones: only if the class
// opts in and its cap and the tenant lane allow it.
func (h *Host) admitPublic(w *workload, app *bundle.App, class, name string) (existing bool, err *apiError) {
	if class == "" || name == "" {
		return false, &apiError{Code: "bad_request", Detail: "admit needs class and name"}
	}
	c := app.Classes[class]
	if c == nil {
		return false, &apiError{Code: "bad_request", Detail: "unknown class " + class}
	}
	h.mu.Lock()
	o := h.byKey[objKey(w.tenantID, w.name, class, name)]
	h.mu.Unlock()
	if o != nil && !o.terminal() {
		return true, nil
	}
	if h.pubCreates.has(w.tenantID, w.name, class, name) {
		return true, nil
	}
	if c.Public == nil || !c.Public.Create {
		return false, &apiError{Code: "public_create_denied", Detail: "a public request may not create " + class + "/" + name +
			": it does not exist, and class " + class + " does not set \"public\": {\"create\": true, \"maxObjects\": N}"}
	}
	ok, why := h.pubCreates.admit(w.tenantID, w.name, class, name, c.Public.MaxObjects, time.Now())
	if !ok {
		h.audit.add(mgmtapi.AuditRecord{ObjectID: h.workloadObjectID(w), Kind: "deny", Detail: mgmtapi.Note("public create " + class + "/" + name + ": " + why), TraceID: newTraceID()})
		return false, &apiError{Code: "public_create_denied", Detail: why}
	}
	d, _ := json.Marshal(map[string]any{"class": class, "name": name, "origin": OriginPublic})
	h.audit.add(mgmtapi.AuditRecord{ObjectID: h.workloadObjectID(w), Kind: "public_object_created", ActingFor: OriginPublic, Detail: d, TraceID: newTraceID()})
	return false, nil
}

// ensureAllowedPublic is the check for /v1/ensure with "public": true.
func (h *Host) ensureAllowedPublic(w *workload, class, name string) *apiError {
	h.mu.Lock()
	o := h.byKey[objKey(w.tenantID, w.name, class, name)]
	h.mu.Unlock()
	if o != nil && !o.terminal() {
		return nil
	}
	if h.pubCreates.has(w.tenantID, w.name, class, name) {
		return nil
	}
	return &apiError{Code: "public_create_denied", Detail: "a public request may not create " + class + "/" + name + " (not admitted)"}
}

func (h *Host) workloadObjectID(w *workload) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if o := h.byKey[objKey(w.tenantID, w.name, "", w.name)]; o != nil {
		return o.ID
	}
	return ""
}

// onPublicRequest is netd's per-request audit hook (D7).
func (h *Host) onPublicRequest(rec netd.PublicRequestRecord) {
	h.mu.Lock()
	o := h.byNetd[rec.ObjectID]
	h.mu.Unlock()
	if o == nil {
		return
	}
	d := map[string]any{"method": rec.Method, "path": rec.Path, "status": rec.Status, "ip": rec.ClientIP.String(),
		"bytesIn": rec.BytesIn, "bytesOut": rec.BytesOut, "ms": rec.Duration.Milliseconds()}
	if rec.Route != "" {
		d["route"] = rec.Route
	}
	if rec.Code != "" {
		d["code"] = rec.Code
	}
	b, _ := json.Marshal(d)
	h.audit.add(mgmtapi.AuditRecord{ObjectID: o.ID, Kind: "public_request", ActingFor: OriginPublic, Code: rec.Code, Detail: b, TraceID: newTraceID()})
}

// onPublicRejected records the per-minute summary of refused requests.
func (h *Host) onPublicRejected(s netd.PublicRejectSummary) {
	h.mu.Lock()
	o := h.byNetd[s.ObjectID]
	h.mu.Unlock()
	if o == nil {
		return
	}
	top := append([]netd.IPCount(nil), s.TopIPs...)
	sort.Slice(top, func(i, j int) bool { return top[i].Count > top[j].Count })
	b, _ := json.Marshal(map[string]any{"from": s.From.UTC().Format(time.RFC3339), "to": s.To.UTC().Format(time.RFC3339),
		"rateLimited": s.RateLimited, "tooLarge": s.TooLarge, "connsRefused": s.ConnsRefused, "topIPs": top, "uniqueIPApprox": s.UniqueIPApprox})
	h.audit.add(mgmtapi.AuditRecord{ObjectID: o.ID, Kind: "public_rejected", ActingFor: OriginPublic, Detail: b, TraceID: newTraceID()})
}

func (h *Host) publicCreatesPath() string { return filepath.Join(h.cfg.DataDir, "public-creates.json") }
