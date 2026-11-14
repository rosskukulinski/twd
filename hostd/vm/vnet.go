// The host side of a guest's NIC: a gVisor userspace TCP/IP stack on the
// VM's ethernet link, acting as the guest's only gateway. The link is the
// VM's TAP (through a per-VM bridge) under Firecracker (vnet_linux.go) and
// a datagram socketpair to Virtualization.framework's file-handle NIC on
// macOS (vnet_dgram.go); everything above the link is the same code.
//
// Adapted from tailscale/tailvisor netd (stack.go, proxy.go; BSD-3-Clause,
// Copyright (c) 2026 Tailscale Inc & contributors; see
// .refs/tailvisor/LICENSE in the design repo). Differences: tailvisor
// proxies *every* guest flow to one tsnet.Server.Dial; twd demultiplexes at
// L7 instead. The guest may open TCP only to the gateway's platform ports
// (attributed egress, workload egress, host API); netd maps each request to
// the right object's tsnet.Server and applies the ADR-019 egress guard.
// Every other flow, to any address or port, is reset and recorded; UDP and
// ICMP are not even registered. Ingress runs the other way: host netd dials
// the guest's workerd socket through this stack.
package vm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/rosskukulinski/twd/hostd/sandbox"
)

const nicID = 1

type vnet struct {
	st      *stack.Stack
	gw      netip.Addr
	guest   netip.Addr
	servers []*http.Server
	udp     *gonet.UDPConn

	onClose func()

	mu      sync.Mutex
	refused []string
	conns   map[*gonet.TCPConn]struct{}
	closed  bool
}

// newVnetOn builds the stack on ep (an ethernet link endpoint whose
// address is the gateway's MAC) and serves the handlers on gateway:port.
// onClose runs after the stack is closed (the link's own cleanup).
func newVnetOn(ep stack.LinkEndpoint, gw, guest netip.Addr, prefix int, handlers map[uint16]http.Handler, gnet sandbox.ContainerNet, onClose func()) (*vnet, error) {
	tps := []stack.TransportProtocolFactory{tcp.NewProtocol}
	if gnet != nil {
		tps = append(tps, udp.NewProtocol) // only for gateway:53 (DNS)
	}
	st := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: tps,
	})
	sack := tcpip.TCPSACKEnabled(true)
	st.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)
	if e := st.CreateNIC(nicID, ep); e != nil {
		return nil, fmt.Errorf("CreateNIC: %v", e)
	}
	// Promiscuous + spoofing: SYNs to *any* address reach the forwarder
	// below, which resets them (fast, explicit failure, and a record),
	// instead of being dropped silently at the IP layer.
	st.SetPromiscuousMode(nicID, true)
	st.SetSpoofing(nicID, true)
	if e := st.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(gw.As4()), PrefixLen: prefix},
	}, stack.AddressProperties{}); e != nil {
		return nil, fmt.Errorf("AddProtocolAddress: %v", e)
	}
	sub, _ := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{}), tcpip.MaskFromBytes(make([]byte, 4)))
	st.SetRouteTable([]tcpip.Route{{Destination: sub, NIC: nicID}})

	v := &vnet{st: st, gw: gw, guest: guest, conns: map[*gonet.TCPConn]struct{}{}, onClose: onClose}
	// Unmatched TCP (anything but the listeners below): a container
	// guest's flow goes to its sandbox egress proxy if that accepts it;
	// everything else is reset and recorded.
	fwd := tcp.NewForwarder(st, 0, 1024, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		dst := netip.AddrPortFrom(addrOf(id.LocalAddress), id.LocalPort)
		if gnet != nil && dst.Addr() != gw && gnet.Accept(dst) {
			var wq waiter.Queue
			ep, e := r.CreateEndpoint(&wq)
			if e != nil {
				r.Complete(true)
				return
			}
			r.Complete(false)
			c := gonet.NewTCPConn(&wq, ep)
			v.mu.Lock()
			if v.closed {
				v.mu.Unlock()
				c.Close()
				return
			}
			v.conns[c] = struct{}{}
			v.mu.Unlock()
			go gnet.Serve(&trackedConn{TCPConn: c, v: v}, dst)
			return
		}
		v.mu.Lock()
		if len(v.refused) < 1024 {
			v.refused = append(v.refused, dst.String())
		}
		v.mu.Unlock()
		r.Complete(true) // RST
	})
	st.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	if gnet != nil {
		pc, err := gonet.DialUDP(st, &tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(gw.As4()), Port: 53}, nil, ipv4.ProtocolNumber)
		if err != nil {
			st.Close()
			return nil, fmt.Errorf("listen gateway:53/udp: %w", err)
		}
		v.udp = pc
		go serveDNS(pc, gnet)
	}

	for port, h := range handlers {
		ln, err := gonet.ListenTCP(st, tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(gw.As4()), Port: port}, ipv4.ProtocolNumber)
		if err != nil {
			v.close()
			return nil, fmt.Errorf("listen gateway:%d: %w", port, err)
		}
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second,
			ConnState: func(c net.Conn, s http.ConnState) {
				if tc, ok := c.(*gonet.TCPConn); ok {
					switch s {
					case http.StateNew:
						v.track(tc, true)
					case http.StateClosed, http.StateHijacked:
						v.track(tc, false)
					}
				}
			}}
		v.servers = append(v.servers, srv)
		go srv.Serve(ln)
	}
	return v, nil
}

