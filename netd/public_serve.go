package netd

// The public (Funnel) listener: see the CONTRACT in public.go.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rosskukulinski/twd/internal/publicspec"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

// publicRejectWindow is PublicRejectWindow; tests shorten it.
var publicRejectWindow = PublicRejectWindow

// maxIPBuckets bounds the per-IP rate limiter and the reject IP counts.
const maxIPBuckets = 10000

// publicState is a public node's listener, limiters and counters. The
// Object's pointer to it is set at Create and never changes; its fields
// are guarded by mu.
type publicState struct {
	mu    sync.Mutex
	opts  PublicOptions
	ln    net.Listener // nil when closed
	hs    *http.Server
	conns int // open public connections (MaxConns)
	// prefetched: the node's certificate was requested once (real Funnel).
	prefetched bool

	node *bucket
	ips  map[netip.Addr]*bucket

	rej       rejectCounts
	stopFlush chan struct{}
	window    time.Duration // publicRejectWindow at Create
}

type rejectCounts struct {
	from                     time.Time
	rate, tooLarge, connsRef int
	ips                      map[netip.Addr]int
}

func newPublicState(o PublicOptions) *publicState {
	o.Routes = append([]string(nil), o.Routes...)
	o.Upgrade = append([]string(nil), o.Upgrade...)
	return &publicState{opts: o, ips: map[netip.Addr]*bucket{},
		node: newBucket(o.Limits.RPS, o.Limits.Burst), rej: rejectCounts{ips: map[netip.Addr]int{}}, window: publicRejectWindow}
}

// bucket is a token bucket. A rate ≤ 0 disables it.
type bucket struct {
	rate, burst, tokens float64
	last                time.Time
}

func newBucket(rate float64, burst int) *bucket {
	b := float64(burst)
	if b < 1 {
		b = 1
	}
	return &bucket{rate: rate, burst: b, tokens: b}
}

// take consumes one token, or returns how long until one is available.
func (b *bucket) take(now time.Time) (bool, time.Duration) {
	if b.rate <= 0 {
		return true, 0
	}
	b.refill(now)
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
}

