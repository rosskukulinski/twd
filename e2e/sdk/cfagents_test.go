//go:build sdk

package sdk

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
)

// TestCloudflareAgents runs real `agents` Agent classes (facet and plain-DO
// egress modes) on twd and checks the SDK surface the RFC relies on.
func TestCloudflareAgents(t *testing.T) {
	s := startSuite(t)
	dCF := s.a.Deploy(app("cfagents"), "cfagents", nil)
	dMCP := s.b.Deploy(hostdtest.Template("mcp"), "mcp", nil)
	cf := s.a.WaitActive("cfagents", dCF, 60*time.Second)
	mcp := s.b.WaitActive("mcp", dMCP, 60*time.Second)
	s.waitPeer(cf.IP)
	s.waitPeer(mcp.IP)
	base := "http://" + cf.IP
	mcpURL := "http://" + mcp.IP + "/mcp"
	modes := []struct{ class, kebab string }{{"StateAgent", "state-agent"}, {"PlainAgent", "plain-agent"}}

	t.Run("RouteAgentRequest", func(t *testing.T) {
		for _, m := range modes {
			code, r := s.getJSON(base + "/agents/" + m.kebab + "/r1")
			if code != 200 || r["agent"] != "r1" || get(r, "caller", "nodeId") != s.clientID {
				t.Fatalf("%s: %d %v", m.class, code, r)
			}
		}
		code, r := s.getJSON(base + "/agents/no-such-agent/r1")
		if code != 400 {
			t.Fatalf("unknown namespace: %d %v", code, r)
		}
		code, r = s.getJSON(base + "/by-name?name=bn1")
		if code != 200 || r["agent"] != "bn1" {
			t.Fatalf("getAgentByName: %d %v", code, r)
		}
	})

	t.Run("SetStateOnStateChangedSQL", func(t *testing.T) {
		for _, m := range modes {
			u := base + "/agents/" + m.kebab + "/st1"
			for i := 1; i <= 3; i++ {
				code, r := s.getJSON(u + "/state?inc=1")
				if code != 200 || get(r, "state", "count") != float64(i) {
					t.Fatalf("%s inc %d: %d %v", m.class, i, code, r)
				}
				ch, _ := r["changes"].([]any)
				// initialState's first persist is a change too, so ≥ i rows.
				if len(ch) < i || get(ch[len(ch)-1].(map[string]any), "count") != float64(i) || get(ch[len(ch)-1].(map[string]any), "source") != "server" {
					t.Fatalf("%s onStateChanged rows: %v", m.class, r["changes"])
				}
			}
			code, r := s.getJSON(u + "/sql?k=a&v=1")
			code2, r2 := s.getJSON(u + "/sql?k=b&v=2")
			if code != 200 || code2 != 200 || js(r2["rows"]) != `[{"k":"a","v":"1"},{"k":"b","v":"2"}]` {
				t.Fatalf("%s this.sql: %d %v / %d %v", m.class, code, r, code2, r2)
			}
			// The same object on its own node sees the same state.
			o := s.a.WaitObject("cfagents", m.class, "st1", 60*time.Second)
			s.waitPeer(o.IP)
			code, r = s.getJSON("http://" + o.IP + "/state")
			if code != 200 || get(r, "state", "count") != float64(3) || get(r, "caller", "nodeId") != s.clientID {
				t.Fatalf("%s via own node: %d %v", m.class, code, r)
			}
		}
	})

	t.Run("EgressMode", func(t *testing.T) {
		for _, m := range modes {
			s.getJSON(base + "/agents/" + m.kebab + "/eg1")
			o := s.a.WaitObject("cfagents", m.class, "eg1", 60*time.Second)
			code, r := s.getJSON(base + "/agents/" + m.kebab + "/eg1/egress?url=" + url.QueryEscape("http://ttest-echo/"))
			n, _ := get(r, "body", "caller", "node").(string)
			want := o.Hostname // facet: the object's own node
			if m.class == "PlainAgent" {
				want = cf.Hostname // plain-DO: the workload node
			}
			if code != 200 || !strings.HasPrefix(n, want+".") {
				t.Fatalf("%s fetch() egress via %q, want %s: %d %v", m.class, n, want, code, r)
			}
		}
	})

	t.Run("Schedule", func(t *testing.T) {
		// Both modes: schedule(delay) and scheduleEvery(seconds) fire. Plain-DO
		// mode uses the DO's own alarm; facet mode (workerd: "Facets currently
		// cannot set alarms") delegates ctx.storage alarms to the Host DO.
		for _, m := range modes {
			u := base + "/agents/" + m.kebab + "/sch1"
			code, r := s.getJSON(u + "/schedule")
			if code != 200 || get(r, "once", "error") != nil || get(r, "every", "error") != nil || r["alarm"] == nil {
				t.Fatalf("%s schedule: %d %v", m.class, code, r)
			}
			st := time.Now()
			eventually(t, 10*time.Second, m.class+" schedule()+scheduleEvery() fire", func() (bool, any) {
				_, r := s.getJSON(u + "/ticks")
				once, _ := get(r, "ticks", "once").(float64)
				every, _ := get(r, "ticks", "every").(float64)
				return once == 1 && every >= 2, r
			})
			t.Logf("%s: once + 2× every fired within %v", m.class, time.Since(st).Round(time.Millisecond))
			_, r = s.getJSON(u + "/ticks?cancel=1")
			if sc, _ := r["schedules"].([]any); len(sc) == 0 {
				t.Fatalf("%s listSchedules: %v", m.class, r)
			}
		}
	})

	t.Run("WebSocketsAndHibernation", func(t *testing.T) {
		for _, m := range modes {
			t.Run(m.class, func(t *testing.T) {
				t.Parallel()
				s.wsCheck(t, base+"/agents/"+m.kebab+"/ws1", m.class)
			})
		}
	})

	t.Run("AgentAsMCPClient", func(t *testing.T) {
		for _, m := range modes {
			s.getJSON(base + "/agents/" + m.kebab + "/mc1")
			o := s.a.WaitObject("cfagents", m.class, "mc1", 60*time.Second)
			code, r := s.getJSON(base + "/agents/" + m.kebab + "/mc1/mcp-client?url=" + url.QueryEscape(mcpURL))
			if code != 200 || r["error"] != nil || get(r, "add", "state") != "ready" {
				t.Fatalf("%s addMcpServer: %d %v", m.class, code, r)
			}
			if js(r["tools"]) != `["lookup_order","refund","whoami"]` {
				t.Fatalf("%s tools: %v", m.class, r["tools"])
			}
			if get(r, "lookup", "isError") != false || !strings.Contains(js(r["lookup"]), "shipped") {
				t.Fatalf("%s allowed tool: %v", m.class, r["lookup"])
			}
			if get(r, "refund", "isError") != true || !strings.Contains(js(r["refund"]), `cap_missing`) {
				t.Fatalf("%s denied tool: %v", m.class, r["refund"])
			}
			// The MCP server's per-request caller is this agent's node
			// (facet) or the workload node (plain-DO).
			var who map[string]any
			json.Unmarshal([]byte(get(r, "whoami", "text").(string)), &who)
			want := o.Hostname
			if m.class == "PlainAgent" {
				want = cf.Hostname
			}
			if n, _ := who["node"].(string); !strings.HasPrefix(n, want+".") {
				t.Fatalf("%s: MCP server saw caller %v, want %s", m.class, who, want)
			}
			t.Logf("%s: MCP server saw caller %v", m.class, who["node"])
		}
	})

	t.Run("SameHostMCPRefused", func(t *testing.T) {
		// Documented limitation: an object's egress may not hairpin to a
		// sibling node on the same host (netd forbidden_destination; RFC
		// §4.5). Here the agent targets its own app's /mcp on host A.
		code, r := s.getJSON(base + "/agents/state-agent/mc2/mcp-client?url=" + url.QueryEscape(base+"/mcp"))
		t.Logf("same-host addMcpServer: %d %v", code, r)
		if r["error"] == nil || !strings.Contains(js(r), "forbidden_destination") && !strings.Contains(js(r), "403") {
			t.Fatalf("same-host MCP call should be refused: %d %v", code, r)
		}
	})

	t.Run("CreateMcpHandlerWithTailnet", func(t *testing.T) {
		ok := s.mcpCall(base+"/mcp", "lookup_order", map[string]any{"order": "o1"})
		if get(ok, "result", "isError") == true || !strings.Contains(toolText(ok), "shipped") {
			t.Fatalf("allowed: %v", ok)
		}
		deny := s.mcpCall(base+"/mcp", "refund", map[string]any{"order": "o1", "usd": 5})
		var te map[string]any
		json.Unmarshal([]byte(toolText(deny)), &te)
		if get(deny, "result", "isError") != true || te["code"] != "cap_missing" || get(te, "missing", "tool") != "refund" {
			t.Fatalf("deny: %v", deny)
		}
		who := s.mcpCall(base+"/mcp", "whoami", map[string]any{})
		if !strings.Contains(toolText(who), s.clientID) {
			t.Fatalf("whoami: %v", who)
		}
	})

	t.Run("UnsupportedBindings", func(t *testing.T) {
		code, r := s.getJSON(base + "/agents/state-agent/pr1/probe")
		t.Logf("probe: %d %v", code, r)
		for _, k := range []string{"AI", "KV", "WORKFLOW", "BROWSER", "LOADER"} {
			if r[k] != "undefined" {
				t.Fatalf("env.%s is %v; expected absent (update docs/COMPAT.md)", k, r[k])
			}
		}
		if !strings.Contains(js(r["email"]), "not supported on twd") {
			t.Fatalf("cloudflare:email: %v", r["email"])
		}
		// subAgent() inside a facet: the facet's Dynamic Worker has no
		// ctx.exports for tenant classes (documented gap).
		if !strings.Contains(js(r["subAgent"]), "not found in worker exports") {
			t.Fatalf("subAgent: %v (update docs/COMPAT.md)", r["subAgent"])
		}
	})
}

