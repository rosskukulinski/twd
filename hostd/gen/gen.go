// Package gen generates the workerd configuration for one deployment of a
// twd app (process tier): the trusted platform wrapper worker, the tenant
// service, and the sockets that connect them to twd-hostd and netd.
//
// Layout of one generated instance directory (0700):
//
//	config.capnp     workerd config (embeds the files below)
//	platform.js      trusted wrapper (js/platform.js + generated Host_<Class> exports)
//	twd-config.json  routing/config for the wrapper
//	twd-modules.js   the tenant modules as text, for the Dynamic Worker facets
//	__twd_facet.js   facet main module: wraps each facet class (ctx.tailnet)
//	__twd_main.js    tenant service main module: wraps default export + plain DO classes
//	__twd_rt.js      ctx.tailnet runtime (built from @twd/tailnet src/runtime.ts)
//	deny.js          globalOutbound of the platform worker (refuses everything)
//	t/<path>         the tenant bundle's modules
//	ingress.sock     workerd listens (cfBlobHeader x-twd-cf); only hostd connects
//	egress-obj.sock  hostd listens: facet egress (platform Egress, MAC'd)
//	egress-wl.sock   hostd listens: tenant service egress (workload identity)
//	hostapi.sock     hostd listens: local host API for the platform
package gen

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/rosskukulinski/twd/hostd/bundle"
)

//go:embed js/platform.js
var platformJS string

//go:embed js/twd-rt.js
var runtimeJS string

// RuntimeJS is the embedded ctx.tailnet runtime (for freshness tests).
func RuntimeJS() string { return runtimeJS }

// PlatformCompatDate is the platform worker's compatibility date (needs
// ctx.exports, facets and the worker loader).
const PlatformCompatDate = "2026-04-01"

const denyJS = `export default { fetch() { return new Response(JSON.stringify({code:"forbidden_destination",message:"the platform has no network egress"}), {status: 403, headers: {"content-type":"application/json","twd-error":"forbidden_destination"}}); } };
`

// Sockets are the instance's socket paths.
type Sockets struct {
	Ingress, EgressObj, EgressWL, HostAPI string
}

// Options configure one generated instance.
type Options struct {
	Dir          string // instance directory (created 0700); keep it short (unix socket paths)
	DOStorage    string // DO storage directory, shared by every generation of the workload
	DeploymentID string
	Workload     string
	// Guest, if set, generates for the microVM tier: workerd listens and
	// dials over TCP (the guest's NIC), and DO storage is guest-local.
	Guest *GuestNet
	// Loopback, if set (process tier under the Linux Landlock sandbox),
	// points workerd's three outbound services at hostd's loopback TCP
	// listeners instead of unix sockets, each request carrying Token in
	// LocalTokenHeader (hostd checks and strips it). The sandbox then needs
	// no AF_UNIX socket() at all: its ingress listener is an inherited fd.
	Loopback *LoopbackNet
}

// LoopbackNet are hostd's per-instance loopback listeners (127.0.0.1:port).
type LoopbackNet struct {
	EgressObj, EgressWL, HostAPI string
	Token                        string
}

// LocalTokenHeader carries an instance's loopback token (Options.Loopback).
const LocalTokenHeader = "x-twd-local-token"

// GuestNet are the guest-side addresses of the microVM tier (host:port).
type GuestNet struct {
	Ingress, EgressObj, EgressWL, HostAPI string
	DOStorage                             string // guest path
}

// Result describes a generated instance.
type Result struct {
	Config  string // path to config.capnp
	Sockets Sockets
	MACKey  string // hex; hostd verifies facet egress MACs with it
}

