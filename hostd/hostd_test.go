package hostd_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/tailcfg"
)

const mcpCap = "example.com/cap/mcp"

type suite struct {
	t        *testing.T
	ctl      *nettest.Control
	e        *hostdtest.Env
	client   *nettest.Node
	echo     *nettest.Node
	hc       *http.Client
	clientID string
}

func startSuite(t *testing.T, fake fakemgmt.Config) *suite {
	ctl, err := nettest.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctl.Close)
	// Every node gets the test cap; MCP callers get lookup_order only.
	ctl.Server.SetGlobalAppCaps(tailcfg.PeerCapMap{
		nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue},
		mcpCap:          []tailcfg.RawMessage{`{"tools":["lookup_order"]}`},
	})
	fake.StaticKey = nettest.DummyKey
	e := hostdtest.Start(t, hostdtest.Options{ControlURL: ctl.URL, Fake: fake, Ephemeral: true, KillTombstone: 5 * time.Second,
		Tenant: mgmtapi.Tenant{Tailnet: "ttest.ts.net", ControlURL: ctl.URL, MintMode: "static"}})
	s := &suite{t: t, ctl: ctl, e: e}
	ctx := context.Background()
	var wg sync.WaitGroup
	var cerr, eerr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.client, cerr = nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-client", filepath.Join(e.Dir, "client"))
	}()
	go func() {
		defer wg.Done()
		s.echo, eerr = nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-echo", filepath.Join(e.Dir, "echo"))
	}()
	wg.Wait()
	if cerr != nil || eerr != nil {
		t.Fatal(cerr, eerr)
	}
	t.Cleanup(func() { s.client.Close(); s.echo.Close() })
	if err := s.echo.ServeEcho(); err != nil {
		t.Fatal(err)
	}
	st, _ := s.client.LC.StatusWithoutPeers(ctx)
	s.clientID = string(st.Self.ID)
	s.hc = s.client.HTTPClient(30 * time.Second)
	return s
}

func (s *suite) waitPeer(ip string) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.client.WaitPeers(ctx, ip); err != nil {
		s.t.Fatal(err)
	}
}

func (s *suite) do(method, url string, body []byte, hdr ...string) (int, http.Header, []byte) {
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return 0, nil, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func (s *suite) getJSON(url string, hdr ...string) (int, map[string]any) {
	code, _, b := s.do("GET", url, nil, hdr...)
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		m = map[string]any{"raw": string(b)}
	}
	return code, m
}

func get(m map[string]any, path ...string) any {
	var v any = m
	for _, p := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[p]
	}
	return v
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

