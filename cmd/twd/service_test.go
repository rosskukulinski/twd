package main

import (
	"os"
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

func TestUserUnitDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	if got := serviceUnitPath(); got != "/x/cfg/systemd/user/twd.service" {
		t.Fatal(got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	home, _ := os.UserHomeDir()
	if got := serviceUnitPath(); got != filepath.Join(home, ".config/systemd/user/twd.service") {
		t.Fatal(got)
	}
}

func TestServiceUnsupportedOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("linux")
	}
	if err := serviceAvailable(); err == nil || !strings.Contains(err.Error(), "Linux-only") {
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
