package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// `twd up --service` / `twd down --service` (LINUX-PLAN DoD 5): run the
// same supervisor (`twd __supervise`) as a systemd *user* unit, so twd
// survives logout (with lingering) and reboot. Nothing here uses sudo:
// enabling lingering is printed for the user to run.

const serviceUnit = "twd.service"

// userUnitDir is $XDG_CONFIG_HOME/systemd/user (default ~/.config/systemd/user).
func userUnitDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user")
}

func serviceUnitPath() string { return filepath.Join(userUnitDir(), serviceUnit) }

func systemctlUser(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// serviceAvailable: Linux, systemd PID 1, and this user's manager answers.
func serviceAvailable() error {
	if runtime.GOOS != "linux" {
		return fail("service_unsupported", "--service installs a systemd user unit and is Linux-only",
			"on macOS, `twd up` runs a detached supervisor (a launchd agent is not built yet)")
	}
	if fi, err := os.Stat("/run/systemd/system"); err != nil || !fi.IsDir() {
		return fail("service_unsupported", "systemd is not the init system on this machine", "use `twd up` without --service")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fail("service_unsupported", "systemctl not found on PATH")
	}
	if out, err := systemctlUser("show-environment"); err != nil {
		return fail("service_unsupported", "your systemd user manager is not reachable (systemctl --user: "+firstLine(out)+")").
			withFix("log in through a real session (ssh or console, not `su`/`sudo -u`), so XDG_RUNTIME_DIR=/run/user/" + fmt.Sprint(os.Getuid()) + " exists")
	}
	return nil
}

// serviceActive reports whether twd.service is installed and active.
func serviceActive() (installed, active bool) {
	if !isFile(serviceUnitPath()) {
		return false, false
	}
	out, _ := systemctlUser("is-active", serviceUnit)
	return true, out == "active"
}

func unitFor(s *state, exe string) string {
	return fmt.Sprintf(`# Installed by `+"`twd up --service`"+`; removed by `+"`twd down --service`"+`. Don't edit:
# twd rewrites it on the next `+"`twd up --service`"+`.
[Unit]
Description=twd: twd-mgmt + twd-hostd for %s (state %s)
Documentation=https://github.com/rosskukulinski/twd/blob/master/docs/INSTALL.md

[Service]
Type=simple
ExecStart=%s __supervise --state-dir %s
# The supervisor stops twd-hostd (nodes close in parallel), then twd-mgmt
# (flushes pending device deletes): give it the SIGTERM alone, and time.
KillMode=mixed
TimeoutStopSec=180
Restart=on-failure
RestartSec=2
UMask=0077
Environment=PATH=%s:/usr/local/bin:/usr/bin:/bin
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, s.Cfg.Tailnet, s.Dir, unitQuote(exe), unitQuote(s.Dir), filepath.Dir(exe), s.log("supervisor"), s.log("supervisor"))
}

// unitQuote quotes a path for a systemd ExecStart= line if it needs it.
func unitQuote(p string) string {
	if strings.ContainsAny(p, " \t\"'\\") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p) + `"`
	}
	return p
}

// installService writes the unit, enables it and starts it (or restarts it
// if the unit changed). It returns once the supervisor has written its pid.
func installService(s *state) (changed bool, err error) {
	if err := serviceAvailable(); err != nil {
		return false, err
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if err := os.MkdirAll(s.logDir(), 0o700); err != nil {
		return false, err
	}
	p := serviceUnitPath()
	want := unitFor(s, exe)
	if old, err := os.ReadFile(p); err == nil {
		if !strings.Contains(string(old), "__supervise --state-dir "+unitQuote(s.Dir)+"\n") {
			return false, fail("service_conflict", p+" runs twd for another state dir").
				withFix("twd down --service --state-dir <that dir>", "then twd up --service again")
		}
		changed = string(old) != want
	} else {
		changed = true
	}
	if s.supervisorAlive() {
		if inst, act := serviceActive(); !inst || !act {
			return false, fail("already_running", "twd is already running in "+s.Dir+" outside systemd").
				withFix("twd down", "then twd up --service")
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	if changed {
		if err := os.WriteFile(p, []byte(want), 0o644); err != nil {
			return false, err
		}
		if out, err := systemctlUser("daemon-reload"); err != nil {
			return false, fail("service_failed", "systemctl --user daemon-reload failed", out)
		}
	}
	if out, err := systemctlUser("enable", serviceUnit); err != nil {
		return false, fail("service_failed", "systemctl --user enable "+serviceUnit+" failed", out)
	}
	verb := "start"
	if changed && s.supervisorAlive() {
		verb = "restart"
	}
	if out, err := systemctlUser(verb, serviceUnit); err != nil {
		return false, fail("service_failed", "systemctl --user "+verb+" "+serviceUnit+" failed", out).withFix("journalctl --user -u " + serviceUnit)
	}
	for i := 0; i < 500 && !s.supervisorAlive(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if !s.supervisorAlive() {
		return changed, fail("supervisor_failed", "twd.service started but the supervisor didn't come up", "log: "+s.log("supervisor")).
			withFix("systemctl --user status " + serviceUnit)
	}
	return changed, nil
}

// removeService stops (if running), disables and deletes the unit.
func removeService() (removed bool, err error) {
	p := serviceUnitPath()
	if !isFile(p) {
		return false, nil
	}
	if out, err := systemctlUser("disable", "--now", serviceUnit); err != nil {
		return false, fail("service_failed", "systemctl --user disable --now "+serviceUnit+" failed", out)
	}
	if err := os.Remove(p); err != nil {
		return false, err
	}
	systemctlUser("daemon-reload")
	systemctlUser("reset-failed", serviceUnit)
	return true, nil
}

// lingerHint returns the enable-linger guidance, or "" if lingering is on.
func lingerHint() string {
	u, err := user.Current()
	if err != nil || u.Username == "" || isFile("/var/lib/systemd/linger/"+u.Username) {
		return ""
	}
	return "lingering is off for " + u.Username + ": twd.service stops when you log out and won't start at boot. To keep it running, run (twd won't sudo for you):\n    sudo loginctl enable-linger " + u.Username
}
