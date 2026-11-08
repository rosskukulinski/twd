// Package hostd is twd-hostd's process tier: it registers with twd-mgmt and
// heartbeats, pulls assignments, and for each deployment unpacks the
// bundle, generates the workerd config (platform wrapper + tenant service),
// supervises one workerd process per tenant workload, and drives the netd
// library so that the workload and every Durable Object instance get their
// own tailnet node (async, on first use).
//
// Data flow (see gen for the workerd side):
//
//	caller ─WG─▶ node (netd: WhoIs, strip, cf blob) ─▶ hostd ingress (adds cf.twd routing)
//	       ─▶ workerd ingress.sock ─▶ platform entry ─▶ Host_<Class> DO ─▶ tenant facet
//	tenant fetch() ─▶ platform Egress (props: class,name; MAC) ─▶ hostd egress-obj.sock
//	       ─▶ netd egress (object's own node, ADR-019 guard) ─WG─▶ destination
package hostd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/netd"
)

// DefaultControlURL is used when a tenant record names no control server.
const DefaultControlURL = "https://controlplane.tailscale.com"

// Config configures a Host.
type Config struct {
	HostID string
	Mgmt   *mgmtapi.Client
	// DataDir holds node state, DO storage, objects.json and the audit key.
	DataDir string
	// RunDir holds unix sockets and generated instance dirs; keep it short
	// (macOS limits socket paths to 104 bytes). Default: a new /tmp dir.
	RunDir     string
	WorkerdBin string
	// HeartbeatEvery is the heartbeat (and assignment poll) period (3 s).
	HeartbeatEvery time.Duration
	// FenceAfter: with no successful heartbeat for this long, the host stops
	// serving every namespace (self-fencing below the 10 s lease TTL). 0 = 9 s;
	// negative disables.
	FenceAfter time.Duration
	// HostnamePrefix is prepended to every node hostname (e2e runs use a
	// unique per-run prefix).
	HostnamePrefix string
	// Ephemeral joins nodes as ephemeral (tests and dev).
	Ephemeral     bool
	CapacityNodes int
	// ReadyTimeout bounds how long requests wait for an object's node
	// (identity_pending after; default 30 s).
	ReadyTimeout time.Duration
	// DrainTimeout bounds the blue/green drain of the old workerd (10 s).
	DrainTimeout time.Duration
	// KillTombstone keeps a killed node up (RST-ing new connections) this
	// long before closing it (default 10 s; negative disables).
	KillTombstone time.Duration
	// ControlURL overrides every tenant's control URL (tests: testcontrol).
	ControlURL string
	// JoinParallel bounds concurrent node joins per tenant (netd default 16).
	JoinParallel int
	Logger       *slog.Logger
	TSNetLogs    bool
	// VM boots microVM sandboxes (hostd/vm: Firecracker on Linux + KVM).
	// Nil means every workload runs in the process tier, whatever it asks for.
	VM sandbox.Backend
	// DefaultTier applies when neither the deployment (twd.jsonc "tier")
	// nor the tenant names one. Default "process".
	DefaultTier string
	// VMDebug enables guest diagnostics (connectivity probes; tests).
	VMDebug bool
	// NodeBin is the node binary for runtime "node" deployments (default:
	// node from PATH).
	NodeBin string
}

// Host is a running twd-hostd.
type Host struct {
	cfg    Config
	log    *slog.Logger
	mgmt   *mgmtapi.Client
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	tenants   map[string]*tenant
	workloads map[string]*workload // tenantID/workload
	objects   map[string]*object   // mgmt object ID
	byNetd    map[string]*object   // netd object ID
	byKey     map[string]*object   // tenantID|namespace|name (live only)
	creating  map[string]*createCall
	instSeq   int
	lastHB    time.Time
	fenced    bool

	syncMu      sync.Mutex
	syncNow     chan struct{}
	pushSrv     *http.Server
	pushSock    string
	audit       *auditLog
	wg          sync.WaitGroup
	runDirOwned bool
}

