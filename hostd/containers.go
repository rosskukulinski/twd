package hostd

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/image"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/internal/container"
)

// Containers (ADR-032): one microVM per container, attached to exactly
// one Durable Object, which is its only controller. hostd owns the VM,
// keyed by (tenant, workload, class, object), not by the isolate: it
// survives the object's eviction and its workload's blue/green redeploys
// (the new code sees `running`), and dies with an inactivity timeout, an
// operator hard max lifetime, destroy(), `twd kill` of the object, or the
// workload being stopped or fenced (ADR-028: a container inherits its
// parent's fence).
//
// The platform's ctx.container polyfill reaches this over the instance's
// HOSTD binding with the calling object as attested by the platform
// (TailnetApi props in facet mode): every op is scoped to that object's
// own container, so a DO can't see or drive another's.

// Container defaults.
const (
	// DefaultContainerIdle matches @cloudflare/containers' default
	// sleepAfter (10m) and the Sandbox SDK 1.0 example's timeout: no
	// container API call or open exec/port stream for this long stops it.
	DefaultContainerIdle    = 10 * time.Minute
	MaxContainerIdle        = 6 * time.Hour // Cloudflare's setInactivityTimeout cap
	DefaultContainerMaxLife = 24 * time.Hour
	DefaultContainerGrace   = 10 * time.Second // destroy(): SIGTERM → VMM SIGKILL cap
	containerMonitorPoll    = 25 * time.Second
)

// ContainerCapacity optionally bounds this host's containers on top of the
// capacity vector they share with microVM DO instances (0 = no extra bound;
// twd-hostd -container-max / -container-mem-mib / -container-vcpu).
type ContainerCapacity struct {
	Max    int
	MemMiB int
	VCPU   float64
}

type ctrKey struct{ tenant, workload, class, name string }

func (k ctrKey) String() string { return k.tenant + "/" + k.workload + "/" + k.class + "/" + k.name }

// ctr is one object's container slot (it outlives single runs: gen counts
// starts).
type ctr struct {
	key ctrKey

	mu         sync.Mutex
	gen        int64
	state      string // "" | starting | running | stopped
	vm         sandbox.Container
	image      string
	digest     string
	labels     map[string]string
	res        sandbox.Resources
	startedAt  time.Time
	readyMS    int64
	exitCode   int
	exitReason string
	done       chan struct{} // closed when the current gen stops
	idle       time.Duration
	lastActive time.Time
	active     int // requests in flight (exec streams, port fetches, monitors)
	release    func()
	intercepts []string
	stopping   bool
}

type ctrManager struct {
	h       *Host
	backend sandbox.ContainerBackend
	images  *image.Store
	idle    time.Duration
	maxLife time.Duration
	grace   time.Duration

	mu    sync.Mutex
	ctrs  map[ctrKey]*ctr
	execs map[string]*ctrExec
	seq   int64
}

type ctrExec struct {
	key ctrKey
	gen int64
	ex  sandbox.Exec
}

func newCtrManager(h *Host) *ctrManager {
	c := h.cfg
	m := &ctrManager{h: h, backend: c.Containers, images: c.Images,
		idle: c.ContainerIdle, maxLife: c.ContainerMaxLife, grace: c.ContainerGrace, ctrs: map[ctrKey]*ctr{}, execs: map[string]*ctrExec{}}
	if m.idle <= 0 {
		m.idle = DefaultContainerIdle
	}
	if m.maxLife <= 0 {
		m.maxLife = DefaultContainerMaxLife
	}
	if m.grace <= 0 {
		m.grace = DefaultContainerGrace
	}
	return m
}

// ctrError is a typed error for the polyfill (code → JS error).
func ctrError(code, format string, a ...any) *apiError {
	return &apiError{Code: code, Detail: fmt.Sprintf(format, a...)}
}

func (m *ctrManager) slot(k ctrKey) *ctr {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.ctrs[k]
	if c == nil {
		c = &ctr{key: k}
		m.ctrs[k] = c
	}
	return c
}

func (m *ctrManager) lookup(k ctrKey) *ctr {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ctrs[k]
}

// touch marks activity; begin/end bracket a long request.
func (c *ctr) touch() {
	c.mu.Lock()
	c.lastActive = time.Now()
	c.mu.Unlock()
}

