package netd

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"strings"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tailcfg"
)

// EgressHandler returns the attributed egress proxy. It expects proxy-style
// (absolute-form) requests whose HeaderObject was set by the trusted platform
// Egress entrypoint; serve it only on a socket tenant code cannot reach
// directly (a 0700 directory shared with workerd).
//
// Each request leaves via its object's own node. Destinations are resolved
// from that object's netmap only (ADR-019): names never go to host DNS and
// non-tailnet IPs never go to the host network.
func (r *Registry) EgressHandler() http.Handler {
	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(r.log.Handler(), slog.LevelDebug),
		Transport:     egressTransport{},
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := *pr.In.URL
			pr.Out.URL = &u
			pr.Out.Host = pr.In.Host
			if pr.Out.Host == "" {
				pr.Out.Host = u.Host
			}
			stripIdentityHeaders(pr.Out.Header)
			pr.Out.Header.Del("proxy-connection")
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			if e, ok := AsError(err); ok {
				WriteError(w, e)
				return
			}
			if req.Context().Err() != nil {
				return
			}
			o := objectFrom(req.Context())
			e := newErr(CodeDialFailed, err.Error())
			if o != nil {
				e.Object = o.id
			}
			e.Destination = req.URL.Host
			WriteError(w, e)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id := req.Header.Get(HeaderObject)
		o := r.Get(id)
		if o == nil {
			WriteError(w, &Error{Code: CodeUnknownObject, Detail: "egress request does not name a known object", Object: id})
			return
		}
		if req.Method == http.MethodConnect {
			WriteError(w, &Error{Code: CodeBadRequest, Detail: "CONNECT is not supported; use ctx.tailnet.connect()", Object: id, status: http.StatusMethodNotAllowed})
			return
		}
		if !req.URL.IsAbs() || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
			WriteError(w, &Error{Code: CodeBadRequest, Detail: "egress needs an absolute http(s) URL", Object: id})
			return
		}
		rctx, cancel := context.WithTimeout(req.Context(), r.cfg.ReadyTimeout)
		err := o.Ready(rctx)
		cancel()
		if err != nil {
			e, _ := AsError(err)
			e.Object, e.Destination = id, req.URL.Host
			WriteError(w, e)
			return
		}
		// Resolve up front for a typed error; dialEgress re-checks at dial time.
		if _, e := o.resolveEgress(req.Context(), req.URL.Host, req.URL.Scheme); e != nil {
			WriteError(w, e)
			return
		}
		rp.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), objectKey{}, o)))
	})
}

// DialEgress opens a raw TCP flow from object id's own node to hostport
// (a netmap name or tailnet IP, with an explicit port), under the same
// ADR-019 policy as EgressHandler. It backs CONNECT tunnels (the Node process
// tier's env proxies). The flow is tracked, so a kill RSTs it.
func (r *Registry) DialEgress(ctx context.Context, id, hostport string) (net.Conn, error) {
	o := r.Get(id)
	if o == nil {
		return nil, &Error{Code: CodeUnknownObject, Detail: "egress request does not name a known object", Object: id}
	}
	if _, port, err := net.SplitHostPort(hostport); err != nil || port == "" {
		return nil, &Error{Code: CodeBadRequest, Detail: "CONNECT needs host:port", Object: id, Destination: hostport}
	}
	rctx, cancel := context.WithTimeout(ctx, r.cfg.ReadyTimeout)
	err := o.Ready(rctx)
	cancel()
	if err != nil {
		e, _ := AsError(err)
		e.Object, e.Destination = id, hostport
		return nil, e
	}
	c, err := o.dialEgress(ctx, "tcp", hostport)
	if err != nil {
		if _, ok := AsError(err); ok {
			return nil, err
		}
		e := newErr(CodeDialFailed, err.Error())
		e.Object, e.Destination = id, hostport
		return nil, e
	}
	return c, nil
}

type objectKey struct{}

func objectFrom(ctx context.Context) *Object {
	o, _ := ctx.Value(objectKey{}).(*Object)
	return o
}

// egressTransport dispatches to the per-object transport carried in the
// request context, so connection pools are never shared between objects.
type egressTransport struct{}

