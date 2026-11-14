package sbxnet

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// query is a guest A (or AAAA) lookup through the proxy's DNS.
func query(t *testing.T, p *Proxy, name string, typ dnsmessage.Type) (dnsmessage.RCode, []netip.Addr) {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name + "."), Type: typ, Class: dnsmessage.ClassINET})
	q, _ := b.Finish()
	resp := p.DNS(q)
	if resp == nil {
		t.Fatalf("DNS %s: no answer", name)
	}
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatal(err)
	}
	if m.ID != 7 || !m.Response {
		t.Fatalf("DNS header %+v", m.Header)
	}
	var out []netip.Addr
	for _, a := range m.Answers {
		if r, ok := a.Body.(*dnsmessage.AResource); ok {
			out = append(out, netip.AddrFrom4(r.A))
		}
	}
	return m.RCode, out
}

func resolve(t *testing.T, p *Proxy, name string) netip.Addr {
	t.Helper()
	code, as := query(t, p, name, dnsmessage.TypeA)
	if code != dnsmessage.RCodeSuccess || len(as) != 1 || !Pool.Contains(as[0]) {
		t.Fatalf("DNS %s: %v %v, want one synthetic address", name, code, as)
	}
	return as[0]
}

// flow is a guest TCP connection to dst served by p (as the vnet does).
func flow(t *testing.T, p *Proxy, dst netip.AddrPort) (net.Conn, bool) {
	t.Helper()
	if !p.Accept(dst) {
		return nil, false
	}
	guest, host := net.Pipe()
	go p.Serve(host, dst)
	t.Cleanup(func() { guest.Close() })
	return guest, true
}

