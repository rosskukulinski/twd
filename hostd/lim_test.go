package hostd_test

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/tailcfg"
)

// RT-01: a spawned child's per-spawn cap limit is enforced at the RECEIVER.
// The policy (here: the control's global caps) grants every node both
// lookup_order and refund. A SupportAgent spawns a child of its own class
// with lim = [lookup_order]; the MCP server, resolving the child's node
// certificate and intersecting, must refuse the child's refund with
// cap_missing while admitting the parent's, and must fail closed when the
// certificate cannot be fetched.
func TestSpawnLimEnforcedAtReceiver(t *testing.T) {
	// The agent and the MCP server share this host: let the child reach the
	// server over the tailnet (hairpin), as e2e check (d) does.
	s := startSuiteCapsOpts(t, fakemgmt.Config{}, tailcfg.PeerCapMap{
		nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue},
		mcpCap:          []tailcfg.RawMessage{`{"tools":["lookup_order","refund"]}`},
	}, func(o *hostdtest.Options) { o.AllowSiblingEgress = true })
	e := s.e
	dMCP := e.Deploy(hostdtest.Template("mcp"), "mcp", nil)
	dAgent := e.Deploy(hostdtest.Template("agent"), "agent", nil)
	mcp := e.WaitActive("mcp", dMCP, 60*time.Second)
	agent := e.WaitActive("agent", dAgent, 60*time.Second)
	s.waitPeer(mcp.IP)
	s.waitPeer(agent.IP)
	server := "http://" + mcp.IP + "/mcp"

	// The client is not a twd object: its WhoIs caps apply unchanged.
	code, r := s.getJSON("http://" + agent.IP + "/?agent=alice")
	if code != 200 {
		t.Fatalf("alice: %d %v", code, r)
	}
	ao := e.WaitObject("agent", "SupportAgent", "alice", 60*time.Second)
	s.waitPeer(ao.IP)

	code, r = s.getJSON("http://" + ao.IP + "/delegate?child=sub1&server=" + url.QueryEscape(server))
	if code != 200 {
		t.Fatalf("delegate: %d %v", code, r)
	}
	if get(r, "lookup_order", "ok") != true {
		t.Fatalf("narrowed child should keep lookup_order: %v", r)
	}
	if get(r, "refund", "ok") != false || get(r, "refund", "error") != "cap_missing" {
		t.Fatalf("narrowed child's refund must be cap_missing: %v", r)
	}
	if get(r, "parent", "refund", "ok") != true {
		t.Fatalf("the unnarrowed parent's refund should be admitted by the cap check (its tag is granted refund): %v", r)
	}
	child := e.WaitObject("agent", "SupportAgent", "sub1", 30*time.Second)
	fo, _ := e.FM.Object(child.ID)
	var lim map[string]any
	json.Unmarshal(fo.Caps, &lim)
	if fo.Parent != ao.ID || js(get(lim, mcpCap, "tools")) != `["lookup_order"]` {
		t.Fatalf("child object in mgmt: %+v", fo)
	}

	// Fail closed: when the receiver cannot fetch the child's certificate,
	// the child's caps are withheld entirely (even lookup_order).
	e.FM.DenyCert(child.NodeID, true)
	e.Host.FlushCertCache()
	s.waitPeer(child.IP)
	code, r = s.getJSON("http://" + child.IP + "/tool?name=lookup_order&server=" + url.QueryEscape(server))
	if code != 200 || get(r, "ok") != false || get(r, "error") != "cap_missing" {
		t.Fatalf("cert lookup failure must withhold caps (fail closed): %d %v", code, r)
	}
	e.FM.DenyCert(child.NodeID, false)
	e.Host.FlushCertCache()
	code, r = s.getJSON("http://" + child.IP + "/tool?name=lookup_order&server=" + url.QueryEscape(server))
	if code != 200 || get(r, "ok") != true {
		t.Fatalf("after the lookup recovers, lookup_order is allowed again: %d %v", code, r)
	}
}

// RT-02: tenant code can only write tenant_note records; the platform
// assigns object, trace and principal, and the note can never masquerade
// as an allow/deny decision.
func TestTenantAuditIsOnlyANote(t *testing.T) {
	s := startSuite(t, fakemgmt.Config{})
	e := s.e
	d := e.Deploy(hostdtest.Fixture("probe"), "probe", nil)
	probe := e.WaitActive("probe", d, 60*time.Second)
	s.waitPeer(probe.IP)
	if code, r := s.getJSON("http://" + probe.IP + "/probe/a/"); code != 200 {
		t.Fatalf("probe a: %d %v", code, r)
	}
	a := e.WaitObject("probe", "Probe", "a", 60*time.Second)
	s.waitPeer(a.IP)
	for _, kind := range []string{"allow", "deny", "kill", "spawn"} {
		if code, r := s.getJSON("http://" + a.IP + "/audit?kind=" + kind); code != 200 {
			t.Fatalf("audit op: %d %v", code, r)
		}
	}
	var resp mgmtapi.AuditResponse
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.Mgmt.Do(context.Background(), "GET", "/v0/audit?object="+a.ID, nil, &resp)
		notes := 0
		for _, rec := range resp.Records {
			if rec.ObjectID != a.ID {
				continue
			}
			switch rec.Kind {
			case "allow", "deny", "kill", "spawn", "egress":
				if rec.Source == "tenant" || strings.Contains(rec.Detail, "wired") {
					t.Fatalf("tenant content reached a privileged kind: %+v", rec)
				}
			case "tenant_note":
				notes++
				if rec.Source != "tenant" {
					t.Fatalf("tenant_note without source tenant: %+v", rec)
				}
				if rec.TraceID == "trace-of-victim" || rec.ActingFor == "ceo@victim.example" || rec.Grant != "" || rec.Code != "" {
					t.Fatalf("tenant controlled a platform field: %+v", rec)
				}
				if !strings.Contains(rec.Detail, `"claimedKind"`) || !strings.Contains(rec.Detail, `"parentHint":"trace-of-victim"`) || !strings.Contains(rec.Detail, "wired") {
					t.Fatalf("note detail lost the tenant's claims: %s", rec.Detail)
				}
			}
		}
		if notes == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("want 4 tenant_note records, saw %d (%d records)", notes, len(resp.Records))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !resp.Verified {
		t.Fatalf("chain not verified: %v", resp.Problems)
	}
}
