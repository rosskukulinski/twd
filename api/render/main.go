// Command render turns api/openapi.yaml into docs/contracts/mgmt-api.md.
//
// The spec is the source of truth; the Markdown is generated (`make api`) and
// never hand-edited. The renderer is deliberately small: a route table per
// tag, then one section per operation (role, audit flag, parameters, request
// body, responses), then every component schema as a field table. Anything
// it does not know how to render is omitted, not invented.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

func main() {
	in := flag.String("in", "api/openapi.yaml", "OpenAPI document")
	out := flag.String("out", "docs/contracts/mgmt-api.md", "Markdown output")
	flag.Parse()

	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	doc, err := loader.LoadFromFile(*in)
	if err != nil {
		log.Fatalf("load %s: %v", *in, err)
	}
	var b strings.Builder
	render(&b, doc, *in)
	if err := os.WriteFile(*out, []byte(b.String()), 0o644); err != nil {
		log.Fatal(err)
	}
}

type op struct {
	method, path string
	op           *openapi3.Operation
}

func render(b *strings.Builder, doc *openapi3.T, src string) {
	fmt.Fprintf(b, "<!-- GENERATED from %s by `make api` (api/render). Do not edit; edit the spec. -->\n\n", src)
	fmt.Fprintf(b, "# %s (%s)\n\n", doc.Info.Title, doc.Info.Version)
	if doc.Info.Summary != "" {
		fmt.Fprintf(b, "_%s_\n\n", doc.Info.Summary)
	}
	b.WriteString(strings.TrimSpace(doc.Info.Description))
	b.WriteString("\n\n")

	// Collect operations in path order, grouped by first tag.
	var ops []op
	for _, path := range sortedPaths(doc) {
		item := doc.Paths.Value(path)
		for _, m := range []string{"GET", "PUT", "POST", "PATCH", "DELETE"} {
			if o := item.GetOperation(m); o != nil {
				ops = append(ops, op{m, path, o})
			}
		}
	}

	b.WriteString("## Route table\n\n")
	b.WriteString("| Method | Path | Role | Audited | Operation | Summary |\n|---|---|---|---|---|---|\n")
	for _, o := range ops {
		fmt.Fprintf(b, "| `%s` | `%s` | %s | %s | `%s` | %s |\n", o.method, o.path, oneLine(role(o.op)), audited(o.op), o.op.OperationID, oneLine(o.op.Summary))
	}
	b.WriteString("\n")

	for _, t := range doc.Tags {
		var mine []op
		for _, o := range ops {
			if len(o.op.Tags) > 0 && o.op.Tags[0] == t.Name {
				mine = append(mine, o)
			}
		}
		if len(mine) == 0 {
			continue
		}
		fmt.Fprintf(b, "## %s\n\n", strings.ToUpper(t.Name[:1])+t.Name[1:])
		if t.Description != "" {
			b.WriteString(strings.TrimSpace(t.Description) + "\n\n")
		}
		for _, o := range mine {
			renderOp(b, doc, o)
		}
	}

	b.WriteString("## Components\n\n")
	b.WriteString("### Security schemes\n\n")
	for _, name := range sortedKeys(doc.Components.SecuritySchemes) {
		s := doc.Components.SecuritySchemes[name].Value
		fmt.Fprintf(b, "#### `%s` (%s)\n\n%s\n\n", name, s.Type, strings.TrimSpace(s.Description))
	}
	b.WriteString("### Schemas\n\n")
	for _, name := range sortedKeys(doc.Components.Schemas) {
		renderSchema(b, name, doc.Components.Schemas[name])
	}
}

func renderOp(b *strings.Builder, doc *openapi3.T, o op) {
	fmt.Fprintf(b, "### `%s %s` — %s\n\n", o.method, o.path, o.op.Summary)
	fmt.Fprintf(b, "`%s` · role **%s** · audited: %s\n\n", o.op.OperationID, oneLine(role(o.op)), audited(o.op))
	if o.op.Description != "" {
		b.WriteString(strings.TrimSpace(o.op.Description) + "\n\n")
	}
	// Parameters: path-level then operation-level.
	var params openapi3.Parameters
	params = append(params, doc.Paths.Value(o.path).Parameters...)
	params = append(params, o.op.Parameters...)
	if len(params) > 0 {
		b.WriteString("| Parameter | In | Type | Required | Description |\n|---|---|---|---|---|\n")
		for _, pr := range params {
			p := pr.Value
			fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s |\n", p.Name, p.In, typeOf(p.Schema), yesNo(p.Required), oneLine(p.Description))
		}
		b.WriteString("\n")
	}
	if rb := o.op.RequestBody; rb != nil && rb.Value != nil {
		mts := sortedKeys(rb.Value.Content)
		if len(mts) > 0 {
			// PATCH bodies list application/json and application/merge-patch+json
			// with one schema; name every media type, show the schema once.
			var names []string
			for _, mt := range mts {
				names = append(names, "`"+mt+"`")
			}
			fmt.Fprintf(b, "**Request** %s: %s\n\n", strings.Join(names, " or "), typeOf(rb.Value.Content[mts[0]].Schema))
		}
	}
	b.WriteString("| Status | Body | Description |\n|---|---|---|\n")
	for _, code := range sortedKeys(o.op.Responses.Map()) {
		r := o.op.Responses.Map()[code]
		desc, body := "", "—"
		if r.Value != nil {
			if r.Value.Description != nil {
				desc = *r.Value.Description
			}
			for _, mt := range sortedKeys(r.Value.Content) {
				body = typeOf(r.Value.Content[mt].Schema)
				break
			}
		}
		if r.Ref != "" && body == "—" {
			body = "`Error`"
		}
		fmt.Fprintf(b, "| %s | %s | %s |\n", code, body, oneLine(desc))
	}
	b.WriteString("\n")
}

