package hostd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/netd"
)

const (
	statusPending = "pending"
	statusReady   = "ready"
	statusFailed  = "failed"
	statusKilled  = "killed"
)

// object is one tailnet identity hosted here: a workload entry node
// (Class == "") or a Durable Object instance.
type object struct {
	ID       string   `json:"id"` // mgmt object ID
	NetdID   string   `json:"netdID"`
	TenantID string   `json:"tenantID"`
	Workload string   `json:"workload"`
	Class    string   `json:"class,omitempty"`
	Name     string   `json:"name"`
	Hostname string   `json:"hostname"`
	Tags     []string `json:"tags"`
	Parent   string   `json:"parent,omitempty"`
	// ActingFor is the principal the object was created for (audit).
	ActingFor string    `json:"actingFor,omitempty"`
	Status    string    `json:"status"`
	NodeID    string    `json:"nodeID,omitempty"`
	MagicDNS  string    `json:"magicDNS,omitempty"`
	IP        string    `json:"ip,omitempty"`
	Err       string    `json:"error,omitempty"`
	Wakes     int       `json:"wakes"`
	Created   time.Time `json:"created"`

	live      bool          // node created in the netd registry (this process)
	readyCh   chan struct{} // closed on ready/failed/killed
	joinFails int           // consecutive bootstrap failures (retryNode)
}

func (o *object) namespace() string { return mgmtapi.Namespace(o.Workload, o.Class) }

// key is the local lookup key: tenant|workload|class|name.
func (o *object) key() string { return objKey(o.TenantID, o.Workload, o.Class, o.Name) }

func objKey(tenantID, workload, class, name string) string {
	return tenantID + "|" + workload + "|" + class + "|" + name
}

func (o *object) terminal() bool { return o.Status == statusKilled }

type createCall struct {
	done chan struct{}
	o    *object
	err  error
}

// apiError is a typed error for the local APIs.
type apiError struct {
	Code   string
	Detail string
	Extra  map[string]any
}

func (e *apiError) Error() string { return e.Code + ": " + e.Detail }

func asAPIError(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	if me, ok := mgmtapi.AsError(err); ok {
		x := map[string]any{}
		if len(me.Missing) > 0 {
			x["missing"] = me.Missing
		}
		if me.Snippet != "" {
			x["snippet"] = me.Snippet
		}
		return &apiError{Code: me.Code, Detail: me.Message, Extra: x}
	}
	if ne, ok := netd.AsError(err); ok {
		return &apiError{Code: ne.Code, Detail: ne.Detail}
	}
	return &apiError{Code: "hostd_unavailable", Detail: netd.Redact(err.Error())}
}

var invalidLabel = regexp.MustCompile(`[^a-z0-9-]+`)

// label sanitizes s into a DNS label fragment.
func label(s string) string {
	s = invalidLabel.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(regexp.MustCompile(`-+`).ReplaceAllString(s, "-"), "-")
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:2])
}

// hostnameFor is "<prefix><workload>" for the workload node and
// "<prefix><workload>-<class>-<name>" for an object (lowercased, sanitized,
// ≤ 63 chars; a short hash is appended when sanitizing lost information).
func (h *Host) hostnameFor(workload, class, name string) string {
	raw := workload
	if class != "" {
		raw = workload + "-" + class + "-" + name
	}
	l := label(raw)
	exact := class == "" || (label(name) == name)
	hn := label(h.cfg.HostnamePrefix + l)
	if !exact || len(hn) > 63 {
		suf := "-" + shortHash(raw)
		if len(hn) > 63-len(suf) {
			hn = strings.TrimRight(hn[:63-len(suf)], "-")
		}
		hn += suf
	}
	return hn
}

// netdIDFor maps a mgmt object ID to a netd object ID (a DNS label).
func netdIDFor(id string) string {
	l := label(id)
	if l == "" || len(l) > 63 || l != strings.ToLower(id) {
		return "o" + shortHash(id) + "-" + func() string {
			if len(l) > 50 {
				return l[:50]
			}
			return l
		}()
	}
	return l
}

