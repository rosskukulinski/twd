// Package tssvc makes a tsnet node a host for Tailscale Services
// (https://tailscale.com/kb/1552/tailscale-services) without giving up
// WireGuard caller identity.
//
// tsnet's own ListenService forwards Service traffic to a host loopback
// socket: the backend then sees 127.0.0.1 as the caller (or has to trust a
// PROXY header that any local process could forge). twd authenticates every
// request by WhoIs on the connection's source address, so that is not usable.
//
// Instead, a Host:
//   - advertises the Service in the node's prefs (AdvertiseServices);
//   - adds an *empty* TCP handler for each Service port to the serve config.
//     That makes netstack intercept flows to the Service VIP on that port,
//     but serve has nothing to do with them (no TCPForward, no HTTP), so the
//     flow falls through to tsnet's fallback TCP handlers;
//   - registers a fallback handler that hands flows to the VIP (any
//     destination that is not one of the node's own addresses, on a routed
//     port) to an in-process handler. The net.Conn's RemoteAddr is the
//     caller's real tailnet address, so WhoIs works exactly as it does for
//     the node's own address.
//
// Advertisement is only a request: control routes traffic to the node once
// the Service is defined and the node is approved (an admin, or policy
// autoApprovers.services for the node's tags).
package tssvc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

// Validate reports whether name is a valid Service name ("svc:" + a DNS label).
func Validate(name string) error {
	if !strings.HasPrefix(name, "svc:") {
		return fmt.Errorf("service name %q must start with svc:", name)
	}
	return tailcfg.ServiceName(name).Validate()
}

// Label returns the DNS label of a Service name ("svc:foo" → "foo").
func Label(name string) string { return strings.TrimPrefix(name, "svc:") }

// Node is the subset of *tsnet.Server a Host needs (tests use a fake).
type Node interface {
	TailscaleIPs() (ip4, ip6 netip.Addr)
	RegisterFallbackTCPHandler(cb tsnet.FallbackTCPHandler) func()
}

// Prefs is the subset of *local.Client a Host needs (tests use a fake).
type Prefs interface {
	GetPrefs(ctx context.Context) (*ipn.Prefs, error)
	EditPrefs(ctx context.Context, mp *ipn.MaskedPrefs) (*ipn.Prefs, error)
	GetServeConfig(ctx context.Context) (*ipn.ServeConfig, error)
	SetServeConfig(ctx context.Context, config *ipn.ServeConfig) error
}

var _ Prefs = (*local.Client)(nil)
var _ Node = (*tsnet.Server)(nil)

// Host routes Service flows on one tsnet node.
type Host struct {
	node  Node
	lc    Prefs
	unreg func()

	mu     sync.Mutex
	routes map[uint16]func(net.Conn)
	ads    map[string][]uint16 // advertised by this Host: name -> ports
	closed bool
}

// NewHost registers the fallback handler on node. Call Close to remove it.
func NewHost(node Node, lc Prefs) *Host {
	h := &Host{node: node, lc: lc, routes: map[uint16]func(net.Conn){}, ads: map[string][]uint16{}}
	h.unreg = node.RegisterFallbackTCPHandler(h.fallback)
	return h
}

// Route delivers Service flows arriving on port to handle. A flow to the
// node's own address on that port is never routed here (it goes to the
// node's normal listener).
func (h *Host) Route(port uint16, handle func(net.Conn)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.routes[port] = handle
}