func (c *ctr) begin() func() {
	c.mu.Lock()
	c.active++
	c.lastActive = time.Now()
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.active--
		c.lastActive = time.Now()
		c.mu.Unlock()
	}
}

type startReq struct {
	Image          string            `json:"image"`
	Entrypoint     []string          `json:"entrypoint"`
	Env            map[string]string `json:"env"`
	EnableInternet bool              `json:"enableInternet"`
	Instance       json.RawMessage   `json:"instance"`
	Labels         map[string]string `json:"labels"`
}

func (m *ctrManager) start(ctx context.Context, inst *instance, k ctrKey, cc *container.Class, r startReq) (map[string]any, error) {
	if !m.h.containersOffered() {
		return nil, ctrError("tier_unavailable", "this host does not offer containers (no microVM container backend; ADR-032 D5: no fallback)")
	}
	refs := cc.Refs()
	ref := r.Image
	if ref == "" {
		ref = cc.Image
	}
	if ref == "" {
		return nil, ctrError("bad_request", "class %s has no built image (wrangler image %q): twd deploy builds and pins it", k.class, cc.Source)
	}
	allowed := false
	for _, a := range refs {
		allowed = allowed || a == ref
	}
	if !allowed {
		return nil, ctrError("bad_request", "image %q is not one of class %s's images %v", ref, k.class, refs)
	}
	var custom *sandbox.Resources
	itype := ""
	if len(r.Instance) > 0 && string(r.Instance) != "null" {
		if json.Unmarshal(r.Instance, &itype) != nil {
			var cr sandbox.Resources
			if err := json.Unmarshal(r.Instance, &cr); err != nil {
				return nil, ctrError("bad_request", "instance: %v", err)
			}
			custom = &cr
		}
	} else if cc.Custom != nil {
		custom = &sandbox.Resources{VCPU: cc.Custom.VCPU, MemoryMiB: cc.Custom.MemMiB, DiskMB: cc.Custom.DiskMiB}
	}
	res, err := sandbox.ResolveInstance(itype, custom, cc.InstanceType)
	if err != nil {
		return nil, ctrError("bad_request", "%v", err)
	}
	for k2, v := range r.Labels {
		if len(k2) > 128 || len(v) > 1024 || len(r.Labels) > 64 {
			return nil, ctrError("bad_request", "labels too large")
		}
	}
	c := m.slot(k)
	c.mu.Lock()
	if c.state == "starting" || c.state == "running" {
		c.mu.Unlock()
		return nil, ctrError("container_running", "the container is already running")
	}
	c.state, c.res, c.image, c.labels = "starting", res, ref, r.Labels
	c.gen++
	gen := c.gen
	c.done = make(chan struct{})
	c.exitCode, c.exitReason, c.stopping, c.intercepts = 0, "", false, nil
	c.idle, c.lastActive = m.idle, time.Now()
	c.mu.Unlock()
	fail := func(e error) (map[string]any, error) {
		c.mu.Lock()
		if c.gen == gen {
			c.state, c.exitCode, c.exitReason = "stopped", -1, e.Error()
			closeOnce(c.done)
		}
		c.mu.Unlock()
		return nil, e
	}
	// max_instances as far as this host can see (its class's objects are
	// co-located with their parents); mgmt's reservation counts all hosts.
	m.mu.Lock()
	same := m.classLiveLocked(k)
	m.mu.Unlock()
	if cc.MaxInstances > 0 && same > cc.MaxInstances {
		return fail(&apiError{Code: "container_quota_exceeded", Detail: fmt.Sprintf("class %s already runs its max_instances (%d) containers", k.class, cc.MaxInstances),
			Extra: map[string]any{"missing": map[string]any{"quota": "max_instances", "limit": cc.MaxInstances, "used": same - 1, "requested": 1}}})
	}
	// Admission (ADR-032 D9): the host's capacity vector, then mgmt's
	// synchronous tenant quota reservation (fails closed).
	vcpus, cpuMax, memMiB, scratch := res.VMShape()
	slot, err := m.h.AcquireContainer(ctx, ContainerKey{TenantID: k.tenant, Workload: k.workload, Class: k.class, Object: k.name},
		container.Resources{VCPU: res.VCPU, MemMiB: memMiB, DiskMiB: scratch})
	if err != nil {
		return fail(err)
	}
	release := func() { slot.Release(context.WithoutCancel(ctx)) }
	img, err := m.images.Get(ctx, k.tenant, ref)
	if err != nil {
		release()
		return fail(ctrError("image_unavailable", "%v", err))
	}
	ic := img.Config.Config
	argv, env, err := sandbox.ContainerProcess(ic.Entrypoint, ic.Cmd, ic.Env, r.Entrypoint, r.Env)
	if err != nil {
		release()
		return fail(ctrError("bad_request", "%v", err))
	}
	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("c%d", m.seq)
	m.mu.Unlock()
	st := time.Now()
	vm, err := m.backend.BootContainer(ctx, sandbox.ContainerSpec{ID: id, Image: img.Path, ScratchMiB: scratch, VCPUs: vcpus, CPUMax: cpuMax,
		MemMiB: memMiB, Pids: 4096, Argv: argv, Env: env, Cwd: ic.WorkingDir, User: ic.User, Hostname: "sandbox",
		EnableInternet: r.EnableInternet, Egress: &sandbox.EgressPolicy{Internet: inst.app.Internet},
		Log: m.h.log.With("container", k.String())})
	if err != nil {
		release()
		return fail(ctrError("container_start_failed", "%v", err))
	}
	ready := time.Since(st).Milliseconds()
	c.mu.Lock()
	if c.gen != gen || c.stopping {
		// destroyed (or killed) while booting
		c.mu.Unlock()
		vm.Stop(0)
		release()
		return nil, ctrError("container_not_running", "the container was destroyed while starting")
	}
	c.vm, c.state, c.digest, c.startedAt, c.readyMS, c.release = vm, "running", img.Digest, time.Now(), ready, release
	c.lastActive = time.Now()
	c.mu.Unlock()
	m.h.audit.add(mgmtapi.AuditRecord{Kind: "container_start", ObjectID: m.objectID(k), TraceID: newTraceID(),
		Detail: mgmtapi.Note(fmt.Sprintf("%s image %s (%s) %s vcpu %.3g mem %d MiB", k, ref, img.Digest, id, res.VCPU, res.MemoryMiB))})
	m.h.log.Info("container started", "container", k.String(), "image", ref, "digest", img.Digest, "ready_ms", ready,
		"vcpu", res.VCPU, "mem_mib", res.MemoryMiB, "convert_ms", img.ConvertMS)
	go m.watch(c, gen, vm)
	return map[string]any{"gen": gen, "running": true, "readyMs": ready, "image": ref, "digest": img.Digest}, nil
}

