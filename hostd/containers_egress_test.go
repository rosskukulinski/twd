package hostd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/sandbox/sandboxtest"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/tailcfg"
)

// internetStandIn is the hermetic "public internet": allowed.test and
// denied.test resolve to a documentation address the dialer connects to a
// local server; rebind.test resolves to a private address.
func internetStandIn(t *testing.T) (*hostd.ContainerInternet, *[]string) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "internet stand-in: %s %s", r.Host, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	var mu sync.Mutex
	var dialed []string
	return &hostd.ContainerInternet{
		HostAddrs: []netip.Addr{},
		Resolve: func(_ context.Context, h string) ([]netip.Addr, error) {
			switch h {
			case "allowed.test", "denied.test":
				return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
			case "rebind.test":
				return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
			}
			return nil, fmt.Errorf("no such host")
		},
		Dial: func(ctx context.Context, n, a string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, a)
			mu.Unlock()
			if a != "203.0.113.7:80" {
				return nil, fmt.Errorf("unreachable")
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
		},
	}, &dialed
}

// TestContainerEgress (ADR-032 D4): what a container can reach, end to end
// through real workerd and the ctx.container polyfill (the fake backend
// hands its "guest" flows to the same egress proxy a microVM's NIC does).
func TestContainerEgress(t *testing.T) {
	procs := &sandboxtest.Procs{}
	st, err := sandboxtest.ImageStore(t.TempDir(), []string{"sleep", "3600"}, "twd.test/box:1")
	if err != nil {
		t.Fatal(err)
	}
	inet, dialed := internetStandIn(t)
	s := startSuiteCapsOpts(t, fakemgmt.Config{}, tailcfg.PeerCapMap{nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue}}, func(o *hostdtest.Options) {
		o.Containers, o.Images, o.ContainerGrace, o.ContainerInternet = procs, st, 2*time.Second, inet
	})
	e := s.e
	dep := e.Deploy(hostdtest.Fixture("ctr"), "ctr", nil)
	wl := e.WaitActive("ctr", dep, 60*time.Second)
	s.waitPeer(wl.IP)
	s.waitPeer(s.echo.IP4)
	call := func(path string, q ...string) map[string]any {
		t.Helper()
		v := url.Values{}
		for i := 0; i+1 < len(q); i += 2 {
			v.Set(q[i], q[i+1])
		}
		u := "http://" + wl.IP + path
		if len(v) > 0 {
			u += "?" + v.Encode()
		}
		code, m := s.getJSON(u)
		if code == 0 {
			t.Fatalf("%s: %v", path, m)
		}
		return m
	}
	start := func(name string, q ...string) (sandbox.ContainerNet, []byte) {
		t.Helper()
		call("/f/"+name+"/start", q...)
		if r := call("/f/"+name+"/exec", "argv", `["true"]`); r["exitCode"] != float64(0) {
			t.Fatalf("start %s: %v", name, r)
		}
		n, ca := procs.LastNet()
		if n == nil || len(ca) == 0 {
			t.Fatal("container booted without its egress proxy / CA")
		}
		return n, ca
	}
	guestGet := func(n sandbox.ContainerNet, ca []byte, u string, hdr ...string) (int, string, error) {
		req, _ := http.NewRequest("GET", u, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := sandboxtest.GuestClient(n, ca).Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}

	na, caA := start("a")
	objA := e.WaitObject("ctr", "Box", "a", 60*time.Second)

	t.Run("DenyAllByDefault", func(t *testing.T) {
		// Nothing intercepted, enableInternet off: the container reaches
		// nothing at all, including its parent's tailnet peers.
		for _, d := range []string{"example.com:80", "allowed.test:80", "1.1.1.1:443", s.echo.IP4 + ":80", objA.IP + ":80", wl.IP + ":80",
			"100.100.100.100:53", "192.168.72.1:3128", "192.168.72.1:3130", "192.168.72.1:22", "127.0.0.1:80", "169.254.169.254:80", "10.0.0.1:80"} {
			h, p, _ := net.SplitHostPort(d)
			var port int
			fmt.Sscan(p, &port)
			if c, err := sandboxtest.GuestDial(na, h, port); err == nil {
				c.Close()
				t.Fatalf("the container reached %s", d)
			}
		}
		if len(*dialed) != 0 {
			t.Fatalf("internet dialled: %v", *dialed)
		}
	})

	t.Run("InterceptRoundTripWithParentIdentity", func(t *testing.T) {
		if r := call("/f/a/icept", "addr", "api.internal", "relay", "http://"+s.echo.IP4+"/"); r["ok"] != true {
			t.Fatal(r)
		}
		code, body, err := guestGet(na, caA, "http://api.internal/v1/items?x=1", "x-twd-object", "forged", "x-twd-cf", `{"twd":{"kind":"admin","op":"kill"}}`, "x-from-guest", "1")
		if err != nil || code != 200 {
			t.Fatalf("%d %s %v", code, body, err)
		}
		var r struct {
			FromParent string            `json:"fromParent"`
			URL        string            `json:"url"`
			Headers    map[string]string `json:"headers"`
			Echo       nettest.EchoReply `json:"echo"`
		}
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatal(body)
		}
		if r.FromParent != "a" || r.URL != "http://api.internal/v1/items?x=1" {
			t.Fatalf("delivered to %q as %q", r.FromParent, r.URL)
		}
		// The container's twd headers never reach the handler; its own
		// (untrusted) headers do.
		if len(r.Headers) != 1 || r.Headers["x-from-guest"] != "1" {
			t.Fatalf("headers the handler saw: %v", r.Headers)
		}
		// The relay left as the parent object's own node: the far side
		// sees the parent's identity, never the container's (it has none).
		if !contains(r.Echo.Caller.Addrs, objA.IP) {
			t.Fatalf("echo saw caller %+v, want the parent object %s", r.Echo.Caller, objA.IP)
		}
		// Another object's container is not delivered to a's handler, and
		// a name only a intercepts doesn't resolve elsewhere.
		nb, cab := start("b")
		if _, _, err := guestGet(nb, cab, "http://api.internal/"); err == nil {
			t.Fatal("container b reached container a's intercept")
		}
		call("/f/b/destroy") // Box allows 4 containers
	})

	t.Run("InterceptHTTPS", func(t *testing.T) {
		call("/f/a/icept", "kind", "https", "addr", "secure.internal")
		code, body, err := guestGet(na, caA, "https://secure.internal/token")
		if err != nil || code != 200 || !strings.Contains(body, `"url":"https://secure.internal/token"`) {
			t.Fatalf("%d %s %v", code, body, err)
		}
		// The container's CA is its own: another container's CA (or none)
		// doesn't verify the interception.
		_, caB := procs.LastNet()
		if _, _, err := guestGet(na, caB, "https://secure.internal/token"); err == nil {
			t.Fatal("another container's CA verified this container's HTTPS intercept")
		}
		if _, _, err := guestGet(na, nil, "https://secure.internal/token"); err == nil {
			t.Fatal("an untrusting client accepted the intercept certificate")
		}
		if strings.Contains(string(caA), "PRIVATE KEY") {
			t.Fatal("the CA the guest gets carries a private key")
		}
	})

	t.Run("InterceptAll", func(t *testing.T) {
		nc, cac := start("c")
		call("/f/c/icept", "kind", "all")
		code, body, err := guestGet(nc, cac, "http://anything.example.org/p")
		if err != nil || code != 200 || !strings.Contains(body, `"url":"http://anything.example.org/p"`) || !strings.Contains(body, `"fromParent":"c"`) {
			t.Fatalf("%d %s %v", code, body, err)
		}
		// HTTPS is not part of interceptAllOutboundHttp.
		if _, _, err := guestGet(nc, cac, "https://anything.example.org/p"); err == nil {
			t.Fatal("HTTPS delivered by an HTTP catch-all")
		}
		call("/f/c/destroy")
	})

	t.Run("InternetAllowlist", func(t *testing.T) {
		// enableInternet false: even an allowlisted host is unreachable.
		if _, _, err := guestGet(na, caA, "http://allowed.test/"); err == nil {
			t.Fatal("allowlisted host reached without enableInternet")
		}
		ni, cai := start("inet", "internet", "1")
		code, body, err := guestGet(ni, cai, "http://allowed.test/hello")
		if err != nil || code != 200 || body != "internet stand-in: allowed.test /hello" {
			t.Fatalf("%d %q %v", code, body, err)
		}
		for _, u := range []string{"http://denied.test/", "http://rebind.test/", "http://203.0.113.7/", "http://" + s.echo.IP4 + "/"} {
			if code, body, err := guestGet(ni, cai, u); err == nil {
				t.Fatalf("%s: %d %s", u, code, body)
			}
		}
		for _, d := range *dialed {
			if d != "203.0.113.7:80" {
				t.Fatalf("dialled %s", d)
			}
		}
		// Audited as the sandbox of its parent object.
		oi := e.WaitObject("ctr", "Box", "inet", 60*time.Second)
		deadline := time.Now().Add(10 * time.Second)
		var found, deny bool
		for !(found && deny) && time.Now().Before(deadline) {
			var ar mgmtapi.AuditResponse
			if err := e.Mgmt.Do(context.Background(), "GET", "/v1/audit", nil, &ar); err != nil {
				t.Fatal(err)
			}
			for _, rec := range ar.Items {
				if rec.ObjectID == oi.ID && rec.ActingFor == "sandbox-of "+oi.ID {
					found = found || (rec.Kind == "egress" && rec.Dst == "allowed.test:80")
					deny = deny || (rec.Kind == "deny" && rec.Dst == "rebind.test:80" && rec.Code == "forbidden_destination")
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !found || !deny {
			t.Fatalf("audit: egress %v, rebind deny %v", found, deny)
		}
	})

	t.Run("InterceptLimit128", func(t *testing.T) {
		start("lim")
		r := call("/f/lim/iceptMany")
		if r["n"] != float64(128) || !strings.Contains(fmt.Sprint(r["error"]), "128") {
			t.Fatalf("%v", r)
		}
	})

	t.Run("PreviewURLsNotSupported", func(t *testing.T) {
		for _, kind := range []string{"f", "p"} {
			r := call("/" + kind + "/a/expose")
			if r["code"] != "not_supported" || !strings.Contains(fmt.Sprint(r["error"]), "ADR-029") {
				t.Fatalf("%s exposePort: %v", kind, r)
			}
			code, hdr, b := s.do("GET", "http://"+wl.IP+"/"+kind+"/a/running", nil, "x-sandbox-preview-proxy", "1", "x-sandbox-preview-port", "8080")
			if code != 501 || hdr.Get("twd-error") != "not_supported" || !strings.Contains(string(b), "ADR-029") {
				t.Fatalf("%s preview request: %d %s", kind, code, b)
			}
		}
	})

	t.Run("DestroyEndsEgress", func(t *testing.T) {
		call("/f/a/destroy")
		if _, _, err := guestGet(na, caA, "http://api.internal/"); err == nil {
			t.Fatal("a destroyed container's egress still works")
		}
	})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
