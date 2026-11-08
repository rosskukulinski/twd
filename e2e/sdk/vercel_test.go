//go:build sdk

package sdk

import (
	"bufio"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/hostdtest"
)

// uiStream is a parsed AI SDK UI message stream (SSE).
type uiStream struct {
	types   []string
	text    string
	outputs map[string]string // toolName -> output JSON
	errors  map[string]string // toolName -> error text
	chunks  int               // SSE events received
	first   time.Duration     // time to first event
	total   time.Duration
}

func (s *suite) agentStream(t *testing.T, u string) *uiStream {
	t.Helper()
	st := time.Now()
	resp, err := s.hc.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("content-type"), "text/event-stream") {
		b := make([]byte, 4096)
		n, _ := resp.Body.Read(b)
		t.Fatalf("%s: %d %s %s", u, resp.StatusCode, resp.Header.Get("content-type"), b[:n])
	}
	out := &uiStream{outputs: map[string]string{}, errors: map[string]string{}}
	names := map[string]string{} // toolCallId -> toolName
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		if out.chunks == 0 {
			out.first = time.Since(st)
		}
		out.chunks++
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		typ, _ := ev["type"].(string)
		out.types = append(out.types, typ)
		id, _ := ev["toolCallId"].(string)
		switch typ {
		case "tool-input-available", "tool-input-start":
			names[id], _ = ev["toolName"].(string)
		case "tool-output-available":
			out.outputs[names[id]] = js(ev["output"])
		case "tool-output-error":
			out.errors[names[id]], _ = ev["errorText"].(string)
		case "text-delta":
			d, _ := ev["delta"].(string)
			out.text += d
		}
	}
	out.total = time.Since(st)
	return out
}

