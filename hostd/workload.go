package hostd

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/hostd/gen"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
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

	inst, err := h.startInstance(w, a.DeploymentID, app)
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
		"blue_green", old != nil, "ms", time.Since(st).Milliseconds())
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

	inflight atomic.Int64
	reqs     atomic.Int64
	stopping atomic.Bool

	mu       sync.Mutex
	cmd      *exec.Cmd
	exited   chan struct{}
	servers  []*http.Server
	restarts int
	started  time.Time
}

func (h *Host) startInstance(w *workload, depID string, app *bundle.App) (*instance, error) {
	h.mu.Lock()
	h.instSeq++
	n := h.instSeq
	h.mu.Unlock()
	dir := filepath.Join(h.cfg.RunDir, fmt.Sprintf("i%d", n))
	res, err := gen.Generate(app, gen.Options{
		Dir:          dir,
		DOStorage:    filepath.Join(h.cfg.DataDir, "tenants", w.tenantID, "workloads", w.name, "do"),
		DeploymentID: depID,
		Workload:     w.name,
	})
	if err != nil {
		return nil, err
	}
	inst := &instance{h: h, w: w, depID: depID, dir: dir, app: app, res: res, ingress: netd.UnixUpstream(res.Sockets.Ingress)}
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

func (inst *instance) spawn() error {
	logf, err := os.OpenFile(filepath.Join(inst.dir, "workerd.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(inst.h.cfg.WorkerdBin, "serve", inst.res.Config, "--experimental")
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
	inst.mu.Lock()
	cmd, ex := inst.cmd, inst.exited
	inst.mu.Unlock()
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
