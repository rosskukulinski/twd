// Package sbxnet is the sandbox egress proxy (ADR-032 D4): the host side
// of a container guest's NIC. It is deliberately separate from netd's DO
// egress path: it has no tsnet server, no tailnet identity and no MAC key,
// so nothing a sandbox sends can ever leave as its parent (or any) node.
//
// A container's flows end here and nowhere else:
//
//   - DNS (gateway:53/udp) answers only names the container may reach: an
//     intercepted host, or (enableInternet) a host on the class's
//     egress.internet allowlist. Each such name gets a synthetic address
//     from 198.18.0.0/15, so the proxy knows the name of every flow without
//     trusting a Host header or SNI. Everything else is NXDOMAIN.
//   - TCP: a flow the policy can't serve is reset before its handshake
//     completes. An accepted flow is either
//     1. intercepted (interceptOutboundHttp / interceptAllOutboundHttp /
//     interceptOutboundHttps): its HTTP requests are delivered to the
//     parent Durable Object's registered handler (Config.Deliver), which
//     decides what, if anything, to relay under its own identity, policy
//     and lim. HTTPS is terminated with a per-container ephemeral CA whose
//     key never leaves this process (only its certificate goes into the
//     guest, at CACertPath and into the trust store); or
//     2. allowlisted internet: spliced as raw TCP to the allowlisted name
//     (resolved on the host; every address must be public, ADR-019), and
//     audited as "sandbox-of <object>".
//   - ADR-019 blocks always apply to the internet path: loopback, private
//     and link-local (metadata) ranges, the 100.64/10 tailnet range, the
//     host's own addresses, the gateway, other guests and the synthetic
//     pool itself are never dialled.
//
// UDP other than DNS, ICMP and IPv6 are not served at all.
package sbxnet

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxIntercepts is the per-container limit on intercept registrations
// (Cloudflare's limit).
const MaxIntercepts = 128

// CACertPath is where the container finds the HTTPS-intercept CA
// certificate (Cloudflare's path).
const CACertPath = "/etc/cloudflare/certs/cloudflare-containers-ca.crt"

// Pool is the synthetic address range DNS answers from (RFC 2544
// benchmarking, never routed).
var Pool = netip.MustParsePrefix("198.18.0.0/15")

const maxNames = 1 << 16

// AuditEvent is one audit-worthy decision (hostd signs and ships it).
type AuditEvent struct {
	Kind   string // "egress" (allowed internet flow) | "deny"
	Dst    string // host:port
	Code   string // deny code (forbidden_destination, not_allowlisted, ...)
	Detail string
}

// Config configures one container's proxy.
type Config struct {
	// Object names the parent (audit "sandbox-of <Object>", logs).
	Object string
	// Internet is start({enableInternet}); Allow is the class's
	// egress.internet allowlist (host names, "*.suffix", IPs or CIDRs).
	// Both are needed for any internet flow.
	Internet bool
	Allow    []string
	// Deliver hands one intercepted request to the parent DO's handler
	// registered under key ("http <addr>", "https <addr>" or "all *").
	// req.URL is the destination the container connected to.
	Deliver func(ctx context.Context, key string, req *http.Request) (*http.Response, error)
	// Dial and Resolve are the internet path (nil: the host network and
	// resolver). Tests substitute a hermetic stand-in.
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// HostAddrs are this host's own addresses (never dialled). Nil: the
	// host's interface addresses.
	HostAddrs []netip.Addr
	// Activity is called as intercepted requests and internet flows pass
	// (the container's inactivity timer).
	Activity func() (done func())
	Audit    func(AuditEvent)
	Log      *slog.Logger
}

type intercept struct {
	kind string // http | https | all
	host string // lowercase; "*" = any; "*.x" = suffix
	port int    // 0 = any
	key  string
}

// Proxy is one container's egress policy and its flows.
type Proxy struct {
	cfg Config
	ca  *ca
	log *slog.Logger

	mu         sync.Mutex
	intercepts []intercept
	names      map[string]netip.Addr
	addrs      map[netip.Addr]string
	next       uint32
	conns      map[net.Conn]struct{}
	refused    []string
	denies     int
	audited    map[string]bool
	closed     bool
	hostAddrs  map[netip.Addr]bool
	allowHosts []string
	allowNets  []netip.Prefix
}

