//go:build e2elinuxvm

// E2E-LINUX-VM (L3): the microVM tier from the installed release on a
// fresh Ubuntu 24.04 arm64 nested-virt Lima VM (twdl-kvm-u24). From this
// Mac: copy the linux release tarball and the tailnet-1 API key file into
// the VM with `limactl copy`; there run install.sh, `twd doctor` (microVM:
// not set up), `sudo twd host setup --microvm` (the only sudo), doctor
// again (microVM ok), then as the normal user `twd up` and the agent + mcp
// templates with "tier": "microvm"; and run the tag:ttest client and the
// private echo here, so checks (a)–(f) cross machines over tailnet 1. Then
// L3's own checks: both workloads are jailed microVMs; DO storage survives
// a SIGKILL of the agent's VMM and a redeploy; no tailnet or volume key in
// either guest (disks, files, full RAM); latency, RSS, boot, fsync; then
// `twd down`, `sudo twd host setup --undo` (everything gone), uninstall,
// zero leftover devices and keys by ID. The key file is removed in
// t.Cleanup and by the script's trap.
//
// Safety: tailnet 1 only (allow list), no policy writes, keys never
// printed; the VM is the test's own (created by scripts/e2e-linux-vm.sh).
package linuxvm

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
)

const prodControl = "https://controlplane.tailscale.com"

var allowedTailnets = map[string]bool{"tail7906d5.ts.net": true}

// remote runs commands in the Lima VM over its ssh config (Lima keeps a
// multiplexed master connection).
type remote struct {
	t       testing.TB
	vm, cfg string
}