// watch ends a run: the entrypoint exits (the VM is then killed, as
// Cloudflare stops a container whose process exits), the VM dies, or a
// timer fires (inactivity, hard max lifetime).
func (m *ctrManager) watch(c *ctr, gen int64, vm sandbox.Container) {
	tk := time.NewTicker(250 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-vm.Exited():
			m.finish(c, gen, vm, vm.ExitCode(), "exited", 0)
			return
		case <-vm.Done():
			code := -1
			select {
			case <-vm.Exited():
				code = vm.ExitCode()
			default:
			}
			m.finish(c, gen, vm, code, "vm exited", 0)
			return
		case <-tk.C:
			c.mu.Lock()
			if c.gen != gen {
				c.mu.Unlock()
				return
			}
			idle := c.active == 0 && time.Since(c.lastActive) > c.idle
			old := time.Since(c.startedAt) > m.maxLife
			c.mu.Unlock()
			switch {
			case old:
				m.finish(c, gen, vm, -1, "max lifetime", m.grace)
				return
			case idle:
				m.finish(c, gen, vm, -1, "inactivity timeout", m.grace)
				return
			}
		}
	}
}

// finish stops run gen (idempotent) and records why.
func (m *ctrManager) finish(c *ctr, gen int64, vm sandbox.Container, code int, reason string, grace time.Duration) {
	c.mu.Lock()
	if c.gen != gen || c.state == "stopped" || c.stopping { // one finisher per run: the first reason wins
		inProgress, done := c.gen == gen && c.stopping && c.state != "stopped", c.done
		c.mu.Unlock()
		if inProgress {
			<-done // return once the run is really gone
		}
		return
	}
	c.stopping = true
	c.mu.Unlock()
	st := time.Now()
	if vm != nil {
		vm.Stop(grace)
		select {
		case <-vm.Exited():
			if code == -1 {
				code = vm.ExitCode()
			}
		default:
		}
	}
	c.mu.Lock()
	rel := c.release
	if c.gen == gen {
		c.state, c.exitCode, c.exitReason, c.vm, c.release = "stopped", code, reason, nil, nil
		closeOnce(c.done)
	}
	c.mu.Unlock()
	if rel != nil {
		rel()
	}
	m.mu.Lock()
	for id, e := range m.execs {
		if e.key == c.key && e.gen == gen {
			delete(m.execs, id)
		}
	}
	m.mu.Unlock()
	m.h.audit.add(mgmtapi.AuditRecord{Kind: "container_stop", ObjectID: m.objectID(c.key), TraceID: newTraceID(),
		Detail: mgmtapi.Note(fmt.Sprintf("%s: %s (exit %d)", c.key, reason, code))})
	m.h.log.Info("container stopped", "container", c.key.String(), "reason", reason, "exit", code, "stop_ms", time.Since(st).Milliseconds())
}