// wsCheck: connect via routeAgentRequest, getCaller in onConnect, state
// sync broadcast to a second connection, then idle past workerd's eviction
// window and check the socket survives and the agent wakes (hibernation).
func (s *suite) wsCheck(t *testing.T, u, class string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(u, "http")
	dial := func() *websocket.Conn {
		c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: s.client.HTTPClient(0)})
		if err != nil {
			t.Fatalf("dial %s: %v %v", wsURL, err, resp)
		}
		return c
	}
	// read returns the next message of type typ (skipping SDK protocol frames
	// unless asked for).
	read := func(c *websocket.Conn, typ string) map[string]any {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				t.Fatalf("%s read (want %s): %v", class, typ, err)
			}
			m := parse(b)
			if m["type"] == typ {
				return m
			}
		}
	}
	c1 := dial()
	defer c1.CloseNow()
	hello := read(c1, "hello")
	if get(hello, "caller", "nodeId") != s.clientID {
		t.Fatalf("getCaller in onConnect: %v", hello)
	}
	c2 := dial()
	defer c2.CloseNow()
	read(c2, "hello")
	c1.Write(ctx, websocket.MessageText, []byte("inc"))
	inc := read(c1, "inc")
	// State sync: the other connection gets the SDK's state broadcast.
	st := read(c2, "cf_agent_state")
	if get(st, "state", "count") != inc["count"] {
		t.Fatalf("state broadcast %v vs %v", st, inc)
	}
	boot1 := hello["boot"]
	// Idle past workerd's ~10 s eviction: hibernatable sockets stay open.
	time.Sleep(14 * time.Second)
	c1.Write(ctx, websocket.MessageText, []byte("who"))
	who := read(c1, "who")
	if who["node"] != s.clientID {
		t.Fatalf("connection state after idle: %v", who)
	}
	t.Logf("%s: boot %v -> %v after 14 s idle (hibernated: %v), conns %v", class, boot1, who["boot"], who["boot"] != boot1, who["conns"])
	if who["boot"] == boot1 {
		t.Logf("NOTE: %s was not evicted while sockets were open", class)
	}
	c1.Close(websocket.StatusNormalClosure, "")
	c2.Close(websocket.StatusNormalClosure, "")
}
