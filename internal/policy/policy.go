// Package policy merges twd's policy additions (tagOwners entries and
// grants) into a tailnet policy file without disturbing existing rules or
// comments, and applies the result safely: validate first, then write with
// If-Match so a concurrent edit is never overwritten.
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/tailscale/hujson"
)

// Snippet is a set of additions.
type Snippet struct {
	TagOwners map[string][]string `json:"tagOwners,omitempty"`
	Grants    []map[string]any    `json:"grants,omitempty"`
}

// Empty reports whether the snippet adds nothing.
func (s Snippet) Empty() bool { return len(s.TagOwners) == 0 && len(s.Grants) == 0 }

// HuJSON renders the snippet for humans ("add to your policy").
func (s Snippet) HuJSON() string {
	var b strings.Builder
	if len(s.TagOwners) > 0 {
		b.WriteString("\"tagOwners\": {\n")
		tags := make([]string, 0, len(s.TagOwners))
		for t := range s.TagOwners {
			tags = append(tags, t)
		}
		sort.Strings(tags)
		for _, t := range tags {
			ob, _ := json.Marshal(s.TagOwners[t])
			fmt.Fprintf(&b, "  %q: %s,\n", t, ob)
		}
		b.WriteString("},\n")
	}
	if len(s.Grants) > 0 {
		b.WriteString("\"grants\": [\n")
		for _, g := range s.Grants {
			gb, _ := json.Marshal(g)
			fmt.Fprintf(&b, "  %s,\n", gb)
		}
		b.WriteString("],\n")
	}
	return b.String()
}

type doc struct {
	TagOwners map[string][]string `json:"tagOwners"`
	Grants    []any               `json:"grants"`
}

func normalize(v any) any {
	b, _ := json.Marshal(v)
	var out any
	json.Unmarshal(b, &out)
	return out
}

// Missing returns the part of s that the policy doesn't already contain
// (a tag already in tagOwners counts as present, whatever its owners).
func Missing(policy []byte, s Snippet) (Snippet, error) {
	std, err := hujson.Standardize(append([]byte(nil), policy...))
	if err != nil {
		return Snippet{}, fmt.Errorf("policy: %w", err)
	}
	var d doc
	if err := json.Unmarshal(std, &d); err != nil {
		return Snippet{}, fmt.Errorf("policy: %w", err)
	}
	out := Snippet{TagOwners: map[string][]string{}}
	for t, owners := range s.TagOwners {
		if _, ok := d.TagOwners[t]; !ok {
			out.TagOwners[t] = owners
		}
	}
	have := make([]any, len(d.Grants))
	for i, g := range d.Grants {
		have[i] = normalize(g)
	}
	for _, g := range s.Grants {
		ng := normalize(g)
		found := false
		for _, h := range have {
			if reflect.DeepEqual(h, ng) {
				found = true
				break
			}
		}
		if !found {
			out.Grants = append(out.Grants, g)
			have = append(have, ng)
		}
	}
	if len(out.TagOwners) == 0 {
		out.TagOwners = nil
	}
	return out, nil
}

func ptrEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// Options relax Check for callers that are not deploying a bundle: the e2e
// harness adds grants for its own test tag.
type Options struct {
	// AllowTags may additionally appear as tagOwners keys and grant sources.
	AllowTags []string
}

// TwdTagPrefix: the only tags twd may own or use as a grant source.
const TwdTagPrefix = "tag:twd"