func httpGet(t *testing.T, c net.Conn, host, path string) (*http.Response, string) {
	t.Helper()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", path, host)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

type delivered struct {
	mu   sync.Mutex
	keys []string
	urls []string
	host []string
}

func (d *delivered) fn(ctx context.Context, key string, r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.keys, d.urls, d.host = append(d.keys, key), append(d.urls, r.URL.String()), append(d.host, r.Header.Get("x-from-guest"))
	d.mu.Unlock()
	body := "handled by parent: " + key + " " + r.URL.String()
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

// Every destination ADR-019 names, plus the internet, is refused when
// nothing is intercepted, whatever enableInternet says when the
// allowlist doesn't name it.
func TestDenyAllByDefault(t *testing.T) {
	var dialed []string
	for _, internet := range []bool{false, true} {
		var audits []AuditEvent
		p, err := New(Config{Object: "t/w/Box/a", Internet: internet, HostAddrs: []netip.Addr{netip.MustParseAddr("192.0.2.10")},
			Dial: func(ctx context.Context, n, a string) (net.Conn, error) {
				dialed = append(dialed, a)
				return nil, fmt.Errorf("must not dial")
			},
			Audit: func(e AuditEvent) { audits = append(audits, e) }})
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"example.com", "api.openai.com", "metadata.google.internal", "localhost", "ttest.ts.net"} {
			if code, as := query(t, p, n, dnsmessage.TypeA); code != dnsmessage.RCodeNameError || len(as) != 0 {
				t.Fatalf("internet=%v DNS %s = %v %v, want NXDOMAIN", internet, n, code, as)
			}
		}
		for _, d := range []string{"1.1.1.1:80", "1.1.1.1:443", "8.8.8.8:53", "127.0.0.1:80", "10.0.0.1:80", "172.16.0.1:22",
			"192.168.72.1:3128", "192.168.72.1:3130", "192.168.72.1:22", "192.168.72.2:8080", "169.254.169.254:80",
			"100.100.100.100:80", "100.64.0.7:80", "192.0.2.10:443", "198.18.0.9:80"} {
			if _, ok := flow(t, p, netip.MustParseAddrPort(d)); ok {
				t.Fatalf("internet=%v: flow to %s accepted", internet, d)
			}
		}
		if len(p.Refused()) < 15 {
			t.Fatalf("refusals not recorded: %v", p.Refused())
		}
		if len(audits) == 0 || audits[0].Kind != "deny" {
			t.Fatalf("refusals not audited: %+v", audits)
		}
	}
	if len(dialed) != 0 {
		t.Fatalf("dialled %v", dialed)
	}
}

func TestInterceptHTTP(t *testing.T) {
	var d delivered
	p, _ := New(Config{Object: "o", Deliver: d.fn})
	if _, err := p.Intercept("http", "api.example.com"); err != nil {
		t.Fatal(err)
	}
	if code, _ := query(t, p, "other.example.com", dnsmessage.TypeA); code != dnsmessage.RCodeNameError {
		t.Fatalf("a name nobody intercepts resolved: %v", code)
	}
	a := resolve(t, p, "api.example.com")
	if a2 := resolve(t, p, "API.Example.com"); a2 != a {
		t.Fatalf("names are case-insensitive and stable: %v %v", a, a2)
	}
	if code, as := query(t, p, "api.example.com", dnsmessage.TypeAAAA); code != dnsmessage.RCodeSuccess || len(as) != 0 {
		t.Fatalf("AAAA: %v %v", code, as)
	}
	c, ok := flow(t, p, netip.AddrPortFrom(a, 80))
	if !ok {
		t.Fatal("intercepted flow refused")
	}
	// The Host header claims another host: the parent still sees the name
	// the container connected to.
	resp, body := httpGet(t, c, "evil.example.net", "/v1/x?y=1")
	if resp.StatusCode != 200 || body != "handled by parent: http api.example.com http://api.example.com/v1/x?y=1" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	// Other ports of the name are intercepted too (no port in the addr).
	c2, ok := flow(t, p, netip.AddrPortFrom(a, 8080))
	if !ok {
		t.Fatal("port 8080 refused")
	}
	if _, body := httpGet(t, c2, "api.example.com:8080", "/"); !strings.Contains(body, "http://api.example.com:8080/") {
		t.Fatal(body)
	}
	// TLS to an HTTP-only intercept goes nowhere.
	c3, ok := flow(t, p, netip.AddrPortFrom(a, 443))
	if !ok {
		t.Fatal("accepted flows are decided by protocol")
	}
	tc := tls.Client(c3, &tls.Config{ServerName: "api.example.com", InsecureSkipVerify: true})
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc.Handshake(); err == nil {
		t.Fatal("TLS handshake completed with no HTTPS intercept")
	}
	if len(d.keys) != 2 {
		t.Fatalf("deliveries %v", d.keys)
	}
}

func TestInterceptPortAndIP(t *testing.T) {
	var d delivered
	p, _ := New(Config{Deliver: d.fn})
	p.Intercept("http", "10.0.0.1:8080")
	p.Intercept("http", "*.internal")
	if _, ok := flow(t, p, netip.MustParseAddrPort("10.0.0.1:9090")); ok {
		t.Fatal("other port of an ip:port intercept accepted")
	}
	c, ok := flow(t, p, netip.MustParseAddrPort("10.0.0.1:8080"))
	if !ok {
		t.Fatal("ip:port intercept refused")
	}
	if _, body := httpGet(t, c, "x", "/"); !strings.Contains(body, "http 10.0.0.1:8080 http://10.0.0.1:8080/") {
		t.Fatal(body)
	}
	a := resolve(t, p, "r2.internal")
	if code, _ := query(t, p, "internal", dnsmessage.TypeA); code != dnsmessage.RCodeNameError {
		t.Fatal("*.internal matched the bare name")
	}
	c, _ = flow(t, p, netip.AddrPortFrom(a, 80))
	if _, body := httpGet(t, c, "r2.internal", "/b"); !strings.Contains(body, "http *.internal http://r2.internal/b") {
		t.Fatal(body)
	}
}

func TestInterceptAll(t *testing.T) {
	var d delivered
	p, _ := New(Config{Deliver: d.fn})
	p.Intercept("http", "special.example.com")
	p.Intercept("all", "*")
	a := resolve(t, p, "anything.example.org")
	c, _ := flow(t, p, netip.AddrPortFrom(a, 80))
	if _, body := httpGet(t, c, "anything.example.org", "/"); body != "handled by parent: all * http://anything.example.org/" {
		t.Fatal(body)
	}
	// A specific intercept wins over the catch-all.
	c, _ = flow(t, p, netip.AddrPortFrom(resolve(t, p, "special.example.com"), 80))
	if _, body := httpGet(t, c, "special.example.com", "/"); !strings.HasPrefix(body, "handled by parent: http special.example.com") {
		t.Fatal(body)
	}
	// Literal addresses, even forbidden ones, reach only the parent's handler.
	c, ok := flow(t, p, netip.MustParseAddrPort("169.254.169.254:80"))
	if !ok {
		t.Fatal("intercept-all refused a literal address")
	}
	if _, body := httpGet(t, c, "169.254.169.254", "/latest/meta-data"); body != "handled by parent: all * http://169.254.169.254/latest/meta-data" {
		t.Fatal(body)
	}
}

func TestInterceptHTTPS(t *testing.T) {
	var d delivered
	p, _ := New(Config{Deliver: d.fn})
	if bytes.Contains(p.CACertPEM(), []byte("PRIVATE")) || len(p.CAKeyDER()) == 0 || bytes.Contains(p.CACertPEM(), p.CAKeyDER()[len(p.CAKeyDER())-32:]) {
		t.Fatal("the CA certificate carries key material")
	}
	p.Intercept("https", "secure.example.com")
	a := resolve(t, p, "secure.example.com")
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(p.CACertPEM()) {
		t.Fatal("CA PEM")
	}
	get := func(cfg *tls.Config) (string, error) {
		c, ok := flow(t, p, netip.AddrPortFrom(a, 443))
		if !ok {
			t.Fatal("HTTPS intercept refused")
		}
		tc := tls.Client(c, cfg)
		tc.SetDeadline(time.Now().Add(10 * time.Second))
		if err := tc.Handshake(); err != nil {
			return "", err
		}
		_, body := httpGet(t, tc, "secure.example.com", "/token")
		return body, nil
	}
	body, err := get(&tls.Config{ServerName: "secure.example.com", RootCAs: roots})
	if err != nil || body != "handled by parent: https secure.example.com https://secure.example.com/token" {
		t.Fatalf("trusting the container CA: %q %v", body, err)
	}
	// Without the per-container CA the interception is visible, not silent.
	if _, err := get(&tls.Config{ServerName: "secure.example.com", RootCAs: x509.NewCertPool()}); err == nil {
		t.Fatal("a client that doesn't trust the container CA accepted the intercept certificate")
	}
	// Another container's CA is a different CA.
	q, _ := New(Config{})
	other := x509.NewCertPool()
	other.AppendCertsFromPEM(q.CACertPEM())
	if _, err := get(&tls.Config{ServerName: "secure.example.com", RootCAs: other}); err == nil {
		t.Fatal("another container's CA verified this container's intercept")
	}
	// Plain HTTP to an HTTPS-only intercept is not delivered.
	c, _ := flow(t, p, netip.AddrPortFrom(a, 80))
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: secure.example.com\r\n\r\n")
	if _, err := http.ReadResponse(bufio.NewReader(c), nil); err == nil {
		t.Fatal("plain HTTP delivered by an HTTPS intercept")
	}
	if len(d.keys) != 1 {
		t.Fatalf("deliveries %v", d.keys)
	}
}

// The hermetic internet stand-in: names resolve to documentation or
// private addresses; the dialer connects 203.0.113.7:80 to a local server.
func standIn(t *testing.T) (resolve func(context.Context, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error), dialed *[]string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "internet stand-in: host=%s path=%s", r.Host, r.URL.Path)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	var mu sync.Mutex
	var ds []string
	names := map[string][]netip.Addr{
		"allowed.test":     {netip.MustParseAddr("203.0.113.7")},
		"api.allowed.test": {netip.MustParseAddr("203.0.113.7")},
		"rebind.test":      {netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("10.0.0.5")},
		"meta.test":        {netip.MustParseAddr("169.254.169.254")},
		"tailnet.test":     {netip.MustParseAddr("100.101.102.103")},
		"denied.test":      {netip.MustParseAddr("203.0.113.7")},
	}
	return func(_ context.Context, h string) ([]netip.Addr, error) {
			if as, ok := names[h]; ok {
				return as, nil
			}
			return nil, fmt.Errorf("no such host %s", h)
		}, func(ctx context.Context, n, a string) (net.Conn, error) {
			mu.Lock()
			ds = append(ds, a)
			mu.Unlock()
			if a != "203.0.113.7:80" {
				return nil, fmt.Errorf("stand-in: unreachable %s", a)
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
		}, &ds
}

func TestInternetAllowlist(t *testing.T) {
	res, dial, dialed := standIn(t)
	var audits []AuditEvent
	var amu sync.Mutex
	p, _ := New(Config{Object: "tn/wl/Box/a", Internet: true, Allow: []string{"allowed.test", "*.allowed.test", "rebind.test", "meta.test", "tailnet.test"},
		Resolve: res, Dial: dial, Audit: func(e AuditEvent) { amu.Lock(); audits = append(audits, e); amu.Unlock() }})
	if code, _ := query(t, p, "denied.test", dnsmessage.TypeA); code != dnsmessage.RCodeNameError {
		t.Fatal("a name off the allowlist resolved")
	}
	a := resolve(t, p, "allowed.test")
	c, ok := flow(t, p, netip.AddrPortFrom(a, 80))
	if !ok {
		t.Fatal("allowlisted flow refused")
	}
	if _, body := httpGet(t, c, "allowed.test", "/hello"); body != "internet stand-in: host=allowed.test path=/hello" {
		t.Fatal(body)
	}
	c, _ = flow(t, p, netip.AddrPortFrom(resolve(t, p, "api.allowed.test"), 80))
	if _, body := httpGet(t, c, "api.allowed.test", "/"); !strings.HasPrefix(body, "internet stand-in") {
		t.Fatal(body)
	}
	// Allowlisted names whose addresses ADR-019 forbids are never dialled.
	for _, n := range []string{"rebind.test", "meta.test", "tailnet.test"} {
		c, ok := flow(t, p, netip.AddrPortFrom(resolve(t, p, n), 80))
		if !ok {
			t.Fatalf("%s: refused at accept (want: at dial, after resolving)", n)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", n)
		if _, err := http.ReadResponse(bufio.NewReader(c), nil); err == nil {
			t.Fatalf("%s reached something", n)
		}
	}
	// Literal addresses must be allowlisted themselves.
	if _, ok := flow(t, p, netip.MustParseAddrPort("203.0.113.7:80")); ok {
		t.Fatal("a literal address off the allowlist was accepted")
	}
	time.Sleep(50 * time.Millisecond)
	for _, d := range *dialed {
		if d != "203.0.113.7:80" {
			t.Fatalf("dialled %s", d)
		}
	}
	amu.Lock()
	defer amu.Unlock()
	var egress, forbidden int
	for _, e := range audits {
		if e.Kind == "egress" && (e.Dst == "allowed.test:80" || e.Dst == "api.allowed.test:80") {
			egress++
		}
		if e.Kind == "deny" && e.Code == "forbidden_destination" {
			forbidden++
		}
	}
	if egress != 2 || forbidden != 3 {
		t.Fatalf("audit: %+v", audits)
	}
	// enableInternet: false with the same allowlist: nothing.
	q, _ := New(Config{Internet: false, Allow: []string{"allowed.test"}, Resolve: res, Dial: dial})
	if code, _ := query(t, q, "allowed.test", dnsmessage.TypeA); code != dnsmessage.RCodeNameError {
		t.Fatal("enableInternet:false resolved an allowlisted name")
	}
}

func TestInterceptLimit(t *testing.T) {
	p, _ := New(Config{})
	for i := 0; i < MaxIntercepts; i++ {
		if _, err := p.Intercept("http", fmt.Sprintf("h%d.example.com", i)); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := p.Intercept("http", "one-more.example.com"); err == nil {
		t.Fatal("intercept 129 accepted")
	}
	// Re-registering (the Container class does on every wake) is not a new one.
	if _, err := p.Intercept("http", "h7.example.com"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][2]string{{"ftp", "x"}, {"http", ""}, {"http", "a:0"}, {"http", "a:99999"}, {"https", strings.Repeat("a", 300)}} {
		if _, err := p.Intercept(bad[0], bad[1]); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
}

func TestBlockedRanges(t *testing.T) {
	p, _ := New(Config{HostAddrs: []netip.Addr{netip.MustParseAddr("203.0.113.99")}})
	for _, a := range []string{"127.0.0.1", "0.0.0.0", "10.1.2.3", "172.31.0.1", "192.168.72.1", "169.254.169.254", "100.64.0.1",
		"100.127.255.254", "100.100.100.100", "198.18.0.1", "224.0.0.1", "255.255.255.255", "203.0.113.99", "::1", "fe80::1",
		"fd7a:115c:a1e0::1", "fc00::1", "64:ff9b::a00:1", "2002:a00:1::1", "::ffff:10.0.0.1", "240.0.0.1"} {
		if p.blocked(netip.MustParseAddr(a)) == "" {
			t.Errorf("%s is not blocked", a)
		}
	}
	for _, a := range []string{"1.1.1.1", "8.8.8.8", "203.0.113.7", "2606:4700::1111"} {
		if why := p.blocked(netip.MustParseAddr(a)); why != "" {
			t.Errorf("%s blocked: %s", a, why)
		}
	}
}

func TestCloseAbortsFlows(t *testing.T) {
	block := make(chan struct{})
	p, _ := New(Config{Deliver: func(ctx context.Context, key string, r *http.Request) (*http.Response, error) {
		<-block
		return nil, ctx.Err()
	}})
	defer close(block)
	p.Intercept("http", "slow.example.com")
	c, _ := flow(t, p, netip.AddrPortFrom(resolve(t, p, "slow.example.com"), 80))
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: slow.example.com\r\n\r\n")
	errc := make(chan error, 1)
	go func() {
		_, err := http.ReadResponse(bufio.NewReader(c), nil)
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	st := time.Now()
	p.Close()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("response after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("flow survived Close")
	}
	if d := time.Since(st); d > time.Second {
		t.Fatalf("Close took %v", d)
	}
	if p.Accept(netip.AddrPortFrom(resolve(t, p, "slow.example.com"), 80)) {
		t.Fatal("flow accepted after Close")
	}
}