func (b *bucket) refill(now time.Time) {
	if !b.last.IsZero() {
		b.tokens = math.Min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
}

// ipBucket returns ip's bucket, evicting idle (refilled) buckets when the
// map is full. p.mu must be held.
func (p *publicState) ipBucket(ip netip.Addr, now time.Time) *bucket {
	if b := p.ips[ip]; b != nil {
		return b
	}
	if len(p.ips) >= maxIPBuckets {
		for k, b := range p.ips {
			b.refill(now)
			if b.tokens >= b.burst {
				delete(p.ips, k)
			}
		}
		// Still full (a flood of distinct IPs): drop arbitrary entries; the
		// per-node bucket still bounds the total.
		for k := range p.ips {
			if len(p.ips) < maxIPBuckets*9/10 {
				break
			}
			delete(p.ips, k)
		}
	}
	b := newBucket(p.opts.Limits.IPRPS, p.opts.Limits.IPBurst)
	p.ips[ip] = b
	return b
}

const (
	rejRate = iota
	rejTooLarge
	rejConns
)

func (p *publicState) countReject(kind int, ip netip.Addr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rej.from.IsZero() {
		p.rej.from = time.Now()
	}
	switch kind {
	case rejRate:
		p.rej.rate++
	case rejConns:
		p.rej.connsRef++
	default:
		p.rej.tooLarge++
	}
	if ip.IsValid() {
		if _, ok := p.rej.ips[ip]; ok || len(p.rej.ips) < maxIPBuckets {
			p.rej.ips[ip]++
		}
	}
}

// takeSummary returns and resets the reject counts (nil if there were none).
func (p *publicState) takeSummary(id string, now time.Time) *PublicRejectSummary {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.rej
	if r.rate+r.tooLarge+r.connsRef == 0 {
		return nil
	}
	p.rej = rejectCounts{ips: map[netip.Addr]int{}}
	s := &PublicRejectSummary{ObjectID: id, From: r.from, To: now, RateLimited: r.rate, TooLarge: r.tooLarge,
		ConnsRefused: r.connsRef, UniqueIPApprox: len(r.ips)}
	for ip, n := range r.ips {
		s.TopIPs = append(s.TopIPs, IPCount{IP: ip, Count: n})
	}
	sort.Slice(s.TopIPs, func(i, j int) bool {
		if s.TopIPs[i].Count != s.TopIPs[j].Count {
			return s.TopIPs[i].Count > s.TopIPs[j].Count
		}
		return s.TopIPs[i].IP.Less(s.TopIPs[j].IP)
	})
	if len(s.TopIPs) > 10 {
		s.TopIPs = s.TopIPs[:10]
	}
	return s
}

// ---- listener ----

type funnelCtxKey struct{}

// funnelConnOf unwraps c (flow, limitedConn, *tls.Conn) down to the
// *ipn.FunnelConn underneath, or returns nil.
func funnelConnOf(c net.Conn) *ipn.FunnelConn {
	for range 8 {
		switch t := c.(type) {
		case *ipn.FunnelConn:
			return t
		case *flow:
			c = t.Conn
		case *limitedConn:
			c = t.Conn
		case *tls.Conn:
			c = t.NetConn()
		default:
			return nil
		}
	}
	return nil
}

// limitedConn releases its MaxConns slot when closed.
type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

// publicListener admits only Funnel conns, enforces MaxConns and tracks
// every admitted conn as a flow (Kill and expose off RST them).
type publicListener struct {
	net.Listener
	o *Object
	p *publicState
}

func (l *publicListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		fc := funnelConnOf(c)
		if fc == nil {
			// Fail closed: nothing on the public listener is ever treated
			// as a tailnet caller.
			l.o.reg.log.Warn("public listener: refusing a non-Funnel connection", "object", l.o.id)
			c.Close()
			continue
		}
		p := l.p
		p.mu.Lock()
		over := p.opts.Limits.MaxConns > 0 && p.conns >= p.opts.Limits.MaxConns
		if !over {
			p.conns++
		}
		p.mu.Unlock()
		if over {
			p.countReject(rejConns, fc.Src.Addr().Unmap())
			rst(c)
			continue
		}
		lc := &limitedConn{Conn: c, release: func() {
			p.mu.Lock()
			p.conns--
			p.mu.Unlock()
		}}
		if f, ok := l.o.track(lc); ok {
			return f, nil
		}
		lc.once.Do(lc.release) // track RST'd it
	}
}

func (r *Registry) listenPublic(srv *tsnet.Server) (net.Listener, error) {
	if r.cfg.PublicListen != nil {
		return r.cfg.PublicListen(srv)
	}
	return srv.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())
}

