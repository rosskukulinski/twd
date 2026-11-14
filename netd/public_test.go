package netd_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/internal/publicspec"
	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

// pubEnv is one registry with one public node whose "Funnel" listener is a
// loopback listener that wraps each conn in an *ipn.FunnelConn (testcontrol
// issues no certificates). The client's fake public source address is
// chosen per dial (dialAs), never from anything the client sends.
type pubEnv struct {
	t      *testing.T
	ctl    *nettest.Control
	reg    *netd.Registry
	cfg    netd.Config
	id     string
	srcs   sync.Map // client local addr -> netip.AddrPort
	mapped atomic.Bool
	mu     sync.Mutex
	addr   string // current public listener address
	opens  int

	upMu  sync.Mutex
	seen  []*http.Request // what reached upstream (headers cloned)
	bodyN []int
	recs  chan netd.PublicRequestRecord
	rejs  chan netd.PublicRejectSummary
	raw   bool // PublicListen returns plain (non-Funnel) conns
	slow  chan struct{}
}

const pubID = "pub1"

func defaultSrc() netip.AddrPort { return netip.MustParseAddrPort("203.0.113.7:40001") }

func newPubEnv(t *testing.T, opts netd.PublicOptions, mod func(*pubEnv)) *pubEnv {
	t.Helper()
	e := &pubEnv{t: t, ctl: startControl(t), recs: make(chan netd.PublicRequestRecord, 256),
		rejs: make(chan netd.PublicRejectSummary, 16), slow: make(chan struct{})}
	if mod != nil {
		mod(e)
	}
	t.Cleanup(func() {
		select {
		case <-e.slow:
		default:
			close(e.slow)
		}
	})
	e.cfg = netd.Config{ControlURL: e.ctl.URL, StateRoot: filepath.Join(newRundir(t), "state"),
		Keys:             (&countingMint{}).source(),
		Upstream:         http.HandlerFunc(e.upstream),
		PublicListen:     e.listen,
		OnPublicRequest:  func(r netd.PublicRequestRecord) { e.recs <- r },
		OnPublicRejected: func(s netd.PublicRejectSummary) { e.rejs <- s },
	}
	e.reg = e.newReg()
	if opts.DeliverTo == "" {
		opts.DeliverTo = "wl-entry"
	}
	o, err := e.reg.Create(pubID, netd.ObjectOptions{Public: &opts})
	if err != nil {
		t.Fatal(err)
	}
	e.id = pubID
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := o.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *pubEnv) newReg() *netd.Registry {
	reg, err := netd.New(e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { reg.Close() })
	return reg
}

type fakeFunnelListener struct {
	net.Listener
	e *pubEnv
}

func (l *fakeFunnelListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if l.e.raw {
		return c, nil
	}
	// The dialer records the client's fake source right after its dial
	// returns, which can be after this Accept: wait briefly for it.
	src := defaultSrc()
	for range 100 {
		if v, ok := l.e.srcs.Load(c.RemoteAddr().String()); ok {
			src = v.(netip.AddrPort)
			break
		}
		if !l.e.mapped.Load() {
			break // only clients from e.client register sources
		}
		time.Sleep(2 * time.Millisecond)
	}
	return &ipn.FunnelConn{Conn: c, Src: src}, nil
}

func (e *pubEnv) listen(*tsnet.Server) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.addr = ln.Addr().String()
	e.opens++
	e.mu.Unlock()
	return &fakeFunnelListener{Listener: ln, e: e}, nil
}

func (e *pubEnv) url(path string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return "http://" + e.addr + path
}

func (e *pubEnv) upstream(w http.ResponseWriter, r *http.Request) {
	n := 0
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		n = len(b)
		if err != nil {
			http.Error(w, "upstream read: "+err.Error(), 502)
			return
		}
	}
	e.upMu.Lock()
	c := r.Clone(context.Background())
	e.seen = append(e.seen, c)
	e.bodyN = append(e.bodyN, n)
	e.upMu.Unlock()
	if r.URL.Path == "/slow/hang" {
		select {
		case <-r.Context().Done():
			return // like hostd's proxy: nothing written on a cancelled request
		case <-e.slow:
		}
	}
	w.Header().Set("x-upstream", "1")
	fmt.Fprintf(w, "ok %s", r.URL.Path)
}

