package hostd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/hostd/gen"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/netd"
)

// workload is one deployed app of a tenant on this host. At most one
// workerd instance is active; a deploy starts a new one, then switches.
type workload struct {
	h        *Host
	tenantID string
	name     string

	deployMu sync.Mutex // serialises deploys

	mu           sync.Mutex
	tenant       *tenant
	app          *bundle.App
	deploymentID string // the assignment being (or last) deployed
	activeDep    string
	epoch        int64
	status       string
	err          string
	active       *instance
	gate         chan struct{} // non-nil while switching: closed when open
	wl           *object       // the workload entry node
	classTags    map[string][]string
}

func (w *workload) statusSnapshot() mgmtapi.DeploymentStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return mgmtapi.DeploymentStatus{DeploymentID: w.deploymentID, Status: w.status, Error: w.err}
}

func (w *workload) epochNow() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.epoch
}

func (w *workload) isActive() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status == "active" || w.status == "starting"
}

func (w *workload) currentWL() *object {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.wl
}

func (w *workload) current() *instance {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.active
}

// lease is the workload's namespace lease name (one per tenant+workload).
func (w *workload) lease() string { return mgmtapi.LeaseName(w.tenantID, w.name) }

// wantDeploy reports whether assignment a is new for this workload.
func (w *workload) wantDeploy(a mgmtapi.Assignment) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.deploymentID == a.DeploymentID && w.status != "stopped" && w.status != "failed" {
		return false
	}
	if w.deploymentID == a.DeploymentID && w.status == "failed" {
		return false // don't retry a failed deployment in a loop
	}
	w.deploymentID, w.status, w.err = a.DeploymentID, "starting", ""
	if a.Epoch > w.epoch {
		w.epoch = a.Epoch // this host holds the lease at the new epoch now
	}
	return true
}

func (w *workload) fail(depID string, err error) {
	w.mu.Lock()
	if w.deploymentID == depID {
		w.status, w.err = "failed", netd.Redact(err.Error())
	}
	w.mu.Unlock()
	w.h.log.Error("deploy failed", "workload", w.name, "deployment", depID, "err", err)
}

// acquire returns the active instance for one request (waiting while a
// blue/green switch is in progress) and a release func.
func (w *workload) acquire(ctx context.Context) (*instance, func()) {
	for {
		w.mu.Lock()
		g, inst := w.gate, w.active
		if g == nil {
			if inst == nil {
				w.mu.Unlock()
				return nil, func() {}
			}
			inst.inflight.Add(1)
			inst.reqs.Add(1)
			w.mu.Unlock()
			return inst, func() { inst.inflight.Add(-1) }
		}
		w.mu.Unlock()
		select {
		case <-g:
		case <-ctx.Done():
			return nil, func() {}
		}
	}
}

func (w *workload) stopAll() {
	w.mu.Lock()
	inst := w.active
	w.active = nil
	w.status = "stopped"
	w.mu.Unlock()
	if inst != nil {
		inst.stop()
	}
}

