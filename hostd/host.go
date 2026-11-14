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
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/rosskukulinski/twd/hostd/image"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/internal/procsb"
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
	// FenceAfter: with no successful heartbeat for this long (monotonic
	// clock), the host stops serving its *stateful* workloads (assignment
	// "fence"; ADR-028 §3); stateless Workers keep serving. It must stay
	// below mgmt's NamespaceLeaseTTL (45 s) minus a margin, so a host has
	// stopped serving before its lease can be taken over: at most
	// MaxFenceAfter. 0 = DefaultFenceAfter (30 s); negative disables.
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
	// Nil means this host offers the process tier only: a microvm
	// deployment is refused (tier_unavailable), never downgraded.
	VM sandbox.Backend
	// ForceProcessTier is explicit local development (twd dev): every
	// workload runs in the process tier whatever its twd.jsonc or tenant
	// asks for, and the host registers the process tier only. Never set it
	// on a host that serves real tenants (ADR-032 D5b).
	ForceProcessTier bool
	// Capacity sets the capacity vector's VM dimensions (nil: measured; see
	// capacity.go). Its nodes are always CapacityNodes; without a microVM
	// backend every VM dimension is 0.
	Capacity *mgmtapi.HostCapacity
	// VMMemMiB and VMVCPUs are a microVM DO instance's guest size, for usage
	// accounting (defaults 512 and 2, twd-hostd's -vm-mem-mib / -vm-vcpus).
	VMMemMiB, VMVCPUs int
	// ContainerQuota reserves container quota at ctx.container.start()
	// (default: mgmt's /v1/container-reservations through Mgmt; tests: a
	// fake).
	ContainerQuota ContainerQuota
	// DefaultTier applies when neither the deployment (twd.jsonc "tier")
	// nor the tenant names one. Default "process".
	DefaultTier string
	// VMDebug enables guest diagnostics (connectivity probes; tests).
	VMDebug bool
	// VMVolumeMiB sizes new DO volumes (microVM tier; sparse; default 1024).
	VMVolumeMiB int
	// Sandbox confines process-tier workerds and Node apps: "none"
	// (default), "seatbelt" (macOS), "landlock" (Linux) or "auto".
	Sandbox string
	// SandboxLauncher is the binary that applies Landlock profiles
	// (default: this executable, which imports internal/procsb).
	SandboxLauncher string
	// InsecureSharedHost lets a host without a process sandbox run
	// process-tier workloads of more than one tenant. Without it such a host
	// refuses the second tenant's deployment (typed sandbox_unavailable), and
	// a Node-tier deployment there with egress_unenforced (RT-08: a hostile
	// Node app can open raw sockets past the egress proxy).
	InsecureSharedHost bool
	// HostSockets overrides the Linux risky-host-socket scan
	// (procsb.ScanHostSockets; tests).
	HostSockets func() procsb.HostSockets
	// SandboxDeny are extra paths no workerd may read, write or connect to
	// (twd up: its whole state dir, which holds mgmt's custody and socket).
	SandboxDeny []string
	// NodeBin is the node binary for runtime "node" deployments (default:
	// node from PATH).
	NodeBin string
	// AllowSiblingEgress lets an object's egress reach other objects hosted
	// by this host over the tailnet (a hairpin). Off by default (ADR-019
	// rule 2: same-host object calls go through the platform); single-host
	// tests and demos turn it on so that objects can exercise each other's
	// tailnet-facing authorization (e.g. e2e check (d), RT-01).
	AllowSiblingEgress bool
	// PushListener, when set, also serves the push API (mgmt → hostd:
	// kills, assignment changes) on it: the host's own tsnet node in
	// normal operation. PushPort is the port it listens on; it is sent
	// to mgmt at registration so pushes go to the WhoIs node's address.
	// The unix push socket (local-dev) is always served too.
	PushListener net.Listener
	PushPort     int

	// Containers boots container guests (ADR-032; hostd/vm on Linux +
	// KVM). Nil: this host offers no containers, and ctx.container.start()
	// fails with tier_unavailable (no fallback, D5).
	Containers sandbox.ContainerBackend
	// Images converts and caches container images (hostd/image).
	Images *image.Store
	// ContainerCapacity optionally bounds this host's running containers
	// further, on top of the capacity vector (Capacity) they share with
	// microVM DO instances (0 = no extra bound).
	ContainerCapacity ContainerCapacity
	// ContainerIdle is the default inactivity timeout (DefaultContainerIdle);
	// setInactivityTimeout changes it per container, up to 6 h.
	ContainerIdle time.Duration
	// ContainerMaxLife is the operator's hard max lifetime of a container
	// (DefaultContainerMaxLife, 24 h).
	ContainerMaxLife time.Duration
	// ContainerGrace caps destroy()'s SIGTERM → SIGKILL grace (10 s).
	ContainerGrace time.Duration
	// ContainerInternet is the sandbox internet path (ADR-032 D4): how a
	// container started with enableInternet reaches the hosts on its
	// workload's egress.internet allowlist. Nil: none; such flows are
	// refused. twd-hostd sets DirectInternet unless -container-internet=off.
	ContainerInternet *ContainerInternet
}

