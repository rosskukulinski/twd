// Package esbundle bundles a twd app (wrangler.jsonc "main") into one ESM
// module, in-process via esbuild's Go API. It is the Go twin of
// js/packages/create-twd/lib/build.mjs (same options, same plugin), so a
// release install can `twd deploy` with no Node.js and no esbuild binary:
// workerd conditions, Node builtins mapped to node:* (needs nodejs_compat),
// cloudflare:* and node:* external, cloudflare:email stubbed.
package esbundle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Version is the esbuild version linked in (matches the npm pin).
const Version = "0.28.2"

// nodeBuiltins is Node's require("module").builtinModules (Node 25),
// without the node:-only ones (they are already node:*).
var nodeBuiltins = map[string]bool{}

func init() {
	for _, m := range strings.Fields(`_http_agent _http_client _http_common _http_incoming _http_outgoing _http_server
		_stream_duplex _stream_passthrough _stream_readable _stream_transform _stream_wrap _stream_writable
		_tls_common _tls_wrap assert assert/strict async_hooks buffer child_process cluster console constants
		crypto dgram diagnostics_channel dns dns/promises domain events fs fs/promises http http2 https
		inspector inspector/promises module net os path path/posix path/win32 perf_hooks process punycode
		querystring readline readline/promises repl stream stream/consumers stream/promises stream/web
		string_decoder sys timers timers/promises tls trace_events tty url util util/types v8 vm wasi
		worker_threads zlib`) {
		nodeBuiltins[m] = true
	}
}

// Options for Build.
type Options struct {
	Dir       string   // the app directory (absWorkingDir)
	Main      string   // the entry, relative to Dir (wrangler.jsonc "main")
	Outfile   string   // absolute path of the output module
	NodePaths []string // module fallbacks (twd's own node_modules)
	Minify    bool
}

var bareRE = regexp.MustCompile(`^[a-z_][a-z0-9_/]*$`)

// Build bundles and writes Outfile; it returns the output size.
func Build(o Options) (int, error) {
	if o.Main == "" {
		return 0, errors.New("wrangler.jsonc has no main")
	}
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return 0, err
	}
	twd := api.Plugin{Name: "twd", Setup: func(b api.PluginBuild) {
		b.OnResolve(api.OnResolveOptions{Filter: `^cloudflare:email$`}, func(a api.OnResolveArgs) (api.OnResolveResult, error) {
			return api.OnResolveResult{Path: "email", Namespace: "twd-stub"}, nil
		})
		b.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: "twd-stub"}, func(a api.OnLoadArgs) (api.OnLoadResult, error) {
			s := "export class EmailMessage { constructor() { throw new Error('cloudflare:email is not supported on twd'); } }"
			return api.OnLoadResult{Contents: &s, Loader: api.LoaderJS}, nil
		})
		b.OnResolve(api.OnResolveOptions{Filter: `^[a-z_][a-z0-9_/]*$`}, func(a api.OnResolveArgs) (api.OnResolveResult, error) {
			if !bareRE.MatchString(a.Path) {
				return api.OnResolveResult{}, nil
			}
			base, _, _ := strings.Cut(a.Path, "/")
			if nodeBuiltins[base] || nodeBuiltins[a.Path] {
				return api.OnResolveResult{Path: "node:" + a.Path, External: true}, nil
			}
			return api.OnResolveResult{}, nil
		})
	}}
	r := api.Build(api.BuildOptions{
		EntryPoints:       []string{filepath.Join(dir, o.Main)},
		Outfile:           o.Outfile,
		AbsWorkingDir:     dir,
		Bundle:            true,
		Write:             true,
		Format:            api.FormatESModule,
		Platform:          api.PlatformNeutral,
		Target:            api.ES2022,
		Conditions:        []string{"workerd", "worker", "browser"},
		MainFields:        []string{"module", "main"},
		External:          []string{"cloudflare:*", "node:*"},
		MinifyWhitespace:  o.Minify,
		MinifyIdentifiers: o.Minify,
		MinifySyntax:      o.Minify,
		NodePaths:         o.NodePaths,
		LegalComments:     api.LegalCommentsNone,
		LogLevel:          api.LogLevelSilent,
		Plugins:           []api.Plugin{twd},
	})
	if len(r.Errors) > 0 {
		msgs := api.FormatMessages(r.Errors, api.FormatMessagesOptions{Kind: api.ErrorMessage})
		return 0, fmt.Errorf("esbuild: %d error(s)\n%s", len(r.Errors), strings.TrimSpace(strings.Join(msgs, "")))
	}
	st, err := os.Stat(o.Outfile)
	if err != nil {
		return 0, err
	}
	return int(st.Size()), nil
}
