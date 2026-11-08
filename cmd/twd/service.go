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

// `twd up --service` / `twd down --service`: run the same supervisor
// (`twd __supervise`) as a service that starts at login: a systemd *user*
// unit on Linux (LINUX-PLAN DoD 5; survives logout with lingering and
// reboot) or a LaunchAgent on macOS (M10; survives logout only if the
// Mac doesn't sleep). Nothing here uses sudo: enabling lingering or
// disabling sleep is printed for the user to run. The systemd half is in
// this file, the launchd half in service_launchd.go; the functions the
// commands call dispatch on the OS.

// serviceUnit names the unit (Linux) or the LaunchAgent label (macOS).
var serviceUnit = func() string {
	if runtime.GOOS == "darwin" {
		return launchdLabel
	}
	return "twd.service"
}()

// serviceKind names the service mechanism for messages.
func serviceKind() string {
	if runtime.GOOS == "darwin" {
		return "LaunchAgent"
	}
	return "systemd user unit"
}

// serviceRestartHint is how to restart the installed service by hand.
func serviceRestartHint() string {
	if runtime.GOOS == "darwin" {
		return "launchctl kickstart -k gui/" + fmt.Sprint(os.Getuid()) + "/" + launchdLabel
	}
	return "systemctl --user restart " + serviceUnit
}

// serviceAvailable reports whether --service can work here.
func serviceAvailable() error {
	if runtime.GOOS == "darwin" {
		return launchdAvailable()
	}
	return systemdAvailable()
}

// serviceActive reports whether the service is installed and running.
func serviceActive() (installed, active bool) {
	if runtime.GOOS == "darwin" {
		return launchdActive()
	}
	return systemdActive()
}

// serviceUnitPath is the unit file / plist path.
func serviceUnitPath() string {
	if runtime.GOOS == "darwin" {
		return launchdPlistPath()
	}
	return filepath.Join(userUnitDir(), serviceUnit)
}

// installService writes and starts the service (restarting it if its
// definition changed); it returns once the supervisor has written its pid.
func installService(s *state) (changed bool, err error) {
	if runtime.GOOS == "darwin" {
		return launchdInstall(s)
	}
	return systemdInstall(s)
}

// removeService stops (if running), disables and deletes the service.
func removeService() (removed bool, err error) {
	if runtime.GOOS == "darwin" {
		return launchdRemove()
	}
	return systemdRemove()
}

// serviceStop stops the running service without removing it (twd down).
func serviceStop() error {
	if runtime.GOOS == "darwin" {
		return launchdStop()
	}
	if out, err := systemctlUser("stop", serviceUnit); err != nil {
		return fail("service_failed", "systemctl --user stop "+serviceUnit+" failed", out)
	}
	return nil
}

// lingerHint returns the enable-linger guidance (Linux), or "".
func lingerHint() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	return systemdLingerHint()
}

// sleepHint warns that a sleeping Mac stops serving (macOS), or "".
func sleepHint() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	return launchdSleepHint()
}

// ---- systemd (Linux) ----

// userUnitDir is $XDG_CONFIG_HOME/systemd/user (default ~/.config/systemd/user).
func userUnitDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user")
}

func systemctlUser(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// systemdAvailable: Linux, systemd PID 1, and this user's manager answers.
func systemdAvailable() error {
	if runtime.GOOS != "linux" {
		return fail("service_unsupported", "--service needs systemd (Linux) or launchd (macOS)")
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

// systemdActive reports whether twd.service is installed and active.
func systemdActive() (installed, active bool) {
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

// systemdInstall writes the unit, enables it and starts it (or restarts it
// if the unit changed). It returns once the supervisor has written its pid.
func systemdInstall(s *state) (changed bool, err error) {
	if err := systemdAvailable(); err != nil {
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

// systemdRemove stops (if running), disables and deletes the unit.
func systemdRemove() (removed bool, err error) {
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

// systemdLingerHint returns the enable-linger guidance, or "" if lingering is on.
func systemdLingerHint() string {
	u, err := user.Current()
	if err != nil || u.Username == "" || isFile("/var/lib/systemd/linger/"+u.Username) {
		return ""
	}
	return "lingering is off for " + u.Username + ": twd.service stops when you log out and won't start at boot. To keep it running, run (twd won't sudo for you):\n    sudo loginctl enable-linger " + u.Username
}