// New validates cfg and prepares (but does not start) a Host.
func New(cfg Config) (*Host, error) {
	if cfg.HostID == "" || cfg.Mgmt == nil || cfg.DataDir == "" || cfg.WorkerdBin == "" {
		return nil, errors.New("hostd: HostID, Mgmt, DataDir and WorkerdBin are required")
	}
	if cfg.HeartbeatEvery == 0 {
		cfg.HeartbeatEvery = 3 * time.Second
	}
	if cfg.FenceAfter == 0 {
		cfg.FenceAfter = 9 * time.Second
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 30 * time.Second
	}
	if cfg.DrainTimeout == 0 {
		cfg.DrainTimeout = 10 * time.Second
	}
	if cfg.KillTombstone == 0 {
		cfg.KillTombstone = 10 * time.Second
	}
	if cfg.CapacityNodes == 0 {
		cfg.CapacityNodes = 1000
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	cfg.Mgmt.HostID = cfg.HostID
	h := &Host{cfg: cfg, log: cfg.Logger, mgmt: cfg.Mgmt,
		tenants: map[string]*tenant{}, workloads: map[string]*workload{}, objects: map[string]*object{},
		byNetd: map[string]*object{}, byKey: map[string]*object{}, creating: map[string]*createCall{},
		syncNow: make(chan struct{}, 1)}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	if h.cfg.RunDir == "" {
		d, err := os.MkdirTemp("/tmp", "twd-h-")
		if err != nil {
			return nil, err
		}
		h.cfg.RunDir, h.runDirOwned = d, true
	}
	if err := os.MkdirAll(h.cfg.RunDir, 0o700); err != nil {
		return nil, err
	}
	os.Chmod(h.cfg.RunDir, 0o700)
	seed, err := loadOrCreateKey(filepath.Join(cfg.DataDir, "audit.key"))
	if err != nil {
		return nil, err
	}
	key := ed25519.NewKeyFromSeed(seed[:ed25519.SeedSize])
	h.audit = newAuditLog(key)
	if err := h.loadObjects(); err != nil {
		return nil, err
	}
	return h, nil
}

// PushSocket is the push API socket ("unix://" + path is registered as pushURL).
func (h *Host) PushSocket() string { return h.pushSock }

// Start registers with mgmt and starts the heartbeat/assignment loop.
func (h *Host) Start(ctx context.Context) error {
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.pushSock = filepath.Join(h.cfg.RunDir, "hostd.sock")
	os.Remove(h.pushSock)
	ln, err := net.Listen("unix", h.pushSock)
	if err != nil {
		return err
	}
	os.Chmod(h.pushSock, 0o600)
	h.pushSrv = &http.Server{Handler: h.pushAPI(), ReadHeaderTimeout: 10 * time.Second}
	go h.pushSrv.Serve(ln)
	if err := h.register(ctx); err != nil {
		h.pushSrv.Close()
		return fmt.Errorf("hostd: register: %w", err)
	}
	h.mu.Lock()
	h.lastHB = time.Now()
	h.mu.Unlock()
	h.wg.Add(1)
	go h.loop()
	return nil
}

func (h *Host) register(ctx context.Context) error {
	tiers := []string{"process"}
	if h.cfg.VM != nil {
		tiers = append(tiers, "microvm")
	}
	return h.mgmt.Register(ctx, mgmtapi.RegisterRequest{HostID: h.cfg.HostID, CapacityNodes: h.cfg.CapacityNodes,
		Tiers: tiers, PushURL: "unix://" + h.pushSock,
		AuditPubKey: base64.StdEncoding.EncodeToString(h.audit.key.Public().(ed25519.PublicKey))})
}

// Close stops every workload and node (node state is kept for rejoin).
func (h *Host) Close() error {
	if h.cancel != nil {
		h.cancel()
	}
	h.wg.Wait()
	if h.pushSrv != nil {
		h.pushSrv.Close()
	}
	h.mu.Lock()
	wls := make([]*workload, 0, len(h.workloads))
	for _, w := range h.workloads {
		wls = append(wls, w)
	}
	ts := make([]*tenant, 0, len(h.tenants))
	for _, t := range h.tenants {
		ts = append(ts, t)
	}
	h.mu.Unlock()
	for _, w := range wls {
		w.stopAll()
	}
	var wg sync.WaitGroup
	for _, t := range ts {
		wg.Add(1)
		go func() { defer wg.Done(); t.reg.Close() }()
	}
	wg.Wait()
	h.flushAudit(context.Background())
	h.saveObjects()
	if h.runDirOwned {
		os.RemoveAll(h.cfg.RunDir)
	}
	return nil
}

// TriggerSync asks the loop to poll assignments now.
func (h *Host) TriggerSync() {
	select {
	case h.syncNow <- struct{}{}:
	default:
	}
}

func (h *Host) loop() {
	defer h.wg.Done()
	t := time.NewTicker(h.cfg.HeartbeatEvery)
	defer t.Stop()
	h.tick()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
		case <-h.syncNow:
		}
		h.tick()
	}
}

func (h *Host) tick() {
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	if err := h.heartbeat(ctx); err != nil {
		if h.ctx.Err() != nil {
			return
		}
		h.log.Warn("heartbeat failed", "err", err)
		if e, ok := mgmtapi.AsError(err); ok && e.Status == 404 {
			h.register(ctx)
		}
		h.mu.Lock()
		stale := h.cfg.FenceAfter > 0 && time.Since(h.lastHB) > h.cfg.FenceAfter && !h.fenced
		if stale {
			h.fenced = true
		}
		h.mu.Unlock()
		if stale {
			h.log.Error("no successful heartbeat; self-fencing every namespace", "after", h.cfg.FenceAfter)
			h.fenceAll("heartbeat lost")
		}
		return
	}
	h.mu.Lock()
	h.lastHB, h.fenced = time.Now(), false
	h.mu.Unlock()
	if err := h.syncAssignments(ctx); err != nil && h.ctx.Err() == nil {
		h.log.Warn("assignment sync failed", "err", err)
	}
	h.flushAudit(ctx)
}

