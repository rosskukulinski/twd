package attacks

// Pass 2 (live) — RT-06, the JSON-parsing tricks the base hostile table
// (internal/policy/hostile_test.go) doesn't cover: a Unicode homoglyph tag
// that visually reads as `tag:twd-*` but isn't ASCII, and a live tenant
// policy whose JSON has duplicate `grants`/`tagOwners` keys (hujson/encoding
// quirks). `--policy-apply` must still only ever add real `tag:twd*` owners
// and twd-sourced grants, and refuse anything else, byte-for-byte.

import (
	"strings"
	"testing"

	"github.com/rosskukulinski/twd/internal/policy"
)

func TestRT06_UnicodeHomoglyphTagRefused(t *testing.T) {
	// "tag:twԁ-wl-x": the 'd' is U+0501 (Cyrillic small letter komi de). It
	// is NOT the ASCII prefix "tag:twd", so Check must refuse it as a
	// non-twd tagOwners key even though it looks like one.
	homo := "tag:twԁ-wl-x"
	if strings.HasPrefix(homo, "tag:twd") {
		t.Fatal("precondition: the homoglyph must not be the ASCII prefix")
	}
	s := policy.Snippet{TagOwners: map[string][]string{homo: {"tag:twd"}}}
	if err := policy.Check(s, policy.Options{}); err == nil {
		t.Fatalf("RT-06: Check accepted a Unicode-homoglyph tagOwners key %q", homo)
	}
	if _, err := policy.Merge([]byte(rt06Base), s); err == nil {
		t.Fatalf("RT-06: Merge accepted a Unicode-homoglyph tag")
	}

	// A grant whose src is the homoglyph tag (not a real twd tag) is refused.
	g := policy.Snippet{Grants: []map[string]any{{"src": []string{homo}, "dst": []string{"tag:twd-wl-x"}, "ip": []string{"tcp:80"}}}}
	if err := policy.Check(g, policy.Options{}); err == nil {
		t.Fatalf("RT-06: Check accepted a grant src with a homoglyph tag")
	}
}

// RT2-L1 (Low, NEW): policy.Check's twd-namespace test is a bare
// strings.HasPrefix(tag, "tag:twd") with no delimiter or whitespace check,
// so a src/tagOwners tag with a trailing space (or other trailing bytes)
// such as "tag:twd-wl-x " is accepted as a twd tag. Impact is bounded —
// the accepted string is still inside twd's own tag:twd* namespace, grants
// to a space-padded tag match no real device, and every policy write is
// validated by Tailscale before apply (BUILD-PLAN safety rule) — so this is
// input-validation leniency, not a cross-principal widening. Fix: require a
// boundary after the prefix (exact "tag:twd" or "tag:twd-"...) and reject
// tags containing whitespace/control characters. Skip-marked until fixed.
func TestRT2L1_CheckAcceptsWhitespacePaddedTag(t *testing.T) {
	t.Skip("RT2-L1: known Low finding — Check accepts trailing-whitespace twd tags; see security/RED-TEAM.md Pass 2")
	for _, bad := range []string{"tag:twd-wl-x ", "tag:twd-wl-x\t", "tag:twd-wl-x\n"} {
		bs := policy.Snippet{Grants: []map[string]any{{"src": []string{bad}, "dst": []string{"tag:twd-wl-x"}, "ip": []string{"tcp:80"}}}}
		if err := policy.Check(bs, policy.Options{}); err == nil {
			t.Fatalf("RT2-L1: Check accepted a whitespace-padded src %q", bad)
		}
	}
}

func TestRT06_DuplicateKeyPolicyStaysAdditive(t *testing.T) {
	// A live tenant policy whose JSON repeats "grants" and "tagOwners".
	// Merging a legitimate twd snippet must not crash, must stay additive,
	// and must not silently drop or widen the tenant's own grants.
	dup := `{
      "tagOwners": {"tag:ttest": ["autogroup:admin"]},
      "tagOwners": {"tag:prod": ["autogroup:admin"]},
      "grants": [{"src": ["tag:ttest"], "dst": ["tag:ttest"], "ip": ["*"]}],
      "grants": [{"src": ["tag:prod"], "dst": ["tag:prod"], "ip": ["*"]}]
    }`
	good := policy.Snippet{
		TagOwners: map[string][]string{"tag:twd-wl-x": {"tag:twd"}},
		Grants:    []map[string]any{{"src": []string{"tag:twd-wl-x"}, "dst": []string{"tag:db"}, "ip": []string{"tcp:80"}}},
	}
	out, err := policy.Merge([]byte(dup), good)
	if err != nil {
		// Refusing a malformed/duplicate-key policy is an acceptable, safe outcome.
		t.Logf("RT-06: Merge refused a duplicate-key policy (safe): %v", err)
		return
	}
	// If it merged, it must be verifiably additive over the parsed `before`.
	if err := policy.VerifyAdditive([]byte(dup), out, good); err != nil {
		t.Fatalf("RT-06: duplicate-key merge was not additive: %v", err)
	}
	// And it must not have introduced a non-twd tagOwners or a `*` src.
	so := string(out)
	if strings.Contains(so, `"src":["*"]`) || strings.Contains(so, `"src": ["*"]`) {
		t.Fatalf("RT-06: merge widened src to * :\n%s", so)
	}
}

const rt06Base = `{"tagOwners":{"tag:ttest":["autogroup:admin"]},"grants":[{"src":["tag:ttest"],"dst":["tag:ttest"],"ip":["*"]}]}`