func (m *ctrManager) running(k ctrKey) (*ctr, sandbox.Container, int64) {
	c := m.lookup(k)
	if c == nil {
		return nil, nil, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != "running" {
		return c, nil, c.gen
	}
	return c, c.vm, c.gen
}

// destroy stops k's container (no-op if none); grace is capped.
func (m *ctrManager) destroy(k ctrKey, reason string, grace time.Duration) bool {
	c := m.lookup(k)
	if c == nil {
		return false
	}
	c.mu.Lock()
	gen, vm, st := c.gen, c.vm, c.state
	if st == "starting" {
		c.stopping = true // start() sees it and stops the VM it gets
		c.state, c.exitCode, c.exitReason = "stopped", -1, reason
		closeOnce(c.done)
	}
	c.mu.Unlock()
	if st != "running" {
		return st == "starting"
	}
	if grace > m.grace {
		grace = m.grace
	}
	m.finish(c, gen, vm, -1, reason, grace)
	return true
}

// destroyWhere stops every container matching (tenant[, workload[, class,
// name]]): object kill, workload stop/fence, host shutdown.
func (m *ctrManager) destroyWhere(match func(ctrKey) bool, reason string, grace time.Duration) int {
	m.mu.Lock()
	var ks []ctrKey
	for k := range m.ctrs {
		if match(k) {
			ks = append(ks, k)
		}
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	n := 0
	for _, k := range ks {
		c := m.lookup(k)
		c.mu.Lock()
		live := c.state == "running" || c.state == "starting"
		c.mu.Unlock()
		if !live {
			continue
		}
		n++
		wg.Add(1)
		go func() { defer wg.Done(); m.destroy(k, reason, grace) }()
	}
	wg.Wait()
	return n
}

// ContainerStatus is one container as seen by tests and `twd host status`.
type ContainerStatus struct {
	Tenant, Workload, Class, Object string
	State                           string
	Gen                             int64
	Image, Digest                   string
	ExitCode                        int
	ExitReason                      string
	ReadyMS                         int64
	RSSKB                           int64
	Jail                            *sandbox.Jail `json:",omitempty"`
	PID                             int
}

// Containers lists this host's containers (running and stopped slots).
func (h *Host) Containers() []ContainerStatus {
	m := h.ctr
	m.mu.Lock()
	cs := make([]*ctr, 0, len(m.ctrs))
	for _, c := range m.ctrs {
		cs = append(cs, c)
	}
	m.mu.Unlock()
	out := make([]ContainerStatus, 0, len(cs))
	for _, c := range cs {
		c.mu.Lock()
		s := ContainerStatus{Tenant: c.key.tenant, Workload: c.key.workload, Class: c.key.class, Object: c.key.name, State: c.state, Gen: c.gen,
			Image: c.image, Digest: c.digest, ExitCode: c.exitCode, ExitReason: c.exitReason, ReadyMS: c.readyMS}
		if c.vm != nil {
			s.RSSKB = c.vm.RSSKB()
			s.Jail = c.vm.Info().Jail
			s.PID = c.vm.Info().PID
		}
		c.mu.Unlock()
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Tenant+out[i].Workload+out[i].Class+out[i].Object < out[j].Tenant+out[j].Workload+out[j].Class+out[j].Object
	})
	return out
}

// ---- the host API (platform → hostd), /v1/container... ----

type ctrAs struct {
	Class string `json:"class"`
	Name  string `json:"name"`
}

type ctrReq struct {
	Op     string          `json:"op"`
	As     *ctrAs          `json:"as"`
	Start  *startReq       `json:"start"`
	Signal int             `json:"signal"`
	Reason string          `json:"reason"`
	MS     json.Number     `json:"ms"`
	Gen    int64           `json:"gen"`
	Exec   string          `json:"exec"`
	Cols   int             `json:"cols"`
	Rows   int             `json:"rows"`
	Addr   string          `json:"addr"`
	Kind   string          `json:"kind"`
	Spec   *ctrExecReq     `json:"spec"`
	Extra  json.RawMessage `json:"extra"`
}

type ctrExecReq struct {
	Argv   []string          `json:"argv"`
	Env    map[string]string `json:"env"`
	Cwd    string            `json:"cwd"`
	User   string            `json:"user"`
	Stdin  bool              `json:"stdin"`
	Stdout string            `json:"stdout"`
	Stderr string            `json:"stderr"`
	PTY    bool              `json:"pty"`
	Cols   int               `json:"cols"`
	Rows   int               `json:"rows"`
}

// ctrKeyFor checks the attested caller and maps it to its own container.
func (h *Host) ctrKeyFor(inst *instance, as *ctrAs) (ctrKey, *container.Class, error) {
	if as == nil || as.Class == "" || as.Name == "" {
		return ctrKey{}, nil, ctrError("bad_request", "container ops need the calling object")
	}
	cc, ok := inst.app.Containers[as.Class]
	if !ok || inst.app.Classes[as.Class] == nil {
		return ctrKey{}, nil, ctrError("bad_request", "class %s has no container (wrangler \"containers\")", as.Class)
	}
	return ctrKey{inst.w.tenantID, inst.w.name, as.Class, as.Name}, &cc, nil
}

// asHeader carries the attested caller on the streaming endpoints.
const asHeader = "x-twd-as"

func (h *Host) containerAPI(inst *instance, w http.ResponseWriter, r *http.Request) {
	m := h.ctr
	sub := strings.TrimPrefix(r.URL.Path, "/v1/container")
	fail := func(err error) { writeAPIError(w, asAPIError(err)) }
	switch {
	case sub == "" || sub == "/":
		var req ctrReq
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		dec.UseNumber()
		if err := dec.Decode(&req); err != nil {
			fail(ctrError("bad_request", "%v", err))
			return
		}
		k, cc, err := h.ctrKeyFor(inst, req.As)
		if err != nil {
			fail(err)
			return
		}
		v, err := m.op(r.Context(), inst, k, cc, &req)
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, 200, v)
	case sub == "/exec":
		var req ctrReq
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Spec == nil {
			fail(ctrError("bad_request", "exec: bad request %v", err))
			return
		}
		k, _, err := h.ctrKeyFor(inst, req.As)
		if err != nil {
			fail(err)
			return
		}
		m.serveExec(w, r, k, req.Spec)
	case sub == "/stdin":
		var as ctrAs
		json.Unmarshal([]byte(r.Header.Get(asHeader)), &as)
		k, _, err := h.ctrKeyFor(inst, &as)
		if err != nil {
			fail(err)
			return
		}
		m.serveStdin(w, r, k, r.URL.Query().Get("exec"))
	case strings.HasPrefix(sub, "/port/"):
		var as ctrAs
		json.Unmarshal([]byte(r.Header.Get(asHeader)), &as)
		k, _, err := h.ctrKeyFor(inst, &as)
		if err != nil {
			fail(err)
			return
		}
		rest := strings.TrimPrefix(sub, "/port/")
		ps, path, _ := strings.Cut(rest, "/")
		port, err := strconv.Atoi(ps)
		if err != nil || port < 1 || port > 65535 {
			fail(ctrError("bad_request", "bad port %q", ps))
			return
		}
		m.servePort(w, r, k, uint16(port), "/"+path)
	default:
		fail(ctrError("bad_request", "unknown container op"))
	}
}