func (egressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	o := objectFrom(req.Context())
	if o == nil {
		return nil, newErr(CodeUnknownObject, "no object in context")
	}
	o.mu.Lock()
	tr := o.tr
	o.mu.Unlock()
	if tr == nil {
		return nil, &Error{Code: CodeKilled, Detail: "object " + o.id + " is not ready", Object: o.id}
	}
	return tr.RoundTrip(req)
}

// dialEgress is the per-object transport's dialer. addr is host:port from the
// request URL (a name or an IP); it is resolved and checked again here so no
// code path can reach tsnet.Server.Dial with an unchecked destination.
func (o *Object) dialEgress(ctx context.Context, network, addr string) (net.Conn, error) {
	ap, e := o.resolveEgress(ctx, addr, "")
	if e != nil {
		return nil, e
	}
	o.mu.Lock()
	srv, state := o.srv, o.state
	o.mu.Unlock()
	if state != StateReady || srv == nil {
		return nil, &Error{Code: CodeKilled, Detail: "object " + o.id + " is " + string(state), Object: o.id}
	}
	c, err := srv.Dial(ctx, "tcp", ap.String())
	if err != nil {
		return nil, err
	}
	f, ok := o.track(c)
	if !ok {
		return nil, &Error{Code: CodeKilled, Detail: "object " + o.id + " was killed", Object: o.id}
	}
	return f, nil
}

// resolveEgress applies the ADR-019 egress policy to hostport and returns the
// tailnet address to dial. scheme supplies the default port.
func (o *Object) resolveEgress(ctx context.Context, hostport, scheme string) (netip.AddrPort, *Error) {
	refuse := func(code, detail string) (netip.AddrPort, *Error) {
		e := newErr(code, detail)
		e.Object, e.Destination = o.id, hostport
		return netip.AddrPort{}, e
	}
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		host, portStr = strings.Trim(hostport, "[]"), ""
	}
	if portStr == "" {
		portStr = "80"
		if scheme == "https" {
			portStr = "443"
		}
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return refuse(CodeBadRequest, "invalid port")
	}
	if host == "" {
		return refuse(CodeBadRequest, "empty host")
	}
	if strings.Contains(host, "%") {
		return refuse(CodeForbiddenDestination, "scoped (zoned) addresses are refused")
	}
	o.mu.Lock()
	lc := o.lc
	o.mu.Unlock()
	if lc == nil {
		return refuse(CodeIdentityPending, "object node not ready")
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return refuse(CodeDialFailed, "netmap unavailable: "+err.Error())
	}
	ip, err := netip.ParseAddr(host)
	if err == nil {
		ip = ip.Unmap()
		if code, detail := o.reg.classify(o, ip); code != "" {
			return refuse(code, detail)
		}
		for _, p := range st.Peer {
			for _, a := range p.TailscaleIPs {
				if a == ip {
					return netip.AddrPortFrom(ip, uint16(port)), nil
				}
			}
		}
		if code, detail, ok := o.reg.servicePeer(o, st, ip); ok {
			if code != "" {
				return refuse(code, detail)
			}
			return netip.AddrPortFrom(ip, uint16(port)), nil
		}
		return refuse(CodePeerNotVisible, "no peer with address "+ip.String()+" in this object's netmap (a grant is missing, or the peer is offline or deleted)")
	}
	// A name: resolve against this object's netmap only. A bare name matches a
	// peer's first MagicDNS label; a dotted name must match the full FQDN.
	want := strings.TrimSuffix(strings.ToLower(host), ".")
	var found netip.Addr
	for _, p := range st.Peer {
		dn := strings.TrimSuffix(strings.ToLower(p.DNSName), ".")
		if dn == "" {
			continue
		}
		first, _, _ := strings.Cut(dn, ".")
		if dn == want || (!strings.Contains(want, ".") && first == want) {
			for _, a := range p.TailscaleIPs {
				if a.Is4() || !found.IsValid() {
					found = a
				}
			}
			break
		}
	}
	if !found.IsValid() {
		// A Tailscale Service name (a MagicDNS extra record from this
		// object's own netmap, never host DNS), routed by a granted host.
		if vip, ok := serviceVIP(ctx, lc, want); ok {
			code, detail, routed := o.reg.servicePeer(o, st, vip)
			if !routed {
				return refuse(CodePeerNotVisible, "service "+host+" has no host visible to this object (a grant to the svc: name is missing, or no host is approved and online)")
			}
			if code != "" {
				return refuse(code, detail)
			}
			if code, detail := o.reg.classify(o, vip); code != "" {
				return refuse(code, detail)
			}
			return netip.AddrPortFrom(vip, uint16(port)), nil
		}
		return refuse(CodePeerNotVisible, "no peer named "+host+" in this object's netmap (names are never resolved via host DNS; a grant may be missing)")
	}
	if code, detail := o.reg.classify(o, found); code != "" {
		return refuse(code, detail)
	}
	return netip.AddrPortFrom(found, uint16(port)), nil
}

