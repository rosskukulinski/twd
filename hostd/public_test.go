package hostd_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

// fakeFunnel stands in for tsnet's ListenFunnel: testcontrol issues no
// certificates, so each public node gets a loopback listener whose conns
// are *ipn.FunnelConn with a fixed public source address (plain HTTP; real
// Funnel terminates TLS on the node).
type fakeFunnel struct {
	mu    sync.Mutex
	addrs map[string]string // node hostname -> listener address
	src   netip.AddrPort
}

type funnelLn struct {
	net.Listener
	src netip.AddrPort
}

func (l *funnelLn) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &ipn.FunnelConn{Conn: c, Src: l.src}, nil
}

func (f *fakeFunnel) listen(srv *tsnet.Server) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.addrs[srv.Hostname] = ln.Addr().String()
	f.mu.Unlock()
	return &funnelLn{Listener: ln, src: f.src}, nil
}

func (f *fakeFunnel) addr(hostname string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addrs[hostname]
}

func publicStatus(e *hostdtest.Env, workload string) *hostd.PublicStatus {
	for _, w := range e.Host.Status().Workloads {
		if w.Workload == workload {
			return w.Public
		}
	}
	return nil
}

// TestPublicIngress is the hermetic ADR-029 suite: a workload with public
// routes gets a public node (namespace _public, named after the workload)
// whose Funnel listener delivers to the workload's default export with an
// unforgeable {kind:"public"} requester, never a tailnet caller.
func TestPublicIngress(t *testing.T) {
	ff := &fakeFunnel{addrs: map[string]string{}, src: netip.MustParseAddrPort("203.0.113.7:40000")}
	s := startSuiteCapsOpts(t, fakemgmt.Config{}, nil, func(o *hostdtest.Options) { o.PublicListen = ff.listen })
	e := s.e
	d := e.Deploy(hostdtest.Fixture("pubprobe"), "pubprobe", nil)
	wl := e.WaitActive("pubprobe", d, 90*time.Second)
	s.waitPeer(wl.IP)

	// The public node comes up (after the workload node) and is named after
	// the workload.
	var ps *hostd.PublicStatus
	deadline := time.Now().Add(60 * time.Second)
	for {
		ps = publicStatus(e, "pubprobe")
		if ps != nil && ps.Status == "ready" && ff.addr(ps.Hostname) != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("public node not ready: %+v", ps)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ps.Hostname != "pubprobe" || !ps.Exposed {
		t.Fatalf("public node: %+v (want hostname pubprobe, exposed)", ps)
	}
	pubObj, _ := e.FM.Object(ps.ObjectID)
	if pubObj.Namespace != mgmtapi.PublicNamespace || len(pubObj.Tags) != 1 || pubObj.Tags[0] != "tag:twd-public" {
		t.Fatalf("public object: ns %q tags %v", pubObj.Namespace, pubObj.Tags)
	}
	paddr := ff.addr(ps.Hostname)
	pc := &http.Client{Timeout: 20 * time.Second}
	pub := func(method, path string, body []byte, hdr ...string) (int, http.Header, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://"+paddr+path, bytes.NewReader(body))
		req.Host = "pubprobe.ttest.ts.net"
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := pc.Do(req)
		if err != nil {
			return 0, nil, map[string]any{"err": err.Error()}
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			m = map[string]any{"raw": string(b)}
		}
		return resp.StatusCode, resp.Header, m
	}

	// 1. Identity: public, from the transport; spoofed headers never land.
	code, _, r := pub("GET", "/hook/echo?token=secret", nil,
		"Tailscale-User-Login", "evil@example.com", "X-Forwarded-For", "6.6.6.6", "CF-Connecting-IP", "6.6.6.6",
		"x-twd-cf", `{"tailscale":{"caller":{"nodeId":"nFORGED","tags":["tag:admin"]}}}`)
	if code != 200 {
		t.Fatalf("public /hook/echo: %d %v", code, r)
	}
	if get(r, "requester", "kind") != "public" || get(r, "requester", "ip") != "203.0.113.7" || get(r, "requester", "proto") != "https" {
		t.Fatalf("requester: %s", js(r["requester"]))
	}
	if get(r, "requester", "nodeId") != nil || r["callerError"] != "public_request" {
		t.Fatalf("a public request must not yield a tailnet caller: %s", js(r))
	}
	seen, _ := r["seen"].(map[string]any)
	if seen["cf-connecting-ip"] != "203.0.113.7" || seen["x-forwarded-for"] != "203.0.113.7" || seen["x-forwarded-proto"] != "https" {
		t.Fatalf("forwarding headers: %s", js(seen))
	}
	if _, ok := seen["tailscale-user-login"]; ok {
		t.Fatalf("spoofed identity header reached the app: %s", js(seen))
	}
	if strings.Contains(js(seen), "nFORGED") {
		t.Fatalf("forged cf blob reached the app: %s", js(seen))
	}

	// The same handler over the tailnet still sees the tailnet caller.
	tcode, tr := s.getJSON("http://" + wl.IP + "/hook/echo")
	if tcode != 200 || get(tr, "requester", "kind") != "tailnet" || get(tr, "requester", "nodeId") != s.clientID || tr["callerError"] != nil {
		t.Fatalf("tailnet /hook/echo: %d %s", tcode, js(tr))
	}

	// 2. Routes: only the listed prefixes; /_twd and odd paths never.
	for _, c := range []struct {
		path string
		want int
		code string
	}{
		{"/private", 404, "not_public"},
		{"/", 404, "not_public"},
		{"/_twd/o/Room/alice/", 0, ""},
		{"/hook/../private", 400, "bad_request"},
		{"/hook%2F..%2Fprivate", 400, "bad_request"},
	} {
		code, h, m := pub("GET", c.path, nil)
		if c.want == 0 {
			if code < 400 || code >= 500 {
				t.Errorf("%s: %d %v (want a 4xx refusal)", c.path, code, m)
			}
			continue
		}
		if code != c.want || h.Get("twd-error") != c.code {
			t.Errorf("%s: %d %q %v (want %d %s)", c.path, code, h.Get("twd-error"), m, c.want, c.code)
		}
	}
	if tcode, _ := s.getJSON("http://" + wl.IP + "/private"); tcode != 200 {
		t.Fatalf("tailnet /private: %d", tcode)
	}

	// 3. Spawn amplification: existing objects only, unless the class opts in.
	if code, r := s.getJSON("http://" + wl.IP + "/agents/room/alice"); code != 200 {
		t.Fatalf("tailnet create room/alice: %d %v", code, r)
	}
	alice := e.WaitObject("pubprobe", "Room", "alice", 30*time.Second)
	if code, _, r := pub("GET", "/agents/room/alice", nil); code != 200 || r["object"] != "alice" || get(r, "requester", "kind") != "public" {
		t.Fatalf("public -> existing room/alice: %d %v", code, r)
	}
	before := len(e.FM.Objects())
	code, h, r := pub("GET", "/agents/room/bob", nil)
	if code != 403 || h.Get("twd-error") != "public_create_denied" {
		t.Fatalf("public -> new room/bob: %d %q %v (want 403 public_create_denied)", code, h.Get("twd-error"), r)
	}
	time.Sleep(500 * time.Millisecond)
	if o, _ := e.Host.Object(e.TenantID, "pubprobe", "Room", "bob"); o.ID != "" {
		t.Fatalf("room/bob was created by a public request: %+v", o)
	}
	if n := len(e.FM.Objects()); n != before {
		t.Fatalf("mgmt objects %d -> %d after a refused public create", before, n)
	}
	for _, n := range []string{"a", "b"} {
		if code, _, r := pub("GET", "/agents/open/"+n, nil); code != 200 {
			t.Fatalf("public -> new open/%s (opted in): %d %v", n, code, r)
		}
		e.WaitObject("pubprobe", "Open", n, 30*time.Second)
	}
	if code, h, r := pub("GET", "/agents/open/c", nil); code != 403 || h.Get("twd-error") != "public_create_denied" {
		t.Fatalf("public -> open/c over maxObjects 2: %d %q %v", code, h.Get("twd-error"), r)
	}
	if code, _, r := pub("GET", "/agents/open/a", nil); code != 200 {
		t.Fatalf("public -> existing open/a: %d %v", code, r)
	}
	if code, _, r := pub("GET", "/spawn", nil); code != 299 || r["code"] != "public_create_denied" {
		t.Fatalf("public spawn: %d %v (want public_create_denied)", code, r)
	}

	// 4. Body limit.
	if code, h, _ := pub("POST", "/big", bytes.Repeat([]byte("x"), 4096)); code != 413 || h.Get("twd-error") != "payload_too_large" {
		t.Fatalf("4 KiB body over a 1 KiB limit: %d %q", code, h.Get("twd-error"))
	}
	if code, _, r := pub("POST", "/big", bytes.Repeat([]byte("x"), 512)); code != 200 || r["len"] != float64(512) {
		t.Fatalf("512 B body: %d %v", code, r)
	}

	// 5. Audit: every public request (incl. refusals by route or by the
	// create guard), path without its query string. hostd ships audit
	// records asynchronously, so poll.
	var n, created int
	var sawBob bool
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		n, created, sawBob = 0, 0, false
		for _, rec := range e.Audit(ps.ObjectID).Items {
			if rec.Kind != "public_request" {
				continue
			}
			n++
			d := string(rec.Detail)
			if strings.Contains(d, "secret") || strings.Contains(d, "token=") {
				t.Fatalf("query string in audit: %s", d)
			}
			if rec.ActingFor != hostd.OriginPublic {
				t.Fatalf("public_request actingFor %q", rec.ActingFor)
			}
			if strings.Contains(d, `"/agents/room/bob"`) && strings.Contains(d, `"status":403`) {
				sawBob = true
			}
		}
		for _, rec := range e.Audit(wl.ID).Items {
			if rec.Kind == "public_object_created" {
				created++
				if rec.ActingFor != hostd.OriginPublic {
					t.Fatalf("public_object_created actingFor %q", rec.ActingFor)
				}
			}
		}
		if n >= 13 && created == 2 && sawBob {
			break
		}
	}
	if n < 13 || created != 2 || !sawBob {
		t.Fatalf("audit: %d public_request records (want ≥ 13), %d public_object_created (want 2), refused room/bob audited: %v", n, created, sawBob)
	}
	_ = alice

	// 6. Per-IP rate limit (ipRps 2, burst 40): a burst gets 429 + Retry-After.
	got429 := false
	for i := 0; i < 80 && !got429; i++ {
		code, h, _ := pub("GET", "/hook/x", nil)
		if code == 429 {
			got429 = h.Get("Retry-After") != "" && h.Get("twd-error") == "rate_limited"
		}
	}
	if !got429 {
		t.Fatal("no 429 rate_limited with Retry-After after 80 requests at ipBurst 40")
	}
	time.Sleep(1500 * time.Millisecond) // refill a little

	// 7. twd expose off: the listener closes and an in-flight stream is cut
	// within a second; on reopens it.
	conn, err := net.Dial("tcp", paddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /slow HTTP/1.1\r\nHost: pubprobe.ttest.ts.net\r\n\r\n")
	br := bufio.NewReader(conn)
	if line, err := br.ReadString('\n'); err != nil || !strings.Contains(line, "200") {
		t.Fatalf("/slow: %q %v", line, err)
	}
	st := time.Now()
	e.FM.SetExposed(e.TenantID, "pubprobe", false)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, err := br.ReadString('\n'); err != nil {
			break
		}
	}
	cut := time.Since(st)
	if cut > time.Second {
		t.Fatalf("expose off cut the in-flight stream after %v (want < 1 s)", cut)
	}
	t.Logf("expose off: in-flight public stream cut in %v", cut)
	if c, err := net.DialTimeout("tcp", paddr, time.Second); err == nil {
		c.Close()
		// A listener on the same address must not answer any more.
		if code, _, _ := pub("GET", "/hook/x", nil); code != 0 {
			t.Fatalf("public request answered %d after expose off", code)
		}
	}
	e.FM.SetExposed(e.TenantID, "pubprobe", true)
	deadline = time.Now().Add(10 * time.Second)
	for {
		paddr = ff.addr(ps.Hostname)
		if code, _, _ := pub("GET", "/hook/x", nil); code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("public listener did not reopen after expose on")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// hostd ships audit records asynchronously: poll.
	kinds := map[string]bool{}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		kinds = map[string]bool{}
		for _, rec := range e.Audit(ps.ObjectID).Items {
			kinds[rec.Kind] = true
		}
		if kinds["expose_off"] && kinds["expose_on"] {
			break
		}
	}
	if !kinds["expose_off"] || !kinds["expose_on"] {
		t.Fatalf("expose audit records missing: %v", kinds)
	}
	_ = context.Background
}