func (h *Host) fallback(src, dst netip.AddrPort) (func(net.Conn), bool) {
	ip4, ip6 := h.node.TailscaleIPs()
	if a := dst.Addr().Unmap(); a == ip4 || a == ip6 {
		return nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || len(h.ads) == 0 {
		return nil, false
	}
	// Only ports of a Service this Host advertises: netstack only intercepts
	// those (serve config), so this is defense in depth.
	served := false
	for _, ports := range h.ads {
		if slices.Contains(ports, dst.Port()) {
			served = true
			break
		}
	}
	if !served {
		return nil, false
	}
	handle := h.routes[dst.Port()]
	if handle == nil {
		return nil, false
	}
	return handle, true
}

// Advertise makes the node a host for name on ports. It is idempotent.
func (h *Host) Advertise(ctx context.Context, name string, ports ...uint16) error {
	if err := Validate(name); err != nil {
		return err
	}
	if len(ports) == 0 {
		return errors.New("tssvc: advertise needs at least one port")
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return errors.New("tssvc: host closed")
	}
	h.ads[name] = slices.Clone(ports)
	h.mu.Unlock()

	// Serve config first: once the advertisement is approved, traffic must
	// already be intercepted.
	if err := h.editServe(ctx, func(sc *ipn.ServeConfig) bool {
		svc := tailcfg.ServiceName(name)
		cfg := sc.Services[svc]
		if cfg == nil {
			cfg = &ipn.ServiceConfig{}
		}
		changed := sc.Services[svc] == nil
		if cfg.TCP == nil {
			cfg.TCP = map[uint16]*ipn.TCPPortHandler{}
		}
		for _, p := range ports {
			if th, ok := cfg.TCP[p]; !ok || *th != (ipn.TCPPortHandler{}) {
				cfg.TCP[p] = &ipn.TCPPortHandler{} // empty: in-process delivery
				changed = true
			}
		}
		if sc.Services == nil {
			sc.Services = map[tailcfg.ServiceName]*ipn.ServiceConfig{}
		}
		sc.Services[svc] = cfg
		return changed
	}); err != nil {
		return fmt.Errorf("tssvc: serve config: %w", err)
	}
	return h.editAds(ctx, func(cur []string) []string {
		if slices.Contains(cur, name) {
			return nil
		}
		return append(cur, name)
	})
}

// Withdraw stops advertising name and removes its serve config. It is
// idempotent. Flows already accepted are not affected.
func (h *Host) Withdraw(ctx context.Context, name string) error {
	h.mu.Lock()
	delete(h.ads, name)
	h.mu.Unlock()
	// Advertisement first, so control stops routing before interception stops.
	adErr := h.editAds(ctx, func(cur []string) []string {
		if !slices.Contains(cur, name) {
			return nil
		}
		return slices.DeleteFunc(slices.Clone(cur), func(s string) bool { return s == name })
	})
	scErr := h.editServe(ctx, func(sc *ipn.ServeConfig) bool {
		if _, ok := sc.Services[tailcfg.ServiceName(name)]; !ok {
			return false
		}
		delete(sc.Services, tailcfg.ServiceName(name))
		return true
	})
	return errors.Join(adErr, scErr)
}

// Advertised returns the Services this node currently advertises (prefs).
func (h *Host) Advertised(ctx context.Context) ([]string, error) {
	p, err := h.lc.GetPrefs(ctx)
	if err != nil {
		return nil, err
	}
	return slices.Clone(p.AdvertiseServices), nil
}

// Close withdraws every Service this Host advertised and removes the
// fallback handler.
func (h *Host) Close(ctx context.Context) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	names := make([]string, 0, len(h.ads))
	for n := range h.ads {
		names = append(names, n)
	}
	h.mu.Unlock()
	var errs []error
	for _, n := range names {
		errs = append(errs, h.Withdraw(ctx, n))
	}
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.unreg()
	return errors.Join(errs...)
}

func (h *Host) editAds(ctx context.Context, f func(cur []string) []string) error {
	p, err := h.lc.GetPrefs(ctx)
	if err != nil {
		return err
	}
	next := f(p.AdvertiseServices)
	if next == nil {
		return nil
	}
	_, err = h.lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		AdvertiseServicesSet: true,
		Prefs:                ipn.Prefs{AdvertiseServices: next},
	})
	return err
}

// editServe applies f to the serve config under its ETag, retrying on a
// concurrent change. f reports whether it changed anything.
func (h *Host) editServe(ctx context.Context, f func(sc *ipn.ServeConfig) bool) error {
	var err error
	for range 5 {
		var sc *ipn.ServeConfig
		sc, err = h.lc.GetServeConfig(ctx)
		if err != nil {
			return err
		}
		if !f(sc) {
			return nil
		}
		if err = h.lc.SetServeConfig(ctx, sc); err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "etag") && !strings.Contains(err.Error(), "412") {
			return err
		}
	}
	return err
}

// ConnListener is a net.Listener fed by Deliver (a Route handler), so an
// http.Server can serve Service flows next to the node's own listener.
type ConnListener struct {
	addr   net.Addr
	ch     chan net.Conn
	done   chan struct{}
	closer sync.Once
}

// NewConnListener returns a listener reporting addr.
func NewConnListener(addr net.Addr) *ConnListener {
	return &ConnListener{addr: addr, ch: make(chan net.Conn), done: make(chan struct{})}
}

// Deliver hands c to Accept; it closes c if the listener is closed.
func (l *ConnListener) Deliver(c net.Conn) {
	select {
	case l.ch <- c:
	case <-l.done:
		c.Close()
	}
}

func (l *ConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *ConnListener) Close() error {
	l.closer.Do(func() { close(l.done) })
	return nil
}

func (l *ConnListener) Addr() net.Addr { return l.addr }

// Addr is a net.Addr naming a Service.
type Addr struct{ Name string }

func (a Addr) Network() string { return "tcp" }
func (a Addr) String() string  { return a.Name }