var flagRE = regexp.MustCompile(`^[a-z0-9_]+$`)
var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Generate writes the instance directory for app.
func Generate(app *bundle.App, o Options) (*Result, error) {
	if !bundle.ValidWorkload(o.Workload) {
		return nil, fmt.Errorf("gen: bad workload name %q", o.Workload)
	}
	if !dateRE.MatchString(app.CompatDate) {
		return nil, fmt.Errorf("gen: bad compatibility_date %q", app.CompatDate)
	}
	for _, f := range app.CompatFlags {
		if !flagRE.MatchString(f) {
			return nil, fmt.Errorf("gen: bad compatibility flag %q", f)
		}
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	if o.Guest == nil {
		if err := os.MkdirAll(o.DOStorage, 0o700); err != nil {
			return nil, err
		}
	}
	keyb := make([]byte, 32)
	rand.Read(keyb)
	res := &Result{
		Config: filepath.Join(o.Dir, "config.capnp"),
		MACKey: hex.EncodeToString(keyb),
		Sockets: Sockets{
			Ingress:   filepath.Join(o.Dir, "ingress.sock"),
			EgressObj: filepath.Join(o.Dir, "egress-obj.sock"),
			EgressWL:  filepath.Join(o.Dir, "egress-wl.sock"),
			HostAPI:   filepath.Join(o.Dir, "hostapi.sock"),
		},
	}
	write := func(name string, b []byte) error {
		p := filepath.Join(o.Dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		return os.WriteFile(p, b, 0o600)
	}

	classes := app.ClassNames()
	var facets, plains []string
	for _, c := range classes {
		if app.Classes[c].Facet() {
			facets = append(facets, c)
		} else {
			plains = append(plains, c)
		}
	}
	bindings := map[string]string{}
	for _, b := range app.Bindings {
		bindings[b.Name] = b.Class
	}
	bjs, _ := json.Marshal(bindings)
	mainImport := "./" + app.Main

	// Tenant modules.
	mods := make([]string, 0, len(app.Modules))
	for p := range app.Modules {
		mods = append(mods, p)
	}
	sort.Strings(mods)
	for _, p := range mods {
		if err := write("t/"+p, app.Modules[p]); err != nil {
			return nil, err
		}
	}

	// Facet main module.
	var fs strings.Builder
	fmt.Fprintf(&fs, "import * as T from %s;\nimport { wrapFacetClass } from \"./__twd_rt.js\";\nconst B = %s;\n", jsq(mainImport), bjs)
	// ctx.container (ADR-032) for classes with a wrangler "containers" entry.
	ctrOpt := func(c string) string {
		cc, ok := app.Containers[c]
		if !ok {
			return ""
		}
		imgs := map[string]string{}
		for n, r := range cc.Images {
			if r == "" {
				r = "unbuilt:" + c + "/" + n // twd deploy has not built and pinned it; start() fails clearly
			}
			imgs[n] = r
		}
		b, _ := json.Marshal(map[string]any{"image": cc.Image, "images": imgs})
		return string(b)
	}
	for _, c := range facets {
		if o := ctrOpt(c); o != "" {
			fmt.Fprintf(&fs, "export const %s = wrapFacetClass(T[%s], %s, B, { container: %s });\n", c, jsq(c), jsq(c), o)
			continue
		}
		fmt.Fprintf(&fs, "export const %s = wrapFacetClass(T[%s], %s, B);\n", c, jsq(c), jsq(c))
	}
	// Tenant service main module.
	var ms strings.Builder
	fmt.Fprintf(&ms, "import * as T from %s;\nimport { wrapHandler, wrapPlainClass } from \"./__twd_rt.js\";\nconst B = %s;\nexport * from %s;\nexport default wrapHandler(T.default, B);\n", jsq(mainImport), bjs, jsq(mainImport))
	for _, c := range plains {
		if o := ctrOpt(c); o != "" {
			fmt.Fprintf(&ms, "export const %s = wrapPlainClass(T[%s], %s, B, { node: %v, container: %s });\n", c, jsq(c), jsq(c), app.Classes[c].Node, o)
			continue
		}
		fmt.Fprintf(&ms, "export const %s = wrapPlainClass(T[%s], %s, B, { node: %v });\n", c, jsq(c), jsq(c), app.Classes[c].Node)
	}

	// twd-modules.js: modules for the facet Dynamic Worker.
	var tm strings.Builder
	type entry struct{ name, ident, kind string }
	var ents []entry
	for i, p := range mods {
		k := moduleKind(p)
		ents = append(ents, entry{p, fmt.Sprintf("m%d", i), k})
		fmt.Fprintf(&tm, "import m%d from %s;\n", i, jsq("t/"+p))
	}
	tm.WriteString("import facetMain from \"__twd_facet.js.txt\";\nimport rt from \"__twd_rt.js.txt\";\nexport default {\n")
	for _, e := range ents {
		switch e.kind {
		case "js":
			fmt.Fprintf(&tm, "  %s: { js: %s },\n", jsq(e.name), e.ident)
		case "json":
			fmt.Fprintf(&tm, "  %s: { json: JSON.parse(%s) },\n", jsq(e.name), e.ident)
		case "text":
			fmt.Fprintf(&tm, "  %s: { text: %s },\n", jsq(e.name), e.ident)
		case "wasm":
			fmt.Fprintf(&tm, "  %s: { wasm: %s },\n", jsq(e.name), e.ident)
		default:
			fmt.Fprintf(&tm, "  %s: { data: %s },\n", jsq(e.name), e.ident)
		}
	}
	tm.WriteString("  \"__twd_facet.js\": { js: facetMain },\n  \"__twd_rt.js\": { js: rt },\n};\n")

	// twd-config.json
	type classCfg struct {
		Mode      string `json:"mode"`
		Node      bool   `json:"node"`
		Container bool   `json:"container,omitempty"`
	}
	type bindCfg struct {
		Name  string `json:"name"`
		Class string `json:"class"`
	}
	cfg := struct {
		DeploymentID string                     `json:"deploymentID"`
		Workload     string                     `json:"workload"`
		CompatDate   string                     `json:"compatDate"`
		CompatFlags  []string                   `json:"compatFlags"`
		Bindings     []bindCfg                  `json:"bindings"`
		Classes      map[string]classCfg        `json:"classes"`
		Vars         map[string]json.RawMessage `json:"vars"`
	}{o.DeploymentID, o.Workload, app.CompatDate, nonNil(app.CompatFlags), []bindCfg{}, map[string]classCfg{}, app.Vars}
	if cfg.Vars == nil {
		cfg.Vars = map[string]json.RawMessage{}
	}
	for _, b := range app.Bindings {
		cfg.Bindings = append(cfg.Bindings, bindCfg{b.Name, b.Class})
	}
	for _, c := range classes {
		m := "plain"
		if app.Classes[c].Facet() {
			m = "facet"
		}
		_, ctr := app.Containers[c]
		cfg.Classes[c] = classCfg{m, app.Classes[c].Node, ctr}
	}
	cfgb, _ := json.MarshalIndent(cfg, "", "  ")

	// platform.js with the Host_<Class> exports.
	var hc strings.Builder
	for _, c := range facets {
		fmt.Fprintf(&hc, "export const Host_%s = makeHost(%s);\n", c, jsq(c))
	}
	pjs := strings.Replace(platformJS, "//@@HOST_CLASSES@@", hc.String(), 1)

	for name, b := range map[string]string{
		"platform.js": pjs, "twd-config.json": string(cfgb), "twd-modules.js": tm.String(),
		"__twd_facet.js": fs.String(), "__twd_main.js": ms.String(), "__twd_rt.js": runtimeJS, "deny.js": denyJS,
	} {
		if err := write(name, []byte(b)); err != nil {
			return nil, err
		}
	}

	// config.capnp
	var c strings.Builder
	c.WriteString("# Generated by twd-hostd for deployment " + o.DeploymentID + ". Do not edit.\n")
	c.WriteString("using Workerd = import \"/workerd/workerd.capnp\";\n\nconst config :Workerd.Config = (\n  services = [\n")
	c.WriteString("    (name = \"platform\", worker = .platform),\n    (name = \"tenant\", worker = .tenant),\n    (name = \"deny\", worker = .deny),\n")
	addrIngress, addrObj, addrWL, addrAPI, doPath := "unix:"+res.Sockets.Ingress, "unix:"+res.Sockets.EgressObj, "unix:"+res.Sockets.EgressWL, "unix:"+res.Sockets.HostAPI, o.DOStorage
	if g := o.Guest; g != nil {
		addrIngress, addrObj, addrWL, addrAPI, doPath = g.Ingress, g.EgressObj, g.EgressWL, g.HostAPI, g.DOStorage
	}
	inject := ""
	if l := o.Loopback; l != nil && o.Guest == nil {
		addrObj, addrWL, addrAPI = l.EgressObj, l.EgressWL, l.HostAPI
		inject = fmt.Sprintf("injectRequestHeaders = [ (name = %s, value = %s) ]", cq(LocalTokenHeader), cq(l.Token))
	}
	sep := func(a string) string {
		if a != "" && inject != "" {
			return a + ", " + inject
		}
		return a + inject
	}
	fmt.Fprintf(&c, "    (name = \"netd-egress\", external = (address = %s, http = (%s))),\n", cq(addrObj), sep("style = proxy"))
	fmt.Fprintf(&c, "    (name = \"egress-wl\", external = (address = %s, http = (%s))),\n", cq(addrWL), sep("style = proxy"))
	fmt.Fprintf(&c, "    (name = \"hostd\", external = (address = %s, http = (%s))),\n", cq(addrAPI), sep(""))
	fmt.Fprintf(&c, "    (name = \"do-storage\", disk = (path = %s, writable = true)),\n  ],\n", cq(doPath))
	fmt.Fprintf(&c, "  sockets = [ (name = \"ingress\", address = %s, service = \"platform\", http = (cfBlobHeader = \"x-twd-cf\")) ],\n);\n\n", cq(addrIngress))

	uk := func(cls string) string { return cq("twd-" + o.Workload + "-" + cls) }

	// platform worker
	fmt.Fprintf(&c, "const platform :Workerd.Worker = (\n  compatibilityDate = %s,\n  modules = [\n", cq(PlatformCompatDate))
	c.WriteString("    (name = \"platform.js\", esModule = embed \"platform.js\"),\n")
	c.WriteString("    (name = \"twd-config.json\", json = embed \"twd-config.json\"),\n")
	c.WriteString("    (name = \"twd-modules.js\", esModule = embed \"twd-modules.js\"),\n")
	c.WriteString("    (name = \"__twd_facet.js.txt\", text = embed \"__twd_facet.js\"),\n")
	c.WriteString("    (name = \"__twd_rt.js.txt\", text = embed \"__twd_rt.js\"),\n")
	for _, p := range mods {
		kind := "text"
		if k := moduleKind(p); k == "wasm" || k == "data" {
			kind = "data"
		}
		fmt.Fprintf(&c, "    (name = %s, %s = embed %s),\n", cq("t/"+p), kind, cq("t/"+p))
	}
	c.WriteString("  ],\n  durableObjectNamespaces = [\n")
	for _, cl := range facets {
		fmt.Fprintf(&c, "    (className = %s, uniqueKey = %s, enableSql = true),\n", cq("Host_"+cl), uk(cl))
	}
	c.WriteString("  ],\n  durableObjectStorage = (localDisk = \"do-storage\"),\n  bindings = [\n")
	c.WriteString("    (name = \"LOADER\", workerLoader = ()),\n    (name = \"NETD_EGRESS\", service = \"netd-egress\"),\n    (name = \"HOSTD\", service = \"hostd\"),\n    (name = \"EGRESS_WL\", service = \"egress-wl\"),\n    (name = \"TENANT\", service = \"tenant\"),\n")
	fmt.Fprintf(&c, "    (name = \"MAC_KEY\", text = %s),\n", cq(res.MACKey))
	for _, cl := range plains {
		fmt.Fprintf(&c, "    (name = %s, durableObjectNamespace = (className = %s, serviceName = \"tenant\")),\n", cq("NS_"+cl), cq(cl))
	}
	c.WriteString("  ],\n  globalOutbound = \"deny\",\n);\n\n")

	// tenant service
	fmt.Fprintf(&c, "const tenant :Workerd.Worker = (\n  compatibilityDate = %s,\n  compatibilityFlags = [", cq(app.CompatDate))
	for i, f := range app.CompatFlags {
		if i > 0 {
			c.WriteString(", ")
		}
		c.WriteString(cq(f))
	}
	c.WriteString("],\n  modules = [\n    (name = \"__twd_main.js\", esModule = embed \"__twd_main.js\"),\n    (name = \"__twd_rt.js\", esModule = embed \"__twd_rt.js\"),\n")
	for _, p := range mods {
		var kind string
		switch moduleKind(p) {
		case "js":
			kind = "esModule"
		case "json":
			kind = "json"
		case "text":
			kind = "text"
		case "wasm":
			kind = "wasm"
		default:
			kind = "data"
		}
		fmt.Fprintf(&c, "    (name = %s, %s = embed %s),\n", cq(p), kind, cq("t/"+p))
	}
	c.WriteString("  ],\n  durableObjectNamespaces = [\n")
	for _, cl := range plains {
		fmt.Fprintf(&c, "    (className = %s, uniqueKey = %s, enableSql = true),\n", cq(cl), uk(cl))
	}
	c.WriteString("  ],\n  durableObjectStorage = (localDisk = \"do-storage\"),\n  bindings = [\n")
	for _, b := range app.Bindings {
		if app.Classes[b.Class].Facet() {
			fmt.Fprintf(&c, "    (name = %s, durableObjectNamespace = (className = %s, serviceName = \"platform\")),\n", cq(b.Name), cq("Host_"+b.Class))
		} else {
			fmt.Fprintf(&c, "    (name = %s, durableObjectNamespace = %s),\n", cq(b.Name), cq(b.Class))
		}
	}
	c.WriteString("    (name = \"__TWD\", service = (name = \"platform\", entrypoint = \"TailnetApi\", props = (json = \"{\\\"kind\\\":\\\"workload\\\"}\"))),\n")
	vars := make([]string, 0, len(app.Vars))
	for k := range app.Vars {
		vars = append(vars, k)
	}
	sort.Strings(vars)
	for _, k := range vars {
		if !regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`).MatchString(k) || strings.HasPrefix(k, "__TWD") {
			return nil, fmt.Errorf("gen: bad var name %q", k)
		}
		fmt.Fprintf(&c, "    (name = %s, json = %s),\n", cq(k), cq(string(app.Vars[k])))
	}
	c.WriteString("  ],\n  globalOutbound = (name = \"platform\", entrypoint = \"WorkloadEgress\"),\n);\n\n")
	c.WriteString("const deny :Workerd.Worker = (\n  compatibilityDate = \"2026-04-01\",\n  modules = [ (name = \"deny.js\", esModule = embed \"deny.js\") ],\n);\n")
	if err := write("config.capnp", []byte(c.String())); err != nil {
		return nil, err
	}
	return res, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func moduleKind(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".js", ".mjs":
		return "js"
	case ".json":
		return "json"
	case ".txt", ".html":
		return "text"
	case ".wasm":
		return "wasm"
	}
	return "data"
}

// jsq quotes s as a JS string literal.
func jsq(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// cq quotes s as a Cap'n Proto text literal.
func cq(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