// serveDNS answers a container guest's DNS queries (gateway:53/udp) from
// its sandbox egress proxy, one datagram at a time.
func serveDNS(pc *gonet.UDPConn, gnet sandbox.ContainerNet) {
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if resp := gnet.DNS(append([]byte(nil), buf[:n]...)); resp != nil {
			pc.WriteTo(resp, from)
		}
	}
}

func addrOf(a tcpip.Address) netip.Addr {
	if a.Len() == 4 {
		return netip.AddrFrom4(a.As4())
	}
	return netip.AddrFrom16(a.As16())
}

func (v *vnet) track(c *gonet.TCPConn, add bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if add {
		v.conns[c] = struct{}{}
	} else {
		delete(v.conns, c)
	}
}

// dialGuest opens a TCP connection to the guest (host netd → workerd ingress).
func (v *vnet) dialGuest(ctx context.Context, port uint16) (net.Conn, error) {
	c, err := gonet.DialContextTCP(ctx, v.st, tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(v.guest.As4()), Port: port}, ipv4.ProtocolNumber)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		c.Close()
		return nil, net.ErrClosed
	}
	v.conns[c] = struct{}{}
	v.mu.Unlock()
	return &trackedConn{TCPConn: c, v: v}, nil
}

type trackedConn struct {
	*gonet.TCPConn
	v    *vnet
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { c.v.track(c.TCPConn, false) })
	return c.TCPConn.Close()
}

func (v *vnet) refusedFlows() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.refused...)
}

// abortAll ends every flow into or out of the guest at once (the VM died):
// callers see an error now, not a hang until their timeout.
func (v *vnet) abortAll() {
	v.mu.Lock()
	cs := make([]*gonet.TCPConn, 0, len(v.conns))
	for c := range v.conns {
		cs = append(cs, c)
	}
	v.mu.Unlock()
	past := time.Unix(1, 0)
	for _, c := range cs {
		c.SetDeadline(past) // wakes blocked reads/writes (gonet Close alone may not)
		c.Close()
	}
}

func (v *vnet) close() {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.closed = true
	v.mu.Unlock()
	for _, s := range v.servers {
		s.Close()
	}
	if v.udp != nil {
		v.udp.Close()
	}
	v.abortAll()
	v.st.Close()
	v.st.Wait()
	if v.onClose != nil {
		v.onClose()
	}
}
