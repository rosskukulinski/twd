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
	// Existing owners untouched.
	if strings.Contains(s, `"tag:twd-marker"`) {
		t.Fatalf("existing tagOwners entry overwritten:\n%s", s)
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
