package main

import (
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Host checks that are not specific to one OS's syscalls: the glibc
// workerd needs (read from workerd's own ELF symbol versions), and the
// version parsing shared by the Linux checks. The Linux-only checks
// (kernel, Landlock, KVM, cgroups, systemd) are in doctor_linux.go.

// minGlibc is the floor documented for twd's workerd on Linux (workerd
// 1.2026xxxx is built against glibc 2.35, Ubuntu 22.04). The real floor
// is read from the binary when possible (workerdGlibcNeed).
var minGlibc = [2]int{2, 35}

var glibcSymRE = regexp.MustCompile(`^GLIBC_(\d+)\.(\d+)`)

// workerdGlibcNeed returns the highest GLIBC_x.y symbol version workerd
// imports (what the dynamic loader will demand), or minGlibc if it can't
// be read (not ELF, or stripped of version info).
func workerdGlibcNeed(path string) (need [2]int, fromELF bool) {
	f, err := elf.Open(path)
	if err != nil {
		return minGlibc, false
	}
	defer f.Close()
	syms, err := f.ImportedSymbols()
	if err != nil {
		return minGlibc, false
	}
	for _, s := range syms {
		m := glibcSymRE.FindStringSubmatch(s.Version)
		if m == nil {
			continue
		}
		v := [2]int{atoi(m[1]), atoi(m[2])}
		if verLess(need, v) {
			need = v
		}
	}
	if need == ([2]int{}) {
		return minGlibc, false
	}
	return need, true
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func verLess(a, b [2]int) bool { return a[0] < b[0] || (a[0] == b[0] && a[1] < b[1]) }

func verString(v [2]int) string { return fmt.Sprintf("%d.%d", v[0], v[1]) }

// parseVer parses "2.39" / "6.8.0-45-generic" into major.minor.
func parseVer(s string) ([2]int, bool) {
	m := regexp.MustCompile(`^(\d+)\.(\d+)`).FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return [2]int{}, false
	}
	return [2]int{atoi(m[1]), atoi(m[2])}, true
}

// muslLoader returns the musl dynamic loader's path if this is a musl
// system (Alpine), "" otherwise.
func muslLoader() string {
	for _, d := range []string{"/lib", "/usr/lib"} {
		if m, _ := filepath.Glob(filepath.Join(d, "ld-musl-*.so.1")); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

// osRelease returns PRETTY_NAME from /etc/os-release ("" if absent).
func osRelease() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}
