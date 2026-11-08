//go:build e2elinux

// E2E-LINUX (LINUX-PLAN DoD 2): the e2e-mbp flow against a fresh Lima VM
// that mounts nothing from this Mac. From here: `limactl copy` the linux
// release tarball and the tailnet-1 API key file into the VM
// (~/.twd-e2e, 0700 dir, 0600 key), run install.sh and `twd doctor`
// there, then `twd up --service` (a systemd user unit) / `twd init` /
// `twd deploy` in the VM over ssh. The tag:ttest test client and the
// private echo run HERE as tsnet nodes, so checks (a)–(f) cross machines
// over tailnet 1. Then `systemctl --user restart twd` must come back and
// stay reachable, `twd down` + `twd down --service`, uninstall, remove
// ~/.twd-e2e (the key file goes even on failure: t.Cleanup here, and the
// trap in scripts/e2e-linux-one.sh), and zero leftover devices and keys
// from this run, by ID.
//
// Safety: tailnet 1 only (allow list); no policy writes (M4 added the
// grants); keys are never printed; the VM's own tailscaled (if any) is
// never logged in or used.
package linux

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/e2e/cliflow"
	"github.com/rosskukulinski/twd/internal/policy"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/tailcfg"
)

const prodControl = "https://controlplane.tailscale.com"

var allowedTailnets = map[string]bool{"tail7906d5.ts.net": true}

// vm runs commands in the Lima VM over its ssh config (Lima's own
// persistent ControlMaster).
type vm struct {
	t    testing.TB
	name string
	cfg  string
}

func (v *vm) run(cmd string, stdin io.Reader) (string, string, error) {
	c := exec.Command("ssh", "-F", v.cfg, "-o", "ServerAliveInterval=15", "lima-"+v.name, cmd)
	var so, se bytes.Buffer
	c.Stdout, c.Stderr, c.Stdin = &so, &se, stdin
	err := c.Run()
	return so.String(), se.String(), err
}

func (v *vm) must(cmd string) string {
	v.t.Helper()
	so, se, err := v.run(cmd, nil)
	if err != nil {
		v.t.Fatalf("%s: %s: %v\n%s\n%s", v.name, cmd, err, netd.Redact(so), netd.Redact(se))
	}
	return so
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// timed runs argv in dir in the VM; the VM measures its own wall time
// (GNU date +%s%N), so ssh round trips are not counted.
var wallRE = regexp.MustCompile(`\n?__twd_wall_us=([0-9]+)\n?$`)

func (v *vm) timed(dir string, argv ...string) (string, string, time.Duration, error) {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shq(a)
	}
	cmd := "cd " + shq(dir) + " 2>/dev/null || cd; s=$(date +%s%N); " + strings.Join(q, " ") +
		"; r=$?; e=$(date +%s%N); printf '\\n__twd_wall_us=%d\\n' $(( (e - s) / 1000 )) >&2; exit $r"
	st := time.Now()
	so, se, err := v.run(cmd, nil)
	d := time.Since(st)
	if m := wallRE.FindStringSubmatch(se); m != nil {
		us, _ := strconv.ParseInt(m[1], 10, 64)
		d = time.Duration(us) * time.Microsecond
		se = se[:len(se)-len(m[0])]
	}
	return so, se, d, err
}

func msOf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	sort.Float64s(s)
	return s[len(s)/2]
}

type doctorOut struct {
	OK     bool `json:"ok"`
	Checks []struct {
		Name, Status, Detail, Fix string
	} `json:"checks"`
}

func (d doctorOut) get(name string) (status, detail string) {
	for _, c := range d.Checks {
		if c.Name == name {
			return c.Status, c.Detail
		}
	}
	return "", ""
}