func renderSchema(b *strings.Builder, name string, ref *openapi3.SchemaRef) {
	s := ref.Value
	fmt.Fprintf(b, "#### `%s`\n\n", name)
	if s.Description != "" {
		b.WriteString(strings.TrimSpace(s.Description) + "\n\n")
	}
	if len(s.Enum) > 0 {
		var vals []string
		for _, e := range s.Enum {
			vals = append(vals, fmt.Sprintf("`%v`", e))
		}
		fmt.Fprintf(b, "Enum: %s\n\n", strings.Join(vals, ", "))
		return
	}
	props, required := s.Properties, s.Required
	for _, part := range s.AllOf {
		if part.Ref != "" {
			fmt.Fprintf(b, "Extends `%s`.\n\n", refName(part.Ref))
			continue
		}
		if props == nil {
			props = openapi3.Schemas{}
		}
		for k, v := range part.Value.Properties {
			props[k] = v
		}
		required = append(required, part.Value.Required...)
	}
	if len(props) == 0 {
		if len(s.OneOf) > 0 {
			var alts []string
			for _, a := range s.OneOf {
				alts = append(alts, typeOf(a))
			}
			fmt.Fprintf(b, "One of: %s\n\n", strings.Join(alts, ", "))
		}
		return
	}
	req := map[string]bool{}
	for _, r := range required {
		req[r] = true
	}
	b.WriteString("| Field | Type | Required | Description |\n|---|---|---|---|\n")
	for _, k := range sortedKeys(props) {
		p := props[k]
		desc := ""
		if p.Value != nil {
			desc = p.Value.Description
			if p.Value.ReadOnly {
				desc = "(read-only) " + desc
			}
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s |\n", k, typeOf(p), yesNo(req[k]), oneLine(desc))
	}
	b.WriteString("\n")
}

func typeOf(ref *openapi3.SchemaRef) string {
	if ref == nil {
		return "—"
	}
	if ref.Ref != "" {
		return "`" + refName(ref.Ref) + "`"
	}
	s := ref.Value
	if s == nil {
		return "—"
	}
	switch {
	case len(s.AllOf) > 0:
		var parts []string
		for _, a := range s.AllOf {
			parts = append(parts, typeOf(a))
		}
		return strings.Join(parts, " + ")
	case len(s.OneOf) > 0:
		var parts []string
		for _, a := range s.OneOf {
			parts = append(parts, typeOf(a))
		}
		return strings.Join(parts, " \\| ")
	case s.Type != nil && s.Type.Is("array"):
		return "array of " + typeOf(s.Items)
	}
	t := "any"
	if s.Type != nil && len(*s.Type) > 0 {
		t = strings.Join(*s.Type, " \\| ")
	}
	if s.Format != "" {
		t += " (" + s.Format + ")"
	}
	if len(s.Enum) > 0 {
		var vals []string
		for _, e := range s.Enum {
			vals = append(vals, fmt.Sprintf("`%v`", e))
		}
		t += ": " + strings.Join(vals, " \\| ")
	}
	return t
}

func role(o *openapi3.Operation) string {
	if v, ok := o.Extensions["x-twd-role"]; ok {
		return fmt.Sprint(v)
	}
	if o.Security != nil && len(*o.Security) == 0 {
		return "public"
	}
	return "—"
}

func audited(o *openapi3.Operation) string {
	if v, ok := o.Extensions["x-twd-audit"]; ok && v == true {
		return "yes"
	}
	return "no"
}

func refName(ref string) string { return ref[strings.LastIndex(ref, "/")+1:] }
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.Join(strings.Fields(s), " ")
}

func sortedPaths(doc *openapi3.T) []string {
	var ps []string
	for p := range doc.Paths.Map() {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	// /v1 first, then the OIDC public paths.
	sort.SliceStable(ps, func(i, j int) bool {
		vi, vj := strings.HasPrefix(ps[i], "/v1/"), strings.HasPrefix(ps[j], "/v1/")
		return vi && !vj
	})
	return ps
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
