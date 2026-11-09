package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/internal/procsb"
)

// doctorSandbox is twd doctor's "sandbox" check: is the process-tier
// sandbox (Seatbelt on macOS, Landlock + seccomp on Linux) available and
// enforcing on this machine, and,
// while twd runs, is every process-tier workload (workerd or Node) under it?
//
// The probe mirrors what hostd does for a tenant: a generated profile that
// denies the home dir (where an installed twd lives, ~/.twd) but allows the
// workerd binary; workerd must start under it, and a denied file must be
// unreadable. It never touches the user's own files.
func doctorSandbox(s *state) (string, string, string) {
	sb, _ := hostd.ResolveSandbox(hostd.SandboxAuto)
	switch {
	case sb == hostd.SandboxLandlock:
		return doctorLandlock(s)
	case runtime.GOOS == "linux":
		_, err := procsb.ABI()
		return "warn", fmt.Sprintf("NO PROCESS SANDBOX: Landlock is unavailable on this kernel (%v). Process-tier workerd and Node apps of all tenants share one uid and are kept apart by 0700 dirs only, so twd-hostd refuses a second tenant's process-tier deployments (unless started with -insecure-shared-host)", err),
			"use a kernel with Landlock (5.13+, CONFIG_SECURITY_LANDLOCK=y, \"landlock\" in the lsm= boot parameter; Ubuntu 22.04+, Debian 12+, Fedora have it), or deploy with tier \"microvm\""
	case sb != hostd.SandboxSeatbelt:
		return "skip", "no process sandbox on " + runtime.GOOS + ": process-tier workerd and Node apps of all tenants share one uid and are kept apart by 0700 dirs only",
			"for isolation between tenants, deploy with tier \"microvm\" on Linux"
	}
	osv := "macOS"
	if out, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
		osv += " " + strings.TrimSpace(string(out))
	}
	wd, err := findWorkerd("")
	if err != nil {
		return "fail", "workerd not found, so the sandbox can't be checked", "reinstall twd"
	}
	tmp, err := os.MkdirTemp("", "twd-doctor-sb-")
	if err != nil {
		return "fail", err.Error(), ""
	}
	defer os.RemoveAll(tmp)
	own, deny := filepath.Join(tmp, "own"), filepath.Join(tmp, "deny")
	for _, d := range []string{own, deny} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "fail", err.Error(), ""
		}
	}
	canary := filepath.Join(deny, "canary")
	if err := os.WriteFile(canary, []byte("x"), 0o600); err != nil {
		return "fail", err.Error(), ""
	}
	denyList := []string{deny}
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		denyList = append(denyList, home)
	}
	prof := filepath.Join(own, "doctor.sb")
	if err := os.WriteFile(prof, []byte(hostd.SeatbeltProfile([]string{own}, []string{wd}, denyList)), 0o600); err != nil {
		return "fail", err.Error(), ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-f", prof, wd, "--version").CombinedOutput(); err != nil {
		return "fail", "workerd (" + wd + ") does not start under a twd Seatbelt profile on " + osv + ": " + err.Error() + " " + firstLine(string(out)),
			"report this; meanwhile twd-hostd -sandbox none runs tenants unconfined"
	}
	if err := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-f", prof, "/bin/cat", canary).Run(); err == nil {
		return "fail", "Seatbelt did not enforce a deny rule on " + osv + " (a denied file was readable)", "report this; tenants are not confined on this machine"
	}
	detail := "Seatbelt works on " + osv + ": workerd starts under a twd profile with the home dir denied, and denied files are unreadable"
	return doctorSandboxActive(s, hostd.SandboxSeatbelt, detail, "")
}