// New makes a container's proxy and its ephemeral CA.
func New(cfg Config) (*Proxy, error) {
	c, err := newCA(cfg.Object)
	if err != nil {
		return nil, err
	}
	p := &Proxy{cfg: cfg, ca: c, names: map[string]netip.Addr{}, addrs: map[netip.Addr]string{}, conns: map[net.Conn]struct{}{},
		audited: map[string]bool{}, hostAddrs: map[netip.Addr]bool{}}
	p.log = cfg.Log
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	if p.cfg.Dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		p.cfg.Dial = d.DialContext
	}
	if p.cfg.Resolve == nil {
		p.cfg.Resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	hosts := cfg.HostAddrs
	if hosts == nil {
		hosts = interfaceAddrs()
	}
	for _, a := range hosts {
		p.hostAddrs[a.Unmap()] = true
	}
	for _, a := range cfg.Allow {
		a = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(a)), ".")
		if a == "" {
			continue
		}
		if pf, err := netip.ParsePrefix(a); err == nil {
			p.allowNets = append(p.allowNets, pf.Masked())
		} else if ip, err := netip.ParseAddr(a); err == nil {
			p.allowNets = append(p.allowNets, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
		} else {
			p.allowHosts = append(p.allowHosts, a)
		}
	}
	return p, nil
}

// CACertPEM is the HTTPS-intercept CA certificate (public; the guest gets it).
func (p *Proxy) CACertPEM() []byte { return p.ca.certPEM }

// CAKeyDER is the CA's private key (PKCS#8 DER). It exists for the no-keys
// scan's needle set only: it never leaves this process otherwise.
func (p *Proxy) CAKeyDER() []byte { return p.ca.keyDER }

// Intercept registers (or replaces) an interception: kind "http" with an
// addr of host, host:port, ip or ip:port ("*." prefixes match subdomains);
// "https" likewise or "*"; "all" (interceptAllOutboundHttp). It returns the
// key the parent's handler is registered under.
func (p *Proxy) Intercept(kind, addr string) (string, error) {
	ic := intercept{kind: kind, key: kind + " " + addr}
	switch kind {
	case "all":
		ic.host = "*"
	case "http", "https":
		h, port, err := parseAddr(addr)
		if err != nil {
			return "", err
		}
		ic.host, ic.port = h, port
	default:
		return "", fmt.Errorf("intercept kind %q (want http, https or all)", kind)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, x := range p.intercepts {
		if x.key == ic.key {
			p.intercepts[i] = ic
			return ic.key, nil
		}
	}
	if len(p.intercepts) >= MaxIntercepts {
		return "", fmt.Errorf("a container may have at most %d intercepts", MaxIntercepts)
	}
	p.intercepts = append(p.intercepts, ic)
	return ic.key, nil
}

// Intercepts lists the registered keys.
func (p *Proxy) Intercepts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.intercepts))
	for i, x := range p.intercepts {
		out[i] = x.key
	}
	return out
}

func parseAddr(addr string) (string, int, error) {
	a := strings.TrimSpace(addr)
	if a == "" || len(a) > 255 {
		return "", 0, errors.New("intercept address is empty or too long")
	}
	if a == "*" {
		return "*", 0, nil
	}
	for _, pre := range []string{"http://", "https://"} {
		a = strings.TrimPrefix(a, pre)
	}
	a = strings.TrimSuffix(a, "/")
	host, port := a, 0
	if h, ps, err := net.SplitHostPort(a); err == nil {
		n, err := strconv.Atoi(ps)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("intercept address %q: bad port", addr)
		}
		host, port = h, n
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" || strings.ContainsAny(host, "/ \t") {
		return "", 0, fmt.Errorf("intercept address %q", addr)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
	}
	return host, port, nil
}

func hostMatch(pattern, name string) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasPrefix(pattern, "*."):
		return strings.HasSuffix(name, pattern[1:]) && len(name) > len(pattern)-1
	}
	return pattern == name
}

// interceptKey picks the registration for a flow: a host-specific one of
// kind first, then (http only) the catch-all. port 0 matches any.
func (p *Proxy) interceptKey(kind, name string, port int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	best, bestScore := "", -1
	for _, x := range p.intercepts {
		if x.kind != kind || (x.port != 0 && port != 0 && x.port != port) || !hostMatch(x.host, name) {
			continue
		}
		score := 1
		if x.host == name {
			score = 3
		} else if x.host != "*" {
			score = 2
		}
		if x.port != 0 {
			score += 4
		}
		if score > bestScore {
			best, bestScore = x.key, score
		}
	}
	if best == "" && kind == "http" {
		for _, x := range p.intercepts {
			if x.kind == "all" {
				return x.key
			}
		}
	}
	return best
}

// anyIntercept: could some intercept serve a flow to name:port?
func (p *Proxy) anyIntercept(name string, port int) bool {
	return p.interceptKey("http", name, port) != "" || p.interceptKey("https", name, port) != ""
}

