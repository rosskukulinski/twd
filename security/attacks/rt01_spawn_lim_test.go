package attacks

// RT-01 regression test (High): spawn `lim` (SpawnOptions.caps) used to be
// validated by mgmt (lim ⊆ effective(parent)) but never bound to the child
// node's identity: mgmt tagged the child with class.Tags and a receiver
// authorized on the raw WhoIs capMap (the policy grants for those tags), so
// the child carried its class tags' FULL caps.
//
// The fix (ADR-010 / RFC §4.5): mgmt signs a node certificate
// {tenant, objectID, stableNodeID, tags, lim, parent, root, exp} with the
// tenant's OBO root key at `ready`; the receiving netd fetches the sender's
// certificate (keyed by the WhoIs stable node ID), verifies it, and
// narrows the sender's capMap to CapMap ∩ lim (internal/caps) before the
// request reaches tenant code. This test drives that path at the unit
// level: the policy evaluator for what WhoIs reports, nodecert for what
// mgmt signs, caps.Intersect for what the receiver computes.
//
// The hostd integration test TestSpawnLimEnforcedAtReceiver and the e2e
// check (d) exercise the same invariant over real nodes.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/internal/caps"
	"github.com/rosskukulinski/twd/internal/nodecert"
	"github.com/rosskukulinski/twd/mgmt"
)

// A policy where the child's sub-agent tag is granted BOTH lookup_order and
// refund, the parent only lookup_order, and tag:sub is owned by tag:agent.
const rt01Policy = `{
  "tagOwners": { "tag:agent": ["tag:twd"], "tag:sub": ["tag:agent"] },
  "grants": [
    { "src": ["tag:agent"], "dst": ["tag:mcp"], "app": { "example.com/cap/mcp": [ {"tools": ["lookup_order"]} ] } },
    { "src": ["tag:sub"],   "dst": ["tag:mcp"], "app": { "example.com/cap/mcp": [ {"tools": ["lookup_order", "refund"]} ] } }
  ]
}`

func toolsOf(capMap map[string][]json.RawMessage) []string {
	var out []string
	for _, raw := range capMap["example.com/cap/mcp"] {
		var v struct {
			Tools []string `json:"tools"`
		}
		_ = json.Unmarshal(raw, &v)
		out = append(out, v.Tools...)
	}
	return out
}

func TestRT01_SpawnLimBindsToIdentity(t *testing.T) {
	p, err := mgmt.ParsePolicy([]byte(rt01Policy))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}

	parentTags := []string{"tag:agent"}
	childTags := []string{"tag:sub"} // == class.Tags that mgmt stamps on the node
	lim := json.RawMessage(`{"example.com/cap/mcp": [{"tools": ["lookup_order"]}]}`)

	// mgmt admits the spawn: lim ⊆ effective(parent).
	if e := p.CheckSpawnCaps(parentTags, lim); e != nil {
		t.Fatalf("precondition: spawn should be admitted by mgmt, got %v", e.Message)
	}

	// What WhoIs reports for the child at the MCP server (dst tag:mcp) = the
	// policy caps for child.Tags, which include refund. This is the gap.
	whois := p.EffectiveCaps(childTags)["tag:mcp"]
	sawRefund := false
	for _, tool := range toolsOf(whois) {
		sawRefund = sawRefund || tool == "refund"
	}
	if !sawRefund {
		t.Fatalf("precondition: the class tag's policy caps must include refund for this test to mean anything: %v", toolsOf(whois))
	}

	// mgmt signs the child's node certificate at ready, carrying lim.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	cert, err := nodecert.Sign(priv, nodecert.Payload{TenantID: "tn", ObjectID: "sub-1", StableNodeID: "nSUB", Tags: childTags,
		Lim: lim, ParentID: "agent-1", RootID: "agent-1", IAT: now.Unix(), EXP: now.Add(nodecert.TTL).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	// The receiver verifies it against the tenant's pinned OBO key and binds
	// it to the transport peer's stable node ID...
	cp, err := nodecert.Verify(pub, cert, "nSUB", now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := nodecert.Verify(pub, cert, "nOTHER", now); err == nil {
		t.Fatal("a certificate must not verify for another node")
	}
	// ... and computes effective = CapMap ∩ lim.
	eff := caps.Intersect(whois, cp.Lim)
	tools := toolsOf(eff)
	for _, tool := range tools {
		if tool == "refund" {
			t.Fatalf("RT-01: child spawned with lim=[lookup_order] still carries `refund` after the receiver applied its certificate: %v", tools)
		}
	}
	if len(tools) != 1 || tools[0] != "lookup_order" {
		t.Fatalf("the child must keep exactly what lim allows: %v", tools)
	}

	// A peer with no certificate (not a twd object) keeps its WhoIs caps:
	// lim never widens, and its absence never narrows.
	if got := toolsOf(caps.Intersect(whois, nil)); len(got) != 2 {
		t.Fatalf("no lim must leave the capMap unchanged: %v", got)
	}
	// A certificate re-signed by another key (lim widened) is rejected.
	d, _ := nodecert.Decode(cert)
	d.Lim = json.RawMessage(`{"example.com/cap/mcp": [{"tools": ["lookup_order","refund"]}]}`)
	forged, _ := nodecert.Sign(ed25519.NewKeyFromSeed(make([]byte, 32)), *d)
	if _, err := nodecert.Verify(pub, forged, "nSUB", now); err == nil {
		t.Fatal("a certificate signed by another key must not verify")
	}
}
