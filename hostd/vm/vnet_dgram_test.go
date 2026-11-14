//go:build darwin || linux

package vm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// fakeGuest is a userspace "guest" on the VMM end of the NIC socketpair:
// it stands in for the VM's kernel, so the macOS NIC path (datagram frames
// → host stack) is tested without a hypervisor.
func fakeGuest(t *testing.T) (*vnet, *stack.Stack, map[uint16]*atomic.Int64) {
	t.Helper()
	hostF, vmmF, err := dgramPair()
	if err != nil {
		t.Fatal(err)
	}
	hits := map[uint16]*atomic.Int64{proto.EgressObj: {}, proto.HostAPI: {}}
	handlers := map[uint16]http.Handler{}
	for port, n := range hits {
		handlers[port] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			fmt.Fprintf(w, "gateway:%d %s", port, r.URL.Path)
		})
	}
	v, err := newDgramVnet(hostF, 1500, gwMAC, gatewayIP, guestIP, proto.PrefixLen, handlers, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.close)
	gmac, _ := net.ParseMAC(guestMAC)
	ep, closeLink, err := newDgramLink(vmmF, 1500, gmac)
	if err != nil {
		t.Fatal(err)
	}
	st := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	if e := st.CreateNIC(1, ep); e != nil {
		t.Fatal(e)
	}
	st.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(guestIP.As4()), PrefixLen: proto.PrefixLen}}, stack.AddressProperties{})
	sub, _ := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{}), tcpip.MaskFromBytes(make([]byte, 4)))
	st.SetRouteTable([]tcpip.Route{{Destination: sub, Gateway: tcpip.AddrFrom4(gatewayIP.As4()), NIC: 1}})
	t.Cleanup(func() { st.Close(); st.Wait(); closeLink() })
	return v, st, hits
}

func guestDial(st *stack.Stack, target string) (net.Conn, error) {
	ap := netip.MustParseAddrPort(target)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	return gonet.DialContextTCP(ctx, st, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ap.Addr().As4()), Port: ap.Port()}, ipv4.ProtocolNumber)
}

// TestDgramNICOnlyGatewayServices: over the macOS NIC transport, the guest
// reaches exactly the gateway's platform handlers. Every other TCP flow
// (other gateway ports, the host's loopback and interfaces, the internet)
// is reset at the host stack and recorded; a canary listening on every
// host interface sees nothing.
func TestDgramNICOnlyGatewayServices(t *testing.T) {
	v, st, hits := fakeGuest(t)

	cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
		return guestDial(st, addr)
	}}}
	resp, err := cl.Get(fmt.Sprintf("http://%s:%d/attributed", proto.GatewayIP, proto.EgressObj))
	if err != nil {
		t.Fatalf("guest → gateway egress handler: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != fmt.Sprintf("gateway:%d /attributed", proto.EgressObj) || hits[proto.EgressObj].Load() != 1 {
		t.Fatalf("egress handler: %d %q", resp.StatusCode, body)
	}

	canary, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer canary.Close()
	var canaryHits atomic.Int64
	go func() {
		for {
			c, err := canary.Accept()
			if err != nil {
				return
			}
			canaryHits.Add(1)
			c.Close()
		}
	}()
	cport := canary.Addr().(*net.TCPAddr).Port
	targets := []string{
		fmt.Sprintf("%s:22", proto.GatewayIP), fmt.Sprintf("%s:%d", proto.GatewayIP, proto.EgressWL), // not served here
		fmt.Sprintf("%s:%d", proto.GatewayIP, cport), fmt.Sprintf("127.0.0.1:%d", cport),
		"10.0.2.2:22", "169.254.169.254:80", "1.1.1.1:443", "192.168.72.3:80",
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			targets = append(targets, fmt.Sprintf("%s:%d", ipn.IP, cport))
		}
	}
	for _, tg := range targets {
		c, err := guestDial(st, tg)
		if err == nil {
			c.Close()
			t.Errorf("guest connected to %s", tg)
			continue
		}
		// Loopback destinations on the wire are martians: the host stack
		// drops them (a real guest kernel never sends them on eth0).
		if !strings.Contains(err.Error(), "refused") && !strings.HasPrefix(tg, "127.") {
			t.Errorf("guest → %s: %v (want a fast refusal, not a hang)", tg, err)
		}
	}
	if n := canaryHits.Load(); n != 0 {
		t.Fatalf("host canary got %d connections from the guest", n)
	}
	refused := strings.Join(v.refusedFlows(), " ")
	for _, tg := range []string{fmt.Sprintf("%s:22", proto.GatewayIP), "1.1.1.1:443", "10.0.2.2:22"} {
		if !strings.Contains(refused, tg) {
			t.Errorf("flow to %s not recorded as refused (%s)", tg, refused)
		}
	}

	// Ingress runs the other way: host → the guest's workerd port.
	gln, err := gonet.ListenTCP(st, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(guestIP.As4()), Port: proto.IngressPort}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer gln.Close()
	go func() {
		c, err := gln.Accept()
		if err != nil {
			return
		}
		line, _ := bufio.NewReader(c).ReadString('\n')
		io.WriteString(c, "guest got "+line)
		c.Close()
	}()
	c, err := v.dialGuest(context.Background(), proto.IngressPort)
	if err != nil {
		t.Fatalf("host → guest ingress: %v", err)
	}
	io.WriteString(c, "hello\n")
	b, _ := io.ReadAll(c)
	c.Close()
	if string(b) != "guest got hello\n" {
		t.Fatalf("ingress: %q", b)
	}

	// The VM dies: every flow through the stack ends at once.
	gc, err := guestDial(st, fmt.Sprintf("%s:%d", proto.GatewayIP, proto.HostAPI))
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(gc, "GET /slow HTTP/1.1\r\nHost: x\r\n\r\n")
	hc, err := v.dialGuest(context.Background(), proto.IngressPort)
	if err == nil {
		v.close()
		hc.SetReadDeadline(time.Now().Add(3 * time.Second))
		st0 := time.Now()
		_, rerr := hc.Read(make([]byte, 1))
		if rerr == nil || time.Since(st0) > time.Second {
			t.Fatalf("host-side flow survived close: %v after %v", rerr, time.Since(st0))
		}
	}
	gc.Close()
}
