//go:build sdk

// Package sdk is the M5 SDK conformance suite (make test-sdk): real
// Cloudflare Agents SDK and Vercel AI SDK code on twd's process tier
// (twd-hostd + generated platform + stock workerd), driven over a hermetic
// tailnet (testcontrol + local DERP/STUN). No real tailnet, no LLM keys.
//
// Two hosts share one control plane: host A runs the agent apps, host B the
// MCP servers, so agent → MCP calls cross the tailnet (netd refuses
// same-host sibling hairpins; see TestSameHostMCPRefused).
package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd"
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
	a, b     *hostdtest.Env
	client   *nettest.Node
	echo     *nettest.Node
	hc       *http.Client
	clientID string
}

// App returns an e2e/sdk fixture app dir.
func app(name string) string {
	p, err := filepath.Abs(filepath.Join("apps", name))
	if err != nil {
		panic(err)
	}
	return p
}

func startSuite(t *testing.T) *suite {
	t.Helper()
	ctl, err := nettest.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctl.Close)
	// Every node gets the test cap; MCP callers get lookup_order only (so
	// refund is the deny case).
	ctl.Server.SetGlobalAppCaps(tailcfg.PeerCapMap{
		nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue},
		mcpCap:          []tailcfg.RawMessage{`{"tools":["lookup_order","whoami"]}`},
	})
	start := func(prefix string) *hostdtest.Env {
		return hostdtest.Start(t, hostdtest.Options{ControlURL: ctl.URL, Fake: fakemgmt.Config{StaticKey: nettest.DummyKey}, Ephemeral: true,
			HostnamePrefix: prefix, KillTombstone: 5 * time.Second, Sandbox: hostd.SandboxAuto, // twd-hostd's default
			Tenant: mgmtapi.Tenant{Tailnet: "ttest.ts.net", ControlURL: ctl.URL, MintMode: "static"}})
	}
	s := &suite{t: t, ctl: ctl, a: start("a-"), b: start("b-")}
	// Every SDK app ran under the host's process sandbox (Seatbelt on
	// macOS, Landlock on Linux).
	t.Cleanup(func() {
		want, _ := hostd.ResolveSandbox(hostd.SandboxAuto)
		n := 0
		for _, h := range []*hostdtest.Env{s.a, s.b} {
			for _, w := range h.Host.Status().Workloads {
				if w.Tier == "microvm" || w.InstanceDir == "" {
					continue
				}
				n++
				if w.Sandbox != want {
					t.Errorf("SDK workload %s ran with sandbox %q, want %q", w.Workload, w.Sandbox, want)
				}
			}
		}
		t.Logf("sandbox %s: %d SDK workloads checked", want, n)
	})
	ctx := context.Background()
	var wg sync.WaitGroup
	var cerr, eerr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.client, cerr = nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-client", filepath.Join(s.a.Dir, "client"))
	}()
	go func() {
		defer wg.Done()
		s.echo, eerr = nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-echo", filepath.Join(s.a.Dir, "echo"))
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
	s.hc = s.client.HTTPClient(60 * time.Second)
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
	return code, parse(b)
}

func parse(b []byte) map[string]any {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		m = map[string]any{"raw": string(b)}
	}
	return m
}

// mcpCall sends one stateless JSON-RPC tools/call over Streamable HTTP.
func (s *suite) mcpCall(url, tool string, args map[string]any) map[string]any {
	s.t.Helper()
	body := js(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	code, _, b := s.do("POST", url, []byte(body), "content-type", "application/json",
		"accept", "application/json, text/event-stream", "mcp-protocol-version", "2025-06-18")
	if code != 200 {
		s.t.Fatalf("%s: %d %s", tool, code, b)
	}
	return parse(sseData(b))
}

// sseData returns the last SSE data payload (or b if it is plain JSON).
func sseData(b []byte) []byte {
	out := b
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "data: ") {
			out = []byte(strings.TrimPrefix(line, "data: "))
		}
	}
	return out
}

func toolText(m map[string]any) string {
	c, _ := get(m, "result", "content").([]any)
	if len(c) == 0 {
		return ""
	}
	t, _ := c[0].(map[string]any)["text"].(string)
	return t
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

// eventually polls f until it returns true.
func eventually(t *testing.T, d time.Duration, what string, f func() (bool, any)) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		ok, last := f()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not after %v (last %v)", what, d, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
