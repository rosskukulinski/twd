package netd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"sync"
	"time"
	"unsafe"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
)

// KillOptions control Registry.Kill.
type KillOptions struct {
	Reason string
	// Logout logs the node out before closing it (bounded to 1.5 s). For
	// ephemeral nodes this removes the device without a lifecycle credential,
	// so peers drop it instead of black-holing packets until it ages out.
	// Measured on production control it is slower and far more variable
	// (0.16-3 s) than OnDelete (~150 ms), so prefer OnDelete when available.
	Logout bool
	// KeepState keeps the object's state dir (default: removed, since a killed
	// object never rejoins and its node key should not linger on disk).
	KeepState bool
	// SkipDelete skips Config.OnDelete.
	SkipDelete bool
	// CloseFirst closes the node before calling Config.OnDelete. The default
	// (delete first) was measured faster on production control: removal from
	// peers' netmaps median 206 ms vs 2.4 s, because closing an ephemeral
	// node first waits on its control-side logout (up to 5 s).
	CloseFirst bool
	// Tombstone keeps the killed node up (flows reset) for this long after
	// the device delete before closing it, so NEW connections from peers
	// fail fast instead of black-holing until their timeout (M1 finding 3):
	// the ingress port answers every request with a typed 410
	// object_killed (Connection: close). Ignored with CloseFirst.
	// Registry.Close still closes tombstoned nodes at once.
	Tombstone time.Duration
}

// KillReport records what a kill did and how long each step took.
type KillReport struct {
	Object       string  `json:"object"`
	NodeID       string  `json:"nodeId,omitempty"`
	PrevState    State   `json:"prevState"`
	FlowsAborted int     `json:"flowsAborted"`
	AbortMS      float64 `json:"abortMs"`  // RST of in-flight flows
	LogoutMS     float64 `json:"logoutMs"` // 0 unless Logout
	CloseMS      float64 `json:"closeMs"`  // tsnet.Server.Close
	DeleteMS     float64 `json:"deleteMs"` // OnDelete
	TotalMS      float64 `json:"totalMs"`
	LogoutErr    string  `json:"logoutError,omitempty"`
	DeleteErr    string  `json:"deleteError,omitempty"`
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// Kill is kill tier 1 (+ tier 2 via OnDelete): it marks the object killed so
// no new ingress or egress is admitted, aborts every in-flight flow with a
// TCP RST (so callers fail now rather than at their own timeout), waits
// KillFlushDelay for the RSTs to leave, then deletes the device (OnDelete)
// and closes the node (see KillOptions.CloseFirst for the order). Kill is
// terminal.
func (r *Registry) Kill(ctx context.Context, id string, opts KillOptions) (*KillReport, error) {
	o := r.Get(id)
	if o == nil {
		return nil, ErrNotFound
	}
	t0 := time.Now()
	o.mu.Lock()
	prev := o.state
	if prev == StateKilled {
		o.mu.Unlock()
		return &KillReport{Object: id, PrevState: prev}, nil
	}
	if prev == StatePending {
		close(o.ready)
	}
	o.state = StateKilled
	o.err = newErr(CodeKilled, "killed: "+opts.Reason)
	ln, lc := o.ln, o.lc
	o.mu.Unlock()
	rep := &KillReport{Object: id, PrevState: prev}
	info := o.Info()
	rep.NodeID = info.NodeID

	// 1. Stop accepting, then RST everything in flight, both directions.
	if ln != nil {
		ln.Close() // new SYNs to the port now get a RST from netstack
	}
	if opts.Tombstone > 0 && !opts.CloseFirst {
		o.serveTombstone(opts.Reason)
	}
	rep.FlowsAborted = o.abortFlows()
	rep.AbortMS = ms(time.Since(t0))
	// Abort only queues the RSTs; tsnet.Close tears down netstack first and
	// would drop them, leaving callers black-holed (seen on production
	// control). Keep the node up briefly so the RSTs leave over WireGuard.
	if rep.FlowsAborted > 0 && r.cfg.KillFlushDelay > 0 {
		time.Sleep(r.cfg.KillFlushDelay)
	}

	// 2. Optionally log out first.
	if opts.Logout && lc != nil {
		ls := time.Now()
		lctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		if err := lc.Logout(lctx); err != nil {
			rep.LogoutErr = Redact(err.Error())
		}
		cancel()
		rep.LogoutMS = ms(time.Since(ls))
	}
	// 3. Tier 2 (delete the device), then Close the node; or, with
	// CloseFirst, the other way round. The RST in step 1 has already failed
	// every in-flight caller; the order only decides when peers drop the node
	// from their netmaps. Running both concurrently was measured to contend at
	// control (both took 4-5 s), so they run in sequence.
	doDelete := func() {
		if r.cfg.OnDelete == nil || opts.SkipDelete || prev == StateFailed || info.NodeID == "" {
			return
		}
		ds := time.Now()
		if err := r.cfg.OnDelete(ctx, info); err != nil {
			rep.DeleteErr = Redact(err.Error())
		}
		rep.DeleteMS = ms(time.Since(ds))
	}
	doClose := func() {
		cs := time.Now()
		o.teardown()
		rep.CloseMS = ms(time.Since(cs))
		if !opts.KeepState {
			os.RemoveAll(o.dir)
		}
	}
	if opts.CloseFirst {
		doClose()
		doDelete()
	} else {
		doDelete()
		if opts.Tombstone > 0 {
			keep := opts.KeepState
			time.AfterFunc(opts.Tombstone, func() {
				o.teardown()
				if !keep {
					os.RemoveAll(o.dir)
				}
			})
		} else {
			doClose()
		}
	}
	rep.TotalMS = ms(time.Since(t0))
	r.log.Info("object killed", "object", id, "reason", opts.Reason, "flows_aborted", rep.FlowsAborted,
		"abort_ms", rep.AbortMS, "close_ms", rep.CloseMS, "delete_ms", rep.DeleteMS, "delete_err", rep.DeleteErr)
	return rep, nil
}

// flow is a tracked connection (ingress accept or egress dial) of an object.
type flow struct {
	net.Conn
	o    *Object
	once sync.Once
}

func (f *flow) Close() error {
	f.once.Do(func() { f.o.untrack(f) })
	return f.Conn.Close()
}

// track registers c as one of o's flows. It returns false (and RSTs c) if the
// object is no longer admitting traffic.
func (o *Object) track(c net.Conn) (*flow, bool) {
	f := &flow{Conn: c, o: o}
	o.mu.Lock()
	ok := o.state == StateReady
	if ok {
		o.flows[f] = struct{}{}
	}
	o.mu.Unlock()
	if !ok {
		rst(c)
	}
	return f, ok
}

func (o *Object) untrack(f *flow) {
	o.mu.Lock()
	delete(o.flows, f)
	o.mu.Unlock()
}

// abortFlows RSTs every tracked flow and returns how many there were.
func (o *Object) abortFlows() int {
	o.mu.Lock()
	fs := make([]*flow, 0, len(o.flows))
	for f := range o.flows {
		fs = append(fs, f)
	}
	o.flows = map[*flow]struct{}{}
	o.mu.Unlock()
	for _, f := range fs {
		f.once.Do(func() {})
		rst(f.Conn)
	}
	return len(fs)
}

// trackedListener tracks every accepted connection as a flow.
type trackedListener struct {
	net.Listener
	o *Object
}

func (l *trackedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if f, ok := l.o.track(c); ok {
			return f, nil
		}
	}
}

