package netd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

// Header names shared with the platform wrapper running in workerd.
const (
	// HeaderCF carries the JSON cf blob; workerd's socket is configured with
	// http.cfBlobHeader = "x-twd-cf", strips it, and exposes it as request.cf.
	HeaderCF = "x-twd-cf"
	// HeaderObject names the object on ingress (set by netd) and on egress
	// (set by the trusted platform Egress entrypoint, never by tenant code).
	HeaderObject = "x-twd-object"
	// HeaderError carries the error code on typed netd refusals.
	HeaderError = "twd-error"
)

// State is an object's node lifecycle state.
type State string

const (
	StatePending State = "pending" // queued or joining
	StateReady   State = "ready"   // node up, ingress listening
	StateFailed  State = "failed"  // join failed
	StateKilled  State = "killed"  // killed (terminal)
	StateClosed  State = "closed"  // node closed, state kept for rejoin
)

// Config configures a Registry.
type Config struct {
	// ControlURL is the coordination server. Required, so that a test or a
	// host can never silently join Tailscale's default control plane:
	// pass "https://controlplane.tailscale.com" explicitly for SaaS.
	ControlURL string
	// StateRoot holds one 0700 directory per object (tsnet state, object.json).
	StateRoot string
	// Keys supplies auth keys for objects without saved state.
	Keys KeySource
	// Upstream receives attributed ingress requests (e.g. UnixUpstream to the
	// workerd ingress socket). Nil answers 503 upstream_unavailable.
	Upstream http.Handler
	// CallerCerts resolves callers' node certificates (RT-01, ADR-010): a
	// twd object's WhoIs CapMap is narrowed to CapMap ∩ cert.lim before it
	// reaches Upstream; a lookup failure withholds the caps. Nil: callers'
	// policy caps apply unchanged (no spawn narrowing is enforced).
	CallerCerts CertSource
	// IngressPort is the TCP port each object node listens on. Default 80.
	IngressPort int
	// JoinParallel bounds concurrent node joins. Default 16.
	JoinParallel int
	// JoinTimeout bounds one node's Up(). Default 2 minutes.
	JoinTimeout time.Duration
	// ReadyTimeout bounds how long egress waits for a pending object before
	// failing with identity_pending. Default 30 s.
	ReadyTimeout time.Duration
	// Ephemeral and Tags are defaults for objects that don't override them.
	Ephemeral bool
	Tags      []string
	// Hostname maps an object ID to its node hostname. Default "twd-"+id.
	Hostname func(objectID string) string
	// AllowSiblings lets an object's egress reach other objects hosted by this
	// registry. Default false: same-host object calls must go through the
	// platform's policy-checked path (RFC §4.5), not a hairpin over WireGuard.
	AllowSiblings bool
	// ExtraForbidden prefixes are always refused on egress (platform endpoints).
	ExtraForbidden []netip.Prefix
	// OnDelete, if set, is called after a killed object's node is closed
	// (kill tier 2: delete the device at the coordination server).
	OnDelete func(ctx context.Context, info ObjectInfo) error
	// KillFlushDelay is how long Kill keeps a node up after aborting its flows
	// so the RSTs are transmitted before Close. Default 50 ms; negative disables.
	KillFlushDelay time.Duration
	// Logger receives structured logs; it is always wrapped in a
	// RedactingHandler. Nil discards.
	Logger *slog.Logger
	// TSNetLogs forwards tsnet's internal logs to Logger at debug level.
	TSNetLogs bool
}

// ObjectOptions override registry defaults for one object.
type ObjectOptions struct {
	Hostname  string
	Tags      []string
	Ephemeral *bool
}

// Registry holds every object node for one tenant on this host.
type Registry struct {
	cfg  Config
	log  *slog.Logger
	sem  chan struct{}
	ctx  context.Context
	stop context.CancelFunc

	mu       sync.Mutex
	objects  map[string]*Object
	localIPs map[netip.Addr]string // tailnet IPs of nodes hosted here -> object ID
	hostIPs  map[netip.Addr]bool
	closed   bool
	joins    sync.WaitGroup
}

var objectIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidObjectID reports whether id can be used as an object ID: a lowercase
// DNS label (it becomes part of the node hostname and the state dir name).
func ValidObjectID(id string) bool { return objectIDRE.MatchString(id) }

// New creates a Registry. It does not start any node.
func New(cfg Config) (*Registry, error) {
	if cfg.ControlURL == "" {
		return nil, errors.New("netd: Config.ControlURL is required")
	}
	if cfg.StateRoot == "" {
		return nil, errors.New("netd: Config.StateRoot is required")
	}
	if cfg.IngressPort == 0 {
		cfg.IngressPort = 80
	}
	if cfg.JoinParallel <= 0 {
		cfg.JoinParallel = 16
	}
	if cfg.JoinTimeout <= 0 {
		cfg.JoinTimeout = 2 * time.Minute
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 30 * time.Second
	}
	if cfg.KillFlushDelay == 0 {
		cfg.KillFlushDelay = 50 * time.Millisecond
	}
	if cfg.Hostname == nil {
		cfg.Hostname = func(id string) string { return "twd-" + id }
	}
	var h slog.Handler = slog.DiscardHandler
	if cfg.Logger != nil {
		h = cfg.Logger.Handler()
	}
	if err := os.MkdirAll(cfg.StateRoot, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.StateRoot, 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Registry{
		cfg:      cfg,
		log:      slog.New(NewRedactingHandler(h)),
		sem:      make(chan struct{}, cfg.JoinParallel),
		ctx:      ctx,
		stop:     cancel,
		objects:  map[string]*Object{},
		localIPs: map[netip.Addr]string{},
	}
	r.RefreshHostIPs()
	return r, nil
}

// RefreshHostIPs re-reads the host's interface addresses, which egress always
// refuses.
func (r *Registry) RefreshHostIPs() {
	m := map[netip.Addr]bool{}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				m[p.Addr().Unmap()] = true
			}
		}
	}
	r.mu.Lock()
	r.hostIPs = m
	r.mu.Unlock()
}

// Object is one object's node.
type Object struct {
	id, hostname string
	dir          string
	ephemeral    bool
	tags         []string
	reg          *Registry
	ready        chan struct{} // closed when state leaves pending

	mu       sync.Mutex
	state    State
	err      error
	srv      *tsnet.Server
	lc       *local.Client
	ip4, ip6 netip.Addr
	nodeID   tailcfg.StableNodeID
	dnsName  string
	queued   time.Time
	joinDur  time.Duration
	rejoined bool
	reminted bool // saved state was rejected by control; joined with a fresh key
	ln       net.Listener
	httpSrv  *http.Server
	tombSrv  *http.Server // after a kill with Tombstone: answers object_killed
	// upCancel/upDone guard the in-flight start(): tsnet.Server.Close must not
	// run concurrently with Up (data race in tsnet init), so teardown cancels
	// the join and waits for start to return first.
	upCancel context.CancelFunc
	upDone   chan struct{}
	tr       *http.Transport
	flows    map[*flow]struct{}
}