func (p *Proxy) allowlisted(name string) bool {
	if ip, err := netip.ParseAddr(name); err == nil {
		for _, n := range p.allowNets {
			if n.Contains(ip.Unmap()) {
				return true
			}
		}
		return false
	}
	for _, h := range p.allowHosts {
		if hostMatch(h, name) {
			return true
		}
	}
	return false
}

// internetName: may the container reach name on the internet at all?
func (p *Proxy) internetName(name string) bool {
	return p.cfg.Internet && p.allowlisted(name)
}

// Lookup is the DNS policy: the synthetic address of name, if the
// container may reach it.
func (p *Proxy) Lookup(name string) (netip.Addr, bool) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if name == "" || strings.ContainsAny(name, " /\\") {
		return netip.Addr{}, false
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return netip.Addr{}, false
	}
	if !p.anyIntercept(name, 0) && !p.internetName(name) {
		return netip.Addr{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.names[name]; ok {
		return a, true
	}
	if len(p.names) >= maxNames {
		return netip.Addr{}, false
	}
	p.next++
	base := Pool.Addr().As4()
	n := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	n += p.next
	a := netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	p.names[name], p.addrs[a] = a, name
	return a, true
}

// flowName is the name the container connected to: the DNS name behind a
// synthetic address, else the literal address. ok is false for a
// synthetic address DNS never handed out.
func (p *Proxy) flowName(a netip.Addr) (name string, synthetic, ok bool) {
	a = a.Unmap()
	if Pool.Contains(a) {
		p.mu.Lock()
		name, ok = p.addrs[a]
		p.mu.Unlock()
		return name, true, ok
	}
	return a.String(), false, true
}

// Accept decides a guest TCP flow before its handshake completes: false
// resets it (and records the refusal).
func (p *Proxy) Accept(dst netip.AddrPort) bool {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return false
	}
	name, synthetic, ok := p.flowName(dst.Addr())
	port := int(dst.Port())
	switch {
	case !ok:
		p.deny(dst.String(), "unknown_destination", "a synthetic address DNS did not hand out")
		return false
	case p.anyIntercept(name, port):
		return true
	case !p.internetName(name):
		code := "not_allowlisted"
		if !p.cfg.Internet {
			code = "internet_disabled"
		}
		p.deny(net.JoinHostPort(name, strconv.Itoa(port)), code, "")
		return false
	case !synthetic:
		if why := p.blocked(dst.Addr()); why != "" {
			p.deny(dst.String(), "forbidden_destination", why)
			return false
		}
	}
	return true
}

func (p *Proxy) deny(dst, code, detail string) {
	p.mu.Lock()
	if len(p.refused) < 1024 {
		p.refused = append(p.refused, dst+" "+code)
	}
	p.denies++
	n := p.denies
	p.mu.Unlock()
	p.log.Debug("sandbox egress refused", "dst", dst, "code", code, "detail", detail)
	if p.cfg.Audit != nil && n <= 32 { // a storm of refusals is logged, not audited one by one
		p.cfg.Audit(AuditEvent{Kind: "deny", Dst: dst, Code: code, Detail: detail})
	}
}

// Refused lists refused flows ("dst code"), for tests and diagnostics.
func (p *Proxy) Refused() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.refused...)
}

// Close aborts every flow; later flows are refused.
func (p *Proxy) Close() {
	p.mu.Lock()
	p.closed = true
	cs := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		cs = append(cs, c)
	}
	p.mu.Unlock()
	for _, c := range cs {
		c.Close()
	}
}

func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

// Serve owns an accepted guest flow.
func (p *Proxy) Serve(c net.Conn, dst netip.AddrPort) {
	if !p.track(c) {
		c.Close()
		return
	}
	defer func() {
		p.untrack(c)
		c.Close()
	}()
	name, _, ok := p.flowName(dst.Addr())
	if !ok {
		return
	}
	port := int(dst.Port())
	br := bufio.NewReaderSize(c, 16<<10)
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	pc := &prefixConn{Conn: c, r: br}
	if first[0] == 0x16 { // a TLS handshake record
		p.serveTLS(pc, name, port)
		return
	}
	if key := p.interceptKey("http", name, port); key != "" {
		p.serveHTTP(pc, "http", name, port, key)
		return
	}
	if p.internetName(name) {
		p.splice(pc, name, port)
		return
	}
	p.deny(net.JoinHostPort(name, strconv.Itoa(port)), "not_intercepted", "plain HTTP with only an HTTPS intercept")
}

