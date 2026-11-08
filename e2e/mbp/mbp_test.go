//go:build e2embp

// E2E-MAC on a different, clean MacBook (BUILD-PLAN definition of done 1,
// the headline): from this Mac, copy the darwin release tarball and the
// tailnet-1 API key file to `mbp` (~/.twd-e2e, mode 0600), run install.sh
// there, `twd up` / `twd init` / `twd deploy` on mbp over ssh, and run the
// tag:ttest test client and the private echo service HERE as tsnet nodes,
// so every check (a)–(f) of e2e/mac crosses machines over tailnet 1. Then
// `twd down` on mbp, uninstall, remove ~/.twd-e2e (the key file goes even
// on failure: t.Cleanup here, and the Makefile's trap), and verify zero
// leftover devices and keys from this run, by ID.
//
// Safety: tailnet 1 only (allow list). mbp's own Tailscale.app (Ross's
// personal tailnet) is never used, read or called: twd on mbp runs only
// tsnet nodes with an explicit --tailnet, and `twd doctor` checks that no
// twd process holds the system tailscaled's socket. No policy is written
// (the scenario's grants were added in M4; this test only checks they are
// there). Keys are never printed.
package mbp

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

// remote runs commands on the other Mac over one multiplexed ssh connection.
type remote struct {
	t    testing.TB
	host string
	ctl  string
}

func (r *remote) args(cmd string) []string {
	return []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-o", "ControlMaster=auto",
		"-o", "ControlPath=" + r.ctl, "-o", "ControlPersist=600", "-o", "ServerAliveInterval=15", r.host, cmd}
}

// run runs a shell command on the remote (stdin optional).
func (r *remote) run(cmd string, stdin io.Reader) (string, string, error) {
	c := exec.Command("ssh", r.args(cmd)...)
	var so, se bytes.Buffer
	c.Stdout, c.Stderr, c.Stdin = &so, &se, stdin
	err := c.Run()
	return so.String(), se.String(), err
}

