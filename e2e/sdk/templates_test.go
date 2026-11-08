//go:build sdk

package sdk

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
)

// TestAgentTemplate: the create-twd `agent` template uses the patterns this
// suite verified: routeAgentRequest (HTTP + WebSocket with getCaller in
// onConnect) and facet-mode schedule().
func TestAgentTemplate(t *testing.T) {
	s := startSuite(t)
	d := s.a.Deploy(hostdtest.Template("agent"), "agent", nil)
	ag := s.a.WaitActive("agent", d, 60*time.Second)
	s.waitPeer(ag.IP)
	base := "http://" + ag.IP + "/agents/support-agent/t1"

	code, r := s.getJSON(base + "/")
	if code != 200 || r["agent"] != "t1" || get(r, "caller", "nodeId") != s.clientID {
		t.Fatalf("routeAgentRequest: %d %v", code, r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+ag.IP+"/agents/support-agent/t1", &websocket.DialOptions{HTTPClient: s.client.HTTPClient(0)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if m := parse(b); m["type"] == "hello" {
			if get(m, "caller", "nodeId") != s.clientID {
				t.Fatalf("onConnect caller: %v", m)
			}
			break
		}
	}
	code, r = s.getJSON(base + "/remind?in=1&note=n1")
	if code != 200 {
		t.Fatalf("remind: %d %v", code, r)
	}
	eventually(t, 10*time.Second, "facet-mode schedule() reminder fires", func() (bool, any) {
		_, r := s.getJSON(base + "/remind")
		return strings.Contains(js(r["fired"]), "n1"), r
	})
}