func (p *Proxy) serveTLS(c *prefixConn, name string, port int) {
	hello, replay, err := sniff(c)
	if err != nil {
		return
	}
	pc := &prefixConn{Conn: c.Conn, r: io.MultiReader(bytes.NewReader(replay), c.r)}
	sni := strings.TrimSuffix(strings.ToLower(hello), ".")
	key := p.interceptKey("https", name, port)
	if key == "" && sni != "" && name != sni {
		if _, err := netip.ParseAddr(name); err == nil { // an IP literal: the SNI names the host
			if key = p.interceptKey("https", sni, port); key != "" {
				name = sni
			}
		}
	}
	if key != "" {
		certName := name
		if sni != "" {
			certName = sni
		}
		tc := tls.Server(pc, &tls.Config{
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return p.ca.leaf(certName) },
			NextProtos:     []string{"http/1.1"},
			MinVersion:     tls.VersionTLS12,
		})
		tc.SetDeadline(time.Now().Add(30 * time.Second))
		if err := tc.Handshake(); err != nil {
			p.log.Debug("sandbox HTTPS intercept handshake failed", "name", certName, "err", err)
			return
		}
		tc.SetDeadline(time.Time{})
		p.serveHTTP(tc, "https", name, port, key)
		return
	}
	if p.internetName(name) {
		p.splice(pc, name, port)
		return
	}
	p.deny(net.JoinHostPort(name, strconv.Itoa(port)), "not_intercepted", "TLS with no HTTPS intercept and no internet")
}

// serveHTTP serves HTTP/1.1 on c, delivering every request to the parent.
func (p *Proxy) serveHTTP(c net.Conn, scheme, name string, port int, key string) {
	l := newOneConn(c)
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.deliver(w, r, scheme, name, port, key) }),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(p.log.Handler(), slog.LevelDebug),
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateClosed || s == http.StateHijacked {
				l.Close()
			}
		},
	}
	srv.Serve(l)
	<-l.done
}