// openPublic opens the public listener if the node is ready and enabled.
func (o *Object) openPublic() error {
	p := o.pub
	o.mu.Lock()
	srv, state := o.srv, o.state
	o.mu.Unlock()
	if state != StateReady || srv == nil {
		return nil // start() opens it once the node is ready
	}
	p.mu.Lock()
	skip := !p.opts.Enabled || p.ln != nil
	lim := p.opts.Limits
	p.mu.Unlock()
	if skip {
		return nil
	}
	raw, err := o.reg.listenPublic(srv)
	if err != nil {
		return err
	}
	ln := &publicListener{Listener: raw, o: o, p: p}
	mhb := 2*lim.MaxHeaderBytes + 4096
	if lim.MaxHeaderBytes <= 0 {
		mhb = 0 // net/http default
	}
	hs := &http.Server{
		Handler:           http.HandlerFunc(o.servePublic),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		// Go answers a plain-text 431 above MaxHeaderBytes (+4 KiB); the
		// handler enforces the configured limit itself with a typed body.
		MaxHeaderBytes: mhb,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, funnelCtxKey{}, funnelConnOf(c))
		},
		ErrorLog: slog.NewLogLogger(o.reg.log.Handler(), slog.LevelDebug),
	}
	p.mu.Lock()
	if !p.opts.Enabled || p.ln != nil { // raced with SetPublicEnabled
		p.mu.Unlock()
		raw.Close()
		return nil
	}
	p.ln, p.hs = ln, hs
	p.mu.Unlock()
	if o.State() != StateReady { // killed meanwhile
		o.closePublic()
		return nil
	}
	go hs.Serve(ln)
	p.mu.Lock()
	first := !p.prefetched
	p.prefetched = true
	p.mu.Unlock()
	if o.reg.cfg.PublicListen == nil && first {
		// Real Funnel: warm the node's certificate now (ADR-029), so the
		// first public client doesn't time out waiting for issuance.
		o.mu.Lock()
		lc, name := o.lc, o.dnsName
		o.mu.Unlock()
		PrefetchCert(lc, name, o.reg.log.With("object", o.id))
	}
	o.reg.log.Info("public listener open", "object", o.id, "addr", raw.Addr().String(), "routes", strings.Join(p.opts.Routes, ","))
	return nil
}

// closePublic closes the public listener and RSTs every flow of the node
// (a public node has no other flows: no tailnet ingress, no egress).
func (o *Object) closePublic() int {
	p := o.pub
	if p == nil {
		return 0
	}
	p.mu.Lock()
	ln, hs := p.ln, p.hs
	p.ln, p.hs = nil, nil
	p.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	n := o.abortFlows()
	if hs != nil {
		go hs.Close() // the conns are already reset; this only reaps goroutines
	}
	return n
}