func (m *ctrManager) op(ctx context.Context, inst *instance, k ctrKey, cc *container.Class, req *ctrReq) (map[string]any, error) {
	switch req.Op {
	case "status":
		c := m.lookup(k)
		if c == nil {
			return map[string]any{"running": false, "gen": 0}, nil
		}
		c.touch()
		c.mu.Lock()
		defer c.mu.Unlock()
		run := c.state == "running" || c.state == "starting"
		return map[string]any{"running": run, "gen": c.gen, "state": c.state, "exitCode": c.exitCode, "exitReason": c.exitReason}, nil
	case "start":
		if req.Start == nil {
			req.Start = &startReq{}
		}
		return m.start(ctx, inst, k, cc, *req.Start)
	case "destroy":
		grace := m.grace
		if req.MS != "" {
			if v, err := req.MS.Int64(); err == nil && v >= 0 {
				grace = time.Duration(v) * time.Millisecond
			}
		}
		reason := req.Reason
		if reason == "" {
			reason = "destroyed"
		}
		return map[string]any{"destroyed": m.destroy(k, "destroy: "+trimReason(reason), grace)}, nil
	case "signal":
		_, vm, _ := m.running(k)
		if vm == nil {
			return nil, ctrError("container_not_running", "the container is not running")
		}
		if req.Signal < 1 || req.Signal > 64 {
			return nil, ctrError("bad_request", "bad signal %d", req.Signal)
		}
		if err := vm.Signal(req.Signal); err != nil {
			return nil, ctrError("container_not_running", "%v", err)
		}
		return map[string]any{"ok": true}, nil
	case "setInactivityTimeout":
		v, err := req.MS.Int64()
		if err != nil || v < 0 {
			return nil, ctrError("bad_request", "setInactivityTimeout: bad duration %q", req.MS)
		}
		d := time.Duration(v) * time.Millisecond
		if d > MaxContainerIdle {
			return nil, ctrError("bad_request", "setInactivityTimeout: %v is over the %v maximum", d, MaxContainerIdle)
		}
		c, vm, _ := m.running(k)
		if vm == nil {
			return map[string]any{"ok": true, "running": false}, nil
		}
		c.mu.Lock()
		c.idle, c.lastActive = d, time.Now()
		c.mu.Unlock()
		return map[string]any{"ok": true, "running": true}, nil
	case "monitor":
		c := m.lookup(k)
		if c == nil {
			return nil, ctrError("container_not_running", "the container is not running")
		}
		c.mu.Lock()
		gen, done := c.gen, c.done
		if req.Gen != 0 && req.Gen != gen || done == nil {
			code, reason := c.exitCode, c.exitReason
			c.mu.Unlock()
			if req.Gen != 0 && req.Gen < gen {
				return map[string]any{"done": true, "gen": req.Gen, "exitCode": -1, "reason": "replaced"}, nil
			}
			return map[string]any{"done": true, "gen": gen, "exitCode": code, "reason": reason}, nil
		}
		c.mu.Unlock()
		// Waiting is not activity: a parked monitor() (the Container class
		// always has one) must not keep an idle container alive forever.
		t := time.NewTimer(containerMonitorPoll)
		defer t.Stop()
		select {
		case <-done:
			c.mu.Lock()
			defer c.mu.Unlock()
			return map[string]any{"done": true, "gen": gen, "exitCode": c.exitCode, "reason": c.exitReason}, nil
		case <-t.C:
			return map[string]any{"done": false, "gen": gen}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case "inspect":
		c := m.lookup(k)
		if c == nil {
			return map[string]any{"info": nil}, nil
		}
		c.touch()
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.state != "running" {
			return map[string]any{"info": nil}, nil
		}
		labels := c.labels
		if labels == nil {
			labels = map[string]string{}
		}
		return map[string]any{"info": map[string]any{"image": c.image, "labels": labels, "digest": c.digest,
			"instance":  map[string]any{"vcpu": c.res.VCPU, "memoryMib": c.res.MemoryMiB, "diskMb": c.res.DiskMB},
			"startedAt": c.startedAt.UnixMilli(), "gen": c.gen}}, nil
	case "intercept":
		// Registered for the egress proxy (ADR-032 D4, phase B). Until it
		// exists the guest has no egress at all, so nothing is delivered.
		c, vm, _ := m.running(k)
		if vm == nil {
			return nil, ctrError("container_not_running", "the container is not running")
		}
		switch req.Kind {
		case "http", "https", "all":
		default:
			return nil, ctrError("bad_request", "intercept kind %q", req.Kind)
		}
		if len(req.Addr) > 255 {
			return nil, ctrError("bad_request", "intercept address too long")
		}
		c.mu.Lock()
		if len(c.intercepts) < 256 {
			c.intercepts = append(c.intercepts, req.Kind+" "+req.Addr)
		}
		c.mu.Unlock()
		return map[string]any{"ok": true, "enforced": false}, nil
	case "exec-signal", "exec-resize":
		m.mu.Lock()
		e := m.execs[req.Exec]
		m.mu.Unlock()
		if e == nil || e.key != k {
			// Another object's exec ID is indistinguishable from none.
			return nil, ctrError("unknown_object", "no such exec")
		}
		var err error
		if req.Op == "exec-signal" {
			if req.Signal < 1 || req.Signal > 64 {
				return nil, ctrError("bad_request", "bad signal")
			}
			err = e.ex.Signal(req.Signal)
		} else {
			err = e.ex.Resize(req.Cols, req.Rows)
		}
		if err != nil {
			return nil, ctrError("bad_request", "%v", err)
		}
		return map[string]any{"ok": true}, nil
	}
	return nil, ctrError("bad_request", "unknown container op %q", req.Op)
}

func trimReason(s string) string {
	if len(s) > 256 {
		s = s[:256]
	}
	return s
}

func newCtrExecID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// serveExec runs one exec and streams it: the response body is frames
// (proto framing: 1 stdout, 2 stderr, 3 exit int32) until the exit frame.
// Headers carry the exec id (for stdin/kill/resize) and the pid.
func (m *ctrManager) serveExec(w http.ResponseWriter, r *http.Request, k ctrKey, s *ctrExecReq) {
	c, vm, gen := m.running(k)
	if vm == nil {
		writeAPIError(w, ctrError("container_not_running", "the container is not running"))
		return
	}
	if len(s.Argv) == 0 || len(s.Argv) > 4096 {
		writeAPIError(w, ctrError("bad_request", "exec needs argv"))
		return
	}
	end := c.begin()
	defer end()
	var env []string
	keys := make([]string, 0, len(s.Env))
	for k2 := range s.Env {
		keys = append(keys, k2)
	}
	sort.Strings(keys)
	for _, k2 := range keys {
		if k2 == "" || strings.ContainsAny(k2, "=\x00") {
			writeAPIError(w, ctrError("bad_request", "bad env name %q", k2))
			return
		}
		env = append(env, k2+"="+s.Env[k2])
	}
	for _, mode := range []string{s.Stdout, s.Stderr} {
		switch mode {
		case "", "pipe", "ignore", "combined":
		default:
			writeAPIError(w, ctrError("bad_request", "bad stdio mode %q", mode))
			return
		}
	}
	if s.Stdout == "combined" {
		writeAPIError(w, ctrError("bad_request", "stdout can't be \"combined\""))
		return
	}
	ex, err := vm.Exec(r.Context(), sandbox.ExecSpec{Argv: s.Argv, Env: env, Cwd: s.Cwd, User: s.User, Stdin: s.Stdin,
		Stdout: s.Stdout, Stderr: s.Stderr, PTY: s.PTY, Cols: s.Cols, Rows: s.Rows})
	if err != nil {
		writeAPIError(w, ctrError("exec_failed", "%v", err))
		return
	}
	id := newCtrExecID()
	m.mu.Lock()
	m.execs[id] = &ctrExec{key: k, gen: gen, ex: ex}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.execs, id)
		m.mu.Unlock()
	}()
	w.Header().Set("content-type", "application/octet-stream")
	w.Header().Set("cache-control", "no-store")
	w.Header().Set("x-twd-exec", id)
	w.Header().Set("x-twd-exec-pid", strconv.Itoa(ex.PID()))
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	var wmu sync.Mutex
	send := func(kind byte, p []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		if err := proto.WriteFrame(w, kind, p); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	var wg sync.WaitGroup
	pump := func(rd io.Reader, kind byte) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := rd.Read(buf)
			if n > 0 {
				c.touch()
				if send(kind, buf[:n]) != nil {
					io.Copy(io.Discard, rd)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	if o := ex.Stdout(); o != nil {
		wg.Add(1)
		go pump(o, proto.FrameStdout)
	}
	if e := ex.Stderr(); e != nil {
		wg.Add(1)
		go pump(e, proto.FrameStderr)
	}
	// The DO going away (its isolate evicted, the request canceled) kills
	// the exec: nothing else could ever read or stop it.
	exited := make(chan struct{})
	defer close(exited)
	go func() {
		select {
		case <-r.Context().Done():
			ex.Signal(9)
		case <-exited:
		}
	}()
	code, err := ex.Wait(context.Background())
	wg.Wait()
	if err != nil {
		code = -1
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(int32(code)))
	send(proto.FrameExit, b[:])
}

// serveStdin streams the request body into an exec's stdin, then closes
// it (half-close).
func (m *ctrManager) serveStdin(w http.ResponseWriter, r *http.Request, k ctrKey, id string) {
	m.mu.Lock()
	e := m.execs[id]
	m.mu.Unlock()
	if e == nil || e.key != k {
		writeAPIError(w, ctrError("unknown_object", "no such exec"))
		return
	}
	in := e.ex.Stdin()
	if in == nil {
		writeAPIError(w, ctrError("bad_request", "the exec has no piped stdin"))
		return
	}
	if c := m.lookup(k); c != nil {
		end := c.begin()
		defer end()
	}
	n, err := io.Copy(in, r.Body)
	cerr := in.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		writeAPIError(w, ctrError("exec_failed", "stdin after %d bytes: %v", n, err))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "bytes": n})
}

