//go:build !linux

package main

import (
	"os/exec"
	"strconv"
	"strings"
)

func doctorPlatform(run func(string, func() (string, string, string))) {}

func doctorLinuxHost(run func(string, func() (string, string, string)), _ *state) {}

// tailscaledHolders reports open files of pids that belong to the system
// tailscaled (its LocalAPI socket, or the macOS app's IPN), using lsof. It
// never connects to tailscaled.
func tailscaledHolders(pids []int) (bad []string, method string) {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return nil, ""
	}
	var ps []string
	for _, p := range pids {
		ps = append(ps, strconv.Itoa(p))
	}
	out, _ := exec.Command(lsof, "-n", "-P", "-Fn", "-p", strings.Join(ps, ",")).Output()
	for _, ln := range strings.Split(string(out), "\n") {
		l := strings.ToLower(ln)
		if strings.HasPrefix(l, "n") && (strings.Contains(l, "tailscaled.sock") || strings.Contains(l, "io.tailscale.ipn") ||
			strings.Contains(l, "sameuserproof") || strings.Contains(l, "/var/run/tailscale")) {
			bad = append(bad, ln[1:])
		}
	}
	return bad, "lsof"
}

func systemTailscaled() bool {
	return isFile("/Applications/Tailscale.app/Contents/Info.plist") || lookPathOK("tailscaled") || lookPathOK("tailscale")
}