// classify returns a refusal for ip, or "" if it is an allowed tailnet peer
// address class.
func (r *Registry) classify(o *Object, ip netip.Addr) (code, detail string) {
	switch {
	case ip.IsLoopback(), ip.IsUnspecified():
		return CodeForbiddenDestination, "loopback/unspecified destinations are refused"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return CodeForbiddenDestination, "link-local/multicast destinations are refused"
	case ip == tsaddr.TailscaleServiceIP() || ip == tsaddr.TailscaleServiceIPv6():
		return CodeForbiddenDestination, "the Tailscale service address (100.100.100.100) is refused"
	}
	o.mu.Lock()
	self := ip == o.ip4 || ip == o.ip6
	o.mu.Unlock()
	if self {
		return CodeForbiddenDestination, "destination is this object's own node"
	}
	r.mu.Lock()
	sib, isSib := r.localIPs[ip]
	host := r.hostIPs[ip]
	r.mu.Unlock()
	if isSib && !r.cfg.AllowSiblings {
		return CodeForbiddenDestination, "destination is a sibling object on this host (" + sib + "); same-host calls go through the platform"
	}
	if host {
		return CodeForbiddenDestination, "destination is an address of this host"
	}
	for _, p := range r.cfg.ExtraForbidden {
		if p.Contains(ip) {
			return CodeForbiddenDestination, "destination is a platform endpoint"
		}
	}
	if !tsaddr.IsTailscaleIP(ip) {
		return CodeForbiddenDestination, "non-tailnet destination (internet egress needs an exit node or gateway; none configured)"
	}
	return "", ""
}

// serviceVIP resolves a Tailscale Service name from the node's netmap DNS
// extra records (the same records MagicDNS answers). want is lowercase with
// no trailing dot; a bare name matches the first label.
func serviceVIP(ctx context.Context, lc interface {
	DNSConfig(context.Context) (*tailcfg.DNSConfig, error)
}, want string) (netip.Addr, bool) {
	dc, err := lc.DNSConfig(ctx)
	if err != nil || dc == nil {
		return netip.Addr{}, false
	}
	var found netip.Addr
	for _, rec := range dc.ExtraRecords {
		if rec.Type != "" && rec.Type != "A" && rec.Type != "AAAA" {
			continue
		}
		dn := strings.TrimSuffix(strings.ToLower(rec.Name), ".")
		first, _, _ := strings.Cut(dn, ".")
		if dn != want && (strings.Contains(want, ".") || first != want) {
			continue
		}
		ip, err := netip.ParseAddr(rec.Value)
		if err != nil {
			continue
		}
		if ip.Is4() || !found.IsValid() {
			found = ip
		}
	}
	return found, found.IsValid()
}

// servicePeer reports whether ip is a Service VIP routed by a peer in st
// (the VIP is in the peer's AllowedIPs; control only routes it to approved
// hosts the caller has a grant to). code is set when every such host is a
// sibling object on this host (same-host calls go through the platform).
func (r *Registry) servicePeer(o *Object, st *ipnstate.Status, ip netip.Addr) (code, detail string, ok bool) {
	var hosts []netip.Addr
	for _, p := range st.Peer {
		if p.AllowedIPs == nil {
			continue
		}
		routes := false
		for _, pfx := range p.AllowedIPs.All() {
			if pfx.Contains(ip) && pfx.Bits() == ip.BitLen() {
				routes = true
				break
			}
		}
		if routes {
			hosts = append(hosts, p.TailscaleIPs...)
		}
	}
	if len(hosts) == 0 {
		return "", "", false
	}
	if r.cfg.AllowSiblings {
		return "", "", true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range hosts {
		if _, sib := r.localIPs[h]; !sib {
			return "", "", true
		}
	}
	return CodeForbiddenDestination, "the service's only reachable host is a sibling object on this host; same-host calls go through the platform", true
}
