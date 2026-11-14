// twd is the twd CLI (M4): an all-in-one homelab control plane on this
// machine (`twd up` runs twd-mgmt + twd-hostd under a supervisor in
// ~/.twd), app scaffolding and deploys, and the day-2 commands (kill,
// audit, ls, logs, status).
//
//	twd up --oauth-client-file <file> [--service] --yes      # recommended (M12): OAuth client, no browser, no Funnel
//	twd up --wif                                             # keyless: tailnet login + WIF trust credential (ADR-026)
//	twd up --tailnet <tailnet> --api-key-file <file> --yes   # fallback: apikey mint mode (tests)
//	twd link twd-mgmt.<tailnet>.ts.net                       # a developer machine: every command below goes to mgmt over the tailnet (WhoIs)
//	twd init hello --template agent && cd hello && twd deploy
//	twd ls · twd kill <object> --cascade · twd audit <object> · twd logs <workload>
//	twd down
//
// twd never uses this machine's tailscaled or its identity: every node is
// a tsnet node joined with a key minted for the tailnet you name.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/netd"
)

type command struct {
	name, usage, help string
	run               func(args []string) error
	hidden            bool
}

var commands []command

func init() {
	commands = []command{
		{name: "up", usage: "up --oauth-client-file <file> [--service] [--yes]  |  up --wif [--wif-client-id <id>]  |  up --tailnet <tailnet> --api-key-file <file> [--policy-apply]", help: "start twd-mgmt + twd-hostd on this machine; recommended: an OAuth client (no browser, no Funnel); --wif for the keyless browser flow, --api-key-file as the fallback", run: cmdUp},
		{name: "down", usage: "down [--purge] [--service]", help: "stop everything twd up started (--purge: kill every object and delete its device first)", run: cmdDown},
		{name: "link", usage: "link [twd-mgmt.<tailnet>.ts.net] [--unlink]", help: "developer machine: use a remote twd-mgmt over your own Tailscale connection (WhoIs identity; no key or token)", run: cmdLink},
		{name: "status", usage: "status", help: "health of mgmt, the host, the tenant and deployments", run: cmdStatus},
		{name: "tenant", usage: "tenant add <tailnet> --api-key-file <file> [--policy-apply] | tenant ls | tenant set <tenant> [--max-objects N] [--registrations-per-minute N] [--pool P] [--tier T] [--wif-client-id ID] [--max-containers N] [--max-container-mem-mib N] [--max-container-vcpu F] | tenant rm <tenant> [--force]", help: "serve another tenant (its own tailnet) from this host; list, update (PATCH) or remove tenants", run: cmdTenant},
		{name: "init", usage: "init <dir> [--template hello|agent|mcp|ai-agent|node-workflow] [--name <workload>]", help: "create an app from a template (node-workflow needs Node.js on the deploying machine)", run: cmdInit},
		{name: "dev", usage: "dev [dir] [--port 8787]", help: "run an app locally: process tier + embedded control plane (no tailnet)", run: cmdDev},
		{name: "deploy", usage: "deploy [dir] [--tenant <tailnet>] [--name <workload>] [--allow-missing-grants] [--no-wait] [--platform linux/amd64,linux/arm64]", help: "bundle, preflight grants, build + push Dockerfile container images (docker buildx), deploy, wait until ready", run: cmdDeploy},
		{name: "ls", usage: "ls [--tenant <tailnet>] [--all] [--limit N]", help: "list deployments and objects (paged; --all follows every page and shows killed objects)", run: cmdLs},
		{name: "undeploy", usage: "undeploy <workload> [--tenant <tailnet>] [--reason <text>]", help: "stop a workload: kill its objects (cascade), release its lease", run: cmdUndeploy},
		{name: "kill", usage: "kill <object> [--tenant <tailnet>] [--cascade] [--reason <text>]", help: "kill an object (and with --cascade its sub-agents)", run: cmdKill},
		{name: "audit", usage: "audit [--tenant <tailnet>] <object> | --human <login> | --trace <id> | --all", help: "show the hash-chained audit trail", run: cmdAudit},
		{name: "logs", usage: "logs [workload] [--tenant <tailnet>] [--component mgmt|hostd|supervisor] [-n 50] [-f]", help: "show logs (workerd for a workload, else the daemons)", run: cmdLogs},
		{name: "doctor", usage: "doctor [--tailnet <tailnet> --api-key-file <file>]", help: "check this machine: workerd runs, bundler offline, ~/.twd writable, API key works, process sandbox active, system tailscaled not used; on Linux also glibc, kernel/Landlock ABI, KVM, firecracker, cgroups v2, systemd and the microVM tier", run: cmdDoctor},
		{name: "host", usage: "host add <host-id> [--tenant <tailnet>] [--out <file>] | host ls | host rm <host-id> [--drain] | host setup --microvm [--user <name>] [--hugepages N] | host setup --undo   (Linux, with sudo, once) | host vmcheck <workload> [--keyscan]", help: "add a remote host (mgmt mints its single-use tag:twd-host join key), list or remove hosts; install the microVM tier's helper; check a microVM", run: cmdHost},
		{name: "image", usage: "image public add <ref> [--arch amd64,arm64] | image public ls | image public rm <id> | image push <oci-layout|docker-save.tar> twd.local/<name>:<tag>", help: "container images (ADR-032): mark public base images shared across tenants (mirrored now), push operator images (the arm64 alias targets) to twd's registry", run: cmdImage},
		{name: "version", usage: "version", help: "print the twd version (and the pinned workerd / built-in esbuild)", run: cmdVersion},
		{name: "__supervise", hidden: true, run: cmdSupervise},
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "twd: Workers and Durable Objects with their own tailnet identities")
	fmt.Fprintln(w, "\nusage: twd <command> [flags]   (every command takes --state-dir, default ~/.twd, and --json)")
	fmt.Fprintln(w, "       twd <command> --help     usage of one command")
	fmt.Fprintln(w)
	for _, c := range commands {
		if !c.hidden {
			fmt.Fprintf(w, "  %-8s %s\n           twd %s\n", c.name, c.help, c.usage)
		}
	}
	fmt.Fprintln(w, "\ntemplates (twd init --template): hello | agent | mcp | ai-agent | node-workflow (node-workflow needs Node.js on the deploying machine)")
}

