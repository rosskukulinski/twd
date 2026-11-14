package mgmt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/tailscale/hujson"
)

// Policy is the subset of a tailnet policy file twd evaluates.
type Policy struct {
	TagOwners map[string][]string `json:"tagOwners"`
	Grants    []Grant             `json:"grants"`
	// AutoApprovers.Services: Service name → tags/users that may host it
	// without an admin approving each node.
	AutoApprovers struct {
		Services map[string][]string `json:"services"`
	} `json:"autoApprovers"`
}

// Grant is one policy grant.
type Grant struct {
	Src []string                     `json:"src"`
	Dst []string                     `json:"dst"`
	IP  []string                     `json:"ip,omitempty"`
	App map[string][]json.RawMessage `json:"app,omitempty"`
}

// ParsePolicy parses HuJSON or JSON.
func ParsePolicy(b []byte) (*Policy, error) {
	std, err := hujson.Standardize(b)
	if err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	var p Policy
	if err := json.Unmarshal(std, &p); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	return &p, nil
}

func srcMatches(src []string, tags []string) bool {
	for _, s := range src {
		if s == "*" || slices.Contains(tags, s) {
			return true
		}
	}
	return false
}

// EffectiveCaps evaluates every app-cap grant whose src matches one of the
// tags, per destination: dst → cap name → values (appended in grant order,
// as wgengine/filter does).
func (p *Policy) EffectiveCaps(tags []string) map[string]map[string][]json.RawMessage {
	out := map[string]map[string][]json.RawMessage{}
	for _, g := range p.Grants {
		if len(g.App) == 0 || !srcMatches(g.Src, tags) {
			continue
		}
		for _, dst := range g.Dst {
			m := out[dst]
			if m == nil {
				m = map[string][]json.RawMessage{}
				out[dst] = m
			}
			for name, vals := range g.App {
				m[name] = append(m[name], vals...)
			}
		}
	}
	return out
}

// TagOwnedBy reports whether a node holding parentTags may assign child (a
// child tag is the parent's own tag or lists a parent tag in tagOwners).
func (p *Policy) TagOwnedBy(child string, parentTags []string) bool {
	if slices.Contains(parentTags, child) {
		return true
	}
	for _, o := range p.TagOwners[child] {
		if slices.Contains(parentTags, o) {
			return true
		}
	}
	return false
}

// CheckSpawnCaps checks requested ⊆ effective(parent) for every requested
// cap, where the parent's effective value for a cap is the merge (union /
// max / or) of every value granted to the parent's tags across all
// destinations. requested is the SpawnOptions.caps object
// ({capName: value} or {capName: [values]}). The returned error is a
// delegation_exceeds_parent *Error naming the offending field.
func (p *Policy) CheckSpawnCaps(parentTags []string, requested json.RawMessage) *Error {
	if len(bytes.TrimSpace(requested)) == 0 || string(bytes.TrimSpace(requested)) == "null" {
		return nil
	}
	var req map[string]any
	if err := decodeNumbers(requested, &req); err != nil {
		return errf(http.StatusBadRequest, "bad_request", "caps must be an object of {capName: value}: %v", err)
	}
	eff := p.EffectiveCaps(parentTags)
	names := make([]string, 0, len(req))
	for n := range req {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		var have any
		var dsts []string
		for dst, caps := range eff {
			for _, raw := range caps[name] {
				var v any
				if err := decodeNumbers(raw, &v); err != nil {
					continue
				}
				have = mergeCap(have, v, have == nil)
			}
			if len(caps[name]) > 0 {
				dsts = append(dsts, dst)
			}
		}
		sort.Strings(dsts)
		path := "caps[" + strconv.Quote(name) + "]"
		var bad string
		if have == nil {
			bad = path
		} else if vs, ok := req[name].([]any); ok {
			for i, v := range vs {
				if bad = capSubset(v, have, path+"["+strconv.Itoa(i)+"]"); bad != "" {
					break
				}
			}
		} else {
			bad = capSubset(req[name], have, path)
		}
		if bad != "" {
			e := errf(http.StatusForbidden, "delegation_exceeds_parent",
				"spawn asks for %s, which the parent's tags %v are not granted by policy", bad, parentTags)
			e.Missing = map[string]any{"src": parentTags, "dst": dsts, "cap": name, "field": bad}
			return e
		}
	}
	return nil
}

func decodeNumbers(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}

// alts is a set of alternative values that could not be merged (type
// mismatch across grants); a request is ⊆ alts if it is ⊆ any of them.
type alts []any