func (e *pubEnv) seenCount() int {
	e.upMu.Lock()
	defer e.upMu.Unlock()
	return len(e.seen)
}

func (e *pubEnv) lastSeen() *http.Request {
	e.upMu.Lock()
	defer e.upMu.Unlock()
	if len(e.seen) == 0 {
		return nil
	}
	return e.seen[len(e.seen)-1]
}

// client returns an HTTP client whose connections appear (to netd, via the
// FunnelConn) to come from src.
func (e *pubEnv) client(src netip.AddrPort) *http.Client {
	e.mapped.Store(true)
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := d.DialContext(ctx, network, addr)
			if err == nil {
				e.srcs.Store(c.LocalAddr().String(), src)
			}
			return c, err
		},
	}}
}

func do(t *testing.T, c *http.Client, method, u string, body io.Reader, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func limits(mod func(*publicspec.Limits)) publicspec.Limits {
	l := publicspec.Defaults
	l.IPRPS, l.IPBurst, l.RPS, l.Burst = 1000, 1000, 1000, 1000
	if mod != nil {
		mod(&l)
	}
	return l
}

func TestPublicHeadersAndBlob(t *testing.T) {
	e := newPubEnv(t, netd.PublicOptions{Routes: []string{"/webhooks/*"}, Limits: limits(nil), Enabled: true}, nil)
	src := netip.MustParseAddrPort("198.51.100.23:51515")
	resp, body := do(t, e.client(src), "POST", e.url("/webhooks/github?token=s3cret"), strings.NewReader(`{"a":1}`), map[string]string{
		"Tailscale-User-Login": "alice@example.com", "Tailscale-User-Name": "Alice",
		"x-twd-cf":         `{"tailscale":{"caller":{"nodeId":"nFORGED","tags":["tag:admin"]}}}`,
		"x-twd-object":     "victim",
		"CF-Connecting-IP": "1.2.3.4", "X-Forwarded-For": "1.2.3.4, 5.6.7.8", "X-Real-IP": "1.2.3.4",
		"Forwarded": "for=1.2.3.4", "Tailnet-Assertion": "forged", "cf-ipcountry": "XX",
	})
	if resp.StatusCode != 200 || body != "ok /webhooks/github" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	r := e.lastSeen()
	for _, k := range []string{"Tailscale-User-Login", "Tailscale-User-Name", "X-Real-Ip", "Forwarded", "Tailnet-Assertion", "Cf-Ipcountry"} {
		if v := r.Header.Get(k); v != "" {
			t.Errorf("%s reached upstream: %q", k, v)
		}
	}
	if got := r.Header.Values("CF-Connecting-IP"); len(got) != 1 || got[0] != "198.51.100.23" {
		t.Errorf("CF-Connecting-IP %v", got)
	}
	if got := r.Header.Values("X-Forwarded-For"); len(got) != 1 || got[0] != "198.51.100.23" {
		t.Errorf("X-Forwarded-For %v", got)
	}
	if got := r.Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto %q", got)
	}
	if got := r.Header.Get(netd.HeaderObject); got != "wl-entry" {
		t.Errorf("x-twd-object %q, want the DeliverTo node", got)
	}
	var blob map[string]any
	if err := json.Unmarshal([]byte(r.Header.Get(netd.HeaderCF)), &blob); err != nil {
		t.Fatalf("cf blob: %v (%q)", err, r.Header.Get(netd.HeaderCF))
	}
	ts, _ := blob["tailscale"].(map[string]any)
	if _, ok := ts["caller"]; ok {
		t.Fatalf("cf blob has a caller: %v", blob)
	}
	req, _ := ts["requester"].(map[string]any)
	if req["kind"] != "public" || req["ip"] != "198.51.100.23" || req["proto"] != "https" || req["port"] != float64(51515) {
		t.Errorf("requester %v", req)
	}
	if blob["objectId"] != "wl-entry" {
		t.Errorf("objectId %v", blob["objectId"])
	}
	rec := <-e.recs
	if rec.Path != "/webhooks/github" || strings.Contains(rec.Path, "token") || rec.Route != "/webhooks/*" ||
		rec.Status != 200 || rec.Method != "POST" || rec.ClientIP.String() != "198.51.100.23" || rec.BytesIn != 7 || rec.BytesOut == 0 || rec.ObjectID != pubID {
		t.Errorf("record %+v", rec)
	}
	info := e.reg.Get(pubID).Info()
	if !info.Public || !info.PublicListening {
		t.Errorf("info %+v", info)
	}
}

