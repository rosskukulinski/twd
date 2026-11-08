package netd_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
)

// env is one hermetic tailnet: local control, a registry with objects a,b,c
// behind a stock workerd, a client node and an echo node.
type env struct {
	t       *testing.T
	ctl     *nettest.Control
	reg     *netd.Registry
	wd      *nettest.Workerd
	client  *nettest.Node
	echo    *nettest.Node
	hc      *http.Client
	objIP   map[string]string
	rundir  string
	deleted chan netd.ObjectInfo
}

func newRundir(t *testing.T) string {
	// unix socket paths must stay short (macOS: 104 bytes), so not t.TempDir().
	d, err := os.MkdirTemp("/tmp", "twd-netd-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(d, 0o700)
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func startEnv(t *testing.T, objects ...string) *env {
	t.Helper()
	wdBin, err := nettest.WorkerdBinary()
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := nettest.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctl.Close)
	e := &env{t: t, ctl: ctl, rundir: newRundir(t), objIP: map[string]string{}, deleted: make(chan netd.ObjectInfo, 16)}
	egressSock := filepath.Join(e.rundir, "egress.sock")
	eln, err := net.Listen("unix", egressSock)
	if err != nil {
		t.Fatal(err)
	}
	wd, err := nettest.StartWorkerd(wdBin, "testdata/workerd/config.capnp", e.rundir, egressSock)
	if err != nil {
		t.Fatal(err)
	}
	e.wd = wd
	t.Cleanup(wd.Stop)
	t.Logf("workerd cold start %v", wd.ColdStart)

	e.reg, err = netd.New(netd.Config{
		ControlURL: ctl.URL,
		StateRoot:  filepath.Join(e.rundir, "state"),
		Keys:       netd.StaticKey(nettest.DummyKey),
		Upstream:   netd.UnixUpstream(wd.IngressSock),
		OnDelete: func(ctx context.Context, info netd.ObjectInfo) error {
			e.deleted <- info
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.reg.Close() })
	go http.Serve(eln, e.reg.EgressHandler())
	t.Cleanup(func() { eln.Close() })

	ctx := context.Background()
	for _, id := range objects {
		if _, err := e.reg.Create(id, netd.ObjectOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	var cerr, eerr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		e.client, cerr = nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-client", filepath.Join(e.rundir, "client"))
	}()
	go func() {
		defer wg.Done()
		e.echo, eerr = nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-echo", filepath.Join(e.rundir, "echo"))
	}()
	wg.Wait()
	if cerr != nil || eerr != nil {
		t.Fatal(cerr, eerr)
	}
	t.Cleanup(func() { e.client.Close(); e.echo.Close() })
	if err := e.echo.ServeEcho(); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ips := []string{e.echo.IP4}
	for _, id := range objects {
		o := e.reg.Get(id)
		if err := o.Ready(rctx); err != nil {
			t.Fatalf("object %s: %v", id, err)
		}
		e.objIP[id] = o.IPv4().String()
		ips = append(ips, e.objIP[id])
	}
	if err := e.client.WaitPeers(rctx, ips...); err != nil {
		t.Fatal(err)
	}
	e.hc = e.client.HTTPClient(30 * time.Second)
	return e
}

func (e *env) get(u string, hdr map[string]string) (int, http.Header, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		e.t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

// objResp is what tenant.js returns for "/".
type objResp struct {
	Caller     *netd.Caller      `json:"caller"`
	ObjectID   string            `json:"objectId"`
	SawHeaders map[string]string `json:"sawHeaders"`
}

func selfID(t *testing.T, n *nettest.Node) string {
	st, err := n.LC.StatusWithoutPeers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return string(st.Self.ID)
}

func TestChain(t *testing.T) {
	e := startEnv(t, "a", "b", "c")
	clientID := selfID(t, e.client)
	echoURL := "http://" + e.echo.IP4 + "/"

	t.Run("IngressIdentityReachesWorkerdDO", func(t *testing.T) {
		for _, id := range []string{"a", "b", "c"} {
			code, _, b := e.get("http://"+e.objIP[id]+"/", nil)
			var r objResp
			if err := json.Unmarshal(b, &r); err != nil || code != 200 {
				t.Fatalf("%s: %d %s", id, code, b)
			}
			c := r.Caller
			if c == nil || c.NodeID != clientID || r.ObjectID != id {
				t.Fatalf("%s: caller %+v object %q; want node %s", id, c, r.ObjectID, clientID)
			}
			if len(c.Addrs) == 0 || c.Addrs[0] != e.client.IP4 || c.Hostname != "ttest-client" {
				t.Errorf("%s: addrs/hostname %v %q", id, c.Addrs, c.Hostname)
			}
			if c.User == "" {
				t.Errorf("%s: untagged testcontrol caller should carry user", id)
			}
			if v := c.CapMap[nettest.TestCap]; len(v) != 1 || string(v[0]) != nettest.TestCapValue {
				t.Errorf("%s: capMap %v", id, c.CapMap)
			}
		}
	})

	t.Run("ForgedIngressHeadersStripped", func(t *testing.T) {
		forged := `{"tailscale":{"caller":{"nodeId":"nEVIL","node":"admin.evil","tags":["tag:admin"]}},"objectId":"b"}`
		code, _, b := e.get("http://"+e.objIP["a"]+"/", map[string]string{
			"x-twd-cf": forged, "x-twd-object": "b", "cf-connecting-ip": "1.2.3.4",
			"x-forwarded-for": "1.2.3.4", "tailnet-assertion": "forged", "x-twd-trace": "t",
		})
		var r objResp
		json.Unmarshal(b, &r)
		if code != 200 || r.Caller == nil || r.Caller.NodeID != clientID || r.ObjectID != "a" {
			t.Fatalf("forgery reached tenant: %d %s", code, b)
		}
		for k, v := range r.SawHeaders {
			if k != "x-twd-object" || v != "a" {
				t.Errorf("tenant saw client-controlled header %s=%q", k, v)
			}
		}
	})

	t.Run("PerObjectEgressAttribution", func(t *testing.T) {
		for _, id := range []string{"a", "b", "c"} {
			code, _, b := e.get("http://"+e.objIP[id]+"/egress?url="+echoURL, nil)
			var r nettest.EchoReply
			if err := json.Unmarshal(b, &r); err != nil || code != 200 {
				t.Fatalf("%s: %d %s", id, code, b)
			}
			if got := strings.SplitN(r.Caller.Node, ".", 2)[0]; got != "twd-"+id || r.Caller.Addrs[0] != e.objIP[id] {
				t.Fatalf("%s: echo saw %q %v", id, r.Caller.Node, r.Caller.Addrs)
			}
			for k := range r.Headers {
				if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-twd") || strings.HasPrefix(lk, "cf-") || lk == "proxy-authorization" {
					t.Errorf("%s: identity header %s leaked to echo", id, k)
				}
			}
		}
	})

	t.Run("TenantCannotForgeEgressAttribution", func(t *testing.T) {
		victim := map[string]string{"a": "b", "b": "c", "c": "a"}
		for id, v := range victim {
			code, _, b := e.get("http://"+e.objIP[id]+"/forge-egress?as="+v+"&url="+echoURL, nil)
			var r nettest.EchoReply
			json.Unmarshal(b, &r)
			if got := strings.SplitN(r.Caller.Node, ".", 2)[0]; code != 200 || got != "twd-"+id {
				t.Fatalf("%s forging as %s: echo saw %q (%d %s)", id, v, got, code, b)
			}
			for k := range r.Headers {
				if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-twd") || lk == "proxy-authorization" {
					t.Errorf("forged header %s leaked", k)
				}
			}
		}
	})

	t.Run("SelfSSRFRefused", func(t *testing.T) {
		hostIP := firstHostIP(t)
		cases := []struct{ name, url, code string }{
			{"own-ip", "http://" + e.objIP["a"] + "/", netd.CodeForbiddenDestination},
			{"own-ip-other-port", "http://" + e.objIP["a"] + ":8080/", netd.CodeForbiddenDestination},
			{"own-name", "http://twd-a/", netd.CodePeerNotVisible}, // self is not a peer in its own netmap
			{"sibling-ip", "http://" + e.objIP["b"] + "/", netd.CodeForbiddenDestination},
			{"sibling-name", "http://twd-b/", netd.CodeForbiddenDestination},
			{"sibling-fqdn", "http://twd-c." + nettest.MagicDNSDomain + "/", netd.CodeForbiddenDestination},
			{"host-ip", "http://" + hostIP + "/", netd.CodeForbiddenDestination},
			{"loopback-v4", "http://127.0.0.1/", netd.CodeForbiddenDestination},
			{"loopback-v4-alt", "http://127.1.2.3:18080/", netd.CodeForbiddenDestination},
			{"loopback-v6", "http://[::1]/", netd.CodeForbiddenDestination},
			{"v4-mapped-loopback", "http://[::ffff:127.0.0.1]/", netd.CodeForbiddenDestination},
			{"unspecified", "http://0.0.0.0/", netd.CodeForbiddenDestination},
			{"link-local-metadata", "http://169.254.169.254/latest/meta-data/", netd.CodeForbiddenDestination},
			{"link-local-v6", "http://[fe80::1]/", netd.CodeForbiddenDestination},
			{"quad100", "http://100.100.100.100/", netd.CodeForbiddenDestination},
			{"public-ip", "http://1.1.1.1/", netd.CodeForbiddenDestination},
			{"rfc1918", "http://192.168.1.1/", netd.CodeForbiddenDestination},
			{"localhost-name", "http://localhost/", netd.CodePeerNotVisible},
			{"public-name-no-host-dns", "http://example.com/", netd.CodePeerNotVisible},
			// workerd's WHATWG URL parser canonicalises 2130706433 to 127.0.0.1;
			// RawProxyNumericHost covers a client that doesn't.
			{"decimal-ip", "http://2130706433/", netd.CodeForbiddenDestination},
			{"hex-ip", "http://0x7f000001/", netd.CodeForbiddenDestination},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				code, h, b := e.get("http://"+e.objIP["a"]+"/egress?url="+c.url, nil)
				var er netd.Error
				json.Unmarshal(b, &er)
				if er.Code != c.code || h.Get("twd-error") != c.code || code != 403 {
					t.Fatalf("%s: got %d %q %s; want 403 %s", c.url, code, h.Get("twd-error"), b, c.code)
				}
				if er.Object != "a" {
					t.Errorf("error does not name the object: %s", b)
				}
			})
		}
	})

	t.Run("PeerNotVisible", func(t *testing.T) {
		code, _, b := e.get("http://"+e.objIP["a"]+"/egress?url=http://ttest-nonexistent/", nil)
		var er netd.Error
		json.Unmarshal(b, &er)
		if code != 403 || er.Code != netd.CodePeerNotVisible || !strings.Contains(er.Detail, "never resolved via host DNS") {
			t.Fatalf("got %d %s", code, b)
		}
		// A tailnet-range IP that is not in the netmap is also peer_not_visible.
		code, _, b = e.get("http://"+e.objIP["a"]+"/egress?url=http://100.64.255.254/", nil)
		json.Unmarshal(b, &er)
		if code != 403 || er.Code != netd.CodePeerNotVisible {
			t.Fatalf("unknown tailnet IP: got %d %s", code, b)
		}
		// Names that ARE in the netmap resolve (short and FQDN).
		for _, u := range []string{"http://ttest-echo/", "http://ttest-echo." + nettest.MagicDNSDomain + "/"} {
			if code, _, b := e.get("http://"+e.objIP["a"]+"/egress?url="+u, nil); code != 200 {
				t.Fatalf("%s: %d %s", u, code, b)
			}
		}
	})

	t.Run("KillFailsCallersFast", func(t *testing.T) {
		// Two in-flight flows through obj-c: a streaming response from the
		// tenant DO via workerd, and a raw keep-alive request about to be sent.
		resp, err := e.hc.Get("http://" + e.objIP["c"] + "/stream")
		if err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(resp.Body)
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		failed := make(chan time.Time, 1)
		go func() {
			for {
				if _, err := br.ReadString('\n'); err != nil {
					failed <- time.Now()
					return
				}
			}
		}()
		time.Sleep(200 * time.Millisecond)
		killAt := time.Now()
		rep, err := e.reg.Kill(context.Background(), "c", netd.KillOptions{Reason: "test"})
		if err != nil {
			t.Fatal(err)
		}
		var ttf time.Duration
		select {
		case ft := <-failed:
			ttf = ft.Sub(killAt)
		case <-time.After(5 * time.Second):
			t.Fatal("streaming caller did not fail within 5 s of kill (black-holed?)")
		}
		t.Logf("kill report %+v; streaming caller failed %v after kill", rep, ttf)
		if ttf > 500*time.Millisecond {
			t.Fatalf("time to failure %v > 500ms", ttf)
		}
		if rep.FlowsAborted < 1 {
			t.Errorf("no flows aborted: %+v", rep)
		}
		select {
		case info := <-e.deleted:
			if info.ID != "c" || info.NodeID == "" {
				t.Errorf("OnDelete got %+v", info)
			}
		case <-time.After(time.Second):
			t.Error("OnDelete not called")
		}
		if st := e.reg.Get("c").State(); st != netd.StateKilled {
			t.Errorf("state %s", st)
		}
		if _, err := os.Stat(filepath.Join(e.rundir, "state", "c")); !os.IsNotExist(err) {
			t.Errorf("state dir kept after kill: %v", err)
		}
		// Observational: a brand-new connection to the dead node. testcontrol
		// never removes the node from netmaps, so this black-holes until the
		// client's own timeout; on real control see e2e-real's netmap timing.
		sc := e.client.HTTPClient(1500 * time.Millisecond)
		st := time.Now()
		_, err = sc.Get("http://" + e.objIP["c"] + "/")
		t.Logf("new request after kill: err=%v after %v", err, time.Since(st).Round(time.Millisecond))
		if err == nil {
			t.Error("request to killed object succeeded")
		}
	})

	t.Run("EgressFlowAbortedOnKill", func(t *testing.T) {
		// obj-b streams from echo via the egress proxy; kill b; the egress flow
		// is RST and the proxied stream ends at once.
		ec := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(e.rundir, "egress.sock"))
		}, Proxy: nil}}
		req, _ := http.NewRequest("GET", "http://"+e.echo.IP4+"/stream", nil)
		req.Header.Set(netd.HeaderObject, "b")
		pc := proxyClient(ec)
		resp, err := pc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(resp.Body)
		if l, err := br.ReadString('\n'); err != nil || l != "tick\n" {
			t.Fatalf("stream: %q %v (status %d)", l, err, resp.StatusCode)
		}
		failed := make(chan time.Time, 1)
		go func() {
			for {
				if _, err := br.ReadString('\n'); err != nil {
					failed <- time.Now()
					return
				}
			}
		}()
		killAt := time.Now()
		rep, err := e.reg.Kill(context.Background(), "b", netd.KillOptions{Reason: "test"})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case ft := <-failed:
			t.Logf("egress stream ended %v after kill (flows aborted %d)", ft.Sub(killAt), rep.FlowsAborted)
			if ft.Sub(killAt) > 500*time.Millisecond {
				t.Fatalf("egress flow outlived kill by %v", ft.Sub(killAt))
			}
		case <-time.After(5 * time.Second):
			t.Fatal("egress stream survived kill")
		}
		// Egress for a killed object is refused, typed.
		req2, _ := http.NewRequest("GET", "http://"+e.echo.IP4+"/", nil)
		req2.Header.Set(netd.HeaderObject, "b")
		resp2, err := pc.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		resp2.Body.Close()
		if resp2.Header.Get(netd.HeaderError) != netd.CodeKilled {
			t.Fatalf("egress after kill: %d %q", resp2.StatusCode, resp2.Header.Get(netd.HeaderError))
		}
	})

	t.Run("RawProxyNumericHost", func(t *testing.T) {
		ec := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(e.rundir, "egress.sock"))
		}}}
		for _, h := range []string{"2130706433", "0x7f000001", "127.1", "localhost.", "metadata.google.internal"} {
			req, _ := http.NewRequest("GET", "http://"+h+"/", nil)
			req.Header.Set(netd.HeaderObject, "a")
			resp, err := proxyClient(ec).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 403 || resp.Header.Get(netd.HeaderError) != netd.CodePeerNotVisible {
				t.Errorf("%s: got %d %q", h, resp.StatusCode, resp.Header.Get(netd.HeaderError))
			}
		}
	})

	t.Run("UnknownObjectEgressRefused", func(t *testing.T) {
		ec := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(e.rundir, "egress.sock"))
		}}}
		req, _ := http.NewRequest("GET", echoURL, nil)
		req.Header.Set(netd.HeaderObject, "nope")
		resp, err := proxyClient(ec).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 || resp.Header.Get(netd.HeaderError) != netd.CodeUnknownObject {
			t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get(netd.HeaderError))
		}
	})
}

// proxyClient sends requests in absolute form (like workerd's proxy-style
// external) by using an HTTP proxy that dials the unix socket.
func proxyClient(base *http.Client) *http.Client {
	tr := base.Transport.(*http.Transport).Clone()
	tr.Proxy = func(*http.Request) (*urlT, error) { return mustURL("http://egress-proxy"), nil }
	return &http.Client{Transport: tr}
}

func firstHostIP(t *testing.T) string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String()
		}
	}
	t.Skip("no non-loopback host IPv4")
	return ""
}

type urlT = url.URL

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