// ObjectInfo is a snapshot of an object, safe to serialize.
type ObjectInfo struct {
	ID        string   `json:"id"`
	Hostname  string   `json:"hostname"`
	State     State    `json:"state"`
	Error     string   `json:"error,omitempty"`
	NodeID    string   `json:"nodeId,omitempty"`
	DNSName   string   `json:"dnsName,omitempty"`
	IPs       []string `json:"ips,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Ephemeral bool     `json:"ephemeral"`
	Rejoined  bool     `json:"rejoined"`
	Reminted  bool     `json:"reminted,omitempty"`
	JoinMS    float64  `json:"joinMs,omitempty"`
	Flows     int      `json:"flows"`
}

// objectMeta is persisted next to the tsnet state so Rejoin can restore it.
type objectMeta struct {
	ID        string   `json:"id"`
	Hostname  string   `json:"hostname"`
	Tags      []string `json:"tags,omitempty"`
	Ephemeral bool     `json:"ephemeral"`
}

// ErrClosed is returned after Close.
var ErrClosed = errors.New("netd: registry closed")

// ErrNotFound is returned for unknown object IDs.
var ErrNotFound = errors.New("netd: no such object")

// Create registers an object and queues its node join. It returns at once;
// use Object.Ready to wait. Creating an existing, non-terminal object returns
// it unchanged.
func (r *Registry) Create(id string, opts ObjectOptions) (*Object, error) {
	if !ValidObjectID(id) {
		return nil, fmt.Errorf("netd: invalid object id %q (want a lowercase DNS label)", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	if o := r.objects[id]; o != nil {
		switch o.State() {
		case StatePending, StateReady:
			return o, nil
		}
	}
	o := &Object{
		id:        id,
		hostname:  opts.Hostname,
		dir:       filepath.Join(r.cfg.StateRoot, id),
		ephemeral: r.cfg.Ephemeral,
		tags:      r.cfg.Tags,
		reg:       r,
		ready:     make(chan struct{}),
		state:     StatePending,
		queued:    time.Now(),
		flows:     map[*flow]struct{}{},
	}
	if o.hostname == "" {
		o.hostname = r.cfg.Hostname(id)
	}
	if opts.Tags != nil {
		o.tags = opts.Tags
	}
	if opts.Ephemeral != nil {
		o.ephemeral = *opts.Ephemeral
	}
	// A saved object.json wins: a rejoin must come back as the same node.
	if m, err := readMeta(o.dir); err == nil {
		o.hostname, o.tags, o.ephemeral = m.Hostname, m.Tags, m.Ephemeral
	}
	r.objects[id] = o
	r.joins.Add(1)
	go func() {
		defer r.joins.Done()
		r.join(o)
	}()
	return o, nil
}

// Rejoin creates every object that has saved state under StateRoot. No auth
// key is requested for them. It returns the IDs it queued.
func (r *Registry) Rejoin() ([]string, error) {
	ents, err := os.ReadDir(r.cfg.StateRoot)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		if !e.IsDir() || !ValidObjectID(e.Name()) || !hasState(filepath.Join(r.cfg.StateRoot, e.Name())) {
			continue
		}
		if _, err := r.Create(e.Name(), ObjectOptions{}); err != nil {
			return ids, err
		}
		ids = append(ids, e.Name())
	}
	return ids, nil
}

func hasState(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, "tailscaled.state"))
	return err == nil && fi.Size() > 2
}

func readMeta(dir string) (*objectMeta, error) {
	b, err := os.ReadFile(filepath.Join(dir, "object.json"))
	if err != nil {
		return nil, err
	}
	var m objectMeta
	return &m, json.Unmarshal(b, &m)
}

func (r *Registry) join(o *Object) {
	select {
	case r.sem <- struct{}{}:
	case <-r.ctx.Done():
		o.fail(ErrClosed)
		return
	}
	defer func() { <-r.sem }()
	if o.State() != StatePending {
		return // killed while queued
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.cfg.JoinTimeout)
	defer cancel()
	done := make(chan struct{})
	o.mu.Lock()
	o.upCancel, o.upDone = cancel, done
	o.mu.Unlock()
	err := o.start(ctx)
	close(done) // before any teardown below, which waits on it
	if err != nil {
		err = RedactError(err)
		r.log.Warn("object join failed", "object", o.id, "err", err)
		o.fail(err)
		o.teardown()
		return
	}
}

// errSavedStateRejected: a rejoin from saved node state left the backend in
// NeedsLogin (control expired or removed the device).
var errSavedStateRejected = errors.New("saved node state rejected by control (NeedsLogin)")

// needsLoginGrace is how long NeedsLogin must persist during a rejoin before
// it counts as a rejection (the backend may pass through it briefly).
var needsLoginGrace = 2 * time.Second

func (o *Object) start(ctx context.Context) error {
	r := o.reg
	if err := os.MkdirAll(o.dir, 0o700); err != nil {
		return err
	}
	rejoin := hasState(o.dir)
	var key string
	if !rejoin {
		if r.cfg.Keys == nil {
			return errors.New("no saved state and no KeySource")
		}
		k, err := r.cfg.Keys.AuthKey(ctx, KeyRequest{ObjectID: o.id, Hostname: o.hostname, Tags: o.tags, Ephemeral: o.ephemeral})
		if err != nil {
			return fmt.Errorf("auth key: %w", err)
		}
		key = k
	}
	mb, _ := json.Marshal(objectMeta{ID: o.id, Hostname: o.hostname, Tags: o.tags, Ephemeral: o.ephemeral})
	if err := os.WriteFile(filepath.Join(o.dir, "object.json"), mb, 0o600); err != nil {
		return err
	}
	st := time.Now()
	lc, sst, err := o.up(ctx, key, rejoin)
	reminted := false
	if errors.Is(err, errSavedStateRejected) {
		// The device is gone from control (an ephemeral node that was
		// offline during a restart, or one an admin removed). Drop the
		// saved state and join again as the same hostname with a fresh key
		// (KeyRequest.Remint lets mgmt-backed sources reopen the object).
		if r.cfg.Keys == nil {
			return err
		}
		r.log.Warn("saved node state rejected by control; re-minting under the same hostname", "object", o.id, "hostname", o.hostname, "ephemeral", o.ephemeral)
		o.dropSavedState()
		k, kerr := r.cfg.Keys.AuthKey(ctx, KeyRequest{ObjectID: o.id, Hostname: o.hostname, Tags: o.tags, Ephemeral: o.ephemeral, Remint: true})
		if kerr != nil {
			return fmt.Errorf("re-mint after %v: %w", err, kerr)
		}
		rejoin, reminted = false, true
		lc, sst, err = o.up(ctx, k, false)
	}
	if err != nil {
		return err
	}
	joinDur := time.Since(st)
	o.mu.Lock()
	srv := o.srv
	o.mu.Unlock()
	ip4, ip6 := srv.TailscaleIPs()
	ln, err := srv.Listen("tcp", fmt.Sprintf(":%d", r.cfg.IngressPort))
	if err != nil {
		return err
	}
	tr := &http.Transport{
		DialContext:         o.dialEgress,
		Proxy:               nil, // never an environment proxy
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     30 * time.Second,
		ForceAttemptHTTP2:   false,
	}
	hs := &http.Server{
		Handler:           http.HandlerFunc(o.serveIngress),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          slog.NewLogLogger(r.log.Handler(), slog.LevelDebug),
	}

	o.mu.Lock()
	if o.state != StatePending {
		o.mu.Unlock()
		ln.Close()
		return errors.New("killed while joining")
	}
	o.lc, o.ip4, o.ip6, o.joinDur, o.rejoined, o.reminted = lc, ip4, ip6, joinDur, rejoin, reminted
	if sst.Self != nil {
		o.nodeID, o.dnsName = sst.Self.ID, sst.Self.DNSName
	}
	o.ln = &trackedListener{Listener: ln, o: o}
	o.httpSrv, o.tr = hs, tr
	o.state = StateReady
	close(o.ready)
	o.mu.Unlock()

	r.mu.Lock()
	for _, a := range []netip.Addr{ip4, ip6} {
		if a.IsValid() {
			r.localIPs[a] = o.id
		}
	}
	r.mu.Unlock()
	go hs.Serve(o.ln)
	r.log.Info("object ready", "object", o.id, "hostname", o.hostname, "ip", ip4.String(),
		"node", string(o.nodeID), "join_ms", joinDur.Milliseconds(), "rejoined", rejoin, "reminted", reminted)
	return nil
}

// up builds the tsnet server (stored in o.srv so teardown can close it) and
// brings it up. With no key (a rejoin), it also watches the IPN bus:
// tsnet.Up waits for Running and never reacts to NeedsLogin, which is where
// a rejected saved state ends up for good; that returns errSavedStateRejected.
func (o *Object) up(ctx context.Context, key string, rejoin bool) (*local.Client, *ipnstate.Status, error) {
	r := o.reg
	logf := func(string, ...any) {}
	if r.cfg.TSNetLogs {
		logf = tsnetLogf(r.log, o.id)
	}
	srv := &tsnet.Server{
		Hostname:      o.hostname,
		Dir:           o.dir,
		ControlURL:    r.cfg.ControlURL,
		AuthKey:       key,
		Ephemeral:     o.ephemeral,
		AdvertiseTags: o.tags,
		Logf:          logf,
		UserLogf:      logf,
	}
	o.mu.Lock()
	if o.state != StatePending {
		o.mu.Unlock()
		return nil, nil, errors.New("killed while joining")
	}
	o.srv = srv
	o.mu.Unlock()

	upCtx, upCancel := context.WithCancel(ctx)
	defer upCancel()
	rejected := make(chan struct{})
	if rejoin && key == "" {
		if err := srv.Start(); err != nil {
			return nil, nil, fmt.Errorf("start %s: %w", o.hostname, err)
		}
		lc0, err := srv.LocalClient()
		if err != nil {
			return nil, nil, err
		}
		lc0.OmitAuth = true
		var once sync.Once
		go watchNeedsLogin(upCtx, lc0, func() { once.Do(func() { close(rejected); upCancel() }) })
	}
	if _, err := srv.Up(upCtx); err != nil {
		select {
		case <-rejected:
			return nil, nil, errSavedStateRejected
		default:
		}
		return nil, nil, fmt.Errorf("up %s: %w", o.hostname, err)
	}
	lc, err := srv.LocalClient()
	if err != nil {
		return nil, nil, err
	}
	// ADR-019 rule 4: without this every LocalClient call on macOS execs lsof
	// and sends the *system* Tailscale.app's token (~25 ms/call).
	lc.OmitAuth = true
	sst, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return nil, nil, err
	}
	return lc, sst, nil
}

// watchNeedsLogin calls onRejected once the backend has sat in NeedsLogin
// for needsLoginGrace without reaching Starting/Running.
func watchNeedsLogin(ctx context.Context, lc *local.Client, onRejected func()) {
	w, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		return
	}
	defer w.Close()
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		n, err := w.Next()
		if err != nil {
			return
		}
		if n.State == nil {
			continue
		}
		switch *n.State {
		case ipn.NeedsLogin:
			if timer == nil {
				timer = time.AfterFunc(needsLoginGrace, onRejected)
			}
		case ipn.Starting, ipn.Running:
			if timer != nil {
				timer.Stop()
				timer = nil
			}
		}
	}
}

// dropSavedState closes the rejected server and removes the node state, so
// the next up joins as a new device (object.json and certs are kept).
func (o *Object) dropSavedState() {
	o.mu.Lock()
	srv := o.srv
	o.srv = nil
	o.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
	os.Remove(filepath.Join(o.dir, "tailscaled.state"))
}

func (o *Object) fail(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.state == StatePending {
		o.state, o.err = StateFailed, err
		close(o.ready)
	}
}

// teardown closes the node and its listeners without aborting flows or
// logging out. It is idempotent.
func (o *Object) teardown() {
	o.mu.Lock()
	upCancel, upDone := o.upCancel, o.upDone
	o.mu.Unlock()
	if upDone != nil {
		select {
		case <-upDone:
		default:
			upCancel()
			<-upDone
		}
	}
	o.mu.Lock()
	srv, hs, tr, ts := o.srv, o.httpSrv, o.tr, o.tombSrv
	o.mu.Unlock()
	if hs != nil {
		hs.Close()
	}
	if ts != nil {
		ts.Close()
	}
	if tr != nil {
		tr.CloseIdleConnections()
	}
	if srv != nil {
		srv.Close()
	}
	o.reg.forgetIPs(o)
}

func (r *Registry) forgetIPs(o *Object) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for a, id := range r.localIPs {
		if id == o.id {
			delete(r.localIPs, a)
		}
	}
}

// Get returns the object with the given ID, or nil.
// RemoveState deletes a killed object's saved node state (twd down
// --purge). A kill with a tombstone removes the dir only when the tombstone
// expires; if the host stops before that, the next object of the same ID
// would find the dead node's state and spend a re-mint round trip on it.
// Refused while the object is live (pending or ready).
func (r *Registry) RemoveState(id string) error {
	if !ValidObjectID(id) {
		return fmt.Errorf("netd: invalid object id %q", id)
	}
	r.mu.Lock()
	o := r.objects[id]
	r.mu.Unlock()
	if o != nil {
		switch o.State() {
		case StatePending, StateReady:
			return fmt.Errorf("netd: object %s is live", id)
		}
	}
	return os.RemoveAll(filepath.Join(r.cfg.StateRoot, id))
}

func (r *Registry) Get(id string) *Object {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.objects[id]
}

// Objects returns a snapshot of every object, sorted by ID.
func (r *Registry) Objects() []ObjectInfo {
	r.mu.Lock()
	objs := make([]*Object, 0, len(r.objects))
	for _, o := range r.objects {
		objs = append(objs, o)
	}
	r.mu.Unlock()
	out := make([]ObjectInfo, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ID returns the object ID.
func (o *Object) ID() string { return o.id }

// State returns the current lifecycle state.
func (o *Object) State() State {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.state
}

// Server returns the object's tsnet.Server (nil before the join starts).
// Callers must not Close it; use Registry.Kill.
func (o *Object) Server() *tsnet.Server {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.srv
}

// Info returns a snapshot.
func (o *Object) Info() ObjectInfo {
	o.mu.Lock()
	defer o.mu.Unlock()
	in := ObjectInfo{ID: o.id, Hostname: o.hostname, State: o.state, NodeID: string(o.nodeID),
		DNSName: o.dnsName, Tags: o.tags, Ephemeral: o.ephemeral, Rejoined: o.rejoined, Reminted: o.reminted,
		JoinMS: float64(o.joinDur.Microseconds()) / 1000, Flows: len(o.flows)}
	if o.err != nil {
		in.Error = Redact(o.err.Error())
	}
	for _, a := range []netip.Addr{o.ip4, o.ip6} {
		if a.IsValid() {
			in.IPs = append(in.IPs, a.String())
		}
	}
	return in
}

// Ready waits until the object's node is up. It returns the join error if the
// join failed, or ctx's error.
func (o *Object) Ready(ctx context.Context) error {
	select {
	case <-o.ready:
	case <-ctx.Done():
		return newErr(CodeIdentityPending, "object "+o.id+" node not ready: "+ctx.Err().Error())
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	switch o.state {
	case StateReady:
		return nil
	case StateKilled:
		return newErr(CodeKilled, "object "+o.id+" was killed")
	case StateClosed:
		return newErr(CodeKilled, "object "+o.id+" is closed")
	default:
		msg := "join failed"
		if o.err != nil {
			msg = Redact(o.err.Error())
		}
		return newErr(CodeBootstrapFailed, msg)
	}
}

// IPv4 returns the object's tailnet IPv4 (zero before ready).
func (o *Object) IPv4() netip.Addr {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ip4
}

// Close closes every node in parallel, keeping saved state so the objects can
// Rejoin later. It waits for in-progress joins to finish or abort.
func (r *Registry) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	objs := make([]*Object, 0, len(r.objects))
	for _, o := range r.objects {
		objs = append(objs, o)
	}
	r.mu.Unlock()
	r.stop() // aborts queued and in-progress joins
	closeParallel(objs, 64, func(o *Object) {
		o.mu.Lock()
		if o.state == StatePending || o.state == StateReady {
			if o.state == StatePending {
				o.err = ErrClosed
				close(o.ready)
			}
			o.state = StateClosed
		}
		o.mu.Unlock()
		o.abortFlows()
		o.teardown()
	})
	r.joins.Wait()
	return nil
}

func closeParallel(objs []*Object, par int, f func(*Object)) {
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for _, o := range objs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			f(o)
		}()
	}
	wg.Wait()
}

var _ io.Closer = (*Registry)(nil)
