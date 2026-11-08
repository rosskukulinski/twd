package netd

import (
	"context"
	"net"
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
	// Logout logs the node out before closing it. For ephemeral nodes this
	// removes the device at once, so peers drop it on their next netmap
	// instead of black-holing packets until the device ages out.
	Logout bool
	// KeepState keeps the object's state dir (default: removed, since a killed
	// object never rejoins and its node key should not linger on disk).
	KeepState bool
	// SkipDelete skips Config.OnDelete.
	SkipDelete bool
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
// TCP RST (so callers fail now rather than at their own timeout), then closes
// the node, then calls the device-delete hook. Kill is terminal.
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
	rep.FlowsAborted = o.abortFlows()
	rep.AbortMS = ms(time.Since(t0))

	// 2. Close the node (optionally logging out first).
	if opts.Logout && lc != nil {
		ls := time.Now()
		lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := lc.Logout(lctx); err != nil {
			rep.LogoutErr = Redact(err.Error())
		}
		cancel()
		rep.LogoutMS = ms(time.Since(ls))
	}
	cs := time.Now()
	o.teardown()
	rep.CloseMS = ms(time.Since(cs))
	if !opts.KeepState {
		os.RemoveAll(o.dir)
	}

	// 3. Tier 2: delete the device.
	if r.cfg.OnDelete != nil && !opts.SkipDelete && prev != StateFailed {
		ds := time.Now()
		if err := r.cfg.OnDelete(ctx, info); err != nil {
			rep.DeleteErr = Redact(err.Error())
		}
		rep.DeleteMS = ms(time.Since(ds))
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
