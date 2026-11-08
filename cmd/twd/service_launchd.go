package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// macOS: `twd up --service` installs a LaunchAgent
// (~/Library/LaunchAgents/dev.twd.up.plist) that runs the supervisor at
// login; `twd down --service` removes it. Per-user (gui/<uid> domain), no
// sudo. KeepAlive on failure only, so `twd down` (SIGTERM, exit 0) stops
// it without launchd restarting it.

const launchdLabel = "dev.twd.up"

func launchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchctl(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func launchdAvailable() error {
	if runtime.GOOS != "darwin" {
		return fail("service_unsupported", "LaunchAgents are macOS-only")
	}
	if _, err := exec.LookPath("launchctl"); err != nil {
		return fail("service_unsupported", "launchctl not found")
	}
	return nil
}

// launchdActive: the plist exists and the agent is loaded with a running pid.
func launchdActive() (installed, active bool) {
	if !isFile(launchdPlistPath()) {
		return false, false
	}
	out, err := launchctl("print", launchdDomain()+"/"+launchdLabel)
	if err != nil {
		return true, false
	}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "pid = ") {
			return true, true
		}
	}
	return true, false
}

// plistFor renders the LaunchAgent for this state dir.
func plistFor(s *state, exe string) string {
	esc := func(v string) string {
		var b strings.Builder
		xml.EscapeText(&b, []byte(v))
		return b.String()
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Installed by `+"`twd up --service`"+`; removed by `+"`twd down --service`"+`. Don't edit: twd rewrites it. -->
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>Comment</key><string>twd: twd-mgmt + twd-hostd for %s (state %s)</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>__supervise</string>
    <string>--state-dir</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key><false/>
  </dict>
  <key>ThrottleInterval</key><integer>2</integer>
  <key>ExitTimeOut</key><integer>180</integer>
  <key>ProcessType</key><string>Background</string>
  <key>Umask</key><integer>63</integer>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>%s:/usr/local/bin:/usr/bin:/bin</string>
  </dict>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, esc(s.Cfg.Tailnet), esc(s.Dir), esc(exe), esc(s.Dir), esc(filepath.Dir(exe)), esc(s.log("supervisor")), esc(s.log("supervisor")))
}

// launchdInstall writes the plist and bootstraps it (re-bootstrapping if
// it changed). It returns once the supervisor has written its pid.
func launchdInstall(s *state) (changed bool, err error) {
	if err := launchdAvailable(); err != nil {
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
	p := launchdPlistPath()
	want := plistFor(s, exe)
	if old, err := os.ReadFile(p); err == nil {
		if !strings.Contains(string(old), "<string>"+s.Dir+"</string>") {
			return false, fail("service_conflict", p+" runs twd for another state dir").
				withFix("twd down --service --state-dir <that dir>", "then twd up --service again")
		}
		changed = string(old) != want
	} else {
		changed = true
	}
	inst, act := launchdActive()
	if s.supervisorAlive() && !(inst && act) {
		return false, fail("already_running", "twd is already running in "+s.Dir+" outside launchd").withFix("twd down", "then twd up --service")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	if changed || !inst {
		if inst {
			launchctl("bootout", launchdDomain()+"/"+launchdLabel) // stops the supervisor (SIGTERM; ExitTimeOut 180 s)
			for i := 0; i < 900 && s.supervisorAlive(); i++ {
				time.Sleep(200 * time.Millisecond)
			}
		}
		if err := os.WriteFile(p, []byte(want), 0o644); err != nil {
			return false, err
		}
	}
	if _, running := launchdActive(); !running {
		if out, err := launchctl("bootstrap", launchdDomain(), p); err != nil && !strings.Contains(out, "already") {
			return false, fail("service_failed", "launchctl bootstrap "+p+" failed", out).withFix("launchctl print " + launchdDomain() + "/" + launchdLabel)
		}
		if _, running := launchdActive(); !running {
			if out, err := launchctl("kickstart", launchdDomain()+"/"+launchdLabel); err != nil {
				return false, fail("service_failed", "launchctl kickstart failed", out)
			}
		}
	}
	for i := 0; i < 500 && !s.supervisorAlive(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if !s.supervisorAlive() {
		return changed, fail("supervisor_failed", launchdLabel+" started but the supervisor didn't come up", "log: "+s.log("supervisor")).
			withFix("launchctl print " + launchdDomain() + "/" + launchdLabel)
	}
	return changed, nil
}

// launchdStop stops the agent's process; RunAtLoad starts it again at the
// next login (KeepAlive only restarts failures).
func launchdStop() error {
	if out, err := launchctl("kill", "SIGTERM", launchdDomain()+"/"+launchdLabel); err != nil {
		return fail("service_failed", "launchctl kill SIGTERM "+launchdLabel+" failed", out)
	}
	return nil
}

// launchdRemove boots the agent out (stopping the supervisor) and deletes
// the plist.
func launchdRemove() (removed bool, err error) {
	p := launchdPlistPath()
	if !isFile(p) {
		return false, nil
	}
	if out, err := launchctl("bootout", launchdDomain()+"/"+launchdLabel); err != nil && !strings.Contains(out, "No such process") && !strings.Contains(out, "Could not find") {
		return false, fail("service_failed", "launchctl bootout "+launchdLabel+" failed", out)
	}
	if err := os.Remove(p); err != nil {
		return false, err
	}
	return true, nil
}

// launchdSleepHint: a sleeping Mac serves nothing; say how to keep it awake.
func launchdSleepHint() string {
	msg := "a sleeping Mac stops twd's nodes (objects become unreachable until it wakes). To keep serving while the lid is open: `sudo pmset -a sleep 0 displaysleep 10` (or `caffeinate -s` in a terminal); on battery, Power Nap is not enough"
	if out, err := exec.Command("pmset", "-g", "custom").Output(); err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			f := strings.Fields(l)
			if len(f) == 2 && f[0] == "sleep" && f[1] == "0" {
				return "" // sleep disabled (at least in the first profile): nothing to warn about
			}
		}
	}
	return msg
}