// ensure returns the object for (workload, class, name), creating it via
// mgmt and starting its node (async) on first use. class "" is the
// workload entry node. parent/caps/ttl apply to spawns.
func (h *Host) ensure(ctx context.Context, w *workload, class, name string, parent *object, caps json.RawMessage, ttl string, actingFor ...string) (*object, error) {
	key := objKey(w.tenantID, w.name, class, name)
	for {
		h.mu.Lock()
		if o := h.byKey[key]; o != nil {
			if !o.live && !o.terminal() && w.isActive() {
				h.startNodeLocked(o)
			}
			h.mu.Unlock()
			return o, nil
		}
		if c := h.creating[key]; c != nil {
			h.mu.Unlock()
			select {
			case <-c.done:
				if c.err != nil {
					return nil, c.err
				}
				continue
			case <-ctx.Done():
				return nil, &apiError{Code: "identity_pending", Detail: "object creation still in progress"}
			}
		}
		c := &createCall{done: make(chan struct{})}
		h.creating[key] = c
		h.mu.Unlock()

		af := ""
		if len(actingFor) > 0 {
			af = actingFor[0]
		}
		o, err := h.create(ctx, w, class, name, parent, caps, ttl, af)
		h.mu.Lock()
		delete(h.creating, key)
		c.o, c.err = o, err
		close(c.done)
		h.mu.Unlock()
		return o, err
	}
}