func (h *Host) heartbeat(ctx context.Context) error {
	req := mgmtapi.HeartbeatRequest{Leases: []mgmtapi.Lease{}}
	h.mu.Lock()
	for _, o := range h.objects {
		if o.Status == statusReady {
			req.LiveNodes++
		}
	}
	keys := make([]string, 0, len(h.workloads))
	for k := range h.workloads {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	wls := make([]*workload, 0, len(keys))
	for _, k := range keys {
		wls = append(wls, h.workloads[k])
	}
	h.mu.Unlock()
	for _, w := range wls {
		st := w.statusSnapshot()
		if st.DeploymentID == "" {
			continue
		}
		req.Deployments = append(req.Deployments, st)
		if st.Status == "active" || st.Status == "starting" {
			req.Leases = append(req.Leases, mgmtapi.Lease{Namespace: w.lease(), Epoch: w.epochNow()})
		}
	}
	resp, err := h.mgmt.Heartbeat(ctx, h.cfg.HostID, req)
	if err != nil {
		return err
	}
	for _, k := range resp.Kill {
		go h.killLocal(k.ObjectID, k.Reason)
	}
	for _, id := range resp.RevokeObjects {
		go h.killLocal(id, "revoked by mgmt")
	}
	if len(resp.Revoke) > 0 {
		h.revoke(resp.Revoke)
	}
	return nil
}

// revoke stops serving namespaces whose lease this host lost — unless mgmt
// re-assigned the workload to this host at a newer epoch (a redeploy), in
// which case the new epoch is adopted.
func (h *Host) revoke(nss []string) {
	if as, err := h.mgmt.Assignments(h.ctx, h.cfg.HostID); err == nil {
		h.mu.Lock()
		for _, a := range as {
			if w := h.workloads[a.TenantID+"/"+a.Workload]; w != nil {
				w.mu.Lock()
				if a.Epoch > w.epoch {
					w.epoch = a.Epoch
				}
				w.mu.Unlock()
			}
		}
		h.mu.Unlock()
		var keep []string
		for _, n := range nss {
			adopted := false
			for _, a := range as {
				if n == mgmtapi.LeaseName(a.TenantID, a.Workload) || n == a.Workload {
					adopted = true
				}
			}
			if !adopted {
				keep = append(keep, n)
			}
		}
		nss = keep
		if len(nss) == 0 {
			h.TriggerSync()
			return
		}
	}
	set := map[string]bool{}
	for _, n := range nss {
		set[n] = true
	}
	h.mu.Lock()
	var hit []*workload
	for _, w := range h.workloads {
		if set[w.lease()] || set[w.name] {
			hit = append(hit, w)
		}
	}
	h.mu.Unlock()
	for _, w := range hit {
		h.log.Warn("lease revoked; fencing workload", "workload", w.name, "namespaces", nss)
		h.undeploy(w, "lease revoked")
	}
}

func (h *Host) fenceAll(reason string) {
	h.mu.Lock()
	wls := make([]*workload, 0, len(h.workloads))
	for _, w := range h.workloads {
		wls = append(wls, w)
	}
	h.mu.Unlock()
	for _, w := range wls {
		h.undeploy(w, reason)
	}
}

func (h *Host) syncAssignments(ctx context.Context) error {
	h.syncMu.Lock()
	defer h.syncMu.Unlock()
	as, err := h.mgmt.Assignments(ctx, h.cfg.HostID)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, a := range as {
		key := a.TenantID + "/" + a.Workload
		want[key] = true
		h.mu.Lock()
		w := h.workloads[key]
		if w == nil {
			w = &workload{h: h, tenantID: a.TenantID, name: a.Workload}
			h.workloads[key] = w
		}
		h.mu.Unlock()
		if w.wantDeploy(a) {
			go h.deploy(w, a)
		}
	}
	h.mu.Lock()
	var gone []*workload
	for k, w := range h.workloads {
		if !want[k] {
			gone = append(gone, w)
		}
	}
	h.mu.Unlock()
	for _, w := range gone {
		if w.statusSnapshot().Status == "active" {
			h.undeploy(w, "no longer assigned")
		}
	}
	return nil
}

// ---- helpers ----

func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	b := make([]byte, 32)
	rand.Read(b)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return b, nil
}

func newTraceID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// writeErr renders a typed error (netd catalog shape).
func writeErr(w http.ResponseWriter, code, detail string) {
	netd.WriteError(w, &netd.Error{Code: code, Detail: detail})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
