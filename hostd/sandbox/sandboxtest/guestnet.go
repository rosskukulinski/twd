package sandboxtest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/rosskukulinski/twd/hostd/sandbox"
	"golang.org/x/net/dns/dnsmessage"
)

// LastNet is the egress proxy (sandbox.ContainerSpec.Net) of the most
// recently booted fake container, and its CA certificate.
func (b *Procs) LastNet() (sandbox.ContainerNet, []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.Specs) == 0 {
		return nil, nil
	}
	s := b.Specs[len(b.Specs)-1]
	return s.Net, s.CACert
}

// GuestLookup resolves name the way a container guest does: one A query
// to its egress proxy's DNS.
func GuestLookup(n sandbox.ContainerNet, name string) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(name); err == nil {
		return ip, nil
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 1, RecursionDesired: true})
	b.StartQuestions()
	qn, err := dnsmessage.NewName(name + ".")
	if err != nil {
		return netip.Addr{}, err
	}
	b.Question(dnsmessage.Question{Name: qn, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	q, _ := b.Finish()
	resp := n.DNS(q)
	var m dnsmessage.Message
	if resp == nil || m.Unpack(resp) != nil {
		return netip.Addr{}, errors.New("no DNS answer")
	}
	for _, a := range m.Answers {
		if r, ok := a.Body.(*dnsmessage.AResource); ok {
			return netip.AddrFrom4(r.A), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("lookup %s: %v", name, m.RCode)
}

// GuestDial opens a TCP flow from the fake guest to host:port through n,
// as the microVM's NIC hands flows to it (a refused flow is a reset).
func GuestDial(n sandbox.ContainerNet, host string, port int) (net.Conn, error) {
	ip, err := GuestLookup(n, host)
	if err != nil {
		return nil, err
	}
	dst := netip.AddrPortFrom(ip, uint16(port))
	if !n.Accept(dst) {
		return nil, fmt.Errorf("connect %s: connection refused", dst)
	}
	g, h := net.Pipe()
	go n.Serve(h, dst)
	return g, nil
}

// GuestClient is an HTTP client in the fake guest: every connection goes
// through n, and HTTPS trusts only the container's CA (caPEM).
func GuestClient(n sandbox.ContainerNet, caPEM []byte) *http.Client {
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			h, p, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, _ := strconv.Atoi(p)
			return GuestDial(n, h, port)
		},
		TLSClientConfig:   &tls.Config{RootCAs: roots},
		DisableKeepAlives: true,
	}}
}