// doctorSandboxActive finishes the sandbox check: before twd up, the probe
// detail; while twd runs, whether every process-tier workload (workerd and
// Node) runs under sb.
func doctorSandboxActive(s *state, sb, detail, warn string) (string, string, string) {
	status := "ok"
	if warn != "" {
		status, detail = "warn", detail+"; "+warn
	}
	if s == nil || !s.supervisorAlive() {
		return status, detail + "; twd up runs every tenant's workerd and Node app under it", ""
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	var hv hostd.StatusView
	if err := s.hostd().Do(ctx2, "GET", "/v1/status", nil, &hv); err != nil {
		return "warn", detail + "; twd-hostd is not answering, so running workloads were not checked", "twd status"
	}
	n, on := 0, 0
	var off []string
	for _, w := range hv.Workloads {
		if w.Tier == "microvm" || w.InstanceDir == "" {
			continue
		}
		n++
		if w.Sandbox == sb {
			on++
		} else {
			off = append(off, w.TenantID+"/"+w.Workload)
		}
	}
	if len(off) > 0 {
		return "warn", fmt.Sprintf("%s; but %d of %d running process-tier workloads are NOT sandboxed: %s", detail, len(off), n, strings.Join(off, ", ")),
			"twd down && twd up (twd-hostd -sandbox auto)"
	}
	if hv.Sandbox != "" {
		detail += "; twd-hostd: " + hv.Sandbox
	}
	return status, fmt.Sprintf("%s; active: %d/%d running process-tier workloads (workerd and Node) are under it", detail, on, n), ""
}

// doctorLandlock probes the Linux sandbox the way hostd uses it: a profile
// that allows only a scratch dir and the workerd binary. workerd must start
// under it (launched by this twd binary, which carries the procsb
// launcher); a file outside it must be unreadable, the home dir
// unlistable, and (Landlock ABI ≥ 4) a TCP connect to the internet refused
// before any packet is sent. It never touches the user's own files.
func doctorLandlock(s *state) (string, string, string) {
	abi, _ := procsb.ABI()
	osv := "Linux"
	if out, err := exec.Command("uname", "-r").Output(); err == nil {
		osv += " " + strings.TrimSpace(string(out))
	}
	wd, err := findWorkerd("")
	if err != nil {
		return "fail", "workerd not found, so the sandbox can't be checked", "reinstall twd"
	}
	self, err := os.Executable()
	if err != nil {
		return "fail", err.Error(), ""
	}
	tmp, err := os.MkdirTemp("", "twd-doctor-sb-")
	if err != nil {
		return "fail", err.Error(), ""
	}
	defer os.RemoveAll(tmp)
	own, deny := filepath.Join(tmp, "own"), filepath.Join(tmp, "deny")
	for _, d := range []string{own, deny} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "fail", err.Error(), ""
		}
	}
	canary := filepath.Join(deny, "canary")
	if err := os.WriteFile(canary, []byte("x"), 0o600); err != nil {
		return "fail", err.Error(), ""
	}
	prof := filepath.Join(own, "doctor.json")
	if err := (&procsb.Profile{Launcher: self, Exec: []string{wd}, Own: []string{own}, NoUnixSockets: true}).Write(prof); err != nil {
		return "fail", err.Error(), ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := procsb.Command(self, prof, wd, "--version")
	if out, err := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...).CombinedOutput(); err != nil {
		return "fail", fmt.Sprintf("workerd (%s) does not start under a twd Landlock profile on %s (ABI %d): %v %s", wd, osv, abi, err, firstLine(string(out))),
			"report this; meanwhile twd-hostd -sandbox none runs tenants unconfined"
	}
	probe := func(op, arg string) string {
		c, err := procsb.ProbeCommand(prof, op, arg)
		if err != nil {
			return err.Error()
		}
		out, _ := exec.CommandContext(ctx, c.Path, c.Args[1:]...).Output()
		return strings.TrimSpace(string(out))
	}
	if r := probe(procsb.ProbeRead, canary); r == "ok" {
		return "fail", "Landlock did not enforce on " + osv + " (a file outside the profile was readable)", "report this; tenants are not confined on this machine"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		if r := probe(procsb.ProbeList, home); r == "ok" {
			return "fail", "Landlock did not enforce on " + osv + " (the home dir was listable)", "report this; tenants are not confined on this machine"
		}
	}
	detail := fmt.Sprintf("Landlock ABI %d + seccomp work on %s: workerd starts under a twd profile, files outside it and the home dir are unreadable", abi, osv)
	warn := ""
	if abi >= 4 {
		if r := probe(procsb.ProbeConnectTCP, "1.1.1.1:443"); r == "ok" {
			return "fail", "Landlock did not restrict TCP on " + osv, "report this"
		}
		detail += ", raw TCP is refused (egress only via netd)"
	} else {
		warn = fmt.Sprintf("Landlock ABI %d < 4 can't restrict TCP: a malicious Node app could bypass the egress proxy (use a 6.7+ kernel)", abi)
	}
	if r := probe(procsb.ProbeConnectUnix, "/var/run/tailscale/tailscaled.sock"); r != "EPERM" {
		return "fail", "the sandbox can create unix sockets on " + osv + " (" + r + ")", "report this"
	}
	detail += fmt.Sprintf("; workloads get no AF_UNIX sockets (seccomp), so no host unix socket is reachable (Landlock ABI %d alone can't restrict unix connects below 9)", abi)
	return doctorSandboxActive(s, hostd.SandboxLandlock, detail, warn)
}

// doctorHostSockets (Linux) lists risky host sockets this user can reach
// and container-engine group memberships. Container engines are
// root-equivalent for twd itself: warn, and twd refuses a second tenant.
// tailscaled's socket is info only while every workload is denied AF_UNIX
// (Landlock+seccomp, no unix-listening Node app); otherwise it warns too.
// twd up --insecure-shared-host overrides the refusal.
func doctorHostSockets(s *state) (string, string, string) {
	if runtime.GOOS != "linux" {
		return "skip", "Linux-only check (on macOS Seatbelt denies sockets under the home dir)", ""
	}
	hs := procsb.ScanHostSockets()
	fix := "run twd as a dedicated user that can't reach those sockets and isn't in the docker/podman/lxd groups"
	if len(hs.Hard) > 0 {
		d := "this user can reach " + strings.Join(hs.Hard, ", ") + ": root-equivalent for twd-hostd itself, so twd serves one tenant only here (twd tenant add refuses unless twd up --insecure-shared-host)"
		if hs.Tailscaled != "" {
			d += "; also " + hs.Tailscaled
		}
		return "warn", d, fix
	}
	if hs.Tailscaled != "" {
		if why := tailscaledExposed(s, hs.Tailscaled); why != "" {
			return "warn", hs.Tailscaled + " (this host's tailnet identity) is reachable by this user and " + why + ", so twd serves one tenant only here (twd tenant add refuses unless twd up --insecure-shared-host)",
				fix + ", or enable Landlock and use TCP listen mode for Node apps"
		}
		return "info", hs.Tailscaled + " is reachable by this user, blocked for workloads by seccomp (AF_UNIX denied); the host may be shared", ""
	}
	return "ok", "this user can't reach the system tailscaled's socket or a container engine (docker/containerd/podman/lxd) and is in none of their groups", ""
}