// Check enforces what a twd-generated snippet may add to a tenant policy
// (RT-06). It never trusts the snippet's origin (a bundle's twd.jsonc):
//   - tagOwners keys must be twd tags (tag:twd, tag:twd-*), owned only by
//     tag:twd, autogroup:admin or other twd tags;
//   - every grant must have a non-empty src of twd tags (never "*",
//     autogroup:*, groups, users or arbitrary tags) and a non-empty dst of
//     tags or svc: names (never "*" or autogroup:*);
//   - grants may only carry src, dst, ip and app.
func Check(s Snippet, o Options) error {
	okTag := func(t string) bool {
		return strings.HasPrefix(t, TwdTagPrefix) || slices.Contains(o.AllowTags, t)
	}
	keys := make([]string, 0, len(s.TagOwners))
	for t := range s.TagOwners {
		keys = append(keys, t)
	}
	sort.Strings(keys)
	for _, t := range keys {
		if !okTag(t) {
			return fmt.Errorf("policy snippet: refusing to add tagOwners for %q: twd only adds %s* tags", t, TwdTagPrefix)
		}
		if len(s.TagOwners[t]) == 0 {
			return fmt.Errorf("policy snippet: tagOwners %q has no owners", t)
		}
		for _, owner := range s.TagOwners[t] {
			if owner != "autogroup:admin" && !okTag(owner) {
				return fmt.Errorf("policy snippet: refusing owner %q for %q: owners may be autogroup:admin or %s* tags", owner, t, TwdTagPrefix)
			}
		}
	}
	for i, g := range s.Grants {
		for k := range g {
			switch k {
			case "src", "dst", "ip", "app":
			default:
				return fmt.Errorf("policy snippet: grant %d has field %q; twd only adds src/dst/ip/app", i, k)
			}
		}
		src, dst := strs(g["src"]), strs(g["dst"])
		if src == nil || dst == nil {
			return fmt.Errorf("policy snippet: grant %d: src and dst must be non-empty lists of strings", i)
		}
		for _, x := range src {
			if !okTag(x) {
				return fmt.Errorf("policy snippet: grant %d: refusing src %q: twd grants only have %s* sources (never *, autogroup:*, groups or users)", i, x, TwdTagPrefix)
			}
		}
		for _, x := range dst {
			if x == "*" || strings.HasPrefix(x, "autogroup:") || !(strings.HasPrefix(x, "tag:") || strings.HasPrefix(x, "svc:")) {
				return fmt.Errorf("policy snippet: grant %d: refusing dst %q: twd grants only target tags or svc: names (never * or autogroup:*)", i, x)
			}
		}
		if _, has := g["ip"]; has && strs(g["ip"]) == nil {
			return fmt.Errorf("policy snippet: grant %d: ip must be a non-empty list of strings", i)
		}
	}
	return nil
}

// strs returns v as a non-empty []string, or nil.
func strs(v any) []string {
	var out []string
	switch x := v.(type) {
	case []string:
		out = x
	case []any:
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil
			}
			out = append(out, s)
		}
	default:
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Merge returns policy with s added: missing tagOwners entries and grants
// are appended; nothing existing is changed or reordered, and comments are
// kept. It refuses (RT-06) snippets Check rejects, and any result in which
// an existing rule was removed or modified, reporting the difference.
func Merge(policy []byte, s Snippet) ([]byte, error) { return MergeWith(policy, s, Options{}) }

// MergeWith is Merge with Options.
func MergeWith(policy []byte, s Snippet, o Options) ([]byte, error) {
	if err := Check(s, o); err != nil {
		return nil, err
	}
	miss, err := Missing(policy, s)
	if err != nil {
		return nil, err
	}
	v, err := hujson.Parse(policy)
	if err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	if miss.Empty() {
		return policy, nil
	}
	var ops []map[string]any
	if len(miss.TagOwners) > 0 {
		if v.Find("/tagOwners") == nil {
			ops = append(ops, map[string]any{"op": "add", "path": "/tagOwners", "value": miss.TagOwners})
		} else {
			tags := make([]string, 0, len(miss.TagOwners))
			for t := range miss.TagOwners {
				tags = append(tags, t)
			}
			sort.Strings(tags)
			for _, t := range tags {
				ops = append(ops, map[string]any{"op": "add", "path": "/tagOwners/" + ptrEscape(t), "value": miss.TagOwners[t]})
			}
		}
	}
	if len(miss.Grants) > 0 {
		if v.Find("/grants") == nil {
			ops = append(ops, map[string]any{"op": "add", "path": "/grants", "value": miss.Grants})
		} else {
			for _, g := range miss.Grants {
				ops = append(ops, map[string]any{"op": "add", "path": "/grants/-", "value": g})
			}
		}
	}
	pb, _ := json.Marshal(ops)
	if err := v.Patch(pb); err != nil {
		return nil, fmt.Errorf("policy patch: %w", err)
	}
	v.Format()
	out := v.Pack()
	if err := VerifyAdditive(policy, out, miss); err != nil {
		return nil, err
	}
	return out, nil
}