func (h *Host) create(ctx context.Context, w *workload, class, name string, parent *object, caps json.RawMessage, ttl, actingFor string) (*object, error) {
	if w.tenant == nil {
		return nil, &apiError{Code: "upstream_unavailable", Detail: "workload " + w.name + " is not deployed"}
	}
	hn := h.hostnameFor(w.name, class, name)
	if actingFor == "" && parent != nil {
		actingFor = parent.ActingFor // a sub-agent acts for its root's principal
	}
	req := mgmtapi.CreateObjectRequest{TenantID: w.tenantID, Workload: w.name, Namespace: mgmtapi.Namespace(w.name, class), ObjectName: name,
		Caps: caps, TTL: ttl, Hostname: hn, ActingFor: actingFor}
	if parent != nil {
		req.Parent = &mgmtapi.ObjectRef{ObjectID: parent.ID}
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := h.mgmt.CreateObject(cctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Hostname != "" && resp.Hostname != hn {
		// mgmt names the node (M2: the object ID); keep the host's prefix
		// (per-run test names, ttest-*/twd-*).
		hn = resp.Hostname
		if p := h.cfg.HostnamePrefix; p != "" && !strings.HasPrefix(hn, p) {
			hn = label(p + hn)
			if len(hn) > 63 {
				hn = strings.TrimRight(hn[:63], "-")
			}
		}
	}
	o := &object{ID: resp.ObjectID, NetdID: netdIDFor(resp.ObjectID), TenantID: w.tenantID, Workload: w.name,
		Class: class, Name: name, Hostname: hn, Tags: resp.Tags, Status: statusPending, Created: time.Now(), ActingFor: actingFor,
		readyCh: make(chan struct{})}
	if parent != nil {
		o.Parent = parent.ID
	}
	h.mu.Lock()
	if old := h.objects[o.ID]; old != nil && old.terminal() && resp.Status == statusPending {
		// A killed tombstone of the same identity, and mgmt made a new
		// object (the old one was purged): forget the tombstone.
		h.forgetLocked(old)
	}
	if old := h.objects[o.ID]; old != nil { // mgmt returned an existing object
		if old.TenantID != w.tenantID || old.Workload != w.name || old.Class != class || old.Name != name {
			// mgmt scopes identity by (tenant, workload, class, name) (M7),
			// so this is an invariant violation (an old or buggy mgmt).
			// Sharing the object would hand one app another's node
			// identity: fail closed.
			h.mu.Unlock()
			h.log.Error("identity invariant violated: mgmt returned another identity's object", "object", o.ID,
				"want", objKey(w.tenantID, w.name, class, name), "have", old.key())
			return nil, &apiError{Code: "namespace_conflict", Detail: fmt.Sprintf(
				"mgmt returned object %s for %s/%s of workload %s, but it is %s/%s of workload %s (tenant %s): refusing to share a node identity",
				o.ID, class, name, w.name, old.Class, old.Name, old.Workload, old.TenantID)}
		}
		h.byKey[o.key()] = old
		if !old.live && !old.terminal() {
			h.startNodeLocked(old)
		}
		h.mu.Unlock()
		return old, nil
	}
	h.objects[o.ID] = o
	h.byNetd[o.NetdID] = o
	h.byKey[o.key()] = o
	h.startNodeLocked(o)
	h.mu.Unlock()
	h.saveObjects()
	if parent != nil {
		h.audit.add(mgmtapi.AuditRecord{ObjectID: o.ID, Kind: "spawn", Detail: "by " + parent.ID + " caps " + string(caps), TraceID: newTraceID()})
	}
	h.log.Info("object created", "object", o.ID, "class", class, "name", name, "hostname", hn, "acting_for", actingFor)
	return o, nil
}

// startNodeLocked creates the object's node in its tenant's netd registry
// (async join; rejoins from saved state if any). h.mu must be held.
func (h *Host) startNodeLocked(o *object) {
	t := h.tenants[o.TenantID]
	if t == nil || o.live {
		return
	}
	if o.readyCh == nil {
		o.readyCh = make(chan struct{})
	}
	eph := h.cfg.Ephemeral
	no, err := t.reg.Create(o.NetdID, netd.ObjectOptions{Hostname: o.Hostname, Tags: o.Tags, Ephemeral: &eph})
	if err != nil {
		o.Status, o.Err = statusFailed, err.Error()
		closeOnce(o.readyCh)
		return
	}
	o.live = true
	if o.Status != statusReady {
		o.Status = statusPending
	}
	go h.awaitNode(t, o, no)
}

func (h *Host) awaitNode(t *tenant, o *object, no *netd.Object) {
	ctx, cancel := context.WithTimeout(h.ctx, 3*time.Minute)
	defer cancel()
	err := no.Ready(ctx)
	h.mu.Lock()
	if o.Status == statusKilled {
		h.mu.Unlock()
		return
	}
	if err != nil {
		o.Status, o.Err, o.live = statusFailed, netd.Redact(err.Error()), false
		ch := o.readyCh
		o.readyCh = make(chan struct{}) // a later ensure may retry
		o.joinFails++
		fails := o.joinFails
		h.mu.Unlock()
		closeOnce(ch)
		h.log.Warn("object node failed", "object", o.ID, "err", o.Err, "attempt", fails)
		h.retryNode(o, fails)
		return
	}
	o.joinFails = 0
	info := no.Info()
	o.NodeID, o.MagicDNS, o.Status, o.Err = info.NodeID, strings.TrimSuffix(info.DNSName, "."), statusReady, ""
	if len(info.IPs) > 0 {
		o.IP = info.IPs[0]
	}
	ch := o.readyCh
	h.mu.Unlock()
	rctx, rcancel := context.WithTimeout(h.ctx, 15*time.Second)
	var epoch int64
	if w := h.workloadOf(o); w != nil {
		epoch = w.epochNow()
	}
	if err := h.mgmt.Ready(rctx, o.ID, mgmtapi.ReadyRequest{NodeID: info.NodeID, StableNodeID: info.NodeID, MagicDNS: o.MagicDNS, Epoch: epoch}); err != nil {
		h.log.Warn("mgmt ready failed", "object", o.ID, "err", err)
		if e, ok := mgmtapi.AsError(err); ok && e.Code == mgmtapi.CodeKilled {
			rcancel()
			closeOnce(ch)
			go h.killLocal(o.ID, "killed during join")
			return
		}
	}
	rcancel()
	closeOnce(ch)
	h.saveObjects()
	h.log.Info("object node ready", "object", o.ID, "hostname", o.Hostname, "ip", o.IP, "join_ms", info.JoinMS, "rejoined", info.Rejoined, "reminted", info.Reminted)
}

// retryNode re-joins a node whose bootstrap failed while its workload is
// still deployed, after a backoff (1, 2, 4, 8, 16 s; 5 tries). The case
// that needs it: a `twd deploy` while this host is still bringing up the
// previous deployment's nodes bumps the lease epoch, so the mint in flight
// is refused with lease_lost; nothing else would retry it and the workload
// would stay "active" with a dead entry node ("node minted" forever). The
// lease epoch is refreshed from mgmt's assignments before the retry.
func (h *Host) retryNode(o *object, fails int) {
	const maxTries = 5
	if fails > maxTries {
		return
	}
	w := h.workloadOf(o)
	if w == nil || !w.isActive() {
		return
	}
	backoff := time.Second << (fails - 1)
	time.AfterFunc(backoff, func() {
		if as, err := h.mgmt.Assignments(h.ctx, h.cfg.HostID); err == nil {
			for _, a := range as {
				if a.TenantID == w.tenantID && a.Workload == w.name {
					w.mu.Lock()
					if a.Epoch > w.epoch {
						w.epoch = a.Epoch
					}
					w.mu.Unlock()
				}
			}
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if o.live || o.terminal() || o.Status == statusReady || !w.isActive() {
			return
		}
		h.log.Info("object node retry", "object", o.ID, "attempt", fails+1, "epoch", w.epochNow())
		h.startNodeLocked(o)
	})
}

func closeOnce(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// waitReady waits for o's node.
func (h *Host) waitReady(ctx context.Context, o *object, timeout time.Duration) error {
	h.mu.Lock()
	ch, st := o.readyCh, o.Status
	h.mu.Unlock()
	if st == statusReady {
		return nil
	}
	if st == statusKilled {
		return &apiError{Code: "killed", Detail: "object " + o.ID + " was killed"}
	}
	select {
	case <-ch:
	case <-time.After(timeout):
		return &apiError{Code: "identity_pending", Detail: fmt.Sprintf("node for %s not ready after %v", o.Hostname, timeout)}
	case <-ctx.Done():
		return &apiError{Code: "identity_pending", Detail: "cancelled while waiting for the node"}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch o.Status {
	case statusReady:
		return nil
	case statusKilled:
		return &apiError{Code: "killed", Detail: "object " + o.ID + " was killed"}
	}
	return &apiError{Code: "bootstrap_failed", Detail: "could not create node for " + o.Hostname + ": " + o.Err}
}

// killLocal is kill tier 1 on this host: RST every flow of the object's
// node (netd), keep it as a tombstone that RSTs new connections, stop the
// object in workerd, and record audit. mgmt deletes the device (tier 2).
func (h *Host) killLocal(objectID, reason string) {
	h.mu.Lock()
	o := h.objects[objectID]
	if o == nil || o.Status == statusKilled {
		h.mu.Unlock()
		return
	}
	o.Status = statusKilled
	delete(h.byKey, o.key())
	ch := o.readyCh
	t := h.tenants[o.TenantID]
	w := h.workloads[o.TenantID+"/"+o.Workload]
	live := o.live
	h.mu.Unlock()
	closeOnce(ch)
	if t != nil && t.certs != nil && o.NodeID != "" {
		t.certs.forget(o.NodeID) // its certificate must not be reused from cache
	}
	st := time.Now()
	if t != nil && live {
		opts := netd.KillOptions{Reason: reason, SkipDelete: true}
		if h.cfg.KillTombstone > 0 {
			opts.Tombstone = h.cfg.KillTombstone
		}
		rep, err := t.reg.Kill(context.Background(), o.NetdID, opts)
		if err == nil {
			h.log.Info("object killed (tier 1)", "object", o.ID, "flows", rep.FlowsAborted, "abort_ms", rep.AbortMS)
		}
	}
	if w != nil && o.Class != "" {
		if inst := w.current(); inst != nil {
			inst.admin(map[string]any{"kind": "admin", "op": "kill", "class": o.Class, "name": o.Name, "reason": reason})
		}
	}
	h.audit.add(mgmtapi.AuditRecord{ObjectID: o.ID, Kind: "kill", Detail: reason, TraceID: newTraceID()})
	h.saveObjects()
	h.log.Info("object killed", "object", o.ID, "reason", reason, "ms", time.Since(st).Milliseconds())
}

// ---- persistence ----

// forgetLocked drops a terminal object from the registry (h.mu held).
func (h *Host) forgetLocked(o *object) {
	delete(h.objects, o.ID)
	if h.byNetd[o.NetdID] == o {
		delete(h.byNetd, o.NetdID)
	}
	if h.byKey[o.key()] == o {
		delete(h.byKey, o.key())
	}
}

// Purge forgets every killed object (twd down --purge), removes its saved
// node state (its device is gone; a kill's tombstone would otherwise keep
// the state dir until it expires, and a redeploy of the same identity would
// re-mint instead of joining fresh), and saves objects.json without them.
// Returns the forgotten IDs.
func (h *Host) Purge() []string {
	h.mu.Lock()
	ids := []string{}
	var gone []*object
	for _, o := range h.objects {
		if o.terminal() {
			h.forgetLocked(o)
			ids = append(ids, o.ID)
			gone = append(gone, o)
		}
	}
	tenants := map[string]*tenant{}
	for _, o := range gone {
		tenants[o.TenantID] = h.tenants[o.TenantID]
	}
	h.mu.Unlock()
	for _, o := range gone {
		if t := tenants[o.TenantID]; t != nil && t.reg != nil {
			if err := t.reg.RemoveState(o.NetdID); err != nil {
				h.log.Warn("purge: node state kept", "object", o.ID, "err", err)
			}
		}
	}
	h.saveObjects()
	return ids
}

func (h *Host) objectsPath() string { return filepath.Join(h.cfg.DataDir, "objects.json") }

func (h *Host) loadObjects() error {
	b, err := os.ReadFile(h.objectsPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []*object
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("hostd: objects.json: %w", err)
	}
	for _, o := range list {
		o.readyCh = make(chan struct{})
		if o.Status == statusKilled {
			closeOnce(o.readyCh)
		} else {
			o.Status = statusPending // until the node rejoins
			h.byKey[o.key()] = o
		}
		h.objects[o.ID] = o
		h.byNetd[o.NetdID] = o
	}
	return nil
}

func (h *Host) saveObjects() {
	h.mu.Lock()
	list := make([]*object, 0, len(h.objects))
	for _, o := range h.objects {
		c := *o
		list = append(list, &c)
	}
	h.mu.Unlock()
	b, _ := json.MarshalIndent(list, "", " ")
	tmp := h.objectsPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		os.Rename(tmp, h.objectsPath())
	}
}

// objectsOf returns the workload's objects.
func (h *Host) objectsOf(tenantID, workload string) []*object {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*object
	for _, o := range h.objects {
		if o.TenantID == tenantID && o.Workload == workload {
			out = append(out, o)
		}
	}
	return out
}

// isDescendant reports whether o descends from anc (local lineage).
func (h *Host) isDescendant(o, anc *object) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for p := o.Parent; p != ""; {
		if p == anc.ID {
			return true
		}
		po := h.objects[p]
		if po == nil {
			return false
		}
		p = po.Parent
	}
	return false
}

func (h *Host) workloadOf(o *object) *workload {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.workloads[o.TenantID+"/"+o.Workload]
}