// commandUsage prints one command's help (`twd <cmd> --help`).
func commandUsage(w io.Writer, c command) {
	fmt.Fprintf(w, "twd %s: %s\n\nusage: twd %s\n", c.name, c.help, c.usage)
	fmt.Fprintln(w, "  (every command takes --state-dir, default ~/.twd, and --json)")
}

// errHelp is returned by flags.parse when -h/--help was given; main prints
// the command's usage to stdout and exits 0.
var errHelp = errors.New("help requested")

// isHelpFlag reports whether a is -h, -help or --help.
func isHelpFlag(a string) bool { return a == "-h" || a == "-help" || a == "--help" }

// wantsHelp reports whether args ask for help before a `--`.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if isHelpFlag(a) {
			return true
		}
	}
	return false
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the process exit (tests call it): the exit code is
// 0 on success and for any help request, 1 for a failed command, 2 for a
// usage error.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		args = []string{"version"}
	}
	if len(args) == 0 || isHelpFlag(args[0]) || args[0] == "help" {
		// `twd help <cmd>` is `twd <cmd> --help`.
		if len(args) == 2 && args[0] == "help" {
			for _, c := range commands {
				if c.name == args[1] && !c.hidden {
					commandUsage(stdout, c)
					return 0
				}
			}
		}
		usage(stdout)
		return 0
	}
	// Allow global flags before the command: twd --state-dir X <cmd> …
	var pre []string
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		pre = append(pre, args[0])
		if !strings.Contains(args[0], "=") && len(args) > 1 && (args[0] == "--state-dir" || args[0] == "-state-dir") {
			pre = append(pre, args[1])
			args = args[1:]
		}
		args = args[1:]
	}
	if len(args) == 0 {
		if wantsHelp(pre) {
			usage(stdout)
			return 0
		}
		usage(stderr)
		return 2
	}
	name, rest := args[0], append(pre, args[1:]...)
	for _, c := range commands {
		if c.name == name {
			if wantsHelp(rest) && !c.hidden {
				commandUsage(stdout, c)
				return 0
			}
			if err := c.run(rest); err != nil {
				if errors.Is(err, errHelp) {
					commandUsage(stdout, c)
					return 0
				}
				printErr(err)
				return 1
			}
			return 0
		}
	}
	printErr(fail("bad_request", fmt.Sprintf("unknown command %q", name), "run `twd help` for the list"))
	return 2
}