// startPublicFlusher emits reject summaries every publicRejectWindow.
func (o *Object) startPublicFlusher() {
	p := o.pub
	p.mu.Lock()
	if p.stopFlush != nil {
		p.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	p.stopFlush = stop
	p.mu.Unlock()
	go func() {
		t := time.NewTicker(p.window)
		defer t.Stop()
		for {
			select {
			case <-stop:
				o.flushRejects()
				return
			case <-t.C:
				o.flushRejects()
			}
		}
	}()
}

func (o *Object) flushRejects() {
	if s := o.pub.takeSummary(o.id, time.Now()); s != nil && o.reg.cfg.OnPublicRejected != nil {
		o.reg.cfg.OnPublicRejected(*s)
	}
}

func (o *Object) stopPublicFlusher() {
	p := o.pub
	if p == nil {
		return
	}
	p.mu.Lock()
	stop := p.stopFlush
	p.stopFlush = nil
	p.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// SetPublicEnabled opens or closes a public node's Funnel listener
// (`twd expose on|off`). Closing RSTs the node's public flows and returns
// at once; the node stays joined. Both directions are idempotent. On a node
// that is still joining it decides what happens once the node is ready.
func (r *Registry) SetPublicEnabled(id string, on bool) error {
	o := r.Get(id)
	if o == nil {
		return ErrNotFound
	}
	p := o.pub
	if p == nil {
		return newErr(CodeBadRequest, "object "+id+" is not a public node")
	}
	p.mu.Lock()
	p.opts.Enabled = on
	p.mu.Unlock()
	if !on {
		n := o.closePublic()
		r.log.Info("public listener closed", "object", id, "flows_aborted", n)
		return nil
	}
	switch st := o.State(); st {
	case StateKilled, StateFailed, StateClosed:
		return newErr(CodeKilled, "object "+id+" is "+string(st))
	}
	return o.openPublic()
}

// IsPublic reports whether o is a public (Funnel) node.
func (o *Object) IsPublic() bool { return o.pub != nil }

// PublicListening reports whether a public node's listener is open.
func (o *Object) PublicListening() bool {
	if o.pub == nil {
		return false
	}
	o.pub.mu.Lock()
	defer o.pub.mu.Unlock()
	return o.pub.ln != nil
}

// ---- request handling ----

// pubWriter records what reached the client, and replaces an upstream
// response produced after a body overflow or a timeout with netd's own.
type pubWriter struct {
	http.ResponseWriter
	status  int
	bytes   int64
	wrote   bool
	swallow bool // upstream output is discarded (netd answered instead)
	gate    func() *Error
}

func (w *pubWriter) WriteHeader(c int) {
	if w.wrote || w.swallow {
		return
	}
	if w.gate != nil {
		if e := w.gate(); e != nil {
			w.swallow = true
			w.writeErr(e)
			return
		}
	}
	w.wrote, w.status = true, c
	w.ResponseWriter.WriteHeader(c)
}

func (w *pubWriter) Write(b []byte) (int, error) {
	if !w.wrote && !w.swallow {
		w.WriteHeader(http.StatusOK)
	}
	if w.swallow {
		return len(b), nil
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *pubWriter) Flush() {
	if !w.wrote && !w.swallow {
		w.WriteHeader(http.StatusOK)
	}
	if w.swallow {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController (ReverseProxy upgrades) reach the
// underlying writer's Hijack.
func (w *pubWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// writeErr answers with e directly.
func (w *pubWriter) writeErr(e *Error) {
	w.wrote, w.status = true, e.Status()
	WriteError(w.ResponseWriter, e)
}

// countingBody counts bytes read and flags a MaxBytesReader overflow.
type countingBody struct {
	io.ReadCloser
	n        int64
	overflow bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		b.overflow = true
	}
	return n, err
}

func headerSize(r *http.Request) int {
	n := len(r.Method) + len(r.RequestURI) + len(r.Proto) + 4 + len(r.Host) + 8
	for k, vs := range r.Header {
		for _, v := range vs {
			n += len(k) + len(v) + 4
		}
	}
	return n
}

func isUpgrade(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return true
	}
	for _, v := range r.Header.Values("Connection") {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), "upgrade") {
				return true
			}
		}
	}
	return false
}

func (o *Object) servePublic(w http.ResponseWriter, req *http.Request) {
	t0 := time.Now()
	p := o.pub
	pw := &pubWriter{ResponseWriter: w}
	fc, _ := req.Context().Value(funnelCtxKey{}).(*ipn.FunnelConn)
	if fc == nil { // unreachable: Accept admits only Funnel conns
		w.Header().Set("Connection", "close")
		pw.writeErr(newErr(CodeNoIdentity, "not a Funnel connection"))
		return
	}
	src := fc.Src
	ip := src.Addr().Unmap()
	if st := o.State(); st != StateReady {
		pw.writeErr(newErr(CodeKilled, "object "+o.id+" is "+string(st)))
		return
	}
	p.mu.Lock()
	opts := p.opts
	p.mu.Unlock()
	lim := opts.Limits

	// 1. Limits. Refusals here are counted for the summary, not reported.
	if lim.MaxHeaderBytes > 0 && headerSize(req) > lim.MaxHeaderBytes {
		p.countReject(rejTooLarge, ip)
		w.Header().Set("Connection", "close")
		pw.writeErr(newErr(CodeHeaderTooLarge, "request headers exceed "+strconv.Itoa(lim.MaxHeaderBytes)+" bytes"))
		return
	}
	now := time.Now()
	p.mu.Lock()
	ok, wait := p.ipBucket(ip, now).take(now)
	if ok {
		ok, wait = p.node.take(now)
	}
	p.mu.Unlock()
	if !ok {
		secs := max(int(math.Ceil(wait.Seconds())), 1)
		p.countReject(rejRate, ip)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		pw.writeErr(newErr(CodeRateLimited, "public request rate limit exceeded; retry after "+strconv.Itoa(secs)+" s"))
		return
	}
	tooLarge := func() *Error {
		return newErr(CodePayloadTooLarge, "request body exceeds "+strconv.FormatInt(lim.MaxBodyBytes, 10)+" bytes")
	}
	if lim.MaxBodyBytes > 0 && req.ContentLength > lim.MaxBodyBytes {
		p.countReject(rejTooLarge, ip)
		w.Header().Set("Connection", "close")
		pw.writeErr(tooLarge())
		return
	}

	// 2. Route. From here on every request is reported.
	rec := PublicRequestRecord{ObjectID: o.id, Time: t0, Method: req.Method, ClientIP: ip}
	report := func() {
		rec.Status, rec.BytesOut, rec.Duration = pw.status, pw.bytes, time.Since(t0)
		if rec.Status == 0 {
			rec.Status = http.StatusOK
		}
		if cb := o.reg.cfg.OnPublicRequest; cb != nil {
			cb(rec)
		}
	}
	cp, err := publicspec.CanonicalPath(req.URL)
	if err != nil {
		rec.Code = CodeBadRequest
		pw.writeErr(newErr(CodeBadRequest, "path refused: "+err.Error()))
		report()
		return
	}
	rec.Path = cp
	route, ok := publicspec.MatchAny(opts.Routes, cp)
	if !ok {
		rec.Code = CodeNotPublic
		pw.writeErr(newErr(CodeNotPublic, "this path is not public"))
		report()
		return
	}
	rec.Route = route
	upgrade := isUpgrade(req)
	if upgrade {
		if _, ok := publicspec.MatchAny(opts.Upgrade, cp); !ok {
			rec.Code = CodeUpgradeNotAllowed
			pw.writeErr(newErr(CodeUpgradeNotAllowed, "upgrades are not allowed on this route"))
			report()
			return
		}
	}

	// 3. Headers: nothing the client sent may claim an identity; the
	// client address comes from the Funnel transport.
	stripIdentityHeaders(req.Header)
	req.Header.Set("CF-Connecting-IP", ip.String())
	req.Header.Set("X-Forwarded-For", ip.String())
	req.Header.Set("X-Forwarded-Proto", "https")

	// 4. cf blob: a requester, never a caller.
	blob := map[string]any{
		"tailscale": map[string]any{"requester": PublicRequester{Kind: "public", IP: ip.String(), Port: int(src.Port()), Proto: "https", Host: req.Host}},
		"objectId":  opts.DeliverTo,
	}
	b, _ := json.Marshal(blob)
	req.Header.Set(HeaderCF, string(b))
	req.Header.Set(HeaderObject, opts.DeliverTo)

	body := &countingBody{ReadCloser: req.Body}
	if req.Body != nil && req.Body != http.NoBody {
		if lim.MaxBodyBytes > 0 {
			body.ReadCloser = http.MaxBytesReader(w, req.Body, lim.MaxBodyBytes)
		}
		req.Body = body
	}
	ctx := req.Context()
	if !upgrade && lim.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, lim.Timeout())
		defer cancel()
	}
	req = req.WithContext(ctx)
	pw.gate = func() *Error {
		if body.overflow {
			return tooLarge()
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return newErr(CodeRequestTimeout, "the request took longer than "+lim.Timeout().String())
		}
		return nil
	}

	// 5. Upstream.
	if up := o.reg.cfg.Upstream; up == nil {
		pw.writeErr(newErr(CodeUpstreamUnavailable, "no upstream configured"))
	} else {
		up.ServeHTTP(pw, req)
	}
	if !pw.wrote && !pw.swallow {
		if e := pw.gate(); e != nil {
			pw.swallow = true
			pw.writeErr(e)
		}
	}
	rec.BytesIn = body.n
	if body.overflow {
		// A body over the cap is a limit refusal: counted, not reported.
		p.countReject(rejTooLarge, ip)
		return
	}
	if pw.swallow {
		rec.Code = CodeRequestTimeout
	}
	report()
}
