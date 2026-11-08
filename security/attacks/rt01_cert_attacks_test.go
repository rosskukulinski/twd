package attacks

// Pass 2 (live) — RT-01 extra attacker angles beyond the base test
// (rt01_spawn_lim_test.go): a stale certificate after a re-mint, an expired
// certificate, a certificate whose iat is in the future, a garbage `lim`
// (must narrow to nothing, never widen), and the no-cert case (caps
// unchanged). These are the receiver-side checks nodecert.Verify +
// caps.Intersect make; netd's applyCert (netd/ingress.go:64) wraps them and
// additionally empties the CapMap when the cert *fetch* fails
// (CapsWithheld), the cross-host "fetch fails -> fail closed" path the brief
// asks about — covered there by code and by netd's ingress tests.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/internal/caps"
	"github.com/rosskukulinski/twd/internal/nodecert"
)

func TestRT01_CertStaleExpiredAndGarbageLim(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	lim := json.RawMessage(`{"example.com/cap/mcp":[{"tools":["lookup_order"]}]}`)

	mk := func(node string, iat, exp time.Time) string {
		c, err := nodecert.Sign(priv, nodecert.Payload{TenantID: "tn", ObjectID: "o1", StableNodeID: node,
			Tags: []string{"tag:sub"}, Lim: lim, RootID: "r", IAT: iat.Unix(), EXP: exp.Unix()})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// Valid cert for node nA verifies for nA, not for nB (re-mint gives a new
	// stable node ID; the old cert must not travel to the new node).
	cert := mk("nA", now, now.Add(nodecert.TTL))
	if _, err := nodecert.Verify(pub, cert, "nA", now); err != nil {
		t.Fatalf("valid cert must verify: %v", err)
	}
	if _, err := nodecert.Verify(pub, cert, "nB", now); err == nil {
		t.Fatal("RT-01: a cert for nA verified for a re-minted node nB (stale-cert reuse)")
	}

	// Expired cert is refused (nodecert.TTL window closed).
	old := mk("nA", now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	if _, err := nodecert.Verify(pub, old, "nA", now); err == nil {
		t.Fatal("RT-01: an expired node cert was accepted")
	}
	// A cert whose iat is far in the future (clock-skew abuse) is refused.
	future := mk("nA", now.Add(time.Hour), now.Add(2*time.Hour))
	if _, err := nodecert.Verify(pub, future, "nA", now); err == nil {
		t.Fatal("RT-01: a not-yet-valid (future-iat) node cert was accepted")
	}

	// A valid cert, but the receiver intersects WhoIs caps with lim: a
	// capMap claiming refund is narrowed to lookup_order only.
	whois := map[string][]json.RawMessage{
		"example.com/cap/mcp": {json.RawMessage(`{"tools":["lookup_order"]}`), json.RawMessage(`{"tools":["refund"]}`)},
	}
	p, err := nodecert.Verify(pub, cert, "nA", now)
	if err != nil {
		t.Fatal(err)
	}
	got := toolsOf(caps.Intersect(whois, p.Lim))
	for _, x := range got {
		if x == "refund" {
			t.Fatalf("RT-01: refund survived the cert intersection: %v", got)
		}
	}

	// Garbage / unparseable lim must fail closed (narrow to nothing), never
	// pass the caps through.
	if g := caps.Intersect(whois, json.RawMessage(`{"not valid json`)); len(g) != 0 {
		t.Fatalf("RT-01: an unparseable lim did not fail closed: %v", g)
	}
	// A lim that doesn't mention the cap key drops it entirely.
	if g := caps.Intersect(whois, json.RawMessage(`{"other.com/cap":[{}]}`)); len(g) != 0 {
		t.Fatalf("RT-01: a lim omitting the cap key still left caps: %v", g)
	}
	// Empty / null lim leaves caps unchanged (a non-twd caller, no narrowing).
	if g := caps.Intersect(whois, nil); len(toolsOf(g)) != 2 {
		t.Fatalf("RT-01: nil lim must not narrow a non-object caller: %v", toolsOf(g))
	}
}
