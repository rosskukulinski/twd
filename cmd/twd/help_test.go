package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestHelp: `twd --help`, `twd help`, `twd help <cmd>` and `twd <cmd> --help`
// (any spelling, any position, also after global flags and for subcommands)
// print usage to stdout and exit 0, never the bad_request error page.
func TestHelp(t *testing.T) {
	runOK := func(args ...string) string {
		t.Helper()
		var so, se bytes.Buffer
		if code := run(args, &so, &se); code != 0 {
			t.Fatalf("twd %s: exit %d, stdout %q stderr %q", strings.Join(args, " "), code, so.String(), se.String())
		}
		if se.Len() != 0 {
			t.Fatalf("twd %s: stderr %q", strings.Join(args, " "), se.String())
		}
		if strings.Contains(so.String(), "help requested") || strings.Contains(so.String(), "bad_request") {
			t.Fatalf("twd %s: help printed as an error:\n%s", strings.Join(args, " "), so.String())
		}
		return so.String()
	}
	for _, args := range [][]string{{}, {"--help"}, {"-h"}, {"help"}} {
		out := runOK(args...)
		for _, want := range []string{"usage: twd <command>", "node-workflow", "needs Node.js", "  init ", "  doctor ", "twd <command> --help"} {
			if !strings.Contains(out, want) {
				t.Errorf("twd %s: global help lacks %q:\n%s", strings.Join(args, " "), want, out)
			}
		}
		if strings.Contains(out, "__supervise") {
			t.Errorf("global help lists the hidden command:\n%s", out)
		}
	}
	for _, c := range commands {
		if c.hidden {
			continue
		}
		for _, args := range [][]string{{c.name, "--help"}, {c.name, "-h"}, {c.name, "-help"}, {"help", c.name}, {"--state-dir", "/nonexistent", c.name, "--help"}, {c.name, "--help", "extra", "--flags"}} {
			out := runOK(args...)
			if !strings.Contains(out, "usage: twd "+c.usage) || !strings.Contains(out, c.help) {
				t.Errorf("twd %s: want the %s usage line, got:\n%s", strings.Join(args, " "), c.name, out)
			}
		}
	}
	// Subcommands and the init template list.
	for _, args := range [][]string{{"host", "setup", "--help"}, {"tenant", "add", "-h"}, {"init", "x", "--template", "agent", "--help"}} {
		runOK(args...)
	}
	init := runOK("init", "--help")
	if !strings.Contains(init, "hello|agent|mcp|ai-agent|node-workflow") || !strings.Contains(init, "Node.js") {
		t.Errorf("init help lacks the five templates / the Node.js note:\n%s", init)
	}
	// After `--` nothing is a help flag; a bare `twd` with only flags is a usage error.
	var so, se bytes.Buffer
	if code := run([]string{"--state-dir", "/nonexistent"}, &so, &se); code != 2 || !strings.Contains(se.String(), "usage: twd <command>") {
		t.Errorf("twd --state-dir X: exit %d, stderr %q", code, se.String())
	}
}
