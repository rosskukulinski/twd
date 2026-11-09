package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const base = `// my tailnet
{
	"tagOwners": {
		"tag:ttest": ["autogroup:admin"], // keep me
	},
	"grants": [
		{"src": ["tag:ttest"], "dst": ["tag:ttest"], "ip": ["*"]},
	],
}
`

func snip() Snippet {
	return Snippet{
		TagOwners: map[string][]string{"tag:ttest": {"tag:twd-marker"}, "tag:twd-agent": {"autogroup:admin", "tag:twd"}},
		Grants: []map[string]any{
			{"src": []string{"tag:ttest"}, "dst": []string{"tag:ttest"}, "ip": []string{"*"}}, // already there
			{"src": []string{"tag:ttest"}, "dst": []string{"tag:twd-agent"}, "ip": []string{"tcp:80"},
				"app": map[string]any{"example.com/cap/mcp": []any{map[string]any{"tools": []string{"lookup_order"}}}}},
		},
	}
}

var testOpts = Options{AllowTags: []string{"tag:ttest"}}

func TestMergeKeepsExisting(t *testing.T) {
	out, err := MergeWith([]byte(base), snip(), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"// my tailnet", "// keep me", `"tag:twd-agent"`, `"example.com/cap/mcp"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("merged policy lacks %q:\n%s", want, s)
		}
	}
	if strings.Count(s, `"dst": ["tag:ttest"]`)+strings.Count(s, `"dst":["tag:ttest"]`) != 1 {
		t.Fatalf("existing grant duplicated:\n%s", s)
	}
	// Idempotent.
	miss, err := Missing(out, snip())
	if err != nil || !miss.Empty() {
		t.Fatalf("second merge still missing %+v (%v)", miss, err)
	}
	// An existing tag gains the snippet's missing owner; its own stay.
	if !strings.Contains(s, `"tag:twd-marker"`) {
		t.Fatalf("missing owner not appended to the existing tagOwners entry:\n%s", s)
	}
	if !strings.Contains(s, `"autogroup:admin"`) {
		t.Fatalf("existing owners lost:\n%s", s)
	}
}

func TestMergeEmptyPolicy(t *testing.T) {
	out, err := MergeWith([]byte(`{}`), snip(), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := Missing(out, snip()); !m.Empty() {
		t.Fatalf("missing after merge into {}: %+v\n%s", m, out)
	}
}

type fakeAPI struct {
	body     []byte
	etag     string
	validErr error
	sets     int
	gotMatch string
}

func (f *fakeAPI) GetPolicyHuJSON(context.Context) ([]byte, string, error) {
	return f.body, f.etag, nil
}
func (f *fakeAPI) ValidatePolicy(context.Context, []byte) error { return f.validErr }
func (f *fakeAPI) SetPolicy(_ context.Context, b []byte, m string) (string, error) {
	f.sets++
	f.gotMatch = m
	f.body, f.etag = b, f.etag+"'"
	return f.etag, nil
}

func TestApply(t *testing.T) {
	f := &fakeAPI{body: []byte(base), etag: `"e1"`}
	r, err := ApplyWith(context.Background(), f, snip(), testOpts)
	if err != nil || !r.Changed || f.sets != 1 || f.gotMatch != `"e1"` {
		t.Fatalf("apply: %+v err=%v sets=%d match=%s", r, err, f.sets, f.gotMatch)
	}
	r, err = ApplyWith(context.Background(), f, snip(), testOpts)
	if err != nil || r.Changed || f.sets != 1 {
		t.Fatalf("second apply should be a no-op: %+v %v", r, err)
	}
	// Validation failure: nothing written.
	f2 := &fakeAPI{body: []byte(base), etag: `"e1"`, validErr: errors.New("bad")}
	if _, err := ApplyWith(context.Background(), f2, snip(), testOpts); err == nil || f2.sets != 0 {
		t.Fatalf("invalid policy applied: err=%v sets=%d", err, f2.sets)
	}
}

// keyless (M10): nodeAttrs entries and the autogroup:admin → tag:twd-mgmt
// admin grant merge additively; without AllowSrcs the autogroup source is
// refused (RT-06), and nodeAttrs may only set funnel on twd tags.
func keylessSnip() Snippet {
	return Snippet{
		TagOwners: map[string][]string{"tag:twd-mgmt": {"autogroup:admin"}},
		Grants: []map[string]any{{"src": []string{"autogroup:admin"}, "dst": []string{"tag:twd-mgmt"}, "ip": []string{"tcp:443", "tcp:80"},
			"app": map[string]any{"twd.dev/cap/admin": []any{map[string]any{}}}}},
		NodeAttrs: []map[string]any{{"target": []string{"tag:twd-mgmt"}, "attr": []string{"funnel"}}},
	}
}

func TestMergeNodeAttrsAndAdminGrant(t *testing.T) {
	withAttrs := strings.TrimSuffix(strings.TrimSpace(base), "}") + `	"nodeAttrs": [
		{"target": ["tag:other"], "attr": ["funnel"]}, // keep me too
	],
}
`
	if _, err := MergeWith([]byte(withAttrs), keylessSnip(), Options{}); err == nil || !strings.Contains(err.Error(), "autogroup:admin") {
		t.Fatalf("autogroup:admin source must need AllowSrcs: %v", err)
	}
	opts := Options{AllowSrcs: []string{"autogroup:admin"}}
	for _, in := range []string{base, withAttrs, `{}`} {
		out, err := MergeWith([]byte(in), keylessSnip(), opts)
		if err != nil {
			t.Fatalf("merge into %q: %v", in[:min(len(in), 20)], err)
		}
		s := string(out)
		for _, want := range []string{`"attr": ["funnel"]`, `"twd.dev/cap/admin"`, `"tag:twd-mgmt"`} {
			if !strings.Contains(s, want) && !strings.Contains(s, strings.ReplaceAll(want, " ", "")) {
				t.Fatalf("merged policy lacks %q:\n%s", want, s)
			}
		}
		if in == withAttrs && (!strings.Contains(s, "// keep me too") || !strings.Contains(s, `"tag:other"`)) {
			t.Fatalf("existing nodeAttrs entry lost:\n%s", s)
		}
		if miss, err := Missing(out, keylessSnip()); err != nil || !miss.Empty() {
			t.Fatalf("not idempotent: %+v (%v)", miss, err)
		}
		if err := VerifyAdditive([]byte(in), out, must(Missing([]byte(in), keylessSnip()))); err != nil {
			t.Fatalf("not additive: %v", err)
		}
	}
	// A modified existing nodeAttrs entry is refused.
	tampered := strings.Replace(withAttrs, `"tag:other"`, `"tag:twd-mgmt"`, 1)
	if err := VerifyAdditive([]byte(withAttrs), []byte(tampered), Snippet{}); err == nil || !strings.Contains(err.Error(), "nodeAttrs[0] modified") {
		t.Fatalf("modified nodeAttrs not caught: %v", err)
	}
	for _, bad := range []Snippet{
		{NodeAttrs: []map[string]any{{"target": []string{"tag:other"}, "attr": []string{"funnel"}}}},
		{NodeAttrs: []map[string]any{{"target": []string{"tag:twd-mgmt"}, "attr": []string{"mullvad"}}}},
		{NodeAttrs: []map[string]any{{"target": []string{"tag:twd-mgmt"}, "attr": []string{"funnel"}, "app": 1}}},
		{NodeAttrs: []map[string]any{{"target": []string{"*"}, "attr": []string{"funnel"}}}},
	} {
		if err := Check(bad, opts); err == nil {
			t.Fatalf("hostile nodeAttrs accepted: %+v", bad)
		}
	}
}

func must(s Snippet, err error) Snippet {
	if err != nil {
		panic(err)
	}
	return s
}