// servePort proxies one request to a TCP port in the container
// (getTcpPort(p).fetch). Not listening → typed not_listening (the
// polyfill raises Cloudflare's "the container is not listening").
func (m *ctrManager) servePort(w http.ResponseWriter, r *http.Request, k ctrKey, port uint16, path string) {
	c, vm, _ := m.running(k)
	if vm == nil {
		writeAPIError(w, ctrError("container_not_running", "the container is not running"))
		return
	}
	end := c.begin()
	defer end()
	// Dial first, so that "not listening" is a typed error, not a 502 page.
	dctx, dcancel := context.WithTimeout(r.Context(), 10*time.Second)
	conn, err := vm.DialPort(dctx, port)
	dcancel()
	if err != nil {
		writeAPIError(w, ctrError("not_listening", "the container is not listening on port %d: %v", port, err))
		return
	}
	used := false
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if !used {
				used = true
				return conn, nil
			}
			return vm.DialPort(ctx, port)
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	target := &url.URL{Scheme: "http", Host: "container"}
	u := *r.URL
	u.Path, u.RawPath = path, ""
	rp := &httputil.ReverseProxy{
		Transport:     tr,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path, pr.Out.URL.RawPath, pr.Out.URL.RawQuery = path, "", r.URL.RawQuery
			if h := r.Header.Get("x-twd-host"); h != "" {
				pr.Out.Host = h
			} else {
				pr.Out.Host = "localhost"
			}
			for hk := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(hk), "x-twd") {
					pr.Out.Header.Del(hk)
				}
			}
			pr.Out.Header.Del("x-forwarded-for")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			writeAPIError(w, ctrError("not_listening", "the container is not listening on port %d: %v", port, err))
		},
	}
	rp.ServeHTTP(&touchWriter{ResponseWriter: w, c: c}, r)
}