// TestVercelAI: an AI SDK ToolLoopAgent (create-twd `ai-agent` template, mock
// model) whose tools are twd MCP servers' tools over the tailnet: Vercel's
// mcp-handler and the Cloudflare createMcpHandler (`mcp` template).
func TestVercelAI(t *testing.T) {
	s := startSuite(t)
	dVM := s.b.Deploy(app("vercel-mcp"), "vercel-mcp", nil)
	dCM := s.b.Deploy(hostdtest.Template("mcp"), "mcp", nil)
	vm := s.b.WaitActive("vercel-mcp", dVM, 60*time.Second)
	cm := s.b.WaitActive("mcp", dCM, 60*time.Second)
	servers := map[string]string{"mcp-handler": "http://" + vm.IP + "/mcp", "createMcpHandler": "http://" + cm.IP + "/mcp"}
	deploy := map[string]string{}
	ai := map[string]string{}
	for k, u := range servers {
		wl := "ai-" + strings.ToLower(strings.ReplaceAll(k, "Handler", ""))
		wl = strings.ReplaceAll(wl, "-handler", "h")
		deploy[k] = s.a.Deploy(hostdtest.Template("ai-agent"), wl, map[string]string{"MCP_URL": u})
		ai[k] = wl
	}
	ips := map[string]string{}
	host := map[string]string{}
	for k, wl := range ai {
		o := s.a.WaitActive(wl, deploy[k], 60*time.Second)
		ips[k], host[k] = o.IP, o.Hostname
		s.waitPeer(o.IP)
	}
	s.waitPeer(vm.IP)

	t.Run("McpHandlerWithTailnet", func(t *testing.T) {
		u := servers["mcp-handler"]
		ok := s.mcpCall(u, "lookup_order", map[string]any{"order": "o1"})
		if get(ok, "result", "isError") == true || !strings.Contains(toolText(ok), "mcp-handler") {
			t.Fatalf("allowed: %v", ok)
		}
		deny := s.mcpCall(u, "refund", map[string]any{"order": "o1", "usd": 5})
		var te map[string]any
		json.Unmarshal([]byte(toolText(deny)), &te)
		if get(deny, "result", "isError") != true || te["code"] != "cap_missing" {
			t.Fatalf("deny: %v", deny)
		}
		who := s.mcpCall(u, "whoami", map[string]any{})
		if !strings.Contains(toolText(who), s.clientID) {
			t.Fatalf("whoami: %v", who)
		}
		// Unverified requests never reach mcp-handler: there is no route to
		// it except through netd (the workload node), which sets the caller.
	})

	for _, k := range []string{"mcp-handler", "createMcpHandler"} {
		t.Run("ToolLoopAgent/"+k, func(t *testing.T) {
			wl := ai[k]
			// In a Durable Object: per-agent identity.
			agent := "alice-" + wl
			st := s.agentStream(t, "http://"+ips[k]+"/agents/"+agent+"?prompt="+url.QueryEscape("where is o1?"))
			t.Logf("%s DO agent: %d events, first after %v, total %v; text %q", k, st.chunks, st.first.Round(time.Millisecond), st.total.Round(time.Millisecond), st.text)
			if st.chunks < 8 || !strings.Contains(strings.Join(st.types, ","), "text-delta") {
				t.Fatalf("not a streamed UI message response: %v", st.types)
			}
			if !strings.Contains(st.outputs["lookup_order"], "shipped") {
				t.Fatalf("lookup_order (allowed) output: %v / errors %v", st.outputs, st.errors)
			}
			refund := st.outputs["refund"] + st.errors["refund"]
			if !strings.Contains(refund, "cap_missing") {
				t.Fatalf("refund (denied) should carry cap_missing: %q", refund)
			}
			o, err := s.a.WaitObjectErr(wl, "AiAgent", agent, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(st.outputs["whoami"], o.Hostname+".") {
				t.Fatalf("MCP server saw %s, want the agent's own node %s", st.outputs["whoami"], o.Hostname)
			}
			if !strings.Contains(st.text, "lookup_order") || !strings.Contains(st.text, "shipped") {
				t.Fatalf("final text: %q", st.text)
			}
			// In the Worker: the workload node is the caller.
			sw := s.agentStream(t, "http://"+ips[k]+"/worker")
			if !strings.Contains(sw.outputs["whoami"], host[k]+".") || !strings.Contains(sw.outputs["lookup_order"], "shipped") {
				t.Fatalf("worker loop: %v %v", sw.outputs, sw.errors)
			}
		})
	}

	t.Run("SameClassAcrossWorkloadsFailsClosed", func(t *testing.T) {
		// Both ai-agent deployments have class AiAgent; mgmt keys objects by
		// (tenant, class, name), so the same name in the second workload is
		// the first one's object. hostd refuses to share the identity.
		s.agentStream(t, "http://"+ips["mcp-handler"]+"/agents/dup")
		code, _, b := s.do("GET", "http://"+ips["createMcpHandler"]+"/agents/dup", nil)
		t.Logf("same class+name in a second workload: %d %s", code, hostdtest.Trim(string(b), 300))
		if code == 200 || !strings.Contains(string(b), "namespace_conflict") {
			t.Fatalf("want namespace_conflict: %d %s", code, b)
		}
	})

	t.Run("TraceparentPropagated", func(t *testing.T) {
		// mcpFetch propagates the inbound trace ID; mcp-handler's whoami
		// echoes the trace ID netd/hostd assigned to the MCP request, which
		// is a fresh one per hop (the header is not trusted for identity).
		st := s.agentStream(t, "http://"+ips["mcp-handler"]+"/agents/bob")
		if !strings.Contains(st.outputs["whoami"], "traceId") {
			t.Fatalf("whoami: %v", st.outputs)
		}
	})

	t.Run("EgressRefusalIsTyped", func(t *testing.T) {
		// An MCP server URL that is not a visible peer: mcpFetch surfaces
		// twd's typed error instead of an opaque HTTP failure.
		d := s.a.Deploy(hostdtest.Template("ai-agent"), "ai-bad", map[string]string{"MCP_URL": "http://no-such-peer/mcp"})
		o := s.a.WaitActive("ai-bad", d, 60*time.Second)
		s.waitPeer(o.IP)
		code, _, b := s.do("GET", "http://"+o.IP+"/agents/carol", nil)
		t.Logf("unreachable MCP server: %d %s", code, b)
		if code == 200 || !strings.Contains(string(b), "peer_not_visible") {
			t.Fatalf("want a typed peer_not_visible: %d %s", code, b)
		}
	})
}