// hopHeaders are never forwarded either way.
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *Proxy) deliver(w http.ResponseWriter, r *http.Request, scheme, name string, port int, key string) {
	if p.cfg.Activity != nil {
		defer p.cfg.Activity()()
	}
	host := name
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if (scheme == "http" && port != 80) || (scheme == "https" && port != 443) {
		host += ":" + strconv.Itoa(port)
	}
	// The URL names what the container connected to (its DNS name or
	// literal address), never only what its Host header claims.
	// (Absolute-form requests keep only their path and query.)
	u := scheme + "://" + host + r.URL.RequestURI()
	out, err := http.NewRequestWithContext(r.Context(), r.Method, u, r.Body)
	if err != nil {
		http.Error(w, "twd: bad request", http.StatusBadRequest)
		return
	}
	out.Header = r.Header.Clone()
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	out.ContentLength = r.ContentLength
	if p.cfg.Deliver == nil {
		http.Error(w, "twd: no intercept handler", http.StatusBadGateway)
		return
	}
	resp, err := p.cfg.Deliver(r.Context(), key, out)
	if err != nil {
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		w.Header().Set("twd-error", "intercept_failed")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "twd: the intercept handler (%s) failed: %v\n", key, err)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	for _, h := range hopHeaders {
		w.Header().Del(h)
	}
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// splice connects the flow to name:port on the internet.
func (p *Proxy) splice(c net.Conn, name string, port int) {
	dst := net.JoinHostPort(name, strconv.Itoa(port))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	up, err := p.dialInternet(ctx, name, port)
	cancel()
	if err != nil {
		var re *refusal
		if errors.As(err, &re) {
			p.deny(dst, re.code, re.detail)
		} else {
			p.log.Info("sandbox internet dial failed", "dst", dst, "err", err)
		}
		return
	}
	if !p.track(up) {
		up.Close()
		return
	}
	defer func() {
		p.untrack(up)
		up.Close()
	}()
	p.auditEgress(dst)
	if p.cfg.Activity != nil {
		defer p.cfg.Activity()()
	}
	done := make(chan struct{}, 2)
	cp := func(dstc, srcc net.Conn, src io.Reader) {
		io.Copy(dstc, src)
		if cw, ok := dstc.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else if pc, ok := dstc.(*prefixConn); ok {
			if cw, ok := pc.Conn.(interface{ CloseWrite() error }); ok {
				cw.CloseWrite()
			}
		}
		done <- struct{}{}
	}
	go cp(up, c, c)
	go cp(c, up, up)
	<-done
	<-done
}

func (p *Proxy) auditEgress(dst string) {
	p.mu.Lock()
	first := !p.audited[dst]
	if len(p.audited) < 4096 {
		p.audited[dst] = true
	}
	p.mu.Unlock()
	p.log.Info("sandbox internet egress", "sandbox_of", p.cfg.Object, "dst", dst)
	if first && p.cfg.Audit != nil { // the first flow per destination is audited
		p.cfg.Audit(AuditEvent{Kind: "egress", Dst: dst})
	}
}

type refusal struct{ code, detail string }

func (r *refusal) Error() string { return r.code + ": " + r.detail }

// dialInternet resolves name on the host and dials it, refusing any
// destination ADR-019 forbids. A name with any forbidden address is
// refused outright (no DNS-rebinding past the check).
func (p *Proxy) dialInternet(ctx context.Context, name string, port int) (net.Conn, error) {
	if !p.internetName(name) {
		return nil, &refusal{"not_allowlisted", name + " is not on egress.internet"}
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(name); err == nil {
		addrs = []netip.Addr{ip.Unmap()}
	} else {
		as, err := p.cfg.Resolve(ctx, name)
		if err != nil {
			return nil, &refusal{"dns_failed", err.Error()}
		}
		addrs = as
	}
	if len(addrs) == 0 {
		return nil, &refusal{"dns_failed", "no addresses for " + name}
	}
	for _, a := range addrs {
		if why := p.blocked(a); why != "" {
			return nil, &refusal{"forbidden_destination", name + " resolves to " + a.String() + ": " + why}
		}
	}
	var last error
	for _, a := range addrs {
		c, err := p.cfg.Dial(ctx, "tcp", netip.AddrPortFrom(a.Unmap(), uint16(port)).String())
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

var blockedNets = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT: the tailnet range (ADR-019)
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), // the synthetic pool
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"), // the tailnet's IPv6 range
	netip.MustParsePrefix("64:ff9b::/96"),        // NAT64 could reach any IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"), // 6to4 embeds IPv4
	netip.MustParsePrefix("2001::/32"), // Teredo
}

// blocked returns why a may not be dialled ("" = a public address).
func (p *Proxy) blocked(a netip.Addr) string {
	a = a.Unmap()
	switch {
	case !a.IsValid():
		return "invalid address"
	case a.IsLoopback(), a.IsUnspecified():
		return "loopback/unspecified"
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(), a.IsMulticast(), a.IsInterfaceLocalMulticast():
		return "link-local/multicast (metadata services)"
	case a.IsPrivate():
		return "a private network"
	case !a.IsGlobalUnicast():
		return "not a global unicast address"
	}
	for _, n := range blockedNets {
		if n.Contains(a) {
			return "a reserved, tailnet or translated range (" + n.String() + ")"
		}
	}
	if a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return "broadcast"
	}
	p.mu.Lock()
	host := p.hostAddrs[a]
	p.mu.Unlock()
	if host {
		return "an address of this host"
	}
	return ""
}

func interfaceAddrs() []netip.Addr {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range as {
		if pn, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(pn.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out
}

// prefixConn reads from r (buffered or replayed bytes first).
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func (c *prefixConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// oneConn is a net.Listener that yields one connection.
type oneConn struct {
	c    net.Conn
	once sync.Once
	ch   chan net.Conn
	done chan struct{}
	cl   sync.Once
}

func newOneConn(c net.Conn) *oneConn {
	l := &oneConn{c: c, ch: make(chan net.Conn, 1), done: make(chan struct{})}
	l.ch <- c
	return l
}

func (l *oneConn) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *oneConn) Close() error {
	l.cl.Do(func() { close(l.done) })
	return nil
}

func (l *oneConn) Addr() net.Addr { return l.c.LocalAddr() }

// sniff reads a TLS ClientHello from c and returns its SNI and the bytes
// it consumed (to replay).
func sniff(c net.Conn) (string, []byte, error) {
	rec := &recordConn{Conn: c}
	var sni string
	var got bool
	errSniffed := errors.New("sniffed")
	tc := tls.Server(rec, &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		sni, got = h.ServerName, true
		return nil, errSniffed
	}})
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	tc.Handshake()
	c.SetReadDeadline(time.Time{})
	if !got {
		return "", nil, errors.New("no TLS ClientHello")
	}
	return sni, rec.buf.Bytes(), nil
}

// recordConn records reads and swallows writes (the sniffing handshake's
// alert must not reach the guest).
type recordConn struct {
	net.Conn
	buf bytes.Buffer
}

func (c *recordConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.buf.Write(b[:n])
	return n, err
}

func (c *recordConn) Write(b []byte) (int, error) { return len(b), nil }
