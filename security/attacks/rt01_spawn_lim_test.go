package attacks

// RT-01 regression test (High): spawn `lim` (SpawnOptions.caps) is validated
// by mgmt (lim ⊆ effective(parent)) but is NEVER bound to the child node's
// identity. mgmt tags the child with `class.Tags`, and a receiver authorizes
// on the WhoIs capMap = the tailnet policy grants for those tags. So the
// deployed child carries the FULL policy caps of its class tags, not `lim`.
//
// This test encodes the behaviour we want AFTER the fix: the effective caps a
// receiver can see for the child must not exceed the requested `lim`. It is
// skip-marked with the finding ID. Remove the Skip to watch it fail against
// the current implementation (effective(child.Tags) ⊋ lim).

import (
	"encoding/json"
	"testing"

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

func TestRT01_SpawnLimBindsToIdentity(t *testing.T) {
	t.Skip("RT-01: lim is not bound to the child node identity; receiver sees full class-tag caps. Remove skip to reproduce the failure.")

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

	// What a receiver actually sees for the child = policy caps for child.Tags.
	eff := p.EffectiveCaps(childTags)
	sawRefund := false
	for _, byCap := range eff {
		for _, raw := range byCap["example.com/cap/mcp"] {
			var v struct {
				Tools []string `json:"tools"`
			}
			_ = json.Unmarshal(raw, &v)
			for _, tool := range v.Tools {
				if tool == "refund" {
					sawRefund = true
				}
			}
		}
	}

	// DESIRED (post-fix): the child's effective authority is capped by `lim`,
	// so `refund` must NOT be reachable. Current code FAILS this: the node is
	// tagged tag:sub and the receiver's WhoIs capMap includes refund.
	if sawRefund {
		t.Fatalf("RT-01: child spawned with lim=[lookup_order] still carries `refund` via its class tag; lim is not enforced at the receiver")
	}
}
