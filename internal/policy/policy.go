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

// Merge returns policy with s added: missing tagOwners entries and grants
// are appended; nothing existing is changed or reordered, and comments are
// kept.
func Merge(policy []byte, s Snippet) ([]byte, error) {
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
	return v.Pack(), nil
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
// → write with If-Match. It is a no-op if nothing is missing.
func Apply(ctx context.Context, api API, s Snippet) (ApplyResult, error) {
	var res ApplyResult
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
	next, err := Merge(cur, s)
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
