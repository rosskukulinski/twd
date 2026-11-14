package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rosskukulinski/twd/internal/policy"
	"github.com/rosskukulinski/twd/mgmt"
)

// TestOperatorSnippet: the oauth-mode snippet is exactly what INSTALL.md
// prints (tag:twd owns tag:twd-mgmt and tag:twd-host; host ⇄ mgmt grants;
// autogroup:admin's admin cap; no nodeAttrs), passes the RT-06 checker with
// the keyless allowance for autogroup:admin, and merges additively into a
// policy that already has rules (existing owners kept, nothing removed).
func TestOperatorSnippet(t *testing.T) {
	s := operatorSnippet()
	wantOwners := map[string][]string{
		"tag:twd":      {"autogroup:admin"},
		"tag:twd-mgmt": {"autogroup:admin", "tag:twd"},
		"tag:twd-host": {"autogroup:admin", "tag:twd"},
	}
	if !reflect.DeepEqual(s.TagOwners, wantOwners) {
		t.Fatalf("tagOwners %v", s.TagOwners)
	}
	gj, _ := json.Marshal(s.Grants)
	for _, want := range []string{
		`{"dst":["tag:twd-mgmt"],"ip":["tcp:443","tcp:80"],"src":["tag:twd-host"]}`,
		`{"dst":["tag:twd-host"],"ip":["tcp:7474"],"src":["tag:twd-mgmt"]}`,
		`{"app":{"` + mgmt.AdminCap + `":[{}]},"dst":["tag:twd-mgmt"],"ip":["tcp:443","tcp:80"],"src":["autogroup:admin"]}`,
	} {
		if !strings.Contains(string(gj), want) {
			t.Fatalf("grants %s lack %s", gj, want)
		}
	}
	if len(s.Grants) != 3 || len(s.NodeAttrs) != 0 {
		t.Fatalf("oauth snippet: %d grants, nodeAttrs %v (no Funnel in oauth mode)", len(s.Grants), s.NodeAttrs)
	}
	// wif: the operator snippet plus the issuer's own node, and Funnel on
	// that node only; mgmt's node is never Funnel-capable.
	k := keylessSnippet()
	wantK := map[string][]string{mgmt.IssuerTag: {"autogroup:admin", "tag:twd"}}
	for t, o := range s.TagOwners {
		wantK[t] = o
	}
	if !reflect.DeepEqual(k.TagOwners, wantK) {
		t.Fatalf("wif tagOwners %v, want %v", k.TagOwners, wantK)
	}
	if kj, _ := json.Marshal(k.NodeAttrs); len(k.NodeAttrs) != 1 || string(kj) != `[{"attr":["funnel"],"target":["`+mgmt.IssuerTag+`"]}]` || strings.Contains(string(kj), mgmt.MgmtTag) {
		t.Fatalf("wif snippet must put funnel on %s only: %s", mgmt.IssuerTag, kj)
	}
	if _, ok := s.TagOwners[mgmt.IssuerTag]; ok {
		t.Fatal("operatorSnippet must not be mutated by keylessSnippet")
	}
	if mgmt.IssuerTag != policy.IssuerTag {
		t.Fatalf("mgmt.IssuerTag %q != policy.IssuerTag %q", mgmt.IssuerTag, policy.IssuerTag)
	}
	if err := policy.Check(policy.Snippet{NodeAttrs: []map[string]any{{"target": []string{mgmt.MgmtTag}, "attr": []string{"funnel"}}}}, keylessPolicyOptions()); err == nil {
		t.Fatal("policy.Check must refuse funnel on " + mgmt.MgmtTag)
	}
	for _, sn := range []policy.Snippet{s, keylessSnippet()} {
		if err := policy.Check(sn, keylessPolicyOptions()); err != nil {
			t.Fatalf("RT-06 check: %v", err)
		}
	}
	if err := policy.Check(s, policy.Options{}); err == nil {
		t.Fatal("autogroup:admin as a grant source must need the explicit allowance")
	}
	before := []byte(`{
  // Ross's rules
  "tagOwners": {"tag:twd": ["group:ops"], "tag:twd-mgmt": ["autogroup:admin"], "tag:web": ["autogroup:admin"]},
  "grants": [{"src": ["autogroup:member"], "dst": ["tag:web"], "ip": ["*"]}],
  "ssh": [{"action": "check", "src": ["autogroup:member"], "dst": ["autogroup:self"], "users": ["autogroup:nonroot"]}],
}`)
	miss, err := policy.Missing(before, s)
	if err != nil {
		t.Fatal(err)
	}
	after, err := policy.MergeWith(before, s, keylessPolicyOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.VerifyAdditive(before, after, miss); err != nil {
		t.Fatalf("not additive: %v", err)
	}
	if !strings.Contains(string(after), "Ross's rules") || !strings.Contains(string(after), `"group:ops"`) {
		t.Fatalf("merge dropped existing content:\n%s", after)
	}
	if m, _ := policy.Missing(after, tagOwnersNeeded()); !m.Empty() {
		t.Fatalf("after merge, tag:twd still doesn't own the infra tags: %v", m)
	}
	if m, _ := policy.Missing(before, tagOwnersNeeded()); !reflect.DeepEqual(m.TagOwners, map[string][]string{"tag:twd-mgmt": {"tag:twd"}, "tag:twd-host": {"tag:twd"}}) {
		t.Fatalf("missing tag owners: %v", m.TagOwners)
	}
}

// TestModeChoiceAndTypedErrors: no mode → the three options, OAuth
// first; policy_missing_tag_owners carries the exact tagOwners snippet.
func TestModeChoiceAndTypedErrors(t *testing.T) {
	e := mintModeRequired()
	text := strings.Join(e.Lines, "\n")
	o, w, a := strings.Index(text, "--oauth-client-file"), strings.Index(text, "twd up --wif"), strings.Index(text, "--api-key-file")
	if e.Code != "mint_mode_required" || o < 0 || w < o || a < w || !strings.Contains(text, "Auth Keys") || !strings.Contains(text, "tag:twd") {
		t.Fatalf("mode choice order/content:\n%s", text)
	}
	pe := policyMissingTagOwners("why", nil)
	fix := strings.Join(pe.Fix, "\n")
	if pe.Code != "policy_missing_tag_owners" || !strings.Contains(fix, `"tag:twd-mgmt": ["autogroup:admin","tag:twd"]`) || !strings.Contains(fix, `"tag:twd-host": ["autogroup:admin","tag:twd"]`) {
		t.Fatalf("policy_missing_tag_owners fix:\n%s", fix)
	}
}
