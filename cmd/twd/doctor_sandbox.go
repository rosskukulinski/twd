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
)

// doctorSandbox is twd doctor's "sandbox" check: is the process-tier
// sandbox (Seatbelt, macOS) available and enforcing on this machine, and,
// while twd runs, is every process-tier workload (workerd or Node) under it?
//
// The probe mirrors what hostd does for a tenant: a generated profile that
// denies the home dir (where an installed twd lives, ~/.twd) but allows the
// workerd binary; workerd must start under it, and a denied file must be
// unreadable. It never touches the user's own files.
func doctorSandbox(s *state) (string, string, string) {
	sb, _ := hostd.ResolveSandbox(hostd.SandboxAuto)
	if sb != hostd.SandboxSeatbelt {
		return "skip", "no process sandbox on " + runtime.GOOS + " (Seatbelt is macOS-only): process-tier workerd and Node apps of all tenants share one uid and are kept apart by 0700 dirs only",
			"for isolation between tenants on Linux, deploy with tier \"microvm\""
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
	if s == nil || !s.supervisorAlive() {
		return "ok", detail + "; twd up runs every tenant's workerd and Node app under it", ""
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	var hv hostd.StatusView
	if err := s.hostd().Do(ctx2, "GET", "/v0/status", nil, &hv); err != nil {
		return "warn", detail + "; twd-hostd is not answering, so running workloads were not checked", "twd status"
	}
	n, on := 0, 0
	var off []string
	for _, w := range hv.Workloads {
		if w.Tier == "microvm" || w.InstanceDir == "" {
			continue
		}
		n++
		if w.Sandbox == hostd.SandboxSeatbelt {
			on++
		} else {
			off = append(off, w.TenantID+"/"+w.Workload)
		}
	}
	if len(off) > 0 {
		return "warn", fmt.Sprintf("%s; but %d of %d running process-tier workloads are NOT sandboxed: %s", detail, len(off), n, strings.Join(off, ", ")),
			"twd down && twd up (twd-hostd -sandbox auto)"
	}
	return "ok", fmt.Sprintf("%s; active: %d/%d running process-tier workloads (workerd and Node) are under it", detail, on, n), ""
}
