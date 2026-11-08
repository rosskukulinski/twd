//go:build e2elinux

package linux

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/e2e/cliflow"
	"github.com/rosskukulinski/twd/netd"
)

// TestUnsupportedHost: on a host twd doesn't support (glibc < 2.35, or
// musl/Alpine), install.sh must refuse with a clear glibc message, and the
// static `bin/twd doctor` from the extracted release must fail its glibc
// check with the same diagnosis. No tailnet or key is involved. It also
// records whether the pinned workerd would actually start there (evidence
// for the floor), without relying on it.
func TestUnsupportedHost(t *testing.T) {
	vmName, tarball := os.Getenv("TWD_E2E_LIMA_VM"), os.Getenv("TWD_E2E_TARBALL")
	if vmName == "" || tarball == "" {
		t.Skip("TWD_E2E_LIMA_VM / TWD_E2E_TARBALL not set")
	}
	if !strings.HasPrefix(vmName, "twdl-") {
		t.Fatalf("refusing to use VM %q", vmName)
	}
	home, _ := os.UserHomeDir()
	v := &vm{t: t, name: vmName, cfg: filepath.Join(home, ".lima", vmName, "ssh.config")}
	res := cliflow.NewResults("l1-"+vmName, "(none)")
	resultsDir := os.Getenv("TWD_E2E_RESULTS_DIR")
	defer func() {
		res.Checks["result"] = map[bool]string{true: "FAIL", false: "PASS"}[t.Failed()]
		b, _ := json.MarshalIndent(res, "", "  ")
		if resultsDir != "" {
			os.WriteFile(filepath.Join(resultsDir, "l1-linux-"+vmName+"-"+time.Now().Format("20060102-150405")+".json"), b, 0o644)
		}
		t.Logf("results:\n%s", b)
		v.run(`rm -rf ~/.twd-e2e ~/.twd`, nil)
	}()
	osInfo := strings.TrimSpace(v.must(`. /etc/os-release; printf '%s | ' "$PRETTY_NAME"; uname -rm | tr '\n' ' '; printf '| libc '; (getconf GNU_LIBC_VERSION 2>/dev/null || ls /lib/ld-musl-* 2>/dev/null) | tr '\n' ' '`))
	res.Checks["os"] = "INFO " + osInfo
	R := strings.TrimSpace(v.must(`printf %s "$HOME"`)) + "/.twd-e2e"
	v.must(`umask 077; mkdir -p ~/.twd-e2e`)
	if out, err := exec.Command("limactl", "copy", tarball, vmName+":"+R+"/").CombinedOutput(); err != nil {
		t.Fatalf("limactl copy: %v %s", err, out)
	}
	dir := R + "/" + strings.TrimSuffix(filepath.Base(tarball), ".tar.gz")
	v.must("cd " + shq(R) + " && tar -xzf " + shq(filepath.Base(tarball)))

	so, se, err := v.run("cd "+shq(dir)+" && ./install.sh", nil)
	msg := strings.TrimSpace(so + se)
	if err == nil {
		t.Fatalf("install.sh succeeded on an unsupported host:\n%s", msg)
	}
	if !strings.Contains(msg, "glibc") || !(strings.Contains(msg, "2.35") || strings.Contains(msg, "musl")) {
		t.Fatalf("install.sh's refusal doesn't explain the libc:\n%s", msg)
	}
	if _, _, err := v.run(`test -e ~/.twd/bin`, nil); err == nil {
		t.Fatal("install.sh refused but left ~/.twd/bin")
	}
	res.Pass("install_refused", msg)

	so, se, err = v.run(shq(dir+"/bin/twd")+" doctor --json", nil)
	var doc doctorOut
	if jerr := json.Unmarshal([]byte(so), &doc); jerr != nil {
		t.Fatalf("bin/twd doctor: %v %v\n%s\n%s", err, jerr, so, netd.Redact(se))
	}
	if err == nil || doc.OK {
		t.Fatalf("bin/twd doctor passed on an unsupported host:\n%s", so)
	}
	st, d := doc.get("glibc")
	if st != "fail" || !strings.Contains(d, "glibc") {
		t.Fatalf("doctor's glibc check: %s %s", st, d)
	}
	for _, c := range doc.Checks {
		if c.Name == "glibc" {
			res.Pass("doctor_glibc_fail", c.Detail+" | fix: "+c.Fix)
		}
		res.Checks["doctor_"+c.Name] = strings.ToUpper(c.Status) + " " + c.Detail
	}
	// Evidence only: does the pinned workerd start here at all?
	wo, we, werr := v.run(shq(dir+"/bin/workerd")+" --version", nil)
	res.Checks["workerd_runs_anyway"] = "INFO " + map[bool]string{true: "yes", false: "no"}[werr == nil] + ": " + strings.TrimSpace(wo+we)
}