// touchWriter marks activity as a streamed response flows.
type touchWriter struct {
	http.ResponseWriter
	c *ctr
}

func (t *touchWriter) Write(p []byte) (int, error) {
	t.c.touch()
	return t.ResponseWriter.Write(p)
}

func (t *touchWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// errStatus additions for container codes.
func init() {
	for c, s := range map[string]int{
		"tier_unavailable": 409, "capacity_exhausted": 503, "container_quota_exceeded": 429, "container_running": 409,
		"container_not_running": 409, "not_listening": 502, "image_unavailable": 502, "container_start_failed": 502,
		"exec_failed": 502, "not_supported": 501,
	} {
		errStatus[c] = s
	}
}

// objectID is the mgmt object ID of k's parent (audit), if it has one.
func (m *ctrManager) objectID(k ctrKey) string {
	h := m.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if o := h.byKey[objKey(k.tenant, k.workload, k.class, k.name)]; o != nil {
		return o.ID
	}
	for _, o := range h.objects {
		if o.TenantID == k.tenant && o.Workload == k.workload && o.Class == k.class && o.Name == k.name {
			return o.ID
		}
	}
	return ""
}

// classLiveLocked counts the live containers of k's class (m.mu held).
func (m *ctrManager) classLiveLocked(k ctrKey) int {
	n := 0
	for ck, c := range m.ctrs {
		if ck.tenant != k.tenant || ck.workload != k.workload || ck.class != k.class {
			continue
		}
		c.mu.Lock()
		if c.state == "starting" || c.state == "running" {
			n++
		}
		c.mu.Unlock()
	}
	return n
}