func TestPublicRoutes(t *testing.T) {
	e := newPubEnv(t, netd.PublicOptions{Routes: []string{"/oauth/callback", "/webhooks/*"}, Limits: limits(nil), Enabled: true}, nil)
	c := e.client(defaultSrc())
	for _, p := range []string{"/webhooks", "/webhooks/x/y", "/oauth/callback"} {
		if resp, body := do(t, c, "GET", e.url(p), nil, nil); resp.StatusCode != 200 {
			t.Errorf("%s: %d %s", p, resp.StatusCode, body)
		}
	}
	before := e.seenCount()
	for _, tc := range []struct {
		path, code string
		status     int
	}{
		{"/", "not_public", 404}, {"/admin", "not_public", 404}, {"/webhooksx", "not_public", 404},
		{"/oauth/callback/x", "not_public", 404},
		{"/webhooks/../admin", "bad_request", 400}, {"/webhooks//x", "bad_request", 400},
		{"/webhooks%2F..%2Fadmin", "bad_request", 400}, {"/_twd/o/Room/lobby", "bad_request", 400},
	} {
		resp, body := do(t, c, "GET", e.url(tc.path), nil, nil)
		if resp.StatusCode != tc.status || resp.Header.Get("twd-error") != tc.code {
			t.Errorf("%s: %d %s %s, want %d %s", tc.path, resp.StatusCode, resp.Header.Get("twd-error"), body, tc.status, tc.code)
		}
	}
	if n := e.seenCount(); n != before {
		t.Errorf("%d refused requests reached upstream", n-before)
	}
	// Refusals at the route step are reported.
	n404 := 0
	for range 3 + 8 {
		r := <-e.recs
		if r.Code == "not_public" && r.Route == "" {
			n404++
		}
	}
	if n404 != 4 {
		t.Errorf("not_public records: %d", n404)
	}
}

func TestPublicCatchAllStillRefusesPlatformPaths(t *testing.T) {
	e := newPubEnv(t, netd.PublicOptions{Routes: []string{"/*"}, Limits: limits(nil), Enabled: true}, nil)
	c := e.client(defaultSrc())
	if resp, _ := do(t, c, "GET", e.url("/anything/at/all"), nil, nil); resp.StatusCode != 200 {
		t.Fatalf("/* did not match: %d", resp.StatusCode)
	}
	for _, p := range []string{"/_twd/o/Room/lobby", "/_twd", "/_twd/o/Room/lobby/x"} {
		resp, _ := do(t, c, "GET", e.url(p), nil, nil)
		if resp.StatusCode != 400 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	if e.seenCount() != 1 {
		t.Errorf("platform path reached upstream")
	}
}

func TestPublicUpgrade(t *testing.T) {
	e := newPubEnv(t, netd.PublicOptions{Routes: []string{"/api/*", "/ws/*"}, Upgrade: []string{"/ws/*"}, Limits: limits(nil), Enabled: true}, nil)
	c := e.client(defaultSrc())
	h := map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}
	resp, _ := do(t, c, "GET", e.url("/api/x"), nil, h)
	if resp.StatusCode != 403 || resp.Header.Get("twd-error") != "upgrade_not_allowed" {
		t.Errorf("upgrade on /api: %d %s", resp.StatusCode, resp.Header.Get("twd-error"))
	}
	if e.seenCount() != 0 {
		t.Error("refused upgrade reached upstream")
	}
	resp, _ = do(t, c, "GET", e.url("/ws/chat"), nil, h)
	if resp.StatusCode != 200 || e.seenCount() != 1 {
		t.Errorf("allowed upgrade: %d (upstream %d)", resp.StatusCode, e.seenCount())
	}
}

