package caps

import (
	"encoding/json"
	"testing"
)

func raws(ss ...string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(ss))
	for _, s := range ss {
		out = append(out, json.RawMessage(s))
	}
	return out
}

func TestIntersect(t *testing.T) {
	mcp := "example.com/cap/mcp"
	cases := []struct {
		name string
		cm   map[string][]json.RawMessage
		lim  string
		want map[string][]string
	}{
		{"nil lim is unchanged", map[string][]json.RawMessage{mcp: raws(`{"tools":["a","b"]}`)}, "", map[string][]string{mcp: {`{"tools":["a","b"]}`}}},
		{"tools narrowed", map[string][]json.RawMessage{mcp: raws(`{"tools":["lookup_order","refund"]}`)}, `{"` + mcp + `":{"tools":["lookup_order"]}}`,
			map[string][]string{mcp: {`{"tools":["lookup_order"]}`}}},
		{"lim cannot grant", map[string][]json.RawMessage{mcp: raws(`{"tools":["lookup_order"]}`)}, `{"` + mcp + `":{"tools":["lookup_order","refund"]}}`,
			map[string][]string{mcp: {`{"tools":["lookup_order"]}`}}},
		{"cap absent from lim is dropped", map[string][]json.RawMessage{mcp: raws(`{"tools":["a"]}`), "x/cap": raws(`{"ok":true}`)}, `{"` + mcp + `":{"tools":["a"]}}`,
			map[string][]string{mcp: {`{"tools":["a"]}`}}},
		{"unnarrowed keys kept, numbers min, bools and", map[string][]json.RawMessage{mcp: raws(`{"tools":["refund"],"limits":{"refund":{"maxUsd":50}},"admin":true}`)},
			`{"` + mcp + `":{"limits":{"refund":{"maxUsd":10}},"admin":false}}`,
			map[string][]string{mcp: {`{"admin":false,"limits":{"refund":{"maxUsd":10}},"tools":["refund"]}`}}},
		{"empty tools list narrows to nothing usable", map[string][]json.RawMessage{mcp: raws(`{"tools":["a","b"]}`)}, `{"` + mcp + `":{"tools":[]}}`,
			map[string][]string{mcp: {`{"tools":[]}`}}},
		{"alternatives", map[string][]json.RawMessage{mcp: raws(`{"tools":["a","b","c"]}`)}, `{"` + mcp + `":[{"tools":["a"]},{"tools":["c"]}]}`,
			map[string][]string{mcp: {`{"tools":["a"]}`, `{"tools":["c"]}`}}},
		{"type mismatch drops", map[string][]json.RawMessage{mcp: raws(`{"tools":["a"]}`)}, `{"` + mcp + `":"all"}`, map[string][]string{}},
		{"string values", map[string][]json.RawMessage{"r": raws(`"admin"`, `"viewer"`)}, `{"r":"viewer"}`, map[string][]string{"r": {`"viewer"`}}},
		{"garbage lim fails closed", map[string][]json.RawMessage{mcp: raws(`{"tools":["a"]}`)}, `{nope`, map[string][]string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Intersect(c.cm, json.RawMessage(c.lim))
			if len(got) != len(c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
			for k, ws := range c.want {
				gs := got[k]
				if len(gs) != len(ws) {
					t.Fatalf("%s: got %s want %v", k, gs, ws)
				}
				for i := range ws {
					if string(gs[i]) != ws[i] {
						t.Fatalf("%s[%d]: got %s want %s", k, i, gs[i], ws[i])
					}
				}
			}
		})
	}
}
