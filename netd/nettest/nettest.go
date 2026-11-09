// Package nettest runs a hermetic tailnet for tests: tailscale's testcontrol
// plus a local DERP and STUN server on 127.0.0.1, and helpers for client and
// echo nodes. Nothing here talks to the internet.
package nettest

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/derp/derpserver"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/stunserver"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// TestCap is the app capability granted between all nodes on the local control.
const TestCap = "example.com/cap/ttest"

// TestCapValue is its value.
const TestCapValue = `{"role":"tester"}`

// MagicDNSDomain is the local tailnet's DNS suffix.
const MagicDNSDomain = "ttest.ts.net"

// DummyKey is the auth key used against testcontrol (it accepts any key).
const DummyKey = "testcontrol-any-key"

// Control is a running local control plane.
type Control struct {
	URL     string
	Server  *testcontrol.Server
	closers []func()
}

// StartControl starts testcontrol, DERP and STUN. Call Close when done.
func StartControl() (*Control, error) {
	d := derpserver.New(key.NewNode(), logger.Discard)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ds := httptest.NewUnstartedServer(derpserver.Handler(d))
	ds.Listener.Close()
	ds.Listener = ln
	ds.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	ds.Config.ErrorLog = nil
	ds.StartTLS()
	sctx, scancel := context.WithCancel(context.Background())
	stun := stunserver.New(sctx)
	if err := stun.Listen("127.0.0.1:0"); err != nil {
		scancel()
		ds.Close()
		return nil, err
	}
	go stun.Serve()
	stunPort := stun.LocalAddr().(*net.UDPAddr).Port
	dm := &tailcfg.DERPMap{Regions: map[tailcfg.DERPRegionID]*tailcfg.DERPRegion{1: {
		RegionID: 1, RegionCode: "twd-local",
		Nodes: []*tailcfg.DERPNode{{Name: "t1", RegionID: 1, HostName: "127.0.0.1", IPv4: "127.0.0.1", IPv6: "none",
			STUNPort: stunPort, DERPPort: ln.Addr().(*net.TCPAddr).Port, InsecureForTests: true, STUNTestIP: "127.0.0.1"}},
	}}}
	// TagOwners non-nil: a node that advertises tags gets them (as the real
	// control plane does at signup), so WhoIs sees tag:twd-host /
	// tag:twd-mgmt / object tags in hermetic runs. Ownership itself is not
	// checked here.
	ctl := &testcontrol.Server{DERPMap: dm, MagicDNSDomain: MagicDNSDomain, Logf: logger.Discard, TagOwners: map[string][]string{},
		DNSConfig: &tailcfg.DNSConfig{Proxied: true}}
	ctl.HTTPTestServer = httptest.NewUnstartedServer(ctl)
	ctl.HTTPTestServer.Start()
	ctl.SetGlobalAppCaps(tailcfg.PeerCapMap{TestCap: []tailcfg.RawMessage{TestCapValue}})
	return &Control{URL: ctl.HTTPTestServer.URL, Server: ctl, closers: []func(){
		ctl.HTTPTestServer.Close, scancel, ds.Close, func() { d.Close() },
	}}, nil
}

// Close stops everything.
func (c *Control) Close() {
	for _, f := range c.closers {
		f()
	}
}

// Node is a test tsnet node (a client or an echo server).
type Node struct {
	*tsnet.Server
	LC  *local.Client
	IP4 string
}

// NewNode starts an ephemeral in-memory node on controlURL with key.
func NewNode(ctx context.Context, controlURL, key, hostname, dir string) (*Node, error) {
	if controlURL == "" {
		return nil, fmt.Errorf("nettest: controlURL required")
	}
	s := &tsnet.Server{
		Hostname: hostname, ControlURL: controlURL, AuthKey: key, Ephemeral: true,
		Store: new(mem.Store), Dir: dir,
		Logf: func(string, ...any) {}, UserLogf: func(string, ...any) {},
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if _, err := s.Up(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("up %s: %w", hostname, err)
	}
	lc, err := s.LocalClient()
	if err != nil {
		s.Close()
		return nil, err
	}
	lc.OmitAuth = true
	ip4, _ := s.TailscaleIPs()
	return &Node{Server: s, LC: lc, IP4: ip4.String()}, nil
}

// HTTPClient returns a client that dials only via this node (never host DNS
// or the host network).
func (n *Node) HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{DialContext: n.Dial, Proxy: nil, MaxIdleConnsPerHost: 8}}
}

// EchoReply is what an echo node returns: the WhoIs of its caller and the
// headers it saw.
type EchoReply struct {
	Caller struct {
		Node   string   `json:"node"`
		NodeID string   `json:"nodeId"`
		Tags   []string `json:"tags"`
		Addrs  []string `json:"addrs"`
	} `json:"caller"`
	Headers http.Header `json:"headers"`
}

// ServeEcho serves the echo protocol on n:80. /stream writes a chunk every
// 50 ms until the connection breaks.
func (n *Node) ServeEcho() error {
	ln, err := n.Listen("tcp", ":80")
	if err != nil {
		return err
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			fl := w.(http.Flusher)
			for {
				if _, err := w.Write([]byte("tick\n")); err != nil {
					return
				}
				fl.Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		}
		who, err := n.LC.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			http.Error(w, "whois: "+err.Error(), 403)
			return
		}
		var rep EchoReply
		rep.Caller.Node = who.Node.Name
		rep.Caller.NodeID = string(who.Node.StableID)
		rep.Caller.Tags = who.Node.Tags
		for _, a := range who.Node.Addresses {
			rep.Caller.Addrs = append(rep.Caller.Addrs, a.Addr().String())
		}
		rep.Headers = r.Header
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(rep)
	}))
	return nil
}

// InNetmap reports whether ip is a peer in n's netmap.
func (n *Node) InNetmap(ctx context.Context, ip string) bool {
	st, err := n.LC.Status(ctx)
	if err != nil {
		return false
	}
	for _, p := range st.Peer {
		for _, a := range p.TailscaleIPs {
			if a.String() == ip {
				return true
			}
		}
	}
	return false
}

// WaitPeers waits until every ip is in n's netmap.
func (n *Node) WaitPeers(ctx context.Context, ips ...string) error {
	for {
		ok := true
		for _, ip := range ips {
			ok = ok && n.InNetmap(ctx, ip)
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("peers %v not all visible: %w", ips, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// FindRepoRoot walks up from the working directory to the directory holding go.mod.
func FindRepoRoot() (string, error) {
	d, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		p := filepath.Dir(d)
		if p == d {
			return "", fmt.Errorf("go.mod not found")
		}
		d = p
	}
}

// WorkerdBinary locates the pinned workerd (npm package at the repo root), or
// $TWD_WORKERD.
func WorkerdBinary() (string, error) {
	if p := os.Getenv("TWD_WORKERD"); p != "" {
		return p, nil
	}
	root, err := FindRepoRoot()
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, "node_modules", ".bin", "workerd")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("workerd not found at %s: run `npm ci` at the repo root (make deps)", p)
	}
	return p, nil
}
