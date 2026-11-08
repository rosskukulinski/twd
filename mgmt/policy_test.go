package mgmt

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// samplePolicy is RFC §7.1, trimmed, plus a second grant that widens the
// parent's limits to exercise merge semantics.
const samplePolicy = `{
  "tagOwners": {
    "tag:twd": ["autogroup:admin"],
    "tag:twd-agent-support": ["tag:twd"],
    "tag:twd-agent-support-sub": ["tag:twd", "tag:twd-agent-support"],
  },
  "grants": [
    { "src": ["tag:twd-agent-support"], "dst": ["svc:payments-mcp"], "ip": ["tcp:443"],
      "app": { "acme.com/cap/mcp": [
        { "tools": ["lookup_order", "refund"],
          "limits": { "refund": { "maxUsd": 50, "perDayUsd": 200 } },
          "groups": { "refund": ["group:support"] } } ] } },
    { "src": ["tag:twd-agent-support"], "dst": ["tag:crm"], "ip": ["tcp:443"],
      "app": { "acme.com/cap/mcp": [ { "tools": ["notes"], "limits": { "refund": { "maxUsd": 75 } } } ],
               "acme.com/cap/crm": [ { "read": true } ] } },
    { "src": ["tag:twd-agent-support-sub"], "dst": ["svc:payments-mcp"], "ip": ["tcp:443"],
      "app": { "acme.com/cap/mcp": [{ "tools": ["lookup_order"] }] } },
    { "src": ["group:support"], "dst": ["svc:support"], "ip": ["tcp:443"] },
  ],
}`

func TestSpawnCapsSubset(t *testing.T) {
	p, err := ParsePolicy([]byte(samplePolicy))
	if err != nil {
		t.Fatal(err)
	}
	parent := []string{"tag:twd-agent-support"}
	ok := []string{
		`{"acme.com/cap/mcp": {"tools": ["lookup_order"]}}`,
		`{"acme.com/cap/mcp": {"tools": ["lookup_order", "refund"], "limits": {"refund": {"maxUsd": 10}}}}`,
		`{"acme.com/cap/mcp": {"tools": ["notes", "refund"]}}`,                       // union across grants/dsts
		`{"acme.com/cap/mcp": {"limits": {"refund": {"maxUsd": 75}}}}`,               // max across grants
		`{"acme.com/cap/mcp": [{"tools": ["refund"]}, {"tools": ["lookup_order"]}]}`, // CapMap-style list
		`{"acme.com/cap/crm": {"read": true}}`,
		`{"acme.com/cap/mcp": {"groups": {"refund": ["group:support"]}}}`,
		`{}`, `null`, ``,
	}
	for _, c := range ok {
		if e := p.CheckSpawnCaps(parent, json.RawMessage(c)); e != nil {
			t.Errorf("%s: unexpected %v", c, e)
		}
	}
	bad := map[string]string{
		`{"acme.com/cap/mcp": {"tools": ["lookup_order", "delete_all"]}}`:  `caps["acme.com/cap/mcp"].tools[1]`,
		`{"acme.com/cap/mcp": {"limits": {"refund": {"maxUsd": 76}}}}`:     `caps["acme.com/cap/mcp"].limits.refund.maxUsd`,
		`{"acme.com/cap/mcp": {"limits": {"refund": {"perDayUsd": 201}}}}`: `caps["acme.com/cap/mcp"].limits.refund.perDayUsd`,
		`{"acme.com/cap/mcp": {"admin": true}}`:                            `caps["acme.com/cap/mcp"].admin`,
		`{"acme.com/cap/secrets": {"get": ["*"]}}`:                         `caps["acme.com/cap/secrets"]`,
		`{"acme.com/cap/crm": {"write": true}}`:                            `caps["acme.com/cap/crm"].write`,
		`{"acme.com/cap/mcp": [{"tools": ["refund"]}, {"tools": ["x"]}]}`:  `caps["acme.com/cap/mcp"][1].tools[0]`,
		`{"acme.com/cap/mcp": {"groups": {"refund": ["group:admins"]}}}`:   `caps["acme.com/cap/mcp"].groups.refund[0]`,
	}
	for c, field := range bad {
		e := p.CheckSpawnCaps(parent, json.RawMessage(c))
		if e == nil || e.Code != "delegation_exceeds_parent" || e.Missing["field"] != field {
			t.Errorf("%s: got %+v, want field %s", c, e, field)
		}
	}
	// The sub-agent's own tag only has lookup_order: it can't re-widen.
	if e := p.CheckSpawnCaps([]string{"tag:twd-agent-support-sub"}, json.RawMessage(`{"acme.com/cap/mcp": {"tools": ["refund"]}}`)); e == nil {
		t.Error("grandchild widened beyond its parent")
	}
	if e := p.CheckSpawnCaps(parent, json.RawMessage(`[1]`)); e == nil || e.Code != "bad_request" {
		t.Errorf("non-object caps: %v", e)
	}
	// Tag ownership.
	if !p.TagOwnedBy("tag:twd-agent-support-sub", parent) || p.TagOwnedBy("tag:crm", parent) || !p.TagOwnedBy("tag:twd-agent-support", parent) {
		t.Error("TagOwnedBy")
	}
	eff := p.EffectiveCaps(parent)
	if len(eff["svc:payments-mcp"]["acme.com/cap/mcp"]) != 1 || len(eff["tag:crm"]) != 2 || eff["svc:support"] != nil {
		t.Errorf("effective %v", eff)
	}
}

func TestPolicyCacheETag(t *testing.T) {
	c := newFakeClock()
	fetches, notMods := 0, 0
	pc := &PolicyCache{Clock: c, MaxAge: 10 * time.Second, Fetch: func(ctx context.Context, tn Tenant, etag string) ([]byte, string, bool, error) {
		fetches++
		if etag == `"v1"` {
			notMods++
			return nil, etag, true, nil
		}
		return []byte(samplePolicy), `"v1"`, false, nil
	}}
	tn := Tenant{ID: "tn"}
	for range 3 {
		if _, err := pc.Get(context.Background(), tn); err != nil {
			t.Fatal(err)
		}
	}
	if fetches != 1 {
		t.Fatalf("fetches within MaxAge: %d", fetches)
	}
	c.Advance(11 * time.Second)
	p, err := pc.Get(context.Background(), tn)
	if err != nil || p == nil || fetches != 2 || notMods != 1 {
		t.Fatalf("revalidate: %v fetches=%d notMods=%d", err, fetches, notMods)
	}
}
