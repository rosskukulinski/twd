package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUnitFor(t *testing.T) {
	s := &state{Dir: "/home/u/.twd", Cfg: Config{Tailnet: "example.ts.net"}}
	u := unitFor(s, "/home/u/.twd/bin/twd")
	for _, want := range []string{
		"ExecStart=/home/u/.twd/bin/twd __supervise --state-dir /home/u/.twd\n",
		"KillMode=mixed\n", "Restart=on-failure\n", "TimeoutStopSec=180\n", "WantedBy=default.target\n",
		"StandardOutput=append:/home/u/.twd/logs/supervisor.log\n",
		"Environment=PATH=/home/u/.twd/bin:",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
	s.Dir = "/home/u/my twd"
	if u := unitFor(s, "/opt/t w/twd"); !strings.Contains(u, `ExecStart="/opt/t w/twd" __supervise --state-dir "/home/u/my twd"`+"\n") {
		t.Errorf("paths with spaces not quoted:\n%s", u)
	}
}

func TestPlistFor(t *testing.T) {
	s := &state{Dir: "/Users/u/.twd", Cfg: Config{Tailnet: "example.ts.net"}}
	p := plistFor(s, "/Users/u/.twd/bin/twd")
	for _, want := range []string{
		"<key>Label</key><string>dev.twd.up</string>",
		"<string>/Users/u/.twd/bin/twd</string>\n    <string>__supervise</string>\n    <string>--state-dir</string>\n    <string>/Users/u/.twd</string>",
		"<key>RunAtLoad</key><true/>", "<key>SuccessfulExit</key><false/>", "<key>ExitTimeOut</key><integer>180</integer>",
		"<key>StandardOutPath</key><string>/Users/u/.twd/logs/supervisor.log</string>",
		"<key>PATH</key><string>/Users/u/.twd/bin:",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q:\n%s", want, p)
		}
	}
	// XML-special characters in paths are escaped, and the plist parses.
	s.Dir = "/Users/u/my & twd"
	p = plistFor(s, "/opt/t<w>/twd")
	if !strings.Contains(p, "<string>/Users/u/my &amp; twd</string>") || !strings.Contains(p, "<string>/opt/t&lt;w&gt;/twd</string>") {
		t.Errorf("paths not XML-escaped:\n%s", p)
	}
	if runtime.GOOS == "darwin" {
		f := filepath.Join(t.TempDir(), "x.plist")
		os.WriteFile(f, []byte(p), 0o644)
		if out, err := exec.Command("plutil", "-lint", f).CombinedOutput(); err != nil {
			t.Errorf("plutil -lint: %v %s", err, out)
		}
	}
}

func TestUserUnitDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		if got := serviceUnitPath(); got != filepath.Join(home, "Library/LaunchAgents/dev.twd.up.plist") {
			t.Fatal(got)
		}
		return
	}
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	if got := serviceUnitPath(); got != "/x/cfg/systemd/user/twd.service" {
		t.Fatal(got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := serviceUnitPath(); got != filepath.Join(home, ".config/systemd/user/twd.service") {
		t.Fatal(got)
	}
}

func TestServiceUnsupportedOffLinuxAndMac(t *testing.T) {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		t.Skip("supported here")
	}
	if err := serviceAvailable(); err == nil || !strings.Contains(err.Error(), "service_unsupported") {
		t.Fatalf("serviceAvailable on %s: %v", runtime.GOOS, err)
	}
}

func TestParseVer(t *testing.T) {
	for in, want := range map[string][2]int{"2.39": {2, 39}, "6.8.0-45-generic": {6, 8}, "2.35\n": {2, 35}} {
		if got, ok := parseVer(in); !ok || got != want {
			t.Errorf("parseVer(%q) = %v %v", in, got, ok)
		}
	}
	if _, ok := parseVer("musl"); ok {
		t.Error("parseVer(musl) ok")
	}
	if !verLess([2]int{2, 31}, minGlibc) || verLess([2]int{2, 39}, minGlibc) || verLess(minGlibc, minGlibc) {
		t.Error("verLess")
	}
}

func TestWorkerdGlibcNeedNonELF(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755)
	if v, ok := workerdGlibcNeed(p); ok || v != minGlibc {
		t.Fatalf("%v %v", v, ok)
	}
}