// rst aborts c so the peer sees a TCP RST rather than a FIN or silence.
func rst(c net.Conn) {
	switch tc := c.(type) {
	case *gonet.TCPConn:
		if ep := gonetEndpoint(tc); ep != nil {
			ep.Abort()
			return
		}
	case *net.TCPConn:
		tc.SetLinger(0)
	}
	c.Close()
}

// gonetEndpoint returns the gVisor endpoint under a gonet.TCPConn. gonet has
// no accessor and its Close sends a FIN, which a caller mid-request can't tell
// apart from a slow response; Abort sends a RST. TestGonetEndpoint fails
// loudly if a gVisor bump renames the field.
func gonetEndpoint(c *gonet.TCPConn) tcpip.Endpoint {
	v := reflect.ValueOf(c).Elem().FieldByName("ep")
	if !v.IsValid() || v.Type() != reflect.TypeFor[tcpip.Endpoint]() {
		return nil
	}
	ep, _ := reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface().(tcpip.Endpoint)
	return ep
}

func hasEPField() bool {
	f, ok := reflect.TypeFor[gonet.TCPConn]().FieldByName("ep")
	return ok && f.Type == reflect.TypeFor[tcpip.Endpoint]()
}

// serveTombstone re-listens on the ingress port of a killed node and answers
// every request with a typed 410 object_killed, so a caller learns at once
// (and why) instead of seeing a bare RST or, once peers drop the node, a
// black-holed dial. Connections are not tracked and never reach upstream.
func (o *Object) serveTombstone(reason string) {
	o.mu.Lock()
	srv := o.srv
	o.mu.Unlock()
	if srv == nil {
		return
	}
	ln, err := srv.Listen("tcp", fmt.Sprintf(":%d", o.reg.cfg.IngressPort))
	if err != nil {
		return
	}
	id := o.id
	hs := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Connection", "close")
			e := newErr(CodeObjectKilled, "object "+id+" was killed")
			if reason != "" {
				e.Detail += ": " + reason
			}
			e.Object = id
			WriteError(w, e)
		}),
	}
	hs.SetKeepAlivesEnabled(false)
	o.mu.Lock()
	o.tombSrv = hs
	o.mu.Unlock()
	go hs.Serve(ln)
}