// deploy runs one assignment: bundle → config → workerd → nodes → switch.
func (h *Host) deploy(w *workload, a mgmtapi.Assignment) {
	w.deployMu.Lock()
	defer w.deployMu.Unlock()
	ctx, cancel := context.WithTimeout(h.ctx, 2*time.Minute)
	defer cancel()
	st := time.Now()
	t, err := h.ensureTenant(ctx, a.TenantID)
	if err != nil {
		w.fail(a.DeploymentID, err)
		return
	}
	raw, err := h.mgmt.Bundle(ctx, a.BundleURL)
	if err != nil {
		w.fail(a.DeploymentID, fmt.Errorf("fetch bundle: %w", err))
		return
	}
	files, err := bundle.Unpack(raw)
	if err != nil {
		w.fail(a.DeploymentID, err)
		return
	}
	app, err := bundle.Parse(files)
	if err != nil {
		w.fail(a.DeploymentID, err)
		return
	}
	for _, ns := range a.Namespaces {
		if c := app.Classes[ns.Class]; c != nil && len(ns.Tags) > 0 {
			c.Tags = ns.Tags
		}
	}
	w.mu.Lock()
	w.tenant, w.epoch = t, a.Epoch
	if w.app == nil {
		w.app = app
	}
	w.mu.Unlock()

	// The workload entry node (async; requests wait for it).
	wl, err := h.ensure(ctx, w, "", w.name, nil, nil, "")
	if err != nil {
		w.fail(a.DeploymentID, fmt.Errorf("workload node: %w", err))
		return
	}
	w.mu.Lock()
	w.wl = wl
	w.mu.Unlock()

	tier := h.tierFor(app, t)
	if app.Runtime == bundle.RuntimeNode || tier == bundle.TierMicroVM {
		// One process / one VM per data directory or volume: the old one
		// releases it before the new one starts.
		h.deploySerial(w, a, app, tier, st)
		return
	}
	inst, err := h.startInstance(w, a.DeploymentID, app, tier)
	if err != nil {
		w.fail(a.DeploymentID, err)
		return
	}
	// Blue/green: new requests wait at the gate while the old instance
	// drains, then go to the new one. Both instances share DO storage, so
	// they never serve at the same time.
	w.mu.Lock()
	if w.deploymentID != a.DeploymentID { // superseded while starting
		w.mu.Unlock()
		inst.stop()
		return
	}
	old := w.active
	gate := make(chan struct{})
	if old != nil {
		w.gate = gate
	}
	w.mu.Unlock()
	if old != nil {
		old.drain(h.cfg.DrainTimeout)
		old.stop()
	}
	w.mu.Lock()
	w.active, w.app, w.activeDep, w.status = inst, app, a.DeploymentID, "active"
	if w.gate != nil {
		close(w.gate)
		w.gate = nil
	}
	w.mu.Unlock()
	// Objects of this workload saved from a previous run rejoin now (nodes
	// stay online; no auth key, no registration budget).
	h.mu.Lock()
	for _, o := range h.objects {
		if o.TenantID == w.tenantID && o.Workload == w.name && !o.live && !o.terminal() {
			if o.Class == "" || app.Classes[o.Class] != nil && app.Classes[o.Class].Node {
				h.startNodeLocked(o)
			}
		}
	}
	h.mu.Unlock()
	h.log.Info("deployment active", "workload", w.name, "deployment", a.DeploymentID, "epoch", a.Epoch,
		"tier", tier, "blue_green", old != nil, "ms", time.Since(st).Milliseconds())
}

// deploySerial switches a workload stop-then-start. Node-tier runtimes
// (Vercel Workflow's local World, SQLite files) assume one process per
// data directory; a microVM's DO volume (L3) can be attached to one VM
// only, so the old VM drains, unmounts and releases it (helper) before the
// new one attaches it. New requests wait at the gate meanwhile.
func (h *Host) deploySerial(w *workload, a mgmtapi.Assignment, app *bundle.App, tier string, st time.Time) {
	w.mu.Lock()
	old := w.active
	gate := make(chan struct{})
	w.gate = gate
	w.mu.Unlock()
	open := func() {
		w.mu.Lock()
		if w.gate == gate {
			close(gate)
			w.gate = nil
		}
		w.mu.Unlock()
	}
	if old != nil {
		old.drain(h.cfg.DrainTimeout)
		old.stop()
	}
	inst, err := h.startInstance(w, a.DeploymentID, app, tier)
	w.mu.Lock()
	if old != nil && w.active == old {
		w.active = nil
	}
	w.mu.Unlock()
	if err != nil {
		open()
		w.fail(a.DeploymentID, err)
		return
	}
	w.mu.Lock()
	if w.deploymentID != a.DeploymentID { // superseded while starting
		w.mu.Unlock()
		open()
		inst.stop()
		return
	}
	w.active, w.app, w.activeDep, w.status = inst, app, a.DeploymentID, "active"
	w.mu.Unlock()
	open()
	h.log.Info("deployment active", "workload", w.name, "deployment", a.DeploymentID, "epoch", a.Epoch,
		"runtime", app.Runtime, "tier", tier, "restart", old != nil, "ms", time.Since(st).Milliseconds())
}

