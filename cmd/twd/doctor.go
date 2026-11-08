package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/internal/esbundle"
	"github.com/rosskukulinski/twd/internal/tsapi"
)

// version is set at release build time (-ldflags -X main.version=…).
var version = "dev"

// workerdPin is the workerd npm version twd is built and tested against;
// `workerd --version` prints its date.
const (
	workerdPin     = "1.20261108.1"
	workerdPinDate = "2026-11-08"
)

func cmdVersion(args []string) error {
	f := newFlags("version")
	if _, err := f.parse(args); err != nil {
		return err
	}
	v := map[string]string{"twd": version, "go": runtime.Version(), "os": runtime.GOOS + "/" + runtime.GOARCH,
		"workerdPin": workerdPin, "esbuild": esbundle.Version}
	emit(f, v, func() {
		fmt.Printf("twd %s (%s, %s) · workerd %s · esbuild %s (built in)\n", version, v["os"], v["go"], workerdPin, esbundle.Version)
	})
	return nil
}

type check struct {
	Name   string  `json:"name"`
	Status string  `json:"status"` // ok | warn | fail | skip
	Detail string  `json:"detail"`
	Fix    string  `json:"fix,omitempty"`
	MS     float64 `json:"ms"`
}

// cmdDoctor checks this machine can run twd: workerd runs, the bundler and
// templates are there (offline), ~/.twd and the state dir are writable, the
// API key works, and twd is not using the system tailscaled.
func cmdDoctor(args []string) error {
	f := newFlags("doctor")
	tailnetF := f.String("tailnet", "", "tailnet to check the API key against (default: from twd up)")
	keyF := f.String("api-key-file", "", "API key file to check (default: from twd up)")
	if _, err := f.parse(args); err != nil {
		return err
	}
	var checks []check
	run := func(name string, fn func() (status, detail, fix string)) {
		t := time.Now()
		st, d, fx := fn()
		checks = append(checks, check{Name: name, Status: st, Detail: d, Fix: fx, MS: ms(time.Since(t))})
	}
	s, serr := openState(*f.stateDir)

	run("binaries", func() (string, string, string) {
		var miss []string
		for _, b := range []string{"twd-mgmt", "twd-hostd"} {
			if _, err := findBin(b); err != nil {
				miss = append(miss, b)
			}
		}
		if len(miss) > 0 {
			return "fail", "not found next to twd (" + exeDir() + ") or on PATH: " + strings.Join(miss, ", "), "reinstall twd (install.sh)"
		}
		return "ok", "twd " + version + ", twd-mgmt and twd-hostd in " + exeDir(), ""
	})

	run("workerd", func() (string, string, string) {
		wd, err := findWorkerd("")
		if err != nil {
			return "fail", "workerd not found", "reinstall twd, or set $TWD_WORKERD"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, wd, "--version").CombinedOutput()
		v := strings.TrimSpace(string(out))
		if err != nil {
			fix := "check the binary runs: " + wd + " --version"
			if runtime.GOOS == "darwin" {
				fix += " (unsigned? `codesign -f -s - " + wd + "`; quarantined? `xattr -d com.apple.quarantine " + wd + "`)"
			}
			return "fail", wd + " does not run: " + err.Error() + " " + v, fix
		}
		if runtime.GOOS == "darwin" {
			if q, _ := exec.Command("xattr", "-p", "com.apple.quarantine", wd).Output(); len(bytes.TrimSpace(q)) > 0 {
				return "warn", wd + " runs (" + v + ") but is quarantined", "xattr -d com.apple.quarantine " + wd
			}
		}
		if !strings.Contains(v, workerdPinDate) {
			return "warn", wd + ": " + v + " (twd is tested with " + workerdPin + ")", ""
		}
		return "ok", wd + ": " + v + " (pinned " + workerdPin + ")", ""
	})

	run("bundler", func() (string, string, string) {
		ct, err := createTwdDir()
		if err != nil {
			return "fail", "templates (create-twd) not found", "reinstall twd"
		}
		np := nodePaths(ct)
		tmp, err := os.MkdirTemp("", "twd-doctor-")
		if err != nil {
			return "fail", err.Error(), ""
		}
		defer os.RemoveAll(tmp)
		var sizes []string
		for _, tm := range []string{"hello", "agent", "mcp"} {
			dir := filepath.Join(ct, "templates", tm)
			wb, err := os.ReadFile(filepath.Join(dir, "wrangler.jsonc"))
			if err != nil {
				return "fail", "template " + tm + " missing in " + ct, "reinstall twd"
			}
			var w struct {
				Main string `json:"main"`
			}
			bundle.UnmarshalJSONC(wb, &w)
			n, err := esbundle.Build(esbundle.Options{Dir: dir, Main: w.Main, Outfile: filepath.Join(tmp, tm+".js"), NodePaths: np})
			if err != nil {
				return "fail", "bundling template " + tm + " failed: " + firstLine(err.Error()), "reinstall twd (the vendored node_modules may be incomplete)"
			}
			sizes = append(sizes, fmt.Sprintf("%s %.0f KB", tm, float64(n)/1024))
		}
		return "ok", "esbuild " + esbundle.Version + " (built in, no Node.js needed) bundles every template offline: " + strings.Join(sizes, ", "), ""
	})

	run("twd_home_writable", func() (string, string, string) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "fail", err.Error(), ""
		}
		return writable(filepath.Join(home, ".twd"))
	})

	run("state_dir_writable", func() (string, string, string) {
		if serr != nil {
			return "fail", serr.Error(), ""
		}
		return writable(s.Dir)
	})

	run("api_key", func() (string, string, string) {
		tn, kf := *tailnetF, *keyF
		if s != nil {
			if tn == "" {
				tn = s.Cfg.Tailnet
			}
			if kf == "" {
				kf = s.Cfg.APIKeyFile
			}
		}
		if tn == "" || kf == "" {
			return "skip", "no tailnet / API key file configured (twd up not run here)", "twd doctor --tailnet <tailnet> --api-key-file <file>"
		}
		kf, _ = filepath.Abs(kf)
		key, err := tsapi.LoadEnvFile(kf, "TS_API_KEY")
		if err != nil {
			return "fail", "can't read TS_API_KEY from " + kf, "the file needs a TS_API_KEY=… line"
		}
		api := &tsapi.Client{Tailnet: tn, APIKey: key}
		if s != nil && s.Cfg.TSAPIURL != "" && *tailnetF == "" {
			api.BaseURL = s.Cfg.TSAPIURL
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, _, err := api.GetPolicyHuJSON(ctx); err != nil {
			return "fail", "the Tailscale API rejected the key for " + tn + ": " + firstLine(err.Error()), "check the tailnet name and the key in " + kf
		}
		st := "ok"
		d := "the key in " + kf + " can read " + tn + "'s policy (key not shown)"
		if fi, err := os.Stat(kf); err == nil && fi.Mode().Perm()&0o077 != 0 {
			return "warn", d + ", but the file is mode " + fi.Mode().Perm().String(), "chmod 600 " + kf
		}
		return st, d, ""
	})

	run("system_tailscaled_not_used", func() (string, string, string) {
		var notes []string
		if s != nil && s.configured() {
			if s.Cfg.ControlURL == "" {
				return "fail", "twd.json has no explicit control URL", "twd down, then twd up --tailnet <tailnet> …"
			}
			notes = append(notes, "twd nodes are tsnet nodes of "+s.Cfg.Tailnet+" (control "+s.Cfg.ControlURL+", state under "+filepath.Join(s.Dir, "hostd")+")")
		} else {
			notes = append(notes, "twd only ever runs tsnet nodes for the tailnet you name (no bootstrap from tailscaled)")
		}
		// Running twd processes must not hold the system tailscaled's
		// LocalAPI socket (checked with lsof; never contacts tailscaled).
		if s != nil && s.supervisorAlive() {
			pids := append([]int{s.supervisorPID()}, childPIDs(s.supervisorPID())...)
			if lsof, err := exec.LookPath("lsof"); err == nil {
				var ps []string
				for _, p := range pids {
					ps = append(ps, strconv.Itoa(p))
				}
				out, _ := exec.Command(lsof, "-n", "-P", "-Fn", "-p", strings.Join(ps, ",")).Output()
				for _, ln := range strings.Split(string(out), "\n") {
					l := strings.ToLower(ln)
					if strings.HasPrefix(l, "n") && (strings.Contains(l, "tailscaled.sock") || strings.Contains(l, "io.tailscale.ipn") ||
						strings.Contains(l, "sameuserproof") || strings.Contains(l, "/var/run/tailscale")) {
						return "fail", "a twd process has the system tailscaled's socket open: " + ln[1:], "report this: twd must never use the system tailscaled"
					}
				}
				notes = append(notes, fmt.Sprintf("%d running twd processes hold no tailscaled LocalAPI socket (lsof)", len(pids)))
			}
		}
		if isFile("/Applications/Tailscale.app/Contents/Info.plist") || lookPathOK("tailscaled") || lookPathOK("tailscale") {
			notes = append(notes, "a system Tailscale is installed on this machine and is left alone (its state is not read)")
		}
		return "ok", strings.Join(notes, "; "), ""
	})

	failed := false
	for _, c := range checks {
		if c.Status == "fail" {
			failed = true
		}
	}
	res := map[string]any{"ok": !failed, "version": version, "checks": checks}
	emit(f, res, func() {
		mark := map[string]string{"ok": "✓", "warn": "!", "fail": "✗", "skip": "-"}
		fmt.Printf("twd doctor · twd %s · %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
		for _, c := range checks {
			fmt.Printf("%s %-28s %s\n", mark[c.Status], c.Name, c.Detail)
			if c.Fix != "" {
				fmt.Printf("  %-28s fix: %s\n", "", c.Fix)
			}
		}
	})
	if failed {
		return fail("doctor_failed", "one or more checks failed (see above)")
	}
	return nil
}

// writable checks dir (or, if it doesn't exist yet, the directory that
// would hold it) accepts new files. It creates nothing that stays.
func writable(dir string) (string, string, string) {
	probe, note := dir, ""
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		probe, note = filepath.Dir(dir), " (not created yet; "+filepath.Dir(dir)+" is writable)"
	}
	fp, err := os.CreateTemp(probe, ".twd-doctor-*")
	if err != nil {
		return "fail", probe + " is not writable: " + err.Error(), "chmod u+w " + probe
	}
	fp.Close()
	os.Remove(fp.Name())
	if note != "" {
		return "ok", dir + note, ""
	}
	return "ok", dir + " is writable", ""
}

func childPIDs(pid int) []int {
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil
	}
	var r []int
	for _, f := range strings.Fields(string(out)) {
		if p, err := strconv.Atoi(f); err == nil {
			r = append(r, p)
			r = append(r, childPIDs(p)...)
		}
	}
	return r
}

func lookPathOK(name string) bool { _, err := exec.LookPath(name); return err == nil }

func firstLine(s string) string { l, _, _ := strings.Cut(strings.TrimSpace(s), "\n"); return l }