// VerifyAdditive checks that after is before plus exactly the additions in
// added: every other top-level key is unchanged, every existing tagOwners
// entry and grant is present and unchanged (grants in the same order), and
// nothing else was added. On failure the error lists the differences.
func VerifyAdditive(before, after []byte, added Snippet) error {
	var b, a map[string]any
	if err := parseStd(before, &b); err != nil {
		return err
	}
	if err := parseStd(after, &a); err != nil {
		return err
	}
	var diff []string
	for k, bv := range b {
		if k == "tagOwners" || k == "grants" {
			continue
		}
		if av, ok := a[k]; !ok {
			diff = append(diff, "- "+k+" (removed)")
		} else if !reflect.DeepEqual(normalize(bv), normalize(av)) {
			diff = append(diff, "~ "+k+" (modified)")
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok && k != "tagOwners" && k != "grants" {
			diff = append(diff, "+ "+k+" (added)")
		}
	}
	bt, _ := b["tagOwners"].(map[string]any)
	at, _ := a["tagOwners"].(map[string]any)
	for t, bo := range bt {
		if ao, ok := at[t]; !ok {
			diff = append(diff, "- tagOwners."+t+" (removed)")
		} else if !reflect.DeepEqual(normalize(bo), normalize(ao)) {
			diff = append(diff, "~ tagOwners."+t+" (modified: "+compactJSON(bo)+" → "+compactJSON(ao)+")")
		}
	}
	for t, ao := range at {
		if _, ok := bt[t]; ok {
			continue
		}
		want, ok := added.TagOwners[t]
		if !ok || !reflect.DeepEqual(normalize(want), normalize(ao)) {
			diff = append(diff, "+ tagOwners."+t+" = "+compactJSON(ao)+" (not in the snippet)")
		}
	}
	bg, _ := b["grants"].([]any)
	ag, _ := a["grants"].([]any)
	if len(ag) < len(bg) {
		diff = append(diff, fmt.Sprintf("- grants: %d of %d existing grants removed", len(bg)-len(ag), len(bg)))
	} else {
		for i := range bg {
			if !reflect.DeepEqual(normalize(bg[i]), normalize(ag[i])) {
				diff = append(diff, fmt.Sprintf("~ grants[%d] modified: %s → %s", i, compactJSON(bg[i]), compactJSON(ag[i])))
			}
		}
		extra := ag[len(bg):]
		if len(extra) != len(added.Grants) {
			diff = append(diff, fmt.Sprintf("+ grants: %d appended, snippet adds %d", len(extra), len(added.Grants)))
		} else {
			for i, g := range extra {
				if !reflect.DeepEqual(normalize(added.Grants[i]), normalize(g)) {
					diff = append(diff, fmt.Sprintf("+ grants[%d] = %s (not in the snippet)", len(bg)+i, compactJSON(g)))
				}
			}
		}
	}
	if len(diff) > 0 {
		return &DiffError{Diff: diff}
	}
	return nil
}

// DiffError: the merged policy is not before + the snippet.
type DiffError struct{ Diff []string }

func (e *DiffError) Error() string {
	return "policy merge refused: the result is not purely additive:\n  " + strings.Join(e.Diff, "\n  ")
}

func parseStd(b []byte, v any) error {
	std, err := hujson.Standardize(append([]byte(nil), b...))
	if err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	if len(bytes.TrimSpace(std)) == 0 {
		std = []byte("{}")
	}
	if err := json.Unmarshal(std, v); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	return nil
}

func compactJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// API is the Tailscale API surface Apply needs.
type API interface {
	GetPolicyHuJSON(ctx context.Context) ([]byte, string, error)
	ValidatePolicy(ctx context.Context, body []byte) error
	SetPolicy(ctx context.Context, body []byte, ifMatch string) (string, error)
}

var _ API = (*tsapi.Client)(nil)

// ApplyResult reports what Apply did.
type ApplyResult struct {
	Added   Snippet
	Changed bool
	OldETag string
	NewETag string
	Before  []byte // the policy before (for backups)
}

// Apply adds s to the tailnet's policy: read (with ETag) → merge → validate
// → write with If-Match. It is a no-op if nothing is missing. It refuses
// snippets Check rejects and merges that are not purely additive (RT-06).
func Apply(ctx context.Context, api API, s Snippet) (ApplyResult, error) {
	return ApplyWith(ctx, api, s, Options{})
}

// ApplyWith is Apply with Options.
func ApplyWith(ctx context.Context, api API, s Snippet, o Options) (ApplyResult, error) {
	var res ApplyResult
	if err := Check(s, o); err != nil {
		return res, err
	}
	cur, etag, err := api.GetPolicyHuJSON(ctx)
	if err != nil {
		return res, fmt.Errorf("read policy: %w", err)
	}
	res.OldETag, res.Before = etag, cur
	miss, err := Missing(cur, s)
	if err != nil {
		return res, err
	}
	res.Added = miss
	if miss.Empty() {
		res.NewETag = etag
		return res, nil
	}
	next, err := MergeWith(cur, s, o)
	if err != nil {
		return res, err
	}
	if bytes.Equal(next, cur) {
		return res, nil
	}
	if err := api.ValidatePolicy(ctx, next); err != nil {
		return res, fmt.Errorf("validate: %w", err)
	}
	ne, err := api.SetPolicy(ctx, next, etag)
	if err != nil {
		return res, fmt.Errorf("apply (If-Match %s): %w", etag, err)
	}
	res.Changed, res.NewETag = true, ne
	return res, nil
}