// undeploy stops serving a workload: workerd stops and its nodes close
// (state kept, so they rejoin if the workload comes back).
func (h *Host) undeploy(w *workload, reason string) {
	w.deployMu.Lock()
	defer w.deployMu.Unlock()
	w.mu.Lock()
	inst := w.active
	w.active = nil
	w.status, w.err = "stopped", reason
	t := w.tenant
	w.mu.Unlock()
	if inst != nil {
		inst.stop()
	}
	if t == nil {
		return
	}
	for _, o := range h.objectsOf(w.tenantID, w.name) {
		h.mu.Lock()
		live := o.live
		o.live = false
		if o.Status == statusReady || o.Status == statusPending {
			o.Status = statusPending
			o.readyCh = make(chan struct{})
		}
		h.mu.Unlock()
		if live {
			t.reg.Kill(context.Background(), o.NetdID, netd.KillOptions{Reason: reason, KeepState: true, SkipDelete: true})
		}
	}
	h.log.Warn("workload stopped", "workload", w.name, "reason", reason)
}

// ---- instance: one workerd process ----

type instance struct {
	h       *Host
	w       *workload
	depID   string
	dir     string
	app     *bundle.App
	res     *gen.Result
	ingress http.Handler
	direct  *http.Client
	tier    string // bundle.TierProcess | bundle.TierMicroVM
	spec    sandbox.Spec
	// doStorage is the workload's DO storage (process tier), under its
	// tenant's dir; profile is the Seatbelt profile it runs under, if any.
	doStorage string
	profile   string

	node *nodeProc // Node process tier (runtime "node"); nil for workerd

	inflight atomic.Int64
	reqs     atomic.Int64
	stopping atomic.Bool

	mu       sync.Mutex
	cmd      *exec.Cmd
	vm       sandbox.VM // microVM tier: the running sandbox
	exited   chan struct{}
	servers  []*http.Server
	restarts int
	started  time.Time
}

// tierFor picks a deployment's isolation tier: the bundle's twd.jsonc
// "tier", else the tenant's, else the host default. microvm falls back to
// process (with a warning) when this host has no microVM backend (macOS, no
// KVM, not root).
func (h *Host) tierFor(app *bundle.App, t *tenant) string {
	if app.Runtime == bundle.RuntimeNode {
		return bundle.TierProcess // the microVM guest has no Node.js (see STATUS.md M9)
	}
	tier := app.Tier
	if tier == "" && t != nil {
		tier = t.info.Tier
	}
	if tier == "" {
		tier = h.cfg.DefaultTier
	}
	if tier == bundle.TierMicroVM && h.cfg.VM == nil {
		h.log.Warn("microvm tier requested but this host has no microVM backend; using the process tier", "app", app.Name)
		tier = bundle.TierProcess
	}
	if tier != bundle.TierMicroVM {
		tier = bundle.TierProcess
	}
	return tier
}