func TestPublicRateLimits(t *testing.T) {
	restore := netd.SetPublicRejectWindow(300 * time.Millisecond)
	defer restore()
	e := newPubEnv(t, netd.PublicOptions{Routes: []string{"/*"}, Limits: limits(func(l *publicspec.Limits) {
		l.IPRPS, l.IPBurst = 0.5, 2
		l.RPS, l.Burst = 0.5, 4
	}), Enabled: true}, nil)
	a := e.client(netip.MustParseAddrPort("192.0.2.1:1000"))
	b := e.client(netip.MustParseAddrPort("192.0.2.2:1000"))
	c := e.client(netip.MustParseAddrPort("192.0.2.3:1000"))
	for i := range 2 {
		if resp, _ := do(t, a, "GET", e.url("/x"), nil, nil); resp.StatusCode != 200 {
			t.Fatalf("a #%d: %d", i, resp.StatusCode)
		}
	}
	resp, _ := do(t, a, "GET", e.url("/x"), nil, nil)
	ra, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
	if resp.StatusCode != 429 || resp.Header.Get("twd-error") != "rate_limited" || ra < 1 {
		t.Fatalf("a #3 (per-IP): %d %s Retry-After %q", resp.StatusCode, resp.Header.Get("twd-error"), resp.Header.Get("Retry-After"))
	}
	// Another IP still has its own bucket; the node bucket (burst 4) has 2 left.
	for i := range 2 {
		if resp, _ := do(t, b, "GET", e.url("/x"), nil, nil); resp.StatusCode != 200 {
			t.Fatalf("b #%d: %d", i, resp.StatusCode)
		}
	}
	resp, _ = do(t, c, "GET", e.url("/x"), nil, nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("c (per-node): %d Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if e.seenCount() != 4 {
		t.Errorf("upstream saw %d, want 4", e.seenCount())
	}
	// Rejections are summarized, not reported per request.
	select {
	case s := <-e.rejs:
		if s.RateLimited != 2 || s.ObjectID != pubID || s.UniqueIPApprox != 2 || len(s.TopIPs) != 2 {
			t.Errorf("summary %+v", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reject summary")
	}
	for range 4 {
		if r := <-e.recs; r.Status != 200 {
			t.Errorf("record %+v", r)
		}
	}
	select {
	case r := <-e.recs:
		t.Errorf("a rejected request was reported: %+v", r)
	default:
	}
}

func TestPublicBodyLimits(t *testing.T) {
	e := newPubEnv(t, netd.PublicOptions{Routes: []string{"/*"}, Limits: limits(func(l *publicspec.Limits) { l.MaxBodyBytes = 1024 }), Enabled: true}, nil)
	c := e.client(defaultSrc())
	if resp, _ := do(t, c, "POST", e.url("/ok"), bytes.NewReader(make([]byte, 1024)), nil); resp.StatusCode != 200 {
		t.Fatalf("at the cap: %d", resp.StatusCode)
	}
	resp, _ := do(t, c, "POST", e.url("/big"), bytes.NewReader(make([]byte, 2048)), nil)
	if resp.StatusCode != 413 || resp.Header.Get("twd-error") != "payload_too_large" {
		t.Errorf("Content-Length over: %d %s", resp.StatusCode, resp.Header.Get("twd-error"))
	}
	if e.seenCount() != 1 {
		t.Errorf("Content-Length over reached upstream")
	}
	// Chunked (no Content-Length): the capped reader trips while upstream reads.
	pr, pw := io.Pipe()
	go func() {
		pw.Write(make([]byte, 4096))
		pw.Close()
	}()
	req, _ := http.NewRequest("POST", e.url("/chunked"), pr)
	req.ContentLength = -1
	r2, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 413 || r2.Header.Get("twd-error") != "payload_too_large" {
		t.Errorf("chunked over: %d %s", r2.StatusCode, r2.Header.Get("twd-error"))
	}
	if r2.Header.Get("x-upstream") != "" {
		t.Error("upstream's response leaked after a body overflow")
	}
	// Header limit.
	e2 := newPubEnv(t, netd.PublicOptions{Routes: []string{"/*"}, Limits: limits(func(l *publicspec.Limits) { l.MaxHeaderBytes = 512 }), Enabled: true}, nil)
	resp, _ = do(t, e2.client(defaultSrc()), "GET", e2.url("/h"), nil, map[string]string{"X-Big": strings.Repeat("a", 1000)})
	if resp.StatusCode != 431 || resp.Header.Get("twd-error") != "header_too_large" {
		t.Errorf("header over: %d %s", resp.StatusCode, resp.Header.Get("twd-error"))
	}
}

func TestPublicTimeout(t *testing.T) {
	e := newPubEnv(t, netd.PublicOptions{Routes: []string{"/*"}, Limits: limits(func(l *publicspec.Limits) { l.TimeoutSeconds = 1 }), Enabled: true}, nil)
	st := time.Now()
	resp, _ := do(t, e.client(defaultSrc()), "GET", e.url("/slow/hang"), nil, nil)
	if resp.StatusCode != 504 || resp.Header.Get("twd-error") != "request_timeout" {
		t.Errorf("timeout: %d %s", resp.StatusCode, resp.Header.Get("twd-error"))
	}
	if d := time.Since(st); d < 900*time.Millisecond || d > 4*time.Second {
		t.Errorf("timeout after %v", d)
	}
	if r := <-e.recs; r.Code != "request_timeout" || r.Status != 504 {
		t.Errorf("record %+v", r)
	}
}

func TestPublicNonFunnelConnClosed(t *testing.T) {
	e := newPubEnv(t, netd.PublicOptions{Routes: []string{"/*"}, Limits: limits(nil), Enabled: true}, func(e *pubEnv) { e.raw = true })
	c := &http.Client{Timeout: 5 * time.Second}
	if resp, err := c.Get(e.url("/x")); err == nil {
		resp.Body.Close()
		t.Fatalf("a non-Funnel conn was served: %d", resp.StatusCode)
	}
	if e.seenCount() != 0 {
		t.Fatal("reached upstream")
	}
}

func TestPublicMaxConns(t *testing.T) {
	restore := netd.SetPublicRejectWindow(300 * time.Millisecond)
	defer restore()
	e := newPubEnv(t, netd.PublicOptions{Routes: []string{"/*"}, Limits: limits(func(l *publicspec.Limits) { l.MaxConns = 1 }), Enabled: true}, nil)
	e.mu.Lock()
	addr := e.addr
	e.mu.Unlock()
	c1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	fmt.Fprintf(c1, "GET /a HTTP/1.1\r\nHost: x\r\n\r\n")
	if resp, err := http.ReadResponse(bufio.NewReader(c1), nil); err != nil || resp.StatusCode != 200 {
		t.Fatalf("first conn: %v %v", resp, err)
	}
	// c1 is kept alive (an open slot); a second conn is refused.
	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	fmt.Fprintf(c2, "GET /b HTTP/1.1\r\nHost: x\r\n\r\n")
	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := http.ReadResponse(bufio.NewReader(c2), nil); err == nil {
		t.Fatal("second conn served over MaxConns")
	}
	select {
	case s := <-e.rejs:
		if s.ConnsRefused != 1 {
			t.Errorf("summary %+v", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no summary")
	}
	// Closing c1 frees the slot.
	c1.Close()
	time.Sleep(100 * time.Millisecond)
	if resp, _ := do(t, e.client(defaultSrc()), "GET", e.url("/c"), nil, nil); resp.StatusCode != 200 {
		t.Errorf("after the slot freed: %d", resp.StatusCode)
	}
}

func TestSetPublicEnabled(t *testing.T) {
	e := newPubEnv(t, netd.PublicOptions{Routes: []string{"/*"}, Limits: limits(nil), Enabled: true}, nil)
	c := e.client(defaultSrc())
	oldURL := e.url("/slow/hang")
	errc := make(chan error, 1)
	go func() {
		resp, err := c.Get(oldURL)
		if err == nil {
			io.ReadAll(resp.Body)
			resp.Body.Close()
			err = fmt.Errorf("completed with %d", resp.StatusCode)
		}
		errc <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for e.seenCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.seenCount() == 0 {
		t.Fatal("slow request never reached upstream")
	}
	st := time.Now()
	if err := e.reg.SetPublicEnabled(pubID, false); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(st); d > time.Second {
		t.Errorf("SetPublicEnabled(false) took %v", d)
	}
	select {
	case err := <-errc:
		t.Logf("in-flight request after expose off (%v): %v", time.Since(st).Round(time.Millisecond), err)
	case <-time.After(time.Second):
		t.Fatal("in-flight request not reset within 1 s")
	}
	if e.reg.Get(pubID).PublicListening() {
		t.Error("still listening")
	}
	if _, err := (&http.Client{Timeout: 2 * time.Second}).Get(oldURL); err == nil {
		t.Error("closed listener still accepts")
	}
	if err := e.reg.SetPublicEnabled(pubID, false); err != nil {
		t.Errorf("idempotent off: %v", err)
	}
	if err := e.reg.SetPublicEnabled(pubID, true); err != nil {
		t.Fatal(err)
	}
	if err := e.reg.SetPublicEnabled(pubID, true); err != nil {
		t.Errorf("idempotent on: %v", err)
	}
	e.mu.Lock()
	opens := e.opens
	e.mu.Unlock()
	if opens != 2 {
		t.Errorf("listener opened %d times, want 2", opens)
	}
	if resp, _ := do(t, c, "GET", e.url("/again"), nil, nil); resp.StatusCode != 200 {
		t.Errorf("after expose on: %d", resp.StatusCode)
	}
	if err := e.reg.SetPublicEnabled("nope", true); !errors.Is(err, netd.ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestPublicDisabledAtCreateAndKill(t *testing.T) {
	e := newPubEnv(t, netd.PublicOptions{Routes: []string{"/*"}, Limits: limits(nil)}, nil)
	o := e.reg.Get(pubID)
	if o.PublicListening() {
		t.Fatal("listening with Enabled=false")
	}
	e.mu.Lock()
	opens := e.opens
	e.mu.Unlock()
	if opens != 0 {
		t.Fatalf("listener opened %d times", opens)
	}
	if err := e.reg.SetPublicEnabled(pubID, true); err != nil {
		t.Fatal(err)
	}
	if resp, _ := do(t, e.client(defaultSrc()), "GET", e.url("/x"), nil, nil); resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	u := e.url("/x")
	if _, err := e.reg.Kill(context.Background(), pubID, netd.KillOptions{Reason: "test", Tombstone: time.Second}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&http.Client{Timeout: 2 * time.Second}).Get(u); err == nil {
		t.Error("killed public node still serves")
	}
	if err := e.reg.SetPublicEnabled(pubID, true); err == nil {
		t.Error("re-enabled a killed node")
	}
}

// TestPublicRejoinStaysPublic: a public node restored from saved state
// alone (Registry.Rejoin) comes back public but closed, and never listens
// on the tailnet (a peer's dial to its :80 fails).
func TestPublicRejoinStaysPublic(t *testing.T) {
	e := newPubEnv(t, netd.PublicOptions{Routes: []string{"/hooks/*"}, Limits: limits(nil), DeliverTo: "wl-x", Enabled: true}, nil)
	ip := e.reg.Get(pubID).Info().IPs[0]
	e.reg.Close()
	reg2 := e.newReg()
	e.reg = reg2
	ids, err := reg2.Rejoin()
	if err != nil || len(ids) != 1 {
		t.Fatalf("Rejoin: %v %v", ids, err)
	}
	o := reg2.Get(pubID)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := o.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	in := o.Info()
	if !in.Public || in.PublicListening || !in.Rejoined {
		t.Fatalf("after rejoin: %+v", in)
	}
	// No tailnet listener: dial its :80 from a peer.
	peer, err := nettest.NewNode(ctx, e.ctl.URL, nettest.DummyKey, "peer", newRundir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if err := peer.WaitPeers(ctx, ip); err != nil {
		t.Fatal(err)
	}
	dctx, dcancel := context.WithTimeout(ctx, 3*time.Second)
	if c, err := peer.Dial(dctx, "tcp", ip+":80"); err == nil {
		c.Close()
		t.Error("a public node accepted a tailnet connection on :80")
	}
	dcancel()
	if err := reg2.SetPublicEnabled(pubID, true); err != nil {
		t.Fatal(err)
	}
	if resp, _ := do(t, e.client(defaultSrc()), "GET", e.url("/hooks/a"), nil, nil); resp.StatusCode != 200 {
		t.Fatalf("after re-enable: %d", resp.StatusCode)
	}
	if r := e.lastSeen(); r.Header.Get(netd.HeaderObject) != "wl-x" {
		t.Errorf("DeliverTo not restored: %q", r.Header.Get(netd.HeaderObject))
	}
	if resp, _ := do(t, e.client(defaultSrc()), "GET", e.url("/other"), nil, nil); resp.StatusCode != 404 {
		t.Errorf("routes not restored: %d", resp.StatusCode)
	}
}