func (r *remote) run(cmd string, stdin io.Reader) (string, string, error) {
	c := exec.Command("ssh", "-F", r.cfg, "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "lima-"+r.vm, cmd)
	var so, se bytes.Buffer
	c.Stdout, c.Stderr, c.Stdin = &so, &se, stdin
	err := c.Run()
	return so.String(), se.String(), err
}

func (r *remote) must(cmd string) string {
	r.t.Helper()
	so, se, err := r.run(cmd, nil)
	if err != nil {
		r.t.Fatalf("%s: %s: %v\n%s\n%s", r.vm, cmd, err, netd.Redact(so), netd.Redact(se))
	}
	return so
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var wallRE = regexp.MustCompile(`\n?__twd_wall_us=([0-9]+)\n?$`)

// timed runs argv in dir on the VM and reports the VM's own wall time.
func (r *remote) timed(dir string, argv ...string) (string, string, time.Duration, error) {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shq(a)
	}
	cmd := "cd " + shq(dir) + " 2>/dev/null || cd; s=$(date +%s%N); " + strings.Join(q, " ") + "; rc=$?; e=$(date +%s%N); printf '\\n__twd_wall_us=%d\\n' $(( (e-s)/1000 )) >&2; exit $rc"
	st := time.Now()
	so, se, err := r.run(cmd, nil)
	d := time.Since(st)
	if m := wallRE.FindStringSubmatch(se); m != nil {
		us, _ := strconv.ParseInt(m[1], 10, 64)
		d = time.Duration(us) * time.Microsecond
		se = se[:len(se)-len(m[0])]
	}
	return so, se, d, err
}

func msOf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func pct(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	sort.Float64s(s)
	return s[int(float64(len(s)-1)*q)]
}

type wlStatus struct {
	Workload, Tier, DOStorage string
	Epoch                     int64
	VMRSSKB                   int64
	VM                        *struct {
		PID                  int
		Boot, Ready, Total   time.Duration
		HugePages, VolumeNew bool
		Volume               string
		VolumeAttachMS       float64
		Jail                 *struct {
			ID, Chroot, Cgroup, CPUMax, Seccomp string
			UID                                 int
			MemMax                              int64
		}
	}
}

func TestE2ELinuxVM(t *testing.T) {
	keyFile, tailnet, vm, tarball := os.Getenv("TWD_E2E_API_KEY_FILE"), os.Getenv("TWD_E2E_TAILNET"), os.Getenv("TWD_E2E_LIMA_VM"), os.Getenv("TWD_E2E_TARBALL")
	if keyFile == "" || tailnet == "" || vm == "" || tarball == "" {
		t.Skip("TWD_E2E_API_KEY_FILE / TWD_E2E_TAILNET / TWD_E2E_LIMA_VM / TWD_E2E_TARBALL not set (make e2e-linux-vm)")
	}
	if !allowedTailnets[tailnet] {
		t.Fatalf("refusing to run against tailnet %q (not in the allow list)", tailnet)
	}
	apiKey, err := tsapi.LoadEnvFile(keyFile, "TS_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	api := &tsapi.Client{Tailnet: tailnet, APIKey: apiKey}
	b := make([]byte, 3)
	rand.Read(b)
	run := hex.EncodeToString(b)
	testPrefix := "ttest-l3-" + run + "-" // this Mac's client + echo
	twdPrefix := "twd-l3-" + run + "-"    // every node twd creates in the VM
	root, _ := nettest.FindRepoRoot()
	res := cliflow.NewResults("l3-linuxvm-"+run, tailnet)
	res.Notes = append(res.Notes, "twd in Lima "+vm+" from "+filepath.Base(tarball)+"; test client + echo on this Mac")
	resultsTo := filepath.Join(root, "e2e", "real", "results", "l3-linuxvm-"+time.Now().Format("20060102-150405")+"-"+run+".json")
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
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
	r := &remote{t: t, vm: vm, cfg: filepath.Join(home, ".lima", vm, "ssh.config")}
	rhome := strings.TrimSpace(r.must(`printf %s "$HOME"`))
	if rhome == "" || !strings.HasPrefix(rhome, "/") {
		t.Fatalf("%s $HOME = %q", vm, rhome)
	}
	R := rhome + "/.twd-e2e"
	rKey, rState, rWork := R+"/tailnet1_api.env", R+"/state", R+"/work"
	rTwd := rhome + "/.twd/bin/twd"
	if so := r.must(`[ -e ~/.twd ] && echo twd; [ -e ~/.twd-e2e ] && echo e2e; [ -e /etc/twd ] && echo etc-twd; systemctl cat twd-vmhelper >/dev/null 2>&1 && echo unit; true`); strings.TrimSpace(so) != "" {
		t.Fatalf("%s is not clean (%s)", vm, strings.TrimSpace(so))
	}
	res.Notes = append(res.Notes, vm+": "+strings.Join(strings.Fields(r.must(`. /etc/os-release; echo $PRETTY_NAME; uname -rm; nproc; free -m | awk '/Mem:/{print $2 "MiB"}'; ls -l /dev/kvm | cut -c1-10; cryptsetup --version; command -v go node npm || echo no-go-node-npm`)), " "))

	quiet := slog.New(netd.NewRedactingHandler(slog.NewTextHandler(io.Discard, nil)))
	minter := tsapi.MintSource(api, quiet)
	tracker := tsapi.NewTracker(api)
	work := t.TempDir()
	var nodes []*nettest.Node
	downDone, setupDone := false, false
	var agentDir string

	e := &cliflow.Env{T: t, BinDir: "(" + vm + ")", StateDir: rState, WorkDir: rWork, Tailnet: tailnet, APIKeyFile: rKey,
		ControlURL: prodControl, Prefix: twdPrefix, Res: res, ClientTagged: true, Tier: "microvm",
		Exec: func(args []string) (string, string, time.Duration, error) {
			return r.timed(rWork, append([]string{rTwd}, args...)...)
		},
		PatchFile: func(path string, edit func([]byte) []byte) error {
			so, se, err := r.run("cat "+shq(path), nil)
			if err != nil {
				return fmt.Errorf("%v %s", err, se)
			}
			_, se, err = r.run("cat > "+shq(path), bytes.NewReader(edit([]byte(so))))
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
	agentDir = filepath.Join(rWork, "agent")
	status := func() map[string]wlStatus {
		var st struct{ Workloads []wlStatus }
		e.MustJSON(&st, "status")
		m := map[string]wlStatus{}
		for _, w := range st.Workloads {
			m[w.Workload] = w
		}
		return m
	}
	e.OnDeployed = func() {
		st := status()
		for _, w := range []string{"agent", "mcp"} {
			ws := st[w]
			if ws.Tier != "microvm" || ws.VM == nil || ws.VM.Jail == nil || ws.VM.Jail.UID < 1000 || !strings.HasPrefix(ws.DOStorage, "volume:") {
				t.Fatalf("%s: not a jailed microVM with a volume: tier %q DOStorage %q VM %+v", w, ws.Tier, ws.DOStorage, ws.VM)
			}
			res.Numbers[w+"_vm_instance_start_to_ready_ms"] = msOf(ws.VM.Ready)
			res.Numbers[w+"_vm_boot_total_ms"] = msOf(ws.VM.Total)
			res.Numbers[w+"_vm_volume_attach_ms"] = ws.VM.VolumeAttachMS
			res.Numbers[w+"_vm_rss_mb"] = float64(ws.VMRSSKB) / 1024
			res.Notes = append(res.Notes, fmt.Sprintf("%s: microVM pid %d, jail %s uid %d, cgroup %s (cpu.max %s, memory.max %d), seccomp %s, hugepages %v, volume %s (new %v, attach %.1f ms)",
				w, ws.VM.PID, ws.VM.Jail.ID, ws.VM.Jail.UID, ws.VM.Jail.Cgroup, ws.VM.Jail.CPUMax, ws.VM.Jail.MemMax, ws.VM.Jail.Seccomp, ws.VM.HugePages, ws.VM.Volume, ws.VM.VolumeNew, ws.VM.VolumeAttachMS))
			// The VMM really runs as the jail uid, under seccomp (procfs).
			pst := r.must(fmt.Sprintf(`grep -E '^(Uid|Seccomp):' /proc/%d/status`, ws.VM.PID))
			if !strings.Contains(pst, fmt.Sprint(ws.VM.Jail.UID)) || !regexp.MustCompile(`Seccomp:\s+2`).MatchString(pst) {
				t.Fatalf("%s VMM: %s", w, pst)
			}
		}
		res.Pass("microvm_jailed", "agent and mcp run in Firecracker microVMs through the jailer (own uid/gid, chroot, cgroup cpu.max/memory.max, seccomp filter) with a persistent encrypted DO volume, launched by twd-vmhelper for the unprivileged twd")
	}
	var hc *http.Client
	var agentIP string
	e.OnPeers = func(client *nettest.Node, h *http.Client, aIP, mIP string) {
		hc, agentIP = h, aIP
		// Latency, this Mac → the VM over tailnet 1: MCP tools/list to the
		// mcp workload (in its microVM), after warm-up.
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
		var lat []float64
		for i := 0; i < 120; i++ {
			req, _ := http.NewRequest("POST", "http://"+mIP+"/mcp", bytes.NewReader(body))
			req.Header.Set("content-type", "application/json")
			req.Header.Set("accept", "application/json, text/event-stream")
			req.Header.Set("mcp-protocol-version", "2025-06-18")
			st := time.Now()
			resp, err := h.Do(req)
			if err != nil {
				continue
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && i >= 20 {
				lat = append(lat, msOf(time.Since(st)))
			}
		}
		if len(lat) < 50 {
			t.Fatalf("mcp latency: only %d good requests", len(lat))
		}
		res.Numbers["mcp_tools_list_p50_ms"], res.Numbers["mcp_tools_list_p95_ms"] = pct(lat, 0.5), pct(lat, 0.95)
		res.Notes = append(res.Notes, fmt.Sprintf("MCP tools/list, this Mac → mcp microVM over tailnet 1: p50 %.2f ms, p95 %.2f ms (n=%d)", pct(lat, 0.5), pct(lat, 0.95), len(lat)))
	}

	t.Cleanup(func() {
		if !downDone {
			if _, se, _, err := e.Twd("down", "--purge"); err != nil {
				t.Logf("twd down (cleanup): %v %s", err, netd.Redact(se))
			}
		}
		if t.Failed() {
			so, _, _ := r.run(`for c in supervisor mgmt hostd; do echo "== $c"; tail -c 6000 ~/.twd-e2e/state/logs/$c.log 2>/dev/null; done; echo "== helper"; sudo journalctl -u twd-vmhelper -n 40 --no-pager 2>/dev/null`, nil)
			t.Logf("%s logs (tail):\n%s", vm, netd.Redact(so))
		}
		for _, n := range nodes {
			n.Close()
		}
		// The VM: undo the setup (if the test didn't), uninstall, remove
		// ~/.twd-e2e and with it the key file; check it's all gone.
		so, se, err := r.run(`pkill -TERM -f "$HOME/.twd/bin/" 2>/dev/null; sleep 0.5; pkill -KILL -f "$HOME/.twd/bin/" 2>/dev/null
			if [ -e /etc/twd ] && [ -x ~/.twd/bin/twd ]; then sudo ~/.twd/bin/twd host setup --undo >/dev/null; fi
			u=$(ls -d ~/.twd-e2e/twd-*/install.sh 2>/dev/null | head -1)
			if [ -n "$u" ]; then "$u" --uninstall --purge --force >/dev/null; fi
			rm -rf ~/.twd-e2e
			for p in ~/.twd ~/.twd-e2e /etc/twd /usr/local/libexec/twd /var/lib/twd-vm /etc/systemd/system/twd-vmhelper.service /run/twd/vmhelper.sock; do [ -e "$p" ] && echo "LEFT $p"; done
			pgrep -fl "$HOME/.twd/" && echo LEFT processes; true`, nil)
		if err != nil || strings.Contains(so, "LEFT") {
			t.Errorf("%s cleanup: %v %s %s", vm, err, netd.Redact(so), netd.Redact(se))
			res.Cleanup["vm_clean"] = false
		} else {
			res.Cleanup["vm_clean"] = true
		}
		res.Cleanup["setup_undone_by_test"] = setupDone
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

	// 1. Copy the tarball and the key file in with limactl copy (as a user
	// would get them); the key file is 0600 and only ever in ~/.twd-e2e.
	r.must(`umask 077; mkdir -p ~/.twd-e2e/work && chmod 700 ~/.twd-e2e`)
	st := time.Now()
	if out, err := exec.Command("limactl", "copy", tarball, vm+":"+R+"/").CombinedOutput(); err != nil {
		t.Fatalf("limactl copy tarball: %v %s", err, out)
	}
	res.Numbers["copy_tarball_ms"] = msOf(time.Since(st))
	if out, err := exec.Command("limactl", "copy", keyFile, vm+":"+rKey).CombinedOutput(); err != nil {
		t.Fatalf("limactl copy key file: %v %s", err, netd.Redact(string(out)))
	}
	r.must(`chmod 600 ~/.twd-e2e/tailnet1_api.env`)
	if m := strings.TrimSpace(r.must(`stat -c %a ~/.twd-e2e/tailnet1_api.env`)); m != "600" {
		t.Fatalf("key file mode in the VM: %s", m)
	}
	res.Checks["copy"] = "PASS tarball + key file (0600) in ~/.twd-e2e via limactl copy"

	// 2. install.sh (no sudo)
	tgz := filepath.Base(tarball)
	dirName := strings.TrimSuffix(tgz, ".tar.gz")
	if _, se, _, err := r.timed(R, "tar", "-xzf", tgz); err != nil {
		t.Fatalf("extract: %v %s", err, se)
	}
	so, se, d, err := r.timed(R, R+"/"+dirName+"/install.sh")
	if err != nil {
		t.Fatalf("install.sh: %v\n%s\n%s", err, so, se)
	}
	res.Numbers["install_ms"] = msOf(d)
	ver := strings.TrimSpace(r.must(`cat ~/.twd/VERSION; ls ~/.twd/share/twd/vm; ~/.twd/share/twd/vm/firecracker --version | head -1`))
	res.Pass("install", fmt.Sprintf("install.sh %.1f s, no sudo: %s", msOf(d)/1000, strings.Join(strings.Fields(ver), " ")))

	// 3. doctor before setup: the microVM tier is off, and says how to fix it.
	type docT struct {
		OK     bool                                         `json:"ok"`
		Checks []struct{ Name, Status, Detail, Fix string } `json:"checks"`
	}
	doctor := func(args ...string) docT {
		var doc docT
		so, se, _, err := e.Twd(append([]string{"doctor", "--json"}, args...)...)
		if json.Unmarshal([]byte(so), &doc) != nil {
			t.Fatalf("twd doctor: %v\n%s\n%s", err, so, netd.Redact(se))
		}
		return doc
	}
	checkOf := func(doc docT, name string) (string, string, string) {
		for _, c := range doc.Checks {
			if c.Name == name {
				return c.Status, c.Detail, c.Fix
			}
		}
		return "", "", ""
	}
	doc := doctor("--tailnet", tailnet, "--api-key-file", rKey)
	if s, dd, fx := checkOf(doc, "microvm"); s != "warn" || !strings.Contains(dd, "not installed") || !strings.Contains(fx, "host setup --microvm") {
		t.Fatalf("doctor microvm before setup: %s %s (fix %s)", s, dd, fx)
	} else {
		res.Pass("doctor_before_setup", "microvm: warn: "+dd+" · fix: "+fx)
	}
	for _, c := range doc.Checks {
		if c.Status == "fail" {
			t.Fatalf("doctor before setup: %s failed: %s", c.Name, c.Detail)
		}
	}

	// 4. The one privileged step.
	var sr struct {
		Steps     []string `json:"steps"`
		UID       int      `json:"uid"`
		HugePages int      `json:"hugePages"`
	}
	so, se, d, err = r.timed(R, "sudo", rTwd, "host", "setup", "--microvm", "--json")
	if err != nil || json.Unmarshal([]byte(so), &sr) != nil {
		t.Fatalf("sudo twd host setup --microvm: %v\n%s\n%s", err, so, netd.Redact(se))
	}
	setupDone = false
	res.Numbers["host_setup_ms"] = msOf(d)
	res.Notes = append(res.Notes, "twd host setup --microvm: "+strings.Join(sr.Steps, " | "))
	if so := r.must(`systemctl is-active twd-vmhelper; stat -c '%U %a' /run/twd/vmhelper.sock /usr/local/libexec/twd/jailer; id -nG; groups $(id -un)`); !strings.HasPrefix(so, "active") {
		t.Fatalf("helper after setup: %s", so)
	} else {
		res.Pass("host_setup", fmt.Sprintf("%.1f s: %d steps; helper active; %s", msOf(d)/1000, len(sr.Steps), strings.Join(strings.Fields(so), " ")))
	}
	doc = doctor()
	if s, dd, _ := checkOf(doc, "microvm"); s != "ok" {
		t.Fatalf("doctor microvm after setup: %s %s", s, dd)
	} else {
		res.Pass("doctor_after_setup", "microvm: ok: "+dd)
	}

	// 5. twd up (as the user, no sudo) + agent/mcp in microVMs + (a)–(f).
	e.Run()
	if hc == nil {
		t.Fatal("no client")
	}

	// 6. Storage: the agent's own SQLite state (setState + this.sql) for a
	// fresh agent, across a VMM SIGKILL and a redeploy.
	agentGet := func() (int, map[string]any, time.Duration) {
		st := time.Now()
		resp, err := hc.Get("http://" + agentIP + "/?agent=persist1")
		if err != nil {
			return 0, nil, time.Since(st)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m, time.Since(st)
	}
	calls := func(m map[string]any) int {
		f, _ := m["calls"].(float64)
		return int(f)
	}
	var put []float64
	var n int
	for i := 0; i < 30; i++ {
		code, m, d := agentGet()
		if code != 200 {
			t.Fatalf("agent persist1: %d %v", code, m)
		}
		n = calls(m)
		if i > 0 {
			put = append(put, msOf(d))
		}
	}
	if n != 30 {
		t.Fatalf("agent persist1 after 30 requests: calls %d", n)
	}
	res.Numbers["agent_sql_write_request_p50_ms"], res.Numbers["agent_sql_write_request_p95_ms"] = pct(put, 0.5), pct(put, 0.95)
	res.Notes = append(res.Notes, fmt.Sprintf("agent request that writes SQLite (insert + setState), this Mac → agent microVM: p50 %.2f ms, p95 %.2f ms (n=%d)", pct(put, 0.5), pct(put, 0.95), len(put)))
	waitServing := func(want int, timeout time.Duration) time.Duration {
		st := time.Now()
		for time.Since(st) < timeout {
			if code, m, _ := agentGet(); code == 200 {
				if calls(m) != want {
					t.Fatalf("agent storage after restart: calls %d, want %d (state %v)", calls(m), want, m["state"])
				}
				return time.Since(st)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("agent not serving %v after restart", timeout)
		return 0
	}
	// 6a. SIGKILL the agent's VMM (a crash): hostd reboots the VM, which
	// reattaches the volume; the last acknowledged write is there.
	old := status()["agent"]
	r.must(fmt.Sprintf("sudo kill -9 %d", old.VM.PID))
	srv := waitServing(n+1, 90*time.Second)
	res.Numbers["vm_kill_to_serving_ms"] = msOf(srv)
	nw := status()["agent"]
	if nw.VM == nil || nw.VM.PID == old.VM.PID {
		t.Fatalf("agent VM not replaced after the kill: %+v", nw.VM)
	}
	res.Pass("storage_vm_kill", fmt.Sprintf("SIGKILL of the agent's VMM (pid %d) → new VM (pid %d) serving in %.0f ms with all %d SQLite writes (incl. the last one acked before the kill)", old.VM.PID, nw.VM.PID, msOf(srv), n))
	// 6b. Redeploy (blue/green → serial for a volume): the old VM releases
	// the volume, the new one attaches it.
	var dep struct {
		Timings map[string]float64 `json:"timingsMs"`
	}
	rd := time.Now()
	e.MustJSON(&dep, "deploy", agentDir)
	srv = waitServing(n+2, 90*time.Second)
	res.Numbers["redeploy_cli_ms"] = msOf(time.Since(rd) - srv)
	res.Numbers["redeploy_to_serving_ms"] = msOf(time.Since(rd))
	nw2 := status()["agent"]
	if nw2.VM == nil || nw2.VM.PID == nw.VM.PID {
		t.Fatalf("agent VM not replaced by the redeploy")
	}
	res.Pass("storage_redeploy", fmt.Sprintf("twd deploy agent again → new VM (pid %d, volume attach %.1f ms) serving with all %d writes", nw2.VM.PID, nw2.VM.VolumeAttachMS, n+1))

	// 7. No keys in either guest, fsync latency and RSS (twd host vmcheck).
	for _, w := range []string{"agent", "mcp"} {
		var vc struct {
			PID   int   `json:"pid"`
			RSSKB int64 `json:"rssKB"`
			Stats *struct {
				MemTotalKB, MemAvailableKB, WorkerdRSSKB, DOUsedKB int64
				DOFS                                               string
			}
			FSBench *struct {
				N          int    `json:"n"`
				FS         string `json:"fs"`
				WriteP50US int64  `json:"writeP50us"`
				SyncP50US  int64  `json:"fsyncP50us"`
				SyncP95US  int64  `json:"fsyncP95us"`
			} `json:"fsbench"`
			KeyScan *struct {
				Needles     int      `json:"needles"`
				NeedleKinds []string `json:"needleKinds"`
				MemoryMiB   int64    `json:"memoryMiB"`
				GuestFiles  int      `json:"guestFiles"`
				Found       []string `json:"found"`
				GuestStateF []string `json:"guestStateFiles"`
				Control     string   `json:"positiveControl"`
			} `json:"keyscan"`
		}
		e.MustJSON(&vc, "host", "vmcheck", w, "--keyscan")
		k := vc.KeyScan
		if k == nil || len(k.Found) > 0 || len(k.GuestStateF) > 0 || !strings.HasPrefix(k.Control, "instance MAC key found") || !slices.Contains(k.NeedleKinds, "volumekey") || !slices.Contains(k.NeedleKinds, "nodekey-raw") {
			t.Fatalf("%s keyscan: %+v", w, k)
		}
		res.Pass("no_keys_in_guest_"+w, fmt.Sprintf("%d needles (%s): none in the rootfs, config drive, %d guest files or %d MiB of guest RAM; %s",
			k.Needles, strings.Join(k.NeedleKinds, ", "), k.GuestFiles, k.MemoryMiB, k.Control))
		if vc.FSBench != nil {
			res.Numbers[w+"_volume_fsync_p50_us"] = float64(vc.FSBench.SyncP50US)
			res.Numbers[w+"_volume_fsync_p95_us"] = float64(vc.FSBench.SyncP95US)
			res.Numbers[w+"_volume_write_p50_us"] = float64(vc.FSBench.WriteP50US)
		}
		res.Numbers[w+"_vm_rss_mb_after"] = float64(vc.RSSKB) / 1024
		if vc.Stats != nil {
			res.Numbers[w+"_guest_used_mb"] = float64(vc.Stats.MemTotalKB-vc.Stats.MemAvailableKB) / 1024
			res.Numbers[w+"_guest_workerd_rss_mb"] = float64(vc.Stats.WorkerdRSSKB) / 1024
		}
	}

	e.Down()
	downDone = true

	// 8. Undo: everything setup installed is gone; doctor says so.
	var ur struct {
		Steps []string `json:"steps"`
	}
	so, se, _, err = r.timed(R, "sudo", rTwd, "host", "setup", "--undo", "--json")
	if err != nil || json.Unmarshal([]byte(so), &ur) != nil {
		t.Fatalf("sudo twd host setup --undo: %v\n%s\n%s", err, so, netd.Redact(se))
	}
	setupDone = true
	left := r.must(`for p in /etc/twd /usr/local/libexec/twd /var/lib/twd-vm /etc/systemd/system/twd-vmhelper.service /etc/sysctl.d/60-twd-microvm.conf /run/twd/vmhelper.sock; do [ -e "$p" ] && echo "LEFT $p"; done; systemctl is-active twd-vmhelper 2>/dev/null; getent group kvm; ip netns list | grep twd; ls /sys/fs/cgroup/system.slice | grep twd; true`)
	if strings.Contains(left, "LEFT") || strings.Contains(left, "\nactive") || strings.HasPrefix(left, "active") || strings.Contains(left, "twds") {
		t.Fatalf("after --undo: %s", left)
	}
	if s, _, _ := checkOf(doctor(), "microvm"); s != "warn" {
		t.Fatalf("doctor microvm after undo: %s", s)
	}
	res.Pass("host_setup_undo", strings.Join(ur.Steps, " | ")+" · nothing left ("+strings.Join(strings.Fields(left), " ")+")")
	time.Sleep(2 * time.Second) // device deletes propagate asynchronously
}