func (h *Host) startInstance(w *workload, depID string, app *bundle.App, tier string) (*instance, error) {
	h.mu.Lock()
	h.instSeq++
	n := h.instSeq
	h.mu.Unlock()
	if app.Runtime == bundle.RuntimeNode {
		return h.startNodeInstance(w, depID, app, n)
	}
	dir := filepath.Join(h.cfg.RunDir, fmt.Sprintf("i%d", n))
	opts := gen.Options{
		Dir:          dir,
		DOStorage:    filepath.Join(h.tenantDir(w.tenantID), "workloads", w.name, "do"),
		DeploymentID: depID,
		Workload:     w.name,
	}
	vmTier := tier == bundle.TierMicroVM
	if vmTier {
		// The guest's workerd listens on its NIC and reaches hostd only via
		// the gateway (host netd's userspace stack); DO storage is the
		// workload's persistent volume, mounted at proto.DODir (L3).
		gw := proto.GatewayIP
		opts.Guest = &gen.GuestNet{
			Ingress:   fmt.Sprintf("*:%d", proto.IngressPort),
			EgressObj: fmt.Sprintf("%s:%d", gw, proto.EgressObj),
			EgressWL:  fmt.Sprintf("%s:%d", gw, proto.EgressWL),
			HostAPI:   fmt.Sprintf("%s:%d", gw, proto.HostAPI),
			DOStorage: proto.DODir,
		}
	}
	res, err := gen.Generate(app, opts)
	if err != nil {
		return nil, err
	}
	inst := &instance{h: h, w: w, depID: depID, dir: dir, app: app, res: res, tier: tier, doStorage: opts.DOStorage}
	var vol *sandbox.Volume
	if vmTier {
		// The DO volume's key comes from mgmt custody, released only to the
		// namespace lease holder at this epoch; it stays in hostd memory
		// and the helper (dm-crypt), never on disk or in the guest.
		w.mu.Lock()
		epoch := w.epoch
		w.mu.Unlock()
		ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
		key, err := h.mgmt.VolumeKey(ctx, w.tenantID, w.name, epoch)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("DO volume key: %w", err)
		}
		vol = &sandbox.Volume{Path: h.volumePath(w), Name: w.tenantID + "/" + w.name, Epoch: epoch, Key: key, SizeMiB: h.cfg.VMVolumeMiB}
		inst.doStorage = "volume:" + vol.Path
	}
	if vmTier {
		inst.ingress = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			vm := inst.currentVM()
			if vm == nil {
				netd.WriteError(rw, &netd.Error{Code: netd.CodeUpstreamUnavailable, Detail: "microVM is not running"})
				return
			}
			vm.Ingress().ServeHTTP(rw, r)
		})
		inst.direct = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			vm := inst.currentVM()
			if vm == nil {
				return nil, errors.New("microVM is not running")
			}
			return vm.DialGuest(ctx)
		}}}
		hc, _ := json.Marshal(map[string]any{"twd": map[string]any{"kind": "admin", "op": "health"}})
		inst.spec = sandbox.Spec{
			ID:          fmt.Sprintf("i%d", n),
			AppDir:      dir,
			WorkerdArgs: []string{"serve", "config.capnp", "--experimental"},
			HealthCF:    string(hc),
			Debug:       h.cfg.VMDebug,
			Volume:      vol,
			Log:         h.log.With("workload", w.name),
			Handlers: map[uint16]http.Handler{
				proto.EgressObj: h.egressObjHandler(inst),
				proto.EgressWL:  h.egressWLHandler(inst),
				proto.HostAPI:   h.hostAPI(inst),
			},
		}
	} else {
		inst.ingress = netd.UnixUpstream(res.Sockets.Ingress)
		inst.direct = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", res.Sockets.Ingress)
		}}}
		for sock, hd := range map[string]http.Handler{
			res.Sockets.EgressObj: h.egressObjHandler(inst),
			res.Sockets.EgressWL:  h.egressWLHandler(inst),
			res.Sockets.HostAPI:   h.hostAPI(inst),
		} {
			ln, err := net.Listen("unix", sock)
			if err != nil {
				inst.stop()
				return nil, err
			}
			os.Chmod(sock, 0o600)
			srv := &http.Server{Handler: hd, ReadHeaderTimeout: 30 * time.Second}
			inst.servers = append(inst.servers, srv)
			go srv.Serve(ln)
		}
	}
	if err := inst.spawn(); err != nil {
		inst.stop()
		return nil, err
	}
	if err := inst.waitHealthy(20 * time.Second); err != nil {
		inst.stop()
		return nil, err
	}
	return inst, nil
}

// volumePath is the (tenant, workload) DO volume image (microVM tier): a
// LUKS2 image under the tenant's 0700 dir.
func (h *Host) volumePath(w *workload) string {
	return filepath.Join(h.tenantDir(w.tenantID), "workloads", w.name, "do.img")
}

func (inst *instance) currentVM() sandbox.VM {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.vm
}