// ContainerInternet dials allowlisted internet hosts for containers. The
// sandbox egress proxy (netd/sbxnet) applies the allowlist and the ADR-019
// blocks to every resolved address before Dial is called.
type ContainerInternet struct {
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// HostAddrs are never dialled (nil: the host's interface addresses).
	HostAddrs []netip.Addr
}

// DirectInternet is the host network and resolver (twd has no tenant
// exit-node or gateway path yet; RFC §4.8).
func DirectInternet() *ContainerInternet {
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &ContainerInternet{Dial: d.DialContext, Resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}}
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
	sandbox   string // resolved Config.Sandbox
	lastHB    time.Time
	fenced    bool

	capOnce    sync.Once
	capVec     mgmtapi.HostCapacity // capacity(): measured once
	containers containerLedger      // admitted containers and held reservations
	quota      ContainerQuota

	syncMu      sync.Mutex
	syncNow     chan struct{}
	pushSrv     *http.Server
	pushSock    string
	audit       *auditLog
	wg          sync.WaitGroup
	runDirOwned bool
	ctr         *ctrManager
}

// DefaultFenceAfter rides out typical mgmt restarts and database failovers;
// MaxFenceAfter keeps it below mgmt's 45 s namespace lease with a 5 s margin
// for heartbeat RTT and clock-rate drift (ADR-028 §3; checked against
// mgmt.NamespaceLeaseTTL in hostd's tests).
const (
	DefaultFenceAfter = 30 * time.Second
	MaxFenceAfter     = 40 * time.Second
)