// mergeCap merges two granted values the way receivers see them: objects
// merge key-wise, arrays union, numbers take the max, bools OR.
func mergeCap(a, b any, first bool) any {
	if first {
		return b
	}
	switch av := a.(type) {
	case alts:
		return append(av, b)
	case map[string]any:
		if bv, ok := b.(map[string]any); ok {
			out := map[string]any{}
			for k, v := range av {
				out[k] = v
			}
			for k, v := range bv {
				if cur, ok := out[k]; ok {
					out[k] = mergeCap(cur, v, false)
				} else {
					out[k] = v
				}
			}
			return out
		}
	case []any:
		if bv, ok := b.([]any); ok {
			out := append([]any(nil), av...)
			for _, v := range bv {
				if !containsJSON(out, v) {
					out = append(out, v)
				}
			}
			return out
		}
	case json.Number:
		if bv, ok := b.(json.Number); ok {
			x, _ := av.Float64()
			y, _ := bv.Float64()
			if y > x {
				return bv
			}
			return av
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return av || bv
		}
	case string:
		if bv, ok := b.(string); ok && av == bv {
			return av
		}
	}
	return alts{a, b}
}

func containsJSON(list []any, v any) bool {
	for _, x := range list {
		if jsonEqual(x, v) {
			return true
		}
	}
	return false
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// capSubset returns "" if req ⊆ have, else the path of the first offending field.
func capSubset(req, have any, path string) string {
	if h, ok := have.(alts); ok {
		for _, alt := range h {
			if capSubset(req, alt, path) == "" {
				return ""
			}
		}
		return path
	}
	switch rv := req.(type) {
	case nil:
		return ""
	case map[string]any:
		hv, ok := have.(map[string]any)
		if !ok {
			return path
		}
		keys := make([]string, 0, len(rv))
		for k := range rv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub := path + "." + k
			hk, ok := hv[k]
			if !ok {
				return sub
			}
			if bad := capSubset(rv[k], hk, sub); bad != "" {
				return bad
			}
		}
		return ""
	case []any:
		for i, e := range rv {
			sub := path + "[" + strconv.Itoa(i) + "]"
			if hv, ok := have.([]any); ok {
				found := false
				for _, h := range hv {
					if capSubset(e, h, sub) == "" {
						found = true
						break
					}
				}
				if !found {
					return sub
				}
			} else if bad := capSubset(e, have, sub); bad != "" {
				return bad
			}
		}
		return ""
	case json.Number:
		hv, ok := have.(json.Number)
		if !ok {
			return path
		}
		x, _ := rv.Float64()
		y, _ := hv.Float64()
		if x > y {
			return path
		}
		return ""
	case bool:
		if !rv {
			return "" // asking for less
		}
		if hv, ok := have.(bool); ok && hv {
			return ""
		}
		return path
	case string:
		switch hv := have.(type) {
		case string:
			if hv == rv {
				return ""
			}
		case []any:
			for _, h := range hv {
				if s, ok := h.(string); ok && s == rv {
					return ""
				}
			}
		}
		return path
	}
	return path
}

// PolicySource fetches a tenant's policy. etag is the cached ETag (send it as
// If-None-Match); notModified reports a 304.
type PolicySource func(ctx context.Context, t Tenant, etag string) (body []byte, newETag string, notModified bool, err error)

// PolicyCache caches each tenant's parsed policy with its ETag, revalidating
// at most every MaxAge.
type PolicyCache struct {
	Fetch  PolicySource
	MaxAge time.Duration
	Clock  Clock

	mu      sync.Mutex
	entries map[string]*policyEntry
}

type policyEntry struct {
	etag    string
	policy  *Policy
	checked time.Time
}

// Get returns the tenant's policy, revalidating it if stale.
func (c *PolicyCache) Get(ctx context.Context, t Tenant) (*Policy, error) {
	clock := c.Clock
	if clock == nil {
		clock = realClock{}
	}
	maxAge := c.MaxAge
	if maxAge == 0 {
		maxAge = 10 * time.Second
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*policyEntry{}
	}
	e := c.entries[t.ID]
	c.mu.Unlock()
	now := clock.Now()
	if e != nil && now.Sub(e.checked) < maxAge {
		return e.policy, nil
	}
	etag := ""
	if e != nil {
		etag = e.etag
	}
	body, newTag, notMod, err := c.Fetch(ctx, t, etag)
	if err != nil {
		if e != nil {
			return e.policy, nil // fail static on the cached policy
		}
		return nil, err
	}
	if notMod && e != nil {
		c.mu.Lock()
		e.checked = now
		c.mu.Unlock()
		return e.policy, nil
	}
	p, err := ParsePolicy(body)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[t.ID] = &policyEntry{etag: newTag, policy: p, checked: now}
	c.mu.Unlock()
	return p, nil
}

// Invalidate forgets a tenant's cached policy (its credentials changed).
func (c *PolicyCache) Invalidate(tenantID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, tenantID)
}

// HasIPGrant reports whether some grant with a network ("ip") part lets a
// node tagged src reach dst (a tag, or "*").
func (p *Policy) HasIPGrant(src, dst string) bool {
	for _, g := range p.Grants {
		if len(g.IP) == 0 || !srcMatches(g.Src, []string{src}) {
			continue
		}
		if slices.Contains(g.Dst, "*") || slices.Contains(g.Dst, dst) {
			return true
		}
	}
	return false
}