// bootVM starts (or restarts) the instance's microVM. Firecracker exiting
// for any reason other than stop() is handled like a workerd crash.
func (inst *instance) bootVM() error {
	ctx, cancel := context.WithTimeout(inst.h.ctx, 60*time.Second)
	defer cancel()
	vm, err := inst.h.cfg.VM.Boot(ctx, inst.spec)
	if err != nil {
		return err
	}
	ex := make(chan struct{})
	inst.mu.Lock()
	if inst.stopping.Load() {
		inst.mu.Unlock()
		vm.Stop()
		return errors.New("instance stopped while booting")
	}
	inst.vm, inst.exited, inst.started = vm, ex, time.Now()
	inst.mu.Unlock()
	go func() {
		<-vm.Done()
		inst.mu.Lock()
		if inst.vm == vm {
			inst.vm = nil
		}
		inst.mu.Unlock()
		close(ex)
		if err := vm.Err(); err != nil {
			inst.h.log.Warn("microVM exited", "workload", inst.w.name, "err", err)
		}
		inst.onExit()
	}()
	return nil
}

func (inst *instance) spawn() error {
	if inst.node != nil {
		return inst.spawnNode()
	}
	if inst.tier == bundle.TierMicroVM {
		return inst.bootVM()
	}
	logf, err := os.OpenFile(filepath.Join(inst.dir, "workerd.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd, err := inst.workerdCommand()
	if err != nil {
		logf.Close()
		return err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Dir = inst.dir
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	ex := make(chan struct{})
	inst.mu.Lock()
	inst.cmd, inst.exited, inst.started = cmd, ex, time.Now()
	inst.mu.Unlock()
	go func() {
		cmd.Wait()
		logf.Close()
		close(ex)
		inst.onExit()
	}()
	return nil
}

// onExit restarts a workerd that died unexpectedly (bounded).
func (inst *instance) onExit() {
	if inst.stopping.Load() || inst.h.ctx.Err() != nil {
		return
	}
	inst.mu.Lock()
	if time.Since(inst.started) > 30*time.Second {
		inst.restarts = 0
	}
	inst.restarts++
	n := inst.restarts
	inst.mu.Unlock()
	tail := inst.logTail(2048)
	if n > 5 {
		inst.h.log.Error("workerd keeps exiting; giving up", "workload", inst.w.name, "log", tail)
		inst.w.fail(inst.depID, fmt.Errorf("workerd exited %d times: %s", n, tail))
		return
	}
	inst.h.log.Warn("workerd exited; restarting", "workload", inst.w.name, "attempt", n, "log", tail)
	time.Sleep(time.Duration(n) * 200 * time.Millisecond)
	if inst.stopping.Load() {
		return
	}
	if err := inst.spawn(); err != nil {
		inst.w.fail(inst.depID, err)
	}
}

func (inst *instance) logTail(n int) string {
	b, _ := os.ReadFile(filepath.Join(inst.dir, "workerd.log"))
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(bytes.TrimSpace(b))
}

func (inst *instance) waitHealthy(d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		code, _ := inst.admin(map[string]any{"kind": "admin", "op": "health"})
		if code == 200 {
			return nil
		}
		inst.mu.Lock()
		ex := inst.exited
		inst.mu.Unlock()
		select {
		case <-ex:
			if inst.restarts == 0 {
				time.Sleep(50 * time.Millisecond)
			}
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("workerd did not become healthy: %s", inst.logTail(4096))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// admin sends a platform admin request (cf.twd.kind = admin).
func (inst *instance) admin(twd map[string]any) (int, []byte) {
	blob, _ := json.Marshal(map[string]any{"twd": twd})
	req, _ := http.NewRequest("POST", "http://platform/_twd/admin", nil)
	req.Header.Set(netd.HeaderCF, string(blob))
	resp, err := inst.direct.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (inst *instance) drain(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for inst.inflight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

func (inst *instance) stop() {
	if !inst.stopping.CompareAndSwap(false, true) {
		return
	}
	if inst.node != nil {
		inst.stopNode()
		if os.Getenv("TWD_KEEP_INSTANCES") == "" {
			os.RemoveAll(inst.dir)
		}
		return
	}
	inst.mu.Lock()
	cmd, ex, vm := inst.cmd, inst.exited, inst.vm
	inst.mu.Unlock()
	if vm != nil {
		vm.Stop()
	}
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-ex:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-ex
		}
	}
	for _, s := range inst.servers {
		s.Close()
	}
	if os.Getenv("TWD_KEEP_INSTANCES") == "" {
		os.RemoveAll(inst.dir)
	}
}