func (r *remote) must(cmd string) string {
	r.t.Helper()
	so, se, err := r.run(cmd, nil)
	if err != nil {
		r.t.Fatalf("mbp: %s: %v\n%s\n%s", cmd, err, netd.Redact(so), netd.Redact(se))
	}
	return so
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// timed wraps a remote command so mbp reports its own wall time (perl ships
// with macOS; ssh round trips are not counted).
const timedPerl = `my $t=time; my $r=system(@ARGV); printf STDERR "\n__twd_wall_ms=%.3f\n",(time-$t)*1000; exit($r==-1?127:$r>>8);`

var wallRE = regexp.MustCompile(`\n?__twd_wall_ms=([0-9.]+)\n?$`)

func (r *remote) timed(dir string, argv ...string) (string, string, time.Duration, error) {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shq(a)
	}
	cmd := "cd " + shq(dir) + " 2>/dev/null || cd; exec /usr/bin/perl -MTime::HiRes=time -e " + shq(timedPerl) + " " + strings.Join(q, " ")
	st := time.Now()
	so, se, err := r.run(cmd, nil)
	d := time.Since(st)
	if m := wallRE.FindStringSubmatch(se); m != nil {
		f, _ := strconv.ParseFloat(m[1], 64)
		d = time.Duration(f * float64(time.Millisecond))
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

func TestE2EMbp(t *testing.T) {
	keyFile, tailnet, host, tarball := os.Getenv("TWD_E2E_API_KEY_FILE"), os.Getenv("TWD_E2E_TAILNET"), os.Getenv("TWD_E2E_SSH_HOST"), os.Getenv("TWD_E2E_TARBALL")
	if keyFile == "" || tailnet == "" || host == "" || tarball == "" {
		t.Skip("TWD_E2E_API_KEY_FILE / TWD_E2E_TAILNET / TWD_E2E_SSH_HOST / TWD_E2E_TARBALL not set")
	}
	if !allowedTailnets[tailnet] {
		t.Fatalf("refusing to run against tailnet %q (not in the allow list)", tailnet)
	}
	if _, err := os.Stat(tarball); err != nil {
		t.Fatalf("tarball: %v (make release)", err)
	}
	apiKey, err := tsapi.LoadEnvFile(keyFile, "TS_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	api := &tsapi.Client{Tailnet: tailnet, APIKey: apiKey}
	b := make([]byte, 3)
	rand.Read(b)
	run := hex.EncodeToString(b)
	testPrefix := "ttest-m6-" + run + "-" // this Mac's client + echo
	twdPrefix := "twd-m6-" + run + "-"    // every node twd creates on mbp
	root, _ := nettest.FindRepoRoot()
	res := cliflow.NewResults("m6-mbp-"+run, tailnet)
	res.Notes = append(res.Notes, "twd on "+host+" from "+filepath.Base(tarball)+"; test client + echo on this Mac")
	resultsTo := filepath.Join(root, "e2e", "real", "results", "m6-mbp-"+time.Now().Format("20060102-150405")+"-"+run+".json")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// The scenario's grants must already be in tailnet 1's policy (M4 added
	// them). Never written here.
	pol, _, err := api.GetPolicyHuJSON(ctx)
	if err != nil {
		t.Fatalf("policy read: %v", netd.RedactError(err))
	}
	if miss, err := policy.Missing(pol, cliflow.Policy()); err != nil || !miss.Empty() {
		t.Fatalf("tailnet 1's policy lacks the scenario's grants/tagOwners (not writing it in this run): %v %s", err, miss.HuJSON())
	}
	res.Notes = append(res.Notes, "policy has the scenario's tagOwners and grants (read only; not written)")

	r := &remote{t: t, host: host, ctl: filepath.Join(os.TempDir(), "twd-m6-ssh-"+run)}
	rhome := strings.TrimSpace(r.must(`printf %s "$HOME"`))
	if rhome == "" || !strings.HasPrefix(rhome, "/") {
		t.Fatalf("mbp $HOME = %q", rhome)
	}
	R := rhome + "/.twd-e2e"
	rKey, rState, rWork := R+"/tailnet1_api.env", R+"/state", R+"/work"
	rTwd := rhome + "/.twd/bin/twd"

	// mbp must be clean of twd state (an earlier crashed run is cleaned by
	// the Makefile trap).
	if so := r.must(`[ -e ~/.twd/twd.json ] && echo state; [ -e ~/.twd-e2e ] && echo e2e; true`); strings.TrimSpace(so) != "" {
		t.Fatalf("mbp is not clean (%s): remove ~/.twd-e2e and ~/.twd first", strings.TrimSpace(so))
	}
	if so := r.must(`command -v node go npm esbuild 2>/dev/null; true`); strings.TrimSpace(so) != "" {
		res.Notes = append(res.Notes, "mbp has on PATH: "+strings.Join(strings.Fields(so), " "))
	} else {
		res.Notes = append(res.Notes, "mbp has no node, go, npm or esbuild on PATH")
	}
	res.Notes = append(res.Notes, "mbp: "+strings.Join(strings.Fields(r.must(`sw_vers -productVersion; uname -m; sysctl -n machdep.cpu.brand_string`)), " "))

	quiet := slog.New(netd.NewRedactingHandler(slog.NewTextHandler(io.Discard, nil)))
	minter := tsapi.MintSource(api, quiet)
	tracker := tsapi.NewTracker(api)
	work := t.TempDir()
	var nodes []*nettest.Node
	downDone := false

	e := &cliflow.Env{T: t, BinDir: "(mbp)", StateDir: rState, WorkDir: rWork, Tailnet: tailnet, APIKeyFile: rKey,
		ControlURL: prodControl, Prefix: twdPrefix, Res: res, ClientTagged: true,
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
	e.OnPeers = func(client *nettest.Node, hc *http.Client, agentIP, mcpIP string) {
		measurePath(t, res, client, hc, agentIP, mcpIP)
		// mbp's LAN address must be the direct endpoint: the workload
		// really runs on the other Mac.
		if lan := strings.TrimSpace(r.must(`ipconfig getifaddr en0 || ipconfig getifaddr en1; true`)); lan != "" && lastEndpoint != "" {
			if ap, err := netip.ParseAddrPort(lastEndpoint); err == nil && ap.Addr().String() == lan {
				res.Pass("cross_machine", "the mcp workload node's direct endpoint is mbp's LAN address (twd host "+strings.TrimSpace(r.must(`hostname -s`))+", test client on "+localHost()+")")
			} else {
				res.Notes = append(res.Notes, "direct endpoint is not mbp's en0/en1 address (another interface or NAT)")
			}
		}
		// DERP: the same requests from a second client that may only use
		// DERP (TS_DEBUG_ALWAYS_USE_DERP=1, a separate process: the knob is
		// process-wide).
		k, err := minter.AuthKey(ctx, netd.KeyRequest{ObjectID: "derp", Tags: []string{cliflow.TestTag}, Ephemeral: true})
		if err != nil {
			t.Fatalf("mint derp: %v", netd.RedactError(err))
		}
		derpProbe(t, res, tracker, k, testPrefix+"derp", mcpIP, work)
	}

	t.Cleanup(func() {
		// 1. twd down on mbp (needs the key file), unless the test did.
		if !downDone {
			if _, se, _, err := e.Twd("down", "--purge"); err != nil {
				t.Logf("twd down (cleanup): %v %s", err, netd.Redact(se))
			}
		}
		if t.Failed() {
			so, _, _ := r.run(`for c in supervisor mgmt hostd; do echo "== $c"; tail -c 5000 ~/.twd-e2e/state/logs/$c.log 2>/dev/null; done`, nil)
			t.Logf("mbp logs (tail):\n%s", netd.Redact(so))
		}
		for _, n := range nodes {
			n.Close()
		}
		// 2. On mbp: stop anything left, uninstall, remove ~/.twd-e2e (and
		// with it the key file), then check it's all gone.
		us := time.Now()
		so, se, err := r.run(`pkill -TERM -f "$HOME/.twd/bin/" 2>/dev/null; sleep 0.5; pkill -KILL -f "$HOME/.twd/bin/" 2>/dev/null
			u=$(ls -d ~/.twd-e2e/twd-*/install.sh 2>/dev/null | head -1)
			if [ -n "$u" ]; then "$u" --uninstall --purge --force; fi
			rm -rf ~/.twd-e2e
			[ -e ~/.twd ] && echo LEFT ~/.twd; [ -e ~/.twd-e2e ] && echo LEFT ~/.twd-e2e
			pgrep -fl "$HOME/.twd/" && echo LEFT processes; true`, nil)
		res.Numbers["uninstall_ms"] = msOf(time.Since(us))
		if err != nil || strings.Contains(so, "LEFT") {
			t.Errorf("mbp cleanup: %v %s %s", err, netd.Redact(so), netd.Redact(se))
			res.Cleanup["mbp_clean"] = false
		} else {
			res.Cleanup["mbp_clean"] = true
		}
		exec.Command("ssh", "-o", "ControlPath="+r.ctl, "-O", "exit", host).Run()

		// 3. Zero leftover devices and keys from this run, by ID.
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
		trep := tracker.Cleanup(cctx, twdPrefix) // reports untracked twd-m6-<run>-* devices
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

	// (a) copy the tarball and the key file (0600, via stdin: never on a
	// command line, never printed).
	r.must(`umask 077; mkdir -p ~/.twd-e2e/work && chmod 700 ~/.twd-e2e`)
	st := time.Now()
	if out, err := exec.Command("scp", "-q", "-o", "BatchMode=yes", "-o", "ControlPath="+r.ctl, tarball, host+":.twd-e2e/").CombinedOutput(); err != nil {
		t.Fatalf("scp tarball: %v %s", err, out)
	}
	res.Numbers["scp_tarball_ms"] = msOf(time.Since(st))
	kf, err := os.Open(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	_, se, err := r.run(`umask 077; cat > ~/.twd-e2e/tailnet1_api.env && chmod 600 ~/.twd-e2e/tailnet1_api.env`, kf)
	kf.Close()
	if err != nil {
		t.Fatalf("copy key file: %v %s", err, se)
	}
	if m := strings.TrimSpace(r.must(`stat -f %Lp ~/.twd-e2e/tailnet1_api.env`)); m != "600" {
		t.Fatalf("key file mode on mbp: %s", m)
	}
	if q := strings.TrimSpace(r.must(`xattr ~/.twd-e2e/*.tar.gz`)); q != "" {
		res.Notes = append(res.Notes, "scp'd tarball xattrs on mbp: "+q)
	} else {
		res.Notes = append(res.Notes, "scp'd tarball has no xattrs on mbp (no com.apple.quarantine)")
	}
	res.Checks["copy"] = "PASS tarball + key file (0600) in ~/.twd-e2e"

	// (b) install.sh
	tgz := filepath.Base(tarball)
	dirName := strings.TrimSuffix(tgz, ".tar.gz")
	so, se, d, err := r.timed(R, "tar", "-xzf", tgz)
	if err != nil {
		t.Fatalf("extract: %v %s", err, se)
	}
	res.Numbers["extract_ms"] = msOf(d)
	so, se, d, err = r.timed(R, R+"/"+dirName+"/install.sh")
	if err != nil {
		t.Fatalf("install.sh: %v\n%s\n%s", err, so, se)
	}
	res.Numbers["install_ms"] = msOf(d)
	t.Logf("install.sh:\n%s", so)
	if !strings.Contains(so, "export PATH=") {
		t.Fatalf("install.sh did not print PATH instructions:\n%s", so)
	}
	// idempotent: a second run succeeds and leaves the same install
	so2, se, d2, err := r.timed(R, R+"/"+dirName+"/install.sh")
	if err != nil || !strings.Contains(so2, "installed twd") {
		t.Fatalf("install.sh (again): %v\n%s\n%s", err, so2, se)
	}
	res.Numbers["install_again_ms"] = msOf(d2)
	ver := strings.TrimSpace(r.must(`cat ~/.twd/VERSION; ~/.twd/bin/workerd --version; codesign -dv ~/.twd/bin/workerd 2>&1 | grep -o 'Signature=.*'; xattr -r ~/.twd/bin | head -3`))
	res.Pass("install", fmt.Sprintf("install.sh %.1f s (again %.1f s), no sudo, ~/.twd: %s", msOf(d)/1000, msOf(d2)/1000, strings.Join(strings.Fields(ver), " ")))

	// twd doctor before up (no state yet: the key check uses the flags).
	var doc struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name, Status, Detail string
		} `json:"checks"`
	}
	so, se, d, err = e.Twd("doctor", "--tailnet", tailnet, "--api-key-file", rKey, "--json")
	if err != nil || json.Unmarshal([]byte(so), &doc) != nil || !doc.OK {
		t.Fatalf("twd doctor on mbp: %v\n%s\n%s", err, so, netd.Redact(se))
	}
	for _, c := range doc.Checks {
		if c.Status != "ok" {
			t.Fatalf("twd doctor on mbp: %s is %s: %s", c.Name, c.Status, c.Detail)
		}
	}
	for _, c := range doc.Checks {
		if c.Name == "bundler" && !strings.Contains(c.Detail, "ai-agent") {
			t.Fatalf("doctor's bundler check didn't build the ai-agent template: %s", c.Detail)
		}
	}
	// ai-agent (Vercel AI SDK + @ai-sdk/mcp) scaffolds on mbp with no npm;
	// doctor above bundled it offline. Not deployed: its tags aren't in
	// tailnet 1's policy, and M6 doesn't write policy.
	if _, se, _, err := e.Twd("init", rWork+"/ai", "--template", "ai-agent", "--json"); err != nil {
		t.Fatalf("twd init --template ai-agent on mbp: %v %s", err, se)
	}
	res.Pass("init_ai_agent_offline", "twd init --template ai-agent on mbp (no npm); bundled offline by doctor")
	res.Numbers["doctor_ms"] = msOf(d)
	res.Pass("doctor", fmt.Sprintf("%d checks ok in %.0f ms (workerd runs, bundler offline, ~/.twd writable, API key works, Seatbelt enforces, system tailscaled not used)", len(doc.Checks), msOf(d)))

	// (c)–(e) on mbp, checks (a)–(f) from this Mac.
	e.Run()

	// Seatbelt from the installed release (M7): every workerd on mbp runs
	// under its tenant's profile, and that profile denies the API key file,
	// twd's state (node keys, mgmt custody) and the installed ~/.twd, while
	// the same reads work unsandboxed (control). Only exit codes are used.
	var sbst struct {
		Workloads []struct {
			Workload, Tier, Sandbox, SandboxProfile string
		}
	}
	e.MustJSON(&sbst, "status")
	nsb := 0
	for _, w := range sbst.Workloads {
		if w.Sandbox != "seatbelt" || w.SandboxProfile == "" {
			t.Fatalf("mbp: workload %s runs with sandbox %q (want seatbelt)", w.Workload, w.Sandbox)
		}
		nsb++
		for _, c := range []string{"/bin/cat " + shq(rKey), "/bin/ls " + shq(rState+"/hostd"), "/bin/ls " + shq(rhome+"/.twd/bin")} {
			if _, _, err := r.run(c+" >/dev/null 2>&1", nil); err != nil {
				t.Fatalf("mbp control (unsandboxed) failed: %s: %v", c, err)
			}
			if _, _, err := r.run("/usr/bin/sandbox-exec -f "+shq(w.SandboxProfile)+" "+c+" >/dev/null 2>&1", nil); err == nil {
				t.Fatalf("mbp: %s's Seatbelt profile allowed %s", w.Workload, c)
			}
		}
	}
	if nsb == 0 {
		t.Fatal("mbp: no workloads in twd status")
	}
	res.Pass("seatbelt_release", fmt.Sprintf("%d workloads under seatbelt from the installed release; each profile denies the API key file, twd's hostd state and ~/.twd/bin (readable unsandboxed)", nsb))

	// doctor again while twd runs: no twd process holds tailscaled's socket,
	// and the sandbox is active for every running workload.
	so, se, _, err = e.Twd("doctor", "--json")
	if err != nil || json.Unmarshal([]byte(so), &doc) != nil || !doc.OK {
		t.Fatalf("twd doctor (running) on mbp: %v\n%s\n%s", err, so, netd.Redact(se))
	}
	for _, c := range doc.Checks {
		if c.Name == "sandbox" {
			if c.Status != "ok" || !strings.Contains(c.Detail, fmt.Sprintf("active: %d/%d", nsb, nsb)) {
				t.Fatalf("doctor (running) sandbox: %s %s", c.Status, c.Detail)
			}
			res.Pass("doctor_sandbox_active", c.Detail)
		}
	}
	for _, c := range doc.Checks {
		if c.Name == "system_tailscaled_not_used" {
			if c.Status != "ok" || !strings.Contains(c.Detail, "hold no tailscaled LocalAPI socket") {
				t.Fatalf("doctor (running): %s %s", c.Status, c.Detail)
			}
			res.Pass("system_tailscaled_not_used", c.Detail)
		}
	}

	e.Down()
	downDone = true
	time.Sleep(2 * time.Second) // device deletes propagate asynchronously
}

// measurePath measures this Mac → mbp over the tailnet: disco pings (which
// say DERP or direct) and HTTP requests (an MCP tools/list to the mcp
// workload node, which creates no objects), from the client's first
// contact until a direct path is up, then steady state.
func measurePath(t testing.TB, res *cliflow.Results, client *nettest.Node, hc *http.Client, agentIP, mcpIP string) {
	ip := netip.MustParseAddr(mcpIP)
	var derpPing, directPing, derpHTTP, directHTTP []float64
	var region, endpoint string
	defer func() { lastEndpoint = endpoint }()
	start := time.Now()
	var toDirect time.Duration
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	httpOnce := func() (float64, bool) {
		req, _ := http.NewRequest("POST", "http://"+mcpIP+"/mcp", bytes.NewReader(body))
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
	direct := func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		st, err := client.LC.Status(ctx)
		if err != nil {
			return false
		}
		for _, p := range st.Peer {
			for _, a := range p.TailscaleIPs {
				if a == ip {
					return p.CurAddr != ""
				}
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
		if ms, ok := httpOnce(); ok {
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
	res.Notes = append(res.Notes, fmt.Sprintf("path this Mac → mbp mcp node: %d DERP pings (region %q, median %.1f ms), %d direct (%s, median %.1f ms) after %v; HTTP tools/list median DERP %.1f ms (n=%d), direct %.1f ms (n=%d)",
		len(derpPing), region, median(derpPing), len(directPing), redactEndpoint(endpoint), median(directPing), toDirect.Round(time.Millisecond),
		median(derpHTTP), len(derpHTTP), median(directHTTP), len(directHTTP)))
	if len(directHTTP)+len(derpHTTP) == 0 {
		t.Fatalf("no HTTP request to the mcp node (%s) succeeded", mcpIP)
	}
	_ = agentIP
}

var lastEndpoint string

func localHost() string { h, _ := os.Hostname(); h, _, _ = strings.Cut(h, "."); return h }

// pathSample is one ping + one HTTP request.
type probeOut struct {
	NodeID  string    `json:"nodeID"`
	Ping    []float64 `json:"pingMs"`
	HTTP    []float64 `json:"httpMs"`
	Regions []string  `json:"regions"`
	Direct  int       `json:"directPongs"`
	Err     string    `json:"err,omitempty"`
}

// derpProbe re-runs this test binary as TestDERPProbe with DERP forced.
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
	cmd := exec.Command(os.Args[0], "-test.run=^TestDERPProbe$", "-test.count=1", "-test.timeout=3m")
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
	res.Notes = append(res.Notes, fmt.Sprintf("DERP-only client (TS_DEBUG_ALWAYS_USE_DERP) → mbp mcp node: disco ping median %.1f ms (n=%d, regions %v, %d direct pongs), HTTP tools/list median %.1f ms (n=%d)",
		median(po.Ping), len(po.Ping), slices.Compact(po.Regions), po.Direct, median(po.HTTP), len(po.HTTP)))
}

// TestDERPProbe is derpProbe's child process (skipped otherwise).
func TestDERPProbe(t *testing.T) {
	kf := os.Getenv("TWD_PROBE_KEY_FILE")
	if kf == "" {
		t.Skip("child of TestE2EMbp only")
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	for i := 0; i < 12; i++ {
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
		pr, err := n.LC.Ping(pctx, ip, tailcfg.PingDisco)
		pcancel()
		if err == nil && pr.Err == "" && i > 0 { // the first includes path setup
			po.Ping = append(po.Ping, pr.LatencySeconds*1000)
			po.Regions = append(po.Regions, pr.DERPRegionCode)
			if pr.Endpoint != "" {
				po.Direct++
			}
		}
		req, _ := http.NewRequest("POST", "http://"+target+"/mcp", bytes.NewReader(body))
		req.Header.Set("content-type", "application/json")
		req.Header.Set("accept", "application/json, text/event-stream")
		req.Header.Set("mcp-protocol-version", "2025-06-18")
		st := time.Now()
		resp, err := hc.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && i > 0 {
				po.HTTP = append(po.HTTP, msOf(time.Since(st)))
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// redactEndpoint keeps only whether the direct path is LAN or not.
func redactEndpoint(ep string) string {
	ap, err := netip.ParseAddrPort(ep)
	if err != nil {
		return "direct"
	}
	if ap.Addr().IsPrivate() {
		return "direct, LAN"
	}
	return "direct, WAN"
}