// New validates cfg and prepares (but does not start) a Host.
func New(cfg Config) (*Host, error) {
	if cfg.HostID == "" || cfg.Mgmt == nil || cfg.DataDir == "" || cfg.WorkerdBin == "" {
		return nil, errors.New("hostd: HostID, Mgmt, DataDir and WorkerdBin are required")
	}
	if cfg.HeartbeatEvery == 0 {
		cfg.HeartbeatEvery = 3 * time.Second
	}
	if cfg.FenceAfter == 0 {
		cfg.FenceAfter = DefaultFenceAfter
	}
	if cfg.FenceAfter > MaxFenceAfter {
		return nil, fmt.Errorf("hostd: FenceAfter %v must be ≤ %v (below mgmt's namespace lease, so a fenced host has stopped before its lease can be taken over)", cfg.FenceAfter, MaxFenceAfter)
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 30 * time.Second
	}
	if cfg.VMVolumeMiB == 0 {
		cfg.VMVolumeMiB = 1024
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
	if cfg.VMMemMiB == 0 {
		cfg.VMMemMiB = 512
	}
	if cfg.VMVCPUs == 0 {
		cfg.VMVCPUs = 2
	}
	if cfg.ContainerQuota == nil {
		cfg.ContainerQuota = mgmtQuota{cfg.Mgmt}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.DefaultTier == "microvm" && cfg.VM == nil && !cfg.ForceProcessTier {
		return nil, errors.New("hostd: default tier microvm needs a microVM backend (Linux + KVM + twd-vmhelper: `sudo twd host setup --microvm`); without one, use -default-tier process")
	}
	sb, err := ResolveSandbox(cfg.Sandbox)
	if err != nil {
		return nil, err
	}
	cfg.Mgmt.HostID = cfg.HostID
	h := &Host{cfg: cfg, sandbox: sb, log: cfg.Logger, mgmt: cfg.Mgmt,
		tenants: map[string]*tenant{}, workloads: map[string]*workload{}, objects: map[string]*object{},
		byNetd: map[string]*object{}, byKey: map[string]*object{}, creating: map[string]*createCall{},
		syncNow: make(chan struct{}, 1), quota: cfg.ContainerQuota, containers: containerLedger{held: map[ContainerKey]*ctrEntry{}}}
	h.ctr = newCtrManager(h)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	os.Chmod(cfg.DataDir, 0o700)
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
	// No sandboxed workload may use the push API (Linux: Landlock < ABI 9
	// can't refuse the connect itself).
	ln = procsb.Guard(ln, nil)
	h.pushSrv = &http.Server{Handler: h.pushAPI(), ReadHeaderTimeout: 10 * time.Second}
	go h.pushSrv.Serve(ln)
	if h.cfg.PushListener != nil {
		go h.pushSrv.Serve(h.cfg.PushListener)
	}
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
	capVec := h.capacity()
	req := mgmtapi.RegisterRequest{HostID: h.cfg.HostID, CapacityNodes: h.cfg.CapacityNodes, Tiers: h.tiers(),
		Arch: runtime.GOARCH, Capacity: &capVec,
		AuditPubKey: base64.StdEncoding.EncodeToString(h.audit.key.Public().(ed25519.PublicKey))}
	if h.cfg.PushPort > 0 {
		// tsnet transport: mgmt pushes to this host's node at PushPort.
		req.PushPort = h.cfg.PushPort
	} else {
		// local-dev: the unix push socket.
		req.PushURL = "unix://" + h.pushSock
	}
	return h.mgmt.Register(ctx, req)
}

// tiers lists what this host offers: the process tier always, the microVM
// tier with a backend (not under ForceProcessTier), and "containers" when
// it can run them (ADR-032).
func (h *Host) tiers() []string {
	tiers := []string{"process"}
	if h.cfg.VM != nil && !h.cfg.ForceProcessTier {
		tiers = append(tiers, "microvm")
	}
	if h.containersOffered() {
		tiers = append(tiers, "containers")
	}
	return tiers
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
	h.ctr.destroyWhere(func(ctrKey) bool { return true }, "host shutdown", 0)
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
			h.log.Error("no successful heartbeat; self-fencing stateful workloads (stateless keep serving)", "after", h.cfg.FenceAfter)
			h.fenceStateful("heartbeat lost")
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
	h.retryReleases(ctx)
}

func (h *Host) heartbeat(ctx context.Context) error {
	usage := h.usage()
	req := mgmtapi.HeartbeatRequest{Leases: []mgmtapi.Lease{}, Usage: &usage, Containers: h.containers.running()}
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

// fenceStateful stops serving every workload whose assignment says fence
// (it has Durable Objects that must not run on two hosts). They are
// redeployed by the next successful assignment sync.
func (h *Host) fenceStateful(reason string) {
	for _, w := range h.fenceTargets() {
		h.undeploy(w, reason)
	}
}

// fenceTargets are the workloads heartbeat loss must stop.
func (h *Host) fenceTargets() []*workload {
	h.mu.Lock()
	defer h.mu.Unlock()
	wls := make([]*workload, 0, len(h.workloads))
	for _, w := range h.workloads {
		if w.fences() {
			wls = append(wls, w)
		}
	}
	return wls
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