// ---- errors (DX.md catalog format) ----

// cliErr is a typed error printed in the DX.md catalog shape:
//
//	error [code]: message
//	  detail…
//	  fix: …
//	  https://twd.dev/e/<code>
type cliErr struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Lines   []string `json:"detail,omitempty"`
	Fix     []string `json:"fix,omitempty"`
	TraceID string   `json:"trace_id,omitempty"`
}

func (e *cliErr) Error() string { return e.Code + ": " + e.Message }

func fail(code, msg string, lines ...string) *cliErr {
	return &cliErr{Code: code, Message: msg, Lines: lines}
}

func (e *cliErr) withFix(fix ...string) *cliErr { e.Fix = append(e.Fix, fix...); return e }

// jsonOut is set by --json: errors are printed as JSON too.
var jsonOut bool

func asCLIErr(err error) *cliErr {
	var ce *cliErr
	if errors.As(err, &ce) {
		return ce
	}
	if me, ok := mgmtapi.AsError(err); ok {
		e := fail(me.Code, me.Message)
		if me.Snippet != "" {
			e.Fix = append(e.Fix, "add to your tailnet policy:", "  "+me.Snippet)
		}
		if len(me.Missing) > 0 {
			b, _ := json.Marshal(me.Missing)
			e.Lines = append(e.Lines, "missing: "+string(b))
		}
		e.TraceID = me.TraceID
		return e
	}
	return fail("internal", netd.Redact(err.Error()))
}

func printErr(err error) {
	e := asCLIErr(err)
	if jsonOut {
		b, _ := json.Marshal(map[string]any{"error": e})
		fmt.Fprintln(os.Stderr, netd.Redact(string(b)))
		return
	}
	fmt.Fprintf(os.Stderr, "error [%s]: %s\n", e.Code, netd.Redact(e.Message))
	for _, l := range e.Lines {
		fmt.Fprintf(os.Stderr, "  %s\n", netd.Redact(l))
	}
	for i, l := range e.Fix {
		if i == 0 {
			fmt.Fprintf(os.Stderr, "  fix: %s\n", l)
		} else {
			fmt.Fprintf(os.Stderr, "    %s\n", l)
		}
	}
	tr := ""
	if e.TraceID != "" {
		tr = "trace " + e.TraceID + " · "
	}
	fmt.Fprintf(os.Stderr, "  %shttps://twd.dev/e/%s\n", tr, e.Code)
}

// ---- flags ----

type flags struct {
	*flag.FlagSet
	stateDir *string
	json     *bool
}

func newFlags(name string) *flags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := &flags{FlagSet: fs}
	f.stateDir = fs.String("state-dir", "", "state directory (default $TWD_STATE_DIR or ~/.twd)")
	f.json = fs.Bool("json", false, "machine-readable output")
	return f
}

// parse parses flags interleaved with positional args (twd kill X --cascade).
func (f *flags) parse(args []string) ([]string, error) {
	var pos []string
	for {
		if err := f.FlagSet.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, errHelp
			}
			return nil, fail("bad_request", err.Error(), "usage: twd "+usageOf(f.Name()))
		}
		args = f.FlagSet.Args()
		if len(args) == 0 {
			break
		}
		if args[0] == "--" {
			pos = append(pos, args[1:]...)
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	jsonOut = *f.json
	return pos, nil
}

func usageOf(name string) string {
	for _, c := range commands {
		if c.name == name {
			return c.usage
		}
	}
	return name
}

// emit prints v as JSON (with --json); otherwise human() runs.
func emit(f *flags, v any, human func()) {
	if *f.json {
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(netd.Redact(string(b)))
		return
	}
	human()
}

func sortedKeys[M ~map[string]V, V any](m M) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
