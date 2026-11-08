// twd is the twd CLI (M4): an all-in-one homelab control plane on this
// machine (`twd up` runs twd-mgmt + twd-hostd under a supervisor in
// ~/.twd), app scaffolding and deploys, and the day-2 commands (kill,
// audit, ls, logs, status).
//
//	twd up --tailnet <tailnet> --api-key-file <file> --yes   # non-interactive (test mode)
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
		{name: "up", usage: "up --tailnet <tailnet> --api-key-file <file> [--yes] [--policy-apply]", help: "start twd-mgmt + twd-hostd on this machine (homelab all-in-one)", run: cmdUp},
		{name: "down", usage: "down [--purge]", help: "stop everything twd up started (--purge: kill every object and delete its device first)", run: cmdDown},
		{name: "status", usage: "status", help: "health of mgmt, the host, the tenant and deployments", run: cmdStatus},
		{name: "init", usage: "init <dir> [--template hello|agent|mcp]", help: "create an app from a template", run: cmdInit},
		{name: "dev", usage: "dev [dir] [--port 8787]", help: "run an app locally: process tier + embedded control plane (no tailnet)", run: cmdDev},
		{name: "deploy", usage: "deploy [dir] [--name <workload>] [--allow-missing-grants] [--no-wait]", help: "bundle, preflight grants, deploy, wait until ready", run: cmdDeploy},
		{name: "ls", usage: "ls [--all]", help: "list deployments and objects", run: cmdLs},
		{name: "kill", usage: "kill <object> [--cascade] [--reason <text>]", help: "kill an object (and with --cascade its sub-agents)", run: cmdKill},
		{name: "audit", usage: "audit <object> | --human <login> | --trace <id>", help: "show the hash-chained audit trail", run: cmdAudit},
		{name: "logs", usage: "logs [workload] [--component mgmt|hostd|supervisor] [-n 50] [-f]", help: "show logs (workerd for a workload, else the daemons)", run: cmdLogs},
		{name: "doctor", usage: "doctor [--tailnet <tailnet> --api-key-file <file>]", help: "check this machine: workerd runs, bundler offline, ~/.twd writable, API key works, system tailscaled not used", run: cmdDoctor},
		{name: "version", usage: "version", help: "print the twd version (and the pinned workerd / built-in esbuild)", run: cmdVersion},
		{name: "__supervise", hidden: true, run: cmdSupervise},
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "twd: Workers and Durable Objects with their own tailnet identities")
	fmt.Fprintln(w, "\nusage: twd <command> [flags]   (every command takes --state-dir, default ~/.twd, and --json)")
	fmt.Fprintln(w)
	for _, c := range commands {
		if !c.hidden {
			fmt.Fprintf(w, "  %-8s %s\n           twd %s\n", c.name, c.help, c.usage)
		}
	}
}

func main() {
	args := os.Args[1:]
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		args = []string{"version"}
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(os.Stdout)
		return
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
		usage(os.Stderr)
		os.Exit(2)
	}
	name, rest := args[0], append(pre, args[1:]...)
	for _, c := range commands {
		if c.name == name {
			if err := c.run(rest); err != nil {
				printErr(err)
				os.Exit(1)
			}
			return
		}
	}
	printErr(fail("bad_request", fmt.Sprintf("unknown command %q", name), "run `twd help` for the list"))
	os.Exit(2)
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
