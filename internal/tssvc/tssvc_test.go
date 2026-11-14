package tssvc

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"testing"

	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

type fakeNode struct {
	cb tsnet.FallbackTCPHandler
}

func (f *fakeNode) TailscaleIPs() (netip.Addr, netip.Addr) {
	return netip.MustParseAddr("100.64.0.5"), netip.MustParseAddr("fd7a:115c:a1e0::5")
}
func (f *fakeNode) RegisterFallbackTCPHandler(cb tsnet.FallbackTCPHandler) func() {
	f.cb = cb
	return func() { f.cb = nil }
}

type fakePrefs struct {
	p  ipn.Prefs
	sc ipn.ServeConfig
}

func (f *fakePrefs) GetPrefs(context.Context) (*ipn.Prefs, error) { p := f.p; return &p, nil }
func (f *fakePrefs) EditPrefs(_ context.Context, mp *ipn.MaskedPrefs) (*ipn.Prefs, error) {
	if mp.AdvertiseServicesSet {
		f.p.AdvertiseServices = slices.Clone(mp.AdvertiseServices)
	}
	return &f.p, nil
}
func (f *fakePrefs) GetServeConfig(context.Context) (*ipn.ServeConfig, error) {
	return f.sc.Clone(), nil
}
func (f *fakePrefs) SetServeConfig(_ context.Context, sc *ipn.ServeConfig) error {
	f.sc = *sc.Clone()
	return nil
}

func TestHostRouting(t *testing.T) {
	ctx := context.Background()
	n, p := &fakeNode{}, &fakePrefs{}
	h := NewHost(n, p)
	hit := 0
	h.Route(80, func(net.Conn) { hit++ })
	vip := netip.MustParseAddrPort("100.96.0.9:80")
	src := netip.MustParseAddrPort("100.64.0.2:5555")

	if _, ok := n.cb(src, vip); ok {
		t.Fatal("routed a flow before any Service was advertised")
	}
	if err := h.Advertise(ctx, "svc:web", 80); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.p.AdvertiseServices, []string{"svc:web"}) {
		t.Fatalf("prefs %v", p.p.AdvertiseServices)
	}
	th := p.sc.Services[tailcfg.ServiceName("svc:web")].TCP[80]
	if th == nil || *th != (ipn.TCPPortHandler{}) {
		t.Fatalf("serve config handler %+v: must be empty (in-process delivery, no TCPForward)", th)
	}
	if f, ok := n.cb(src, vip); !ok || f == nil {
		t.Fatal("VIP flow not routed")
	}
	// The node's own addresses are never captured (its normal listeners own them).
	if _, ok := n.cb(src, netip.MustParseAddrPort("100.64.0.5:80")); ok {
		t.Fatal("captured a flow to the node's own IPv4")
	}
	if _, ok := n.cb(src, netip.MustParseAddrPort("[fd7a:115c:a1e0::5]:80")); ok {
		t.Fatal("captured a flow to the node's own IPv6")
	}
	if _, ok := n.cb(src, netip.MustParseAddrPort("100.96.0.9:22")); ok {
		t.Fatal("captured a port no Service advertises")
	}
	// Idempotent.
	if err := h.Advertise(ctx, "svc:web", 80); err != nil || len(p.p.AdvertiseServices) != 1 {
		t.Fatalf("re-advertise: %v %v", err, p.p.AdvertiseServices)
	}
	// Withdraw: prefs and serve config cleaned, flows no longer routed.
	if err := h.Withdraw(ctx, "svc:web"); err != nil {
		t.Fatal(err)
	}
	if len(p.p.AdvertiseServices) != 0 || p.sc.Services[tailcfg.ServiceName("svc:web")] != nil {
		t.Fatalf("after withdraw: prefs %v serve %+v", p.p.AdvertiseServices, p.sc.Services)
	}
	if _, ok := n.cb(src, vip); ok {
		t.Fatal("routed a flow after withdraw")
	}
	if err := h.Advertise(ctx, "bad name", 80); err == nil {
		t.Fatal("invalid name accepted")
	}
	h.Advertise(ctx, "svc:web", 80)
	if err := h.Close(ctx); err != nil || len(p.p.AdvertiseServices) != 0 || n.cb != nil {
		t.Fatalf("close: %v prefs %v cb %v", err, p.p.AdvertiseServices, n.cb != nil)
	}
}
