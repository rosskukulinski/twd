// Package caps narrows a receiver's capability map by a node certificate's
// `lim` (RFC §4.5: effective = union(CapMap values) ∩ cert.lim; lim only
// narrows, never grants).
//
// CapMap is what WhoIs returns: {capName: [granted values...]}. lim is the
// SpawnOptions.caps object mgmt validated at spawn ({capName: value} or
// {capName: [alternatives]}). The rules, per cap:
//
//   - a cap absent from lim is dropped entirely (a narrowed child holds only
//     what its spawner named);
//   - each granted value is intersected with each lim alternative; the
//     non-empty results are the child's values for that cap;
//   - objects intersect key-wise: keys only in lim are dropped (lim can't
//     grant), keys only in the grant are kept (lim did not narrow them),
//     keys in both recurse;
//   - arrays intersect as sets (an element survives if it is JSON-equal to a
//     lim element, or is an object whose intersection with some lim element
//     is non-empty);
//   - numbers take the min, bools AND, strings must be equal;
//   - a type mismatch, or an empty intersection, drops the value.
//
// A nil/empty lim means "not narrowed": the CapMap is returned unchanged.
package caps

import (
	"bytes"
	"encoding/json"
)

// Intersect returns CapMap ∩ lim as described above. The result never
// aliases the input.
func Intersect(capMap map[string][]json.RawMessage, lim json.RawMessage) map[string][]json.RawMessage {
	out := map[string][]json.RawMessage{}
	for k, vs := range capMap {
		out[k] = append([]json.RawMessage(nil), vs...)
	}
	if len(bytes.TrimSpace(lim)) == 0 || string(bytes.TrimSpace(lim)) == "null" {
		return out
	}
	var l map[string]any
	if err := decode(lim, &l); err != nil {
		// An unparseable lim narrows to nothing: fail closed.
		return map[string][]json.RawMessage{}
	}
	narrowed := map[string][]json.RawMessage{}
	for name, granted := range capMap {
		lv, ok := l[name]
		if !ok {
			continue
		}
		alts := []any{lv}
		if arr, isArr := lv.([]any); isArr {
			alts = arr
		}
		var vals []json.RawMessage
		for _, raw := range granted {
			var g any
			if err := decode(raw, &g); err != nil {
				continue
			}
			for _, alt := range alts {
				if r, ok := intersect(g, alt); ok {
					b, _ := json.Marshal(r)
					if !containsRaw(vals, b) {
						vals = append(vals, b)
					}
				}
			}
		}
		if len(vals) > 0 {
			narrowed[name] = vals
		}
	}
	return narrowed
}

func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}

func containsRaw(list []json.RawMessage, b []byte) bool {
	for _, x := range list {
		if bytes.Equal(x, b) {
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

// intersect returns g ∩ l and whether the result is non-empty.
func intersect(g, l any) (any, bool) {
	switch gv := g.(type) {
	case map[string]any:
		lv, ok := l.(map[string]any)
		if !ok {
			return nil, false
		}
		out := map[string]any{}
		for k, v := range gv {
			lk, has := lv[k]
			if !has {
				out[k] = v // lim did not narrow this key
				continue
			}
			r, ok := intersect(v, lk)
			if !ok {
				return nil, false // a named key narrowed to nothing: no value survives
			}
			out[k] = r
		}
		return out, true
	case []any:
		lv, ok := l.([]any)
		if !ok {
			return nil, false
		}
		out := []any{}
		for _, e := range gv {
			for _, le := range lv {
				if jsonEqual(e, le) {
					out = append(out, e)
					break
				}
				if _, isObj := e.(map[string]any); isObj {
					if r, ok := intersect(e, le); ok {
						out = append(out, r)
						break
					}
				}
			}
		}
		return out, true // an empty list is a valid (maximally narrowed) value
	case json.Number:
		lv, ok := l.(json.Number)
		if !ok {
			return nil, false
		}
		x, _ := gv.Float64()
		y, _ := lv.Float64()
		if y < x {
			return lv, true
		}
		return gv, true
	case bool:
		lv, ok := l.(bool)
		if !ok {
			return nil, false
		}
		return gv && lv, true
	case string:
		lv, ok := l.(string)
		if !ok || lv != gv {
			return nil, false
		}
		return gv, true
	case nil:
		return nil, l == nil
	}
	return nil, false
}