// TestHostd is the hermetic M3 suite: testcontrol + local DERP + fakemgmt +
// twd-hostd (stock workerd) + tsnet client/echo nodes.
func TestHostd(t *testing.T) {
	s := startSuite(t, fakemgmt.Config{})
	e := s.e
	echoURL := "http://ttest-echo/"

	// Deploy all apps up front; nodes come up concurrently.
	dHello := e.Deploy(hostdtest.Template("hello"), "hello", map[string]string{"UPSTREAM": echoURL})
	dProbe := e.Deploy(hostdtest.Fixture("probe"), "probe", nil)
	dMCP := e.Deploy(hostdtest.Template("mcp"), "mcp", nil)
	dAgent := e.Deploy(hostdtest.Template("agent"), "agent", nil)
	hello := e.WaitActive("hello", dHello, 60*time.Second)
	probe := e.WaitActive("probe", dProbe, 60*time.Second)
	mcp := e.WaitActive("mcp", dMCP, 60*time.Second)
	agent := e.WaitActive("agent", dAgent, 60*time.Second)
	for _, o := range []string{hello.IP, probe.IP, mcp.IP, agent.IP} {
		s.waitPeer(o)
	}

	t.Run("HelloVerifiedCaller", func(t *testing.T) {
		code, r := s.getJSON("http://" + hello.IP + "/")
		if code != 200 || get(r, "caller", "nodeId") != s.clientID {
			t.Fatalf("%d %v", code, r)
		}
		// The Worker's private fetch left from the workload node.
		if n, _ := get(r, "upstream", "body", "caller", "node").(string); !strings.HasPrefix(n, hello.Hostname+".") {
			t.Fatalf("hello egress attributed to %q, want %s: %v", n, hello.Hostname, r)
		}
	})

	var objs = []string{"a", "b", "c"}
	objIP := map[string]string{}
	t.Run("ObjectsGetOwnNodes", func(t *testing.T) {
		// First request via the workload entry node reaches the object at
		// once, before its node exists (creation is async).
		for _, n := range objs {
			st := time.Now()
			code, r := s.getJSON("http://" + probe.IP + "/probe/" + n + "/")
			if code != 200 || r["name"] != n || get(r, "caller", "nodeId") != s.clientID {
				t.Fatalf("via workload %s: %d %v", n, code, r)
			}
			t.Logf("object %s first request via workload: %v", n, time.Since(st))
		}
		seen := map[string]bool{}
		for _, n := range objs {
			o := e.WaitObject("probe", "Probe", n, 60*time.Second)
			if seen[o.IP] || o.IP == probe.IP {
				t.Fatalf("object %s shares an IP: %+v", n, o)
			}
			seen[o.IP] = true
			objIP[n] = o.IP
			s.waitPeer(o.IP)
			code, r := s.getJSON("http://" + o.IP + "/")
			if code != 200 || r["name"] != n || get(r, "caller", "nodeId") != s.clientID || r["objectId"] != o.NetdID {
				t.Fatalf("direct %s: %d %v", n, code, r)
			}
			fo, ok := e.FM.Object(o.ID)
			if !ok || fo.Status != "ready" || fo.NodeID == "" {
				t.Fatalf("mgmt object %s: %+v", o.ID, fo)
			}
		}
	})

	t.Run("EgressAttributedPerObject", func(t *testing.T) {
		for _, n := range objs {
			o, _ := e.Host.Object(e.TenantID, "probe", "Probe", n)
			code, r := s.getJSON("http://" + objIP[n] + "/egress?url=" + echoURL)
			node, _ := get(r, "caller", "node").(string)
			if code != 200 || !strings.HasPrefix(node, o.Hostname+".") {
				t.Fatalf("%s egress: %d %v (want %s)", n, code, r, o.Hostname)
			}
			// Typed helper: same attribution.
			code, r = s.getJSON("http://" + objIP[n] + "/tegress?url=" + echoURL)
			if node, _ := get(r, "caller", "node").(string); code != 200 || !strings.HasPrefix(node, o.Hostname+".") {
				t.Fatalf("%s ctx.tailnet.fetch: %d %v", n, code, r)
			}
		}
		// Typed errors from ctx.tailnet.fetch.
		code, r := s.getJSON("http://" + objIP["a"] + "/tegress?url=http://nonexistent-peer/")
		if code != 299 || r["thrown"] != true || r["code"] != "peer_not_visible" {
			t.Fatalf("typed error: %d %v", code, r)
		}
		code, r = s.getJSON("http://" + objIP["a"] + "/tegress?url=http://" + objIP["a"] + "/")
		if code != 299 || r["code"] != "forbidden_destination" {
			t.Fatalf("self SSRF: %d %v", code, r)
		}
	})

	t.Run("ForgedHeadersStripped", func(t *testing.T) {
		code, r := s.getJSON("http://"+objIP["a"]+"/", "x-twd-cf", `{"tailscale":{"caller":{"nodeId":"nEVIL"}},"objectId":"b","twd":{"kind":"object","class":"Probe","name":"b"}}`,
			"x-twd-object", "b", "cf-connecting-ip", "1.2.3.4", "tailnet-assertion", "x", "x-forwarded-for", "9.9.9.9")
		if code != 200 || r["name"] != "a" || get(r, "caller", "nodeId") != s.clientID {
			t.Fatalf("%d %v", code, r)
		}
		for k := range r["sawHeaders"].(map[string]any) {
			if k != "x-twd-object" {
				t.Fatalf("tenant saw identity header %q: %v", k, r["sawHeaders"])
			}
		}
		// Forged egress attribution from tenant code.
		ob, _ := e.Host.Object(e.TenantID, "probe", "Probe", "b")
		oa, _ := e.Host.Object(e.TenantID, "probe", "Probe", "a")
		code, r = s.getJSON("http://" + objIP["a"] + "/forge-egress?as=" + ob.Name + "&url=" + echoURL)
		node, _ := get(r, "caller", "node").(string)
		if code != 200 || !strings.HasPrefix(node, oa.Hostname+".") {
			t.Fatalf("forged egress: %d %v", code, r)
		}
		for k := range r["headers"].(map[string]any) {
			if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-twd") || lk == "proxy-authorization" {
				t.Fatalf("header %s reached echo", k)
			}
		}
	})

	t.Run("SpawnChildNode", func(t *testing.T) {
		code, r := s.getJSON("http://" + objIP["a"] + "/spawn?name=kid1&caps=" + url.QueryEscape(`{"example.com/cap/ttest":{"role":"tester"}}`))
		if code != 200 || r["reply"] != "hello parent from kid1" || r["before"] != "pending" || r["after"] != "ready" {
			t.Fatalf("%d %v", code, r)
		}
		kid := e.WaitObject("probe", "Probe", "kid1", 10*time.Second)
		pa, _ := e.Host.Object(e.TenantID, "probe", "Probe", "a")
		fo, _ := e.FM.Object(kid.ID)
		if kid.Parent != pa.ID || fo.Parent != pa.ID || len(fo.Lineage) != 1 {
			t.Fatalf("lineage: %+v %+v", kid, fo)
		}
		if !strings.HasSuffix(r["name"].(string), "."+nettest.MagicDNSDomain) {
			t.Fatalf("child name %v", r["name"])
		}
	})

	t.Run("PlainDOAlarmWorkloadEgress", func(t *testing.T) {
		code, r := s.getJSON("http://" + objIP["a"] + "/ticker?name=t1&op=arm")
		if code != 200 || r["armed"] != true {
			t.Fatalf("%d %v", code, r)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, r = s.getJSON("http://" + objIP["a"] + "/ticker?name=t1")
			if r["alarms"] == float64(1) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("alarm did not fire: %v", r)
			}
			time.Sleep(50 * time.Millisecond)
		}
		to := e.WaitObject("probe", "Ticker", "t1", 30*time.Second)
		s.waitPeer(to.IP)
		code, r = s.getJSON("http://" + to.IP + "/egress?url=" + echoURL)
		if node, _ := get(r, "caller", "node").(string); code != 200 || !strings.HasPrefix(node, probe.Hostname+".") {
			t.Fatalf("plain DO egress should be the workload node %s: %d %v", probe.Hostname, code, r)
		}
		code, r = s.getJSON("http://" + to.IP + "/")
		if code != 200 || get(r, "caller", "nodeId") != s.clientID {
			t.Fatalf("plain DO ingress via own node: %d %v", code, r)
		}
	})

	t.Run("MCP", func(t *testing.T) {
		call := func(tool string, args map[string]any) map[string]any {
			body := js(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
			code, _, b := s.do("POST", "http://"+mcp.IP+"/mcp", []byte(body), "content-type", "application/json",
				"accept", "application/json, text/event-stream", "mcp-protocol-version", "2025-06-18")
			if code != 200 {
				t.Fatalf("%s: %d %s", tool, code, b)
			}
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "data: ") {
					b = []byte(strings.TrimPrefix(line, "data: "))
				}
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("%s: %s", tool, b)
			}
			return m
		}
		ok := call("lookup_order", map[string]any{"order": "o1"})
		text := get(ok, "result", "content").([]any)[0].(map[string]any)["text"].(string)
		if get(ok, "result", "isError") == true || !strings.Contains(text, `"shipped"`) {
			t.Fatalf("allowed tool: %v", ok)
		}
		deny := call("refund", map[string]any{"order": "o1", "usd": 10})
		if get(deny, "result", "isError") != true {
			t.Fatalf("refund should be denied: %v", deny)
		}
		var te map[string]any
		json.Unmarshal([]byte(get(deny, "result", "content").([]any)[0].(map[string]any)["text"].(string)), &te)
		if te["code"] != "cap_missing" || get(te, "missing", "tool") != "refund" || !strings.Contains(js(te["have"]), "lookup_order") {
			t.Fatalf("cap_missing JSON: %v", te)
		}
	})

	t.Run("AgentSDK", func(t *testing.T) {
		for i := 1; i <= 2; i++ {
			code, r := s.getJSON("http://" + agent.IP + "/?agent=alice")
			if code != 200 || r["agent"] != "alice" || get(r, "state", "visits") != float64(i) || r["calls"] != float64(i) ||
				get(r, "caller", "nodeId") != s.clientID {
				t.Fatalf("visit %d: %d %v", i, code, r)
			}
		}
		ao := e.WaitObject("agent", "SupportAgent", "alice", 60*time.Second)
		s.waitPeer(ao.IP)
		code, r := s.getJSON("http://" + ao.IP + "/?upstream=" + echoURL)
		if code != 200 || get(r, "state", "visits") != float64(3) {
			t.Fatalf("direct: %d %v", code, r)
		}
		if n, _ := get(r, "upstream", "body", "caller", "node").(string); !strings.HasPrefix(n, ao.Hostname+".") {
			t.Fatalf("agent egress via %q, want its own node %s", n, ao.Hostname)
		}
		code, r = s.getJSON("http://" + ao.IP + "/spawn?child=lk1")
		if code != 200 || get(r, "child", "reply", "lookup") != "lk1" || get(r, "child", "reply", "parent") != "alice" {
			t.Fatalf("agent spawn: %d %v", code, r)
		}
	})

	t.Run("KillFailsFast", func(t *testing.T) {
		o, _ := e.Host.Object(e.TenantID, "probe", "Probe", "c")
		req, _ := http.NewRequest("GET", "http://"+objIP["c"]+"/stream", nil)
		resp, err := s.hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
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
		st := time.Now()
		killed, err := e.FM.Kill(context.Background(), o.ID, true, "test kill")
		if err != nil || len(killed) != 1 {
			t.Fatal(killed, err)
		}
		select {
		case ft := <-failed:
			ttf := ft.Sub(st)
			t.Logf("streaming caller failed %v after the mgmt kill", ttf)
			if ttf > 500*time.Millisecond {
				t.Fatalf("time to failure %v", ttf)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stream survived the kill")
		}
		// A new call fails fast with a typed object_killed from the
		// tombstoned node, not a timeout.
		qc := s.client.HTTPClient(5 * time.Second)
		rs := time.Now()
		nr, err := qc.Get("http://" + objIP["c"] + "/")
		d := time.Since(rs)
		if err != nil || d > time.Second {
			t.Fatalf("new request after kill: err=%v after %v", err, d)
		}
		var kb map[string]any
		json.NewDecoder(nr.Body).Decode(&kb)
		nr.Body.Close()
		t.Logf("new request after kill: %d %v in %v", nr.StatusCode, kb["code"], d)
		if nr.StatusCode != 410 || kb["code"] != "object_killed" || nr.Header.Get("twd-error") != "object_killed" {
			t.Fatalf("new request after kill: %d %v", nr.StatusCode, kb)
		}
		// Via the workload: typed killed.
		code, r := s.getJSON("http://" + probe.IP + "/probe/c/")
		if code != 410 || r["code"] != "killed" {
			t.Fatalf("via workload after kill: %d %v", code, r)
		}
		// Re-creating a killed name is refused (no identity resurrection).
		code, r = s.getJSON("http://" + objIP["a"] + "/sibling?name=c")
		if code == 200 {
			t.Fatalf("RPC to killed object: %v", r)
		}
	})

	t.Run("HibernationKeepsNode", func(t *testing.T) {
		o, _ := e.Host.Object(e.TenantID, "probe", "Probe", "b")
		_, r1 := s.getJSON("http://" + objIP["b"] + "/")
		w1 := o.Wakes
		time.Sleep(14 * time.Second) // workerd evicts idle DOs after ~10 s
		o2, _ := e.Host.Object(e.TenantID, "probe", "Probe", "b")
		if o2.Status != "ready" || !s.client.InNetmap(context.Background(), objIP["b"]) {
			t.Fatalf("node went away while hibernated: %+v", o2)
		}
		code, r2 := s.getJSON("http://" + objIP["b"] + "/")
		o3, _ := e.Host.Object(e.TenantID, "probe", "Probe", "b")
		if code != 200 || o3.Wakes <= w1 {
			t.Fatalf("expected a wake after idle eviction: wakes %d -> %d, %v / %v", w1, o3.Wakes, r1, r2)
		}
		t.Logf("wakes %d -> %d; boots %v -> %v", w1, o3.Wakes, r1["boots"], r2["boots"])
	})

	t.Run("BlueGreenRedeploy", func(t *testing.T) {
		var okN, failN atomic.Int64
		var lastErr atomic.Value
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				code, r := s.getJSON("http://" + objIP["a"] + "/")
				if code == 200 && r["name"] == "a" {
					okN.Add(1)
				} else {
					failN.Add(1)
					lastErr.Store(js(r))
				}
			}
		}()
		time.Sleep(200 * time.Millisecond)
		d2 := e.Deploy(hostdtest.Fixture("probe"), "probe", map[string]string{"GREETING": "v2"})
		p2 := e.WaitActive("probe", d2, 60*time.Second)
		time.Sleep(300 * time.Millisecond)
		close(stop)
		wg.Wait()
		if p2.IP != probe.IP {
			t.Fatalf("workload node changed across deploys: %s -> %s", probe.IP, p2.IP)
		}
		t.Logf("during redeploy: %d ok, %d failed", okN.Load(), failN.Load())
		if failN.Load() > 0 {
			t.Fatalf("%d requests failed during blue/green (last %v)", failN.Load(), lastErr.Load())
		}
		// State survived the switch (same DO storage).
		_, r := s.getJSON("http://" + agent.IP + "/?agent=alice")
		if get(r, "state", "visits") == nil {
			t.Fatalf("agent state: %v", r)
		}
	})

	t.Run("Audit", func(t *testing.T) {
		var resp mgmtapi.AuditResponse
		deadline := time.Now().Add(5 * time.Second)
		for {
			e.Mgmt.Do(context.Background(), "GET", "/v0/audit", nil, &resp)
			kinds := map[string]bool{}
			for _, r := range resp.Records {
				kinds[r.Kind] = true
			}
			if kinds["spawn"] && kinds["kill"] && kinds["deny"] {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("audit kinds %v", kinds)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !resp.Verified {
			t.Fatalf("audit chain not verified: %v", resp.Problems)
		}
	})
}

// TestKillViaHeartbeat: with no push (what M2's twd-mgmt does), kills
// arrive in the heartbeat's revokeObjects.
func TestKillViaHeartbeat(t *testing.T) {
	s := startSuite(t, fakemgmt.Config{NoPush: true})
	e := s.e
	d := e.Deploy(hostdtest.Fixture("probe"), "probe", nil)
	probe := e.WaitActive("probe", d, 60*time.Second)
	s.waitPeer(probe.IP)
	if code, r := s.getJSON("http://" + probe.IP + "/probe/k/"); code != 200 {
		t.Fatalf("%d %v", code, r)
	}
	o := e.WaitObject("probe", "Probe", "k", 30*time.Second)
	st := time.Now()
	if _, err := e.FM.Kill(context.Background(), o.ID, false, "hb"); err != nil {
		t.Fatal(err)
	}
	for {
		if o2, _ := e.Host.Object(e.TenantID, "probe", "Probe", "k"); o2.ID == "" {
			break
		}
		if time.Since(st) > 5*time.Second {
			t.Fatal("kill not delivered via heartbeat")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("kill delivered via heartbeat in %v (heartbeat every 200ms)", time.Since(st))
}