func TestE2ELinux(t *testing.T) {
	keyFile, tailnet, vmName, tarball := os.Getenv("TWD_E2E_API_KEY_FILE"), os.Getenv("TWD_E2E_TAILNET"), os.Getenv("TWD_E2E_LIMA_VM"), os.Getenv("TWD_E2E_TARBALL")
	if keyFile == "" || tailnet == "" || vmName == "" || tarball == "" {
		t.Skip("TWD_E2E_API_KEY_FILE / TWD_E2E_TAILNET / TWD_E2E_LIMA_VM / TWD_E2E_TARBALL not set")
	}
	if !allowedTailnets[tailnet] {
		t.Fatalf("refusing to run against tailnet %q (not in the allow list)", tailnet)
	}
	if !strings.HasPrefix(vmName, "twdl-") {
		t.Fatalf("refusing to use VM %q (matrix VMs are twdl-*)", vmName)
	}
	if _, err := os.Stat(tarball); err != nil {
		t.Fatalf("tarball: %v (make release)", err)
	}
	slow, _ := strconv.ParseFloat(os.Getenv("TWD_E2E_SLOW"), 64)
	doReboot := os.Getenv("TWD_E2E_REBOOT") == "1"       // enable-linger + limactl stop/start; the service must come back
	doEphemeral := os.Getenv("TWD_E2E_EPHEMERAL") == "1" // a second, --ephemeral run: a sub-agent across a service restart
	scale := func(d time.Duration) time.Duration {
		if slow > 1 {
			return time.Duration(float64(d) * slow)
		}
		return d
	}
	apiKey, err := tsapi.LoadEnvFile(keyFile, "TS_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	api := &tsapi.Client{Tailnet: tailnet, APIKey: apiKey}
	b := make([]byte, 3)
	rand.Read(b)
	run := hex.EncodeToString(b)
	testPrefix := "ttest-l1-" + run + "-" // this Mac's client + echo
	twdPrefix := "twd-l1-" + run + "-"    // every node twd creates in the VM
	root, _ := nettest.FindRepoRoot()
	res := cliflow.NewResults("l1-"+vmName+"-"+run, tailnet)
	res.Notes = append(res.Notes, "twd in Lima VM "+vmName+" (no mounts) from "+filepath.Base(tarball)+"; test client + echo on this Mac")
	resultsDir := os.Getenv("TWD_E2E_RESULTS_DIR")
	if resultsDir == "" {
		resultsDir = filepath.Join(root, "e2e", "real", "results")
	}
	resultsTo := filepath.Join(resultsDir, "l1-linux-"+vmName+"-"+time.Now().Format("20060102-150405")+"-"+run+".json")
	ctx, cancel := context.WithTimeout(context.Background(), scale(25*time.Minute))
	defer cancel()

	pol, _, err := api.GetPolicyHuJSON(ctx)
	if err != nil {
		t.Fatalf("policy read: %v", netd.RedactError(err))
	}
	if miss, err := policy.Missing(pol, cliflow.Policy()); err != nil || !miss.Empty() {
		t.Fatalf("tailnet 1's policy lacks the scenario's grants/tagOwners (not writing it in this run): %v %s", err, miss.HuJSON())
	}
	res.Notes = append(res.Notes, "policy has the scenario's tagOwners and grants (read only; not written)")

	home, _ := os.UserHomeDir()
	v := &vm{t: t, name: vmName, cfg: filepath.Join(home, ".lima", vmName, "ssh.config")}
	vhome := strings.TrimSpace(v.must(`printf %s "$HOME"`))
	if vhome == "" || !strings.HasPrefix(vhome, "/") {
		t.Fatalf("VM $HOME = %q", vhome)
	}
	R := vhome + "/.twd-e2e"
	rKey, rState, rWork := R+"/tailnet1_api.env", R+"/state", R+"/work"
	rTwd := vhome + "/.twd/bin/twd"

	if so := v.must(`[ -e ~/.twd ] && echo twd; [ -e ~/.twd-e2e ] && echo e2e; [ -e ~/.config/systemd/user/twd.service ] && echo unit; true`); strings.TrimSpace(so) != "" {
		t.Fatalf("VM is not clean (%s): remove ~/.twd-e2e, ~/.twd and the unit first", strings.TrimSpace(so))
	}
	if so := v.must(`grep -E ' (virtiofs|9p|fuse.sshfs) ' /proc/mounts; true`); strings.TrimSpace(so) != "" {
		t.Fatalf("VM %s has host mounts (must mount nothing from the Mac):\n%s", vmName, so)
	}
	res.Notes = append(res.Notes, "VM mounts nothing from the Mac (no virtiofs/9p/sshfs in /proc/mounts)")
	if so := v.must(`command -v node go npm esbuild 2>/dev/null; true`); strings.TrimSpace(so) != "" {
		res.Notes = append(res.Notes, "VM has on PATH: "+strings.Join(strings.Fields(so), " "))
	} else {
		res.Notes = append(res.Notes, "VM has no node, go, npm or esbuild on PATH")
	}
	osInfo := strings.TrimSpace(v.must(`. /etc/os-release; printf '%s | ' "$PRETTY_NAME"; uname -rm | tr '\n' ' '; printf '| glibc '; getconf GNU_LIBC_VERSION 2>/dev/null | cut -d' ' -f2; nproc | tr '\n' ' '; echo cpus`))
	res.Notes = append(res.Notes, "VM: "+osInfo)
	res.Checks["os"] = "INFO " + osInfo
	res.Checks["sandbox"] = "INFO sandbox: none (L2 pending)"

	quiet := slog.New(netd.NewRedactingHandler(slog.NewTextHandler(io.Discard, nil)))
	minter := tsapi.MintSource(api, quiet)
	tracker := tsapi.NewTracker(api)
	work := t.TempDir()
	var nodes []*nettest.Node
	downDone := false
	var client *nettest.Node
	var mcpIP string

	e := &cliflow.Env{T: t, BinDir: "(" + vmName + ")", StateDir: rState, WorkDir: rWork, Tailnet: tailnet, APIKeyFile: rKey,
		ControlURL: prodControl, Prefix: twdPrefix, Res: res, ClientTagged: true, Slow: slow,
		// twd up's defaults (persistent nodes, as a real install) under
		// --service: an --ephemeral node that goes offline for a restart
		// may be removed by control and then can't rejoin (see STATUS L1).
		UpArgs: []string{"--service"}, Persistent: true,
		Exec: func(args []string) (string, string, time.Duration, error) {
			return v.timed(rWork, append([]string{rTwd}, args...)...)
		},
		PatchFile: func(path string, edit func([]byte) []byte) error {
			so, se, err := v.run("cat "+shq(path), nil)
			if err != nil {
				return fmt.Errorf("%v %s", err, se)
			}
			_, se, err = v.run("cat > "+shq(path), bytes.NewReader(edit([]byte(so))))
			if err != nil {
				return fmt.Errorf("%v %s", err, se)
			}
			return nil
		},
		NewNode: func(role string) *nettest.Node {
			k, err := minter.AuthKey(ctx, netd.KeyRequest{ObjectID: role, Tags: []string{cliflow.TestTag}, Ephemeral: true})
			if err != nil {
				t.Fatalf("mint %s: %v", role, netd.RedactError(err))
			}
			n, err := nettest.NewNode(ctx, prodControl, k, testPrefix+role, filepath.Join(work, role))
			if err != nil {
				t.Fatalf("%s: %v", role, netd.RedactError(err))
			}
			nodes = append(nodes, n)
			if st, err := n.LC.StatusWithoutPeers(ctx); err == nil && st.Self != nil {
				tracker.Track(string(st.Self.ID), role)
			}
			return n
		}}
	e.OnPeers = func(c *nettest.Node, hc *http.Client, agentIP, mIP string) {
		client, mcpIP = c, mIP
		measurePath(t, res, c, hc, mIP)
		// The workload node is a Linux node (this Mac is darwin): it
		// really runs in the VM.
		if os, addrs := peerOS(c, mIP); os == "linux" {
			res.Pass("cross_machine", fmt.Sprintf("the mcp workload node reports OS linux (test client on %s, darwin); its endpoints: %s", localHost(), addrs))
		} else {
			t.Fatalf("mcp workload node reports OS %q, want linux", os)
		}
		k, err := minter.AuthKey(ctx, netd.KeyRequest{ObjectID: "derp", Tags: []string{cliflow.TestTag}, Ephemeral: true})
		if err != nil {
			t.Fatalf("mint derp: %v", netd.RedactError(err))
		}
		derpProbe(t, res, tracker, k, testPrefix+"derp", mIP, work)
	}

	t.Cleanup(func() {
		if !downDone {
			if _, se, _, err := e.Twd("down", "--purge", "--service"); err != nil {
				t.Logf("twd down (cleanup): %v %s", err, netd.Redact(se))
			}
		}
		if t.Failed() {
			so, _, _ := v.run(`for c in supervisor mgmt hostd; do echo "== $c"; tail -c 6000 ~/.twd-e2e/state/logs/$c.log 2>/dev/null; done; systemctl --user status twd.service 2>&1 | tail -20`, nil)
			t.Logf("VM logs (tail):\n%s", netd.Redact(so))
		}
		for _, n := range nodes {
			n.Close()
		}
		us := time.Now()
		so, se, err := v.run(`systemctl --user disable --now twd.service >/dev/null 2>&1; rm -f ~/.config/systemd/user/twd.service; systemctl --user daemon-reload 2>/dev/null
			pkill -TERM -f "$HOME/.twd/bin/" 2>/dev/null; sleep 0.5; pkill -KILL -f "$HOME/.twd/bin/" 2>/dev/null
			u=$(ls -d ~/.twd-e2e/twd-*/install.sh 2>/dev/null | head -1)
			if [ -n "$u" ]; then "$u" --uninstall --purge --force; fi
			rm -rf ~/.twd-e2e
			[ -e ~/.twd ] && echo LEFT ~/.twd; [ -e ~/.twd-e2e ] && echo LEFT ~/.twd-e2e; [ -e ~/.config/systemd/user/twd.service ] && echo LEFT unit
			pgrep -fa "$HOME/.twd/" && echo LEFT processes; true`, nil)
		res.Numbers["uninstall_ms"] = msOf(time.Since(us))
		if err != nil || strings.Contains(so, "LEFT") {
			t.Errorf("VM cleanup: %v %s %s", err, netd.Redact(so), netd.Redact(se))
			res.Cleanup["vm_clean"] = false
		} else {
			res.Cleanup["vm_clean"] = true
		}

		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		devs, keys := cliflow.Leftovers(cctx, api, res)
		res.Cleanup["twd_devices_tracked"] = len(res.NodeIDs)
		res.Cleanup["twd_keys_tracked"] = len(res.KeyIDs)
		res.Cleanup["twd_devices_left_after_down"] = len(devs)
		res.Cleanup["twd_keys_valid_after_down"] = len(keys)
		for _, id := range res.NodeIDs {
			tracker.Track(id, "twd")
		}
		crep := tracker.Cleanup(cctx, testPrefix)
		trep := tracker.Cleanup(cctx, twdPrefix)
		for _, k := range keys {
			api.DeleteKey(cctx, k)
		}
		var revErrs []string
		if err := minter.RevokeAll(cctx); err != nil {
			revErrs = append(revErrs, netd.Redact(err.Error()))
		}
		res.Cleanup["devices_tracked"] = crep.Tracked
		res.Cleanup["devices_deleted_by_cleanup"] = crep.Deleted
		res.Cleanup["devices_left"] = crep.Left
		res.Cleanup["untracked_run_devices"] = append(crep.Untracked, trep.Untracked...)
		res.Cleanup["test_keys_minted"] = len(minter.KeyIDs())
		res.Cleanup["test_keys_revoke_errors"] = revErrs
		res.Checks["result"] = map[bool]string{true: "FAIL", false: "PASS"}[t.Failed()]
		os.MkdirAll(filepath.Dir(resultsTo), 0o755)
		out, _ := json.MarshalIndent(res, "", "  ")
		out = []byte(netd.Redact(string(out)))
		os.WriteFile(resultsTo, out, 0o644)
		t.Logf("results: %s\n%s", resultsTo, out)
		if len(devs) > 0 || len(keys) > 0 {
			t.Errorf("after twd down: leftover twd devices %v, valid keys %d", devs, len(keys))
		}
		if crep.Left != 0 || len(crep.Untracked) > 0 || len(trep.Untracked) > 0 {
			t.Errorf("cleanup left devices: %+v %+v", crep, trep)
		}
	})

	// Copy the tarball and the key file in with `limactl copy` (as a user
	// would get them; the key is 0600 inside a 0700 dir, never on a
	// command line, never printed).
	v.must(`umask 077; mkdir -p ~/.twd-e2e/work && chmod 700 ~/.twd-e2e`)
	st := time.Now()
	if out, err := exec.Command("limactl", "copy", tarball, vmName+":"+R+"/").CombinedOutput(); err != nil {
		t.Fatalf("limactl copy tarball: %v %s", err, out)
	}
	res.Numbers["copy_tarball_ms"] = msOf(time.Since(st))
	if out, err := exec.Command("limactl", "copy", keyFile, vmName+":"+rKey).CombinedOutput(); err != nil {
		t.Fatalf("limactl copy key file: %v %s", err, netd.Redact(string(out)))
	}
	v.must("chmod 600 " + shq(rKey))
	if m := strings.TrimSpace(v.must("stat -c %a " + shq(rKey))); m != "600" {
		t.Fatalf("key file mode in the VM: %s", m)
	}
	res.Checks["copy"] = "PASS tarball + key file (0600, in a 0700 dir) via limactl copy"

	// install.sh
	tgz := filepath.Base(tarball)
	dirName := strings.TrimSuffix(tgz, ".tar.gz")
	_, se, d, err := v.timed(R, "tar", "-xzf", tgz)
	if err != nil {
		t.Fatalf("extract: %v %s", err, se)
	}
	res.Numbers["extract_ms"] = msOf(d)
	so, se, d, err := v.timed(R, R+"/"+dirName+"/install.sh")
	if err != nil {
		t.Fatalf("install.sh: %v\n%s\n%s", err, so, se)
	}
	res.Numbers["install_ms"] = msOf(d)
	t.Logf("install.sh:\n%s", so)
	if !strings.Contains(so, "export PATH=") {
		t.Fatalf("install.sh did not print PATH instructions:\n%s", so)
	}
	so2, se, d2, err := v.timed(R, R+"/"+dirName+"/install.sh")
	if err != nil || !strings.Contains(so2, "installed twd") {
		t.Fatalf("install.sh (again): %v\n%s\n%s", err, so2, se)
	}
	res.Numbers["install_again_ms"] = msOf(d2)
	ver := strings.TrimSpace(v.must(`cat ~/.twd/VERSION; ~/.twd/bin/workerd --version`))
	res.Pass("install", fmt.Sprintf("install.sh %.1f s (again %.1f s), no sudo, ~/.twd: %s", msOf(d)/1000, msOf(d2)/1000, strings.Join(strings.Fields(ver), " ")))

	// twd doctor before up: no check may fail. The Linux checks must be ok
	// (the microVM pieces may be skip: they're optional).
	var doc doctorOut
	so, se, d, err = e.Twd("doctor", "--tailnet", tailnet, "--api-key-file", rKey, "--json")
	if err != nil || json.Unmarshal([]byte(so), &doc) != nil || !doc.OK {
		t.Fatalf("twd doctor in the VM: %v\n%s\n%s", err, so, netd.Redact(se))
	}
	res.Numbers["doctor_ms"] = msOf(d)
	mustOK := []string{"binaries", "glibc", "workerd", "bundler", "twd_home_writable", "state_dir_writable", "api_key", "kernel", "cgroups_v2", "systemd", "system_tailscaled_not_used"}
	var summary []string
	for _, c := range doc.Checks {
		summary = append(summary, c.Name+"="+c.Status)
		res.Checks["doctor_"+c.Name] = strings.ToUpper(c.Status) + " " + c.Detail
		if c.Status == "fail" || (slices.Contains(mustOK, c.Name) && c.Status != "ok") {
			t.Fatalf("twd doctor in the VM: %s is %s: %s", c.Name, c.Status, c.Detail)
		}
	}
	if st, d := doc.get("landlock"); st == "" {
		t.Fatal("doctor has no landlock check")
	} else {
		res.Notes = append(res.Notes, "doctor landlock: "+st+": "+d)
	}
	_, mv := doc.get("microvm_tier")
	res.Checks["microvm_tier"] = "INFO " + mv
	res.Pass("doctor", fmt.Sprintf("%d checks in %.0f ms, none failed: %s", len(doc.Checks), msOf(d), strings.Join(summary, " ")))

	if _, se, _, err := e.Twd("init", rWork+"/ai", "--template", "ai-agent", "--json"); err != nil {
		t.Fatalf("twd init --template ai-agent in the VM: %v %s", err, se)
	}
	res.Pass("init_ai_agent_offline", "twd init --template ai-agent in the VM (no npm); bundled offline by doctor")

	// Checks (a)–(f), with twd under `twd up --service`.
	e.Run()

	// The service: the unit exists, is active, and its main PID is the
	// supervisor's.
	unit := vhome + "/.config/systemd/user/twd.service"
	svcState := func() (active, pid string) {
		// key=value: the order of --value output differs across systemd versions.
		so, _, _ := v.run(`systemctl --user show -p ActiveState -p MainPID twd.service`, nil)
		for _, l := range strings.Split(so, "\n") {
			if x, ok := strings.CutPrefix(l, "ActiveState="); ok {
				active = strings.TrimSpace(x)
			}
			if x, ok := strings.CutPrefix(l, "MainPID="); ok {
				pid = strings.TrimSpace(x)
			}
		}
		return active, pid
	}
	sup := func() string {
		return strings.TrimSpace(v.must(`cat ~/.twd-e2e/state/supervisor.pid 2>/dev/null; true`))
	}
	if _, _, err := v.run("test -f "+shq(unit), nil); err != nil {
		t.Fatalf("twd up --service didn't write %s", unit)
	}
	act, pid1 := svcState()
	if act != "active" || pid1 == "" || pid1 != sup() {
		t.Fatalf("twd.service: ActiveState=%s MainPID=%s supervisor.pid=%s", act, pid1, sup())
	}
	linger := strings.TrimSpace(v.must(`loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null; true`))
	res.Pass("service", fmt.Sprintf("twd up --service: %s active, MainPID %s = supervisor (linger=%s)", unit, pid1, linger))

	// systemctl --user restart: a new supervisor, twd healthy again, and the
	// mcp workload answers this Mac over the tailnet again.
	if client == nil || mcpIP == "" {
		t.Fatal("no client / mcp IP from the run")
	}
	rs := time.Now()
	if _, se, rd, err := v.timed(R, "systemctl", "--user", "restart", "twd.service"); err != nil {
		t.Fatalf("systemctl --user restart twd: %v %s", err, se)
	} else {
		res.Numbers["service_restart_cmd_ms"] = msOf(rd)
	}
	var healthyAt time.Duration
	for time.Since(rs) < scale(3*time.Minute) {
		var stt struct {
			Healthy bool `json:"healthy"`
		}
		if so, _, _, err := e.Twd("status", "--json"); err == nil && json.Unmarshal([]byte(so), &stt) == nil && stt.Healthy {
			healthyAt = time.Since(rs)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if healthyAt == 0 {
		t.Fatalf("twd not healthy within %v of systemctl --user restart", scale(3*time.Minute))
	}
	act, pid2 := svcState()
	if act != "active" || pid2 == pid1 || pid2 != sup() {
		t.Fatalf("after restart: ActiveState=%s MainPID=%s (was %s) supervisor.pid=%s", act, pid2, pid1, sup())
	}
	hc := client.HTTPClient(10 * time.Second)
	var reachAt time.Duration
	for time.Since(rs) < scale(4*time.Minute) {
		if ms, ok := toolsList(hc, mcpIP); ok {
			reachAt = time.Since(rs)
			res.Numbers["service_restart_first_call_ms"] = ms
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if reachAt == 0 {
		t.Fatalf("mcp workload (%s) not reachable from this Mac within %v of the restart", mcpIP, scale(4*time.Minute))
	}
	res.Numbers["service_restart_healthy_ms"] = msOf(healthyAt)
	res.Numbers["service_restart_reachable_ms"] = msOf(reachAt)
	res.Pass("service_restart", fmt.Sprintf("systemctl --user restart twd: new MainPID %s, healthy after %.1f s, mcp workload answers this Mac (same tailnet IP) after %.1f s",
		pid2, healthyAt.Seconds(), reachAt.Seconds()))

	// Reboot survival: lingering on (sudo inside the VM only), then the VM
	// is stopped and started from this Mac. The user service must come
	// back by itself, every object must rejoin from saved state (same node
	// IDs; nothing re-minted), and the workload must answer this Mac again.
	if doReboot {
		user := strings.TrimSpace(v.must(`id -un`))
		if _, se, err := v.run(`sudo -n loginctl enable-linger "$(id -un)"`, nil); err != nil {
			t.Fatalf("enable-linger: %v %s", err, se)
		}
		if l := strings.TrimSpace(v.must(`loginctl show-user "$(id -un)" -p Linger --value`)); l != "yes" {
			t.Fatalf("linger after enable-linger: %q", l)
		}
		// Lines written from here on: tail -n +K is inclusive, so K = lines+1.
		hostdLines := strconv.Itoa(atoi(strings.TrimSpace(v.must(`wc -l < ~/.twd-e2e/state/logs/hostd.log`))) + 1)
		var ls struct {
			Objects []cliflow.Object `json:"objects"`
		}
		nodeOf := func(o cliflow.Object) string {
			if o.StableNodeID != "" {
				return o.StableNodeID
			}
			return o.NodeID
		}
		e.MustJSON(&ls, "ls")
		beforeNodes := map[string]string{}
		for _, o := range ls.Objects {
			if o.Status == "ready" {
				beforeNodes[o.ID] = nodeOf(o)
			}
		}
		if len(beforeNodes) == 0 {
			t.Fatalf("no ready objects before the reboot: %+v", ls.Objects)
		}
		_, pidBefore := svcState()
		ts := time.Now()
		if out, err := exec.Command("limactl", "stop", vmName).CombinedOutput(); err != nil {
			t.Fatalf("limactl stop: %v %s", err, out)
		}
		stopDur := time.Since(ts)
		t0 := time.Now()
		if out, err := exec.Command("limactl", "start", "--tty=false", vmName).CombinedOutput(); err != nil {
			t.Fatalf("limactl start: %v %s", err, out)
		}
		sshAt := time.Since(t0)
		var activeAt, healthyAt, servingAt time.Duration
		var pidAfter string
		for time.Since(t0) < scale(5*time.Minute) {
			if a, p := svcState(); a == "active" && p != "" && p != "0" {
				activeAt, pidAfter = time.Since(t0), p
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if activeAt == 0 {
			so, _, _ := v.run(`systemctl --user status twd.service 2>&1 | tail -15; loginctl list-users 2>&1`, nil)
			t.Fatalf("twd.service not active within 5 min of boot:\n%s", so)
		}
		for time.Since(t0) < scale(5*time.Minute) {
			var stt struct {
				Healthy bool `json:"healthy"`
			}
			if so, _, _, err := e.Twd("status", "--json"); err == nil && json.Unmarshal([]byte(so), &stt) == nil && stt.Healthy {
				healthyAt = time.Since(t0)
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if healthyAt == 0 {
			t.Fatal("twd not healthy within 5 min of boot")
		}
		for time.Since(t0) < scale(6*time.Minute) {
			if ms, ok := toolsList(hc, mcpIP); ok {
				servingAt = time.Since(t0)
				res.Numbers["reboot_first_call_ms"] = ms
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if servingAt == 0 {
			t.Fatalf("mcp workload (%s) not answering this Mac within 6 min of boot", mcpIP)
		}
		// Every object rejoined from saved state: same node IDs, none re-minted.
		deadline := time.Now().Add(scale(3 * time.Minute))
		for {
			e.MustJSON(&ls, "ls")
			ready := 0
			for _, o := range ls.Objects {
				if want, ok := beforeNodes[o.ID]; ok && o.Status == "ready" && nodeOf(o) == want {
					ready++
				}
			}
			if ready == len(beforeNodes) || time.Now().After(deadline) {
				if ready != len(beforeNodes) {
					t.Fatalf("after reboot only %d/%d objects are ready with their old node IDs: %+v", ready, len(beforeNodes), ls.Objects)
				}
				break
			}
			time.Sleep(time.Second)
		}
		joins := strings.TrimSpace(v.must(`tail -n +` + hostdLines + ` ~/.twd-e2e/state/logs/hostd.log | grep -c 'object node ready.*rejoined=true reminted=false' || true`))
		reminted := strings.TrimSpace(v.must(`tail -n +` + hostdLines + ` ~/.twd-e2e/state/logs/hostd.log | grep -c 'reminted=true' || true`))
		if joins != strconv.Itoa(len(beforeNodes)) || reminted != "0" {
			t.Fatalf("hostd log after reboot: %s rejoins (want %d), %s reminted", joins, len(beforeNodes), reminted)
		}
		res.Numbers["reboot_stop_ms"] = msOf(stopDur)
		res.Numbers["reboot_ssh_ms"] = msOf(sshAt)
		res.Numbers["reboot_service_active_ms"] = msOf(activeAt)
		res.Numbers["reboot_healthy_ms"] = msOf(healthyAt)
		res.Numbers["reboot_serving_ms"] = msOf(servingAt)
		res.Pass("reboot", fmt.Sprintf("enable-linger %s, limactl stop (%.1f s) + start: ssh %.1f s, twd.service active %.1f s (MainPID %s → %s), healthy %.1f s, mcp workload answers this Mac %.1f s; %d objects rejoined with their old node IDs, 0 re-minted",
			user, stopDur.Seconds(), sshAt.Seconds(), activeAt.Seconds(), pidBefore, pidAfter, healthyAt.Seconds(), servingAt.Seconds(), len(beforeNodes)))
	}

	// doctor while twd runs: no twd process holds tailscaled's socket; the
	// workloads have no sandbox yet (L2).
	so, se, _, err = e.Twd("doctor", "--json")
	if err != nil || json.Unmarshal([]byte(so), &doc) != nil || !doc.OK {
		t.Fatalf("twd doctor (running): %v\n%s\n%s", err, so, netd.Redact(se))
	}
	if st, d := doc.get("system_tailscaled_not_used"); st != "ok" || !strings.Contains(d, "hold no tailscaled LocalAPI socket") {
		t.Fatalf("doctor (running): system_tailscaled_not_used %s %s", st, d)
	} else {
		res.Pass("system_tailscaled_not_used", d)
	}
	if st, d := doc.get("systemd"); !strings.Contains(d, "twd.service installed (active)") {
		t.Fatalf("doctor (running): systemd %s %s", st, d)
	}
	var sbst struct {
		Workloads []struct{ Workload, Tier, Sandbox string }
	}
	e.MustJSON(&sbst, "status")
	var sbs []string
	for _, w := range sbst.Workloads {
		sbs = append(sbs, w.Workload+":"+w.Tier+"/"+orNone(w.Sandbox))
	}
	res.Checks["sandbox"] = "INFO sandbox: none (L2 pending); workloads " + strings.Join(sbs, " ")

	// Persistent nodes: purge (kill every object, delete its device), and
	// remove the unit, in one `twd down`.
	var dr struct {
		Killed         []string `json:"killed"`
		Flushed        int      `json:"devicesDeleted"`
		ServiceRemoved bool     `json:"serviceRemoved"`
	}
	dd := e.MustJSON(&dr, "down", "--purge", "--service")
	res.Numbers["down_ms"] = msOf(dd)
	if !dr.ServiceRemoved {
		t.Fatal("twd down --purge --service: serviceRemoved false")
	}
	res.Pass("down", fmt.Sprintf("twd down --purge --service: %d objects purged, %d devices deleted, %v", len(dr.Killed), dr.Flushed, dd.Round(time.Millisecond)))
	if _, _, err := v.run("test -e "+shq(unit), nil); err == nil {
		t.Fatalf("twd down --service left %s", unit)
	}
	if so, _, _ := v.run(`systemctl --user is-enabled twd.service 2>&1; true`, nil); strings.TrimSpace(so) == "enabled" {
		t.Fatal("twd.service still enabled after twd down --service")
	}
	res.Pass("service_removed", "twd down --purge --service: unit disabled and removed")
	downDone = true

	// Ephemeral nodes across a service restart. A second `twd up` on the
	// same state, now --ephemeral: the agent plus a spawned sub-agent, then
	// `systemctl --user restart twd`. Control removes ephemeral devices that
	// go offline, so their saved state is rejected (NeedsLogin): hostd must
	// re-mint under the same hostname and both must answer this Mac again
	// with new node IDs. (Before this fix, they hung in tsnet.Up and failed.)
	if doEphemeral {
		downDone = false
		// The SAME state dir (and app dir): `twd down --purge` forgets the
		// killed workload-node objects (mgmt rows + revocations, hostd
		// tombstones), so `twd up && twd deploy` works again here. Before
		// that fix this phase needed a fresh state dir (bootstrap_failed
		// "the workload node for agent is killed").
		ephLog := "~/.twd-e2e/state/logs/hostd.log"
		e.MustJSON(nil, "up", "--tailnet", tailnet, "--api-key-file", rKey, "--yes", "--ephemeral",
			"--hostname-prefix", twdPrefix, "--control-url", prodControl, "--service")
		var dep struct {
			Endpoint struct{ MagicDNS string } `json:"endpoint"`
		}
		e.MustJSON(&dep, "deploy", rWork+"/agent")
		agentIP := cliflow.PeerIP(t, client, dep.Endpoint.MagicDNS, scale(90*time.Second))
		getJSON := func(u string) (int, map[string]any) {
			resp, err := hc.Get(u)
			if err != nil {
				return 0, nil
			}
			defer resp.Body.Close()
			var m map[string]any
			b, _ := io.ReadAll(resp.Body)
			json.Unmarshal(b, &m)
			return resp.StatusCode, m
		}
		if c, _ := getJSON("http://" + agentIP + "/?agent=e1"); c != 200 {
			t.Fatalf("ephemeral: agent e1 via workload: %d", c)
		}
		parent := e.WaitObjectReady("SupportAgent", "e1", scale(90*time.Second))
		parentIP := cliflow.PeerIP(t, client, parent.MagicDNS, scale(60*time.Second))
		if c, b := getJSON("http://" + parentIP + "/spawn?child=e2"); c != 200 || fmt.Sprint(b["child"].(map[string]any)["reply"].(map[string]any)["lookup"]) != "e2" {
			t.Fatalf("ephemeral: spawn: %d %v", c, b)
		}
		child := e.WaitObjectReady("LookupAgent", "e2", scale(90*time.Second))
		childIP := cliflow.PeerIP(t, client, child.MagicDNS, scale(60*time.Second))
		if c, b := getJSON("http://" + childIP + "/"); c != 200 || b["lookup"] != "e2" {
			t.Fatalf("ephemeral: child via its own node: %d %v", c, b)
		}
		e.TrackObjectNodes()
		// Lines written from here on: tail -n +K is inclusive, so K = lines+1.
		hostdLines := strconv.Itoa(atoi(strings.TrimSpace(v.must(`wc -l < `+ephLog+``))) + 1)
		if _, se, _, err := v.timed(R, "systemctl", "--user", "restart", "twd.service"); err != nil {
			t.Fatalf("ephemeral: systemctl --user restart twd: %v %s", err, se)
		}
		t0 := time.Now()
		var child2 cliflow.Object
		var readyAt time.Duration
		for time.Since(t0) < scale(4*time.Minute) {
			if o, ok := e.FindObject("LookupAgent", "e2"); ok && o.Status == "ready" && o.StableNodeID != "" && o.StableNodeID != child.StableNodeID {
				child2, readyAt = o, time.Since(t0)
				break
			}
			time.Sleep(time.Second)
		}
		if readyAt == 0 {
			o, _ := e.FindObject("LookupAgent", "e2")
			t.Fatalf("ephemeral: sub-agent not ready with a new node ID within 4 min of the restart: %+v (was node %s)", o, child.StableNodeID)
		}
		if child2.MagicDNS != child.MagicDNS || child2.Hostname != child.Hostname {
			t.Fatalf("ephemeral: sub-agent came back as %s / %s, want the same hostname %s / %s", child2.Hostname, child2.MagicDNS, child.Hostname, child.MagicDNS)
		}
		// Its predecessor's netmap entry may linger briefly: try every IP
		// the client has for that name until one answers.
		var answerAt time.Duration
		var childIP2 string
		for time.Since(t0) < scale(5*time.Minute) && answerAt == 0 {
			for _, ip := range cliflow.PeerIPs(client, child.MagicDNS) {
				if c, b := getJSON("http://" + ip + "/"); c == 200 && b["lookup"] == "e2" {
					answerAt, childIP2 = time.Since(t0), ip
					break
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
		if answerAt == 0 {
			t.Fatalf("ephemeral: sub-agent %s not answering this Mac within 5 min of the restart", child.MagicDNS)
		}
		reminted := strings.TrimSpace(v.must(`tail -n +` + hostdLines + ` ` + ephLog + ` | grep -c 'object node ready.*reminted=true' || true`))
		childReminted := strings.TrimSpace(v.must(`tail -n +` + hostdLines + ` ` + ephLog + ` | grep -c 'object node ready.*object=` + child.ID + ` .*reminted=true' || true`))
		rejected := strings.TrimSpace(v.must(`tail -n +` + hostdLines + ` ` + ephLog + ` | grep -c 'saved node state rejected by control' || true`))
		if childReminted != "1" {
			t.Fatalf("ephemeral: hostd log has %s 'reminted=true' lines for the sub-agent %s (total reminted %s, rejected %s)", childReminted, child.ID, reminted, rejected)
		}
		e.TrackObjectNodes()
		res.Numbers["eph_restart_child_ready_ms"] = msOf(readyAt)
		res.Numbers["eph_restart_child_answer_ms"] = msOf(answerAt)
		res.Pass("ephemeral_remint_after_restart", fmt.Sprintf("--ephemeral: sub-agent %s (node %s, %s) across systemctl --user restart: saved state rejected (%s netd rejections, %s nodes re-minted), back as the same hostname with node %s after %.1f s, answering this Mac (%s → %s) after %.1f s",
			child.ID, child.StableNodeID, child.MagicDNS, rejected, reminted, child2.StableNodeID, readyAt.Seconds(), childIP, childIP2, answerAt.Seconds()))
		var dr2 struct {
			Killed         []string `json:"killed"`
			Flushed        int      `json:"devicesDeleted"`
			ServiceRemoved bool     `json:"serviceRemoved"`
		}
		e.MustJSON(&dr2, "down", "--purge", "--service")
		if !dr2.ServiceRemoved {
			t.Fatal("ephemeral: twd down --purge --service: serviceRemoved false")
		}
		res.Pass("ephemeral_down", fmt.Sprintf("%d objects purged, %d devices deleted, unit removed", len(dr2.Killed), dr2.Flushed))
		downDone = true
	}
	time.Sleep(2 * time.Second)
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func toolsList(hc *http.Client, ip string) (float64, bool) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	req, _ := http.NewRequest("POST", "http://"+ip+"/mcp", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json, text/event-stream")
	req.Header.Set("mcp-protocol-version", "2025-06-18")
	st := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return 0, false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return msOf(time.Since(st)), resp.StatusCode == 200
}

// peerOS returns the OS the peer with ip reports, and its endpoint kinds.
func peerOS(c *nettest.Node, ip string) (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := c.LC.Status(ctx)
	if err != nil {
		return "", ""
	}
	a := netip.MustParseAddr(ip)
	for _, p := range st.Peer {
		if slices.Contains(p.TailscaleIPs, a) {
			cur := p.CurAddr
			if cur == "" {
				cur = "none (DERP " + p.Relay + ")"
			} else {
				cur = redactEndpoint(cur)
			}
			return p.OS, "current path " + cur
		}
	}
	return "", ""
}

// measurePath: this Mac → the VM over the tailnet: disco pings (DERP or
// direct) and MCP tools/list requests, until a direct path is up (or 60
// rounds), then steady state.
func measurePath(t testing.TB, res *cliflow.Results, client *nettest.Node, hc *http.Client, mcpIP string) {
	ip := netip.MustParseAddr(mcpIP)
	var derpPing, directPing, derpHTTP, directHTTP []float64
	var region, endpoint string
	start := time.Now()
	var toDirect time.Duration
	direct := func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		st, err := client.LC.Status(ctx)
		if err != nil {
			return false
		}
		for _, p := range st.Peer {
			if slices.Contains(p.TailscaleIPs, ip) {
				return p.CurAddr != ""
			}
		}
		return false
	}
	for i := 0; i < 60 && (len(directPing) < 10 || len(directHTTP) < 10); i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pr, err := client.LC.Ping(ctx, ip, tailcfg.PingDisco)
		cancel()
		if err == nil && pr.Err == "" {
			lat := pr.LatencySeconds * 1000
			if pr.Endpoint != "" {
				directPing = append(directPing, lat)
				endpoint = pr.Endpoint
				if toDirect == 0 {
					toDirect = time.Since(start)
				}
			} else {
				derpPing = append(derpPing, lat)
				region = pr.DERPRegionCode
			}
		}
		isDirect := direct()
		if ms, ok := toolsList(hc, mcpIP); ok {
			if isDirect {
				directHTTP = append(directHTTP, ms)
			} else {
				derpHTTP = append(derpHTTP, ms)
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	res.Numbers["path_derp_ping_ms"] = median(derpPing)
	res.Numbers["path_direct_ping_ms"] = median(directPing)
	res.Numbers["path_derp_http_ms"] = median(derpHTTP)
	res.Numbers["path_direct_http_ms"] = median(directHTTP)
	res.Numbers["path_time_to_direct_ms"] = msOf(toDirect)
	res.Numbers["path_direct"] = map[bool]float64{true: 1, false: 0}[len(directPing) > 0]
	res.Notes = append(res.Notes, fmt.Sprintf("path this Mac → VM mcp node: %d DERP pings (region %q, median %.1f ms), %d direct (%s, median %.1f ms) after %v; HTTP tools/list median DERP %.1f ms (n=%d), direct %.1f ms (n=%d)",
		len(derpPing), region, median(derpPing), len(directPing), redactEndpoint(endpoint), median(directPing), toDirect.Round(time.Millisecond),
		median(derpHTTP), len(derpHTTP), median(directHTTP), len(directHTTP)))
	if len(directHTTP)+len(derpHTTP) == 0 {
		t.Fatalf("no HTTP request to the mcp node (%s) succeeded", mcpIP)
	}
}

func localHost() string { h, _ := os.Hostname(); h, _, _ = strings.Cut(h, "."); return h }

type probeOut struct {
	NodeID  string    `json:"nodeID"`
	Ping    []float64 `json:"pingMs"`
	HTTP    []float64 `json:"httpMs"`
	Regions []string  `json:"regions"`
	Direct  int       `json:"directPongs"`
	Err     string    `json:"err,omitempty"`
}

// derpProbe re-runs this test binary as TestDERPProbe with DERP forced
// (TS_DEBUG_ALWAYS_USE_DERP is process-wide).
func derpProbe(t testing.TB, res *cliflow.Results, tracker *tsapi.Tracker, key, hostname, target, work string) {
	dir := filepath.Join(work, "derp")
	os.MkdirAll(dir, 0o700)
	kf, out := filepath.Join(dir, "key"), filepath.Join(dir, "out.json")
	if err := os.WriteFile(kf, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TWD_E2E_") {
			env = append(env, kv)
		}
	}
	env = append(env, "TS_DEBUG_ALWAYS_USE_DERP=1", "TWD_PROBE_KEY_FILE="+kf, "TWD_PROBE_HOSTNAME="+hostname,
		"TWD_PROBE_TARGET="+target, "TWD_PROBE_OUT="+out, "TWD_PROBE_DIR="+dir)
	cmd := exec.Command(os.Args[0], "-test.run=^TestDERPProbe$", "-test.count=1", "-test.timeout=4m")
	cmd.Env = env
	b, err := cmd.CombinedOutput()
	os.Remove(kf)
	var po probeOut
	jb, _ := os.ReadFile(out)
	json.Unmarshal(jb, &po)
	if po.NodeID != "" {
		tracker.Track(po.NodeID, "derp")
	}
	if err != nil || po.Err != "" || len(po.HTTP) == 0 {
		t.Fatalf("derp probe: %v %s\n%s", err, po.Err, netd.Redact(string(b)))
	}
	res.Numbers["path_derp_ping_ms"] = median(po.Ping)
	res.Numbers["path_derp_http_ms"] = median(po.HTTP)
	res.Notes = append(res.Notes, fmt.Sprintf("DERP-only client (TS_DEBUG_ALWAYS_USE_DERP) → VM mcp node: disco ping median %.1f ms (n=%d, regions %v, %d direct pongs), HTTP tools/list median %.1f ms (n=%d)",
		median(po.Ping), len(po.Ping), slices.Compact(po.Regions), po.Direct, median(po.HTTP), len(po.HTTP)))
}

// TestDERPProbe is derpProbe's child process (skipped otherwise).
func TestDERPProbe(t *testing.T) {
	kf := os.Getenv("TWD_PROBE_KEY_FILE")
	if kf == "" {
		t.Skip("child of TestE2ELinux only")
	}
	var po probeOut
	defer func() {
		b, _ := json.Marshal(po)
		os.WriteFile(os.Getenv("TWD_PROBE_OUT"), b, 0o600)
	}()
	key, err := os.ReadFile(kf)
	if err != nil {
		po.Err = err.Error()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	n, err := nettest.NewNode(ctx, prodControl, strings.TrimSpace(string(key)), os.Getenv("TWD_PROBE_HOSTNAME"), os.Getenv("TWD_PROBE_DIR"))
	if err != nil {
		po.Err = netd.Redact(err.Error())
		return
	}
	defer n.Close()
	if st, err := n.LC.StatusWithoutPeers(ctx); err == nil && st.Self != nil {
		po.NodeID = string(st.Self.ID)
		b, _ := json.Marshal(po)
		os.WriteFile(os.Getenv("TWD_PROBE_OUT"), b, 0o600)
	}
	target := os.Getenv("TWD_PROBE_TARGET")
	if err := n.WaitPeers(ctx, target); err != nil {
		po.Err = "netmap: " + err.Error()
		return
	}
	ip := netip.MustParseAddr(target)
	hc := n.HTTPClient(20 * time.Second)
	for i := 0; i < 12; i++ {
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
		pr, err := n.LC.Ping(pctx, ip, tailcfg.PingDisco)
		pcancel()
		if err == nil && pr.Err == "" && i > 0 {
			po.Ping = append(po.Ping, pr.LatencySeconds*1000)
			po.Regions = append(po.Regions, pr.DERPRegionCode)
			if pr.Endpoint != "" {
				po.Direct++
			}
		}
		if ms, ok := toolsList(hc, target); ok && i > 0 {
			po.HTTP = append(po.HTTP, ms)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func redactEndpoint(ep string) string {
	ap, err := netip.ParseAddrPort(ep)
	if err != nil {
		return "direct"
	}
	if ap.Addr().IsPrivate() {
		return "direct, private addr"
	}
	return "direct, public addr"
}
