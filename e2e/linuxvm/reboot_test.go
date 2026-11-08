//go:build e2elinuxvm

package linuxvm

import (
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
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/e2e/cliflow"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
)

// TestE2ELinuxReboot (make e2e-linux-reboot): DO storage of a microVM
// workload survives host reboots of twdl-kvm-u24. An agent object in a
// jailed microVM writes SQLite; then the whole VM reboots twice (1: a
// clean `limactl stop` + start; 2: `limactl stop --force`, a power cut).
// After each boot: twd-vmhelper is back via systemd, `twd up` again (as the
// user, no sudo), and the object rejoins from saved node state (same
// stable node ID and IP, no new device), the dm-crypt volume re-opens
// (LUKS2, not re-formatted) with the custody key, and every write is there.
func TestE2ELinuxReboot(t *testing.T) {
	keyFile, tailnet, vm, tarball := os.Getenv("TWD_E2E_API_KEY_FILE"), os.Getenv("TWD_E2E_TAILNET"), os.Getenv("TWD_E2E_LIMA_VM"), os.Getenv("TWD_E2E_TARBALL")
	if keyFile == "" || tailnet == "" || vm == "" || tarball == "" {
		t.Skip("TWD_E2E_API_KEY_FILE / TWD_E2E_TAILNET / TWD_E2E_LIMA_VM / TWD_E2E_TARBALL not set (make e2e-linux-reboot)")
	}
	if !allowedTailnets[tailnet] {
		t.Fatalf("refusing to run against tailnet %q", tailnet)
	}
	apiKey, err := tsapi.LoadEnvFile(keyFile, "TS_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	api := &tsapi.Client{Tailnet: tailnet, APIKey: apiKey}
	b := make([]byte, 3)
	rand.Read(b)
	run := hex.EncodeToString(b)
	testPrefix, twdPrefix := "ttest-l3r-"+run+"-", "twd-l3r-"+run+"-"
	root, _ := nettest.FindRepoRoot()
	res := cliflow.NewResults("l3-reboot-"+run, tailnet)
	resultsTo := filepath.Join(root, "e2e", "real", "results", "l3-reboot-"+time.Now().Format("20060102-150405")+"-"+run+".json")
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()

	home, _ := os.UserHomeDir()
	r := &remote{t: t, vm: vm, cfg: filepath.Join(home, ".lima", vm, "ssh.config")}
	rhome := strings.TrimSpace(r.must(`printf %s "$HOME"`))
	R := rhome + "/.twd-e2e"
	rKey, rState, rWork := R+"/tailnet1_api.env", R+"/state", R+"/work"
	rTwd := rhome + "/.twd/bin/twd"
	if so := r.must(`[ -e ~/.twd ] && echo twd; [ -e ~/.twd-e2e ] && echo e2e; [ -e /etc/twd ] && echo etc-twd; true`); strings.TrimSpace(so) != "" {
		t.Fatalf("%s is not clean (%s)", vm, strings.TrimSpace(so))
	}
	quiet := slog.New(netd.NewRedactingHandler(slog.NewTextHandler(io.Discard, nil)))
	minter := tsapi.MintSource(api, quiet)
	tracker := tsapi.NewTracker(api)
	work := t.TempDir()
	var nodes []*nettest.Node
	e := &cliflow.Env{T: t, StateDir: rState, WorkDir: rWork, Tailnet: tailnet, APIKeyFile: rKey, ControlURL: prodControl,
		Prefix: twdPrefix, Res: res, ClientTagged: true,
		Exec: func(args []string) (string, string, time.Duration, error) {
			return r.timed(rWork, append([]string{rTwd}, args...)...)
		}}

	t.Cleanup(func() {
		if _, se, _, err := e.Twd("down", "--purge"); err != nil {
			t.Logf("twd down --purge (cleanup): %v %s", err, netd.Redact(se))
		}
		if t.Failed() {
			so, _, _ := r.run(`for c in supervisor mgmt hostd; do echo "== $c"; tail -c 5000 ~/.twd-e2e/state/logs/$c.log 2>/dev/null; done; sudo journalctl -u twd-vmhelper -b -n 30 --no-pager`, nil)
			t.Logf("%s logs:\n%s", vm, netd.Redact(so))
		}
		for _, n := range nodes {
			n.Close()
		}
		so, se, err := r.run(`pkill -TERM -f "$HOME/.twd/bin/" 2>/dev/null; sleep 0.5; pkill -KILL -f "$HOME/.twd/bin/" 2>/dev/null
			if [ -e /etc/twd ] && [ -x ~/.twd/bin/twd ]; then sudo ~/.twd/bin/twd host setup --undo >/dev/null; fi
			u=$(ls -d ~/.twd-e2e/twd-*/install.sh 2>/dev/null | head -1)
			if [ -n "$u" ]; then "$u" --uninstall --purge --force >/dev/null; fi
			rm -rf ~/.twd-e2e
			for p in ~/.twd ~/.twd-e2e /etc/twd /usr/local/libexec/twd /var/lib/twd-vm; do [ -e "$p" ] && echo "LEFT $p"; done; true`, nil)
		res.Cleanup["vm_clean"] = err == nil && !strings.Contains(so, "LEFT")
		if !res.Cleanup["vm_clean"].(bool) {
			t.Errorf("cleanup: %v %s %s", err, so, se)
		}
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		devs, keys := cliflow.Leftovers(cctx, api, res)
		for _, id := range res.NodeIDs {
			tracker.Track(id, "twd")
		}
		crep := tracker.Cleanup(cctx, testPrefix)
		trep := tracker.Cleanup(cctx, twdPrefix)
		for _, k := range keys {
			api.DeleteKey(cctx, k)
		}
		minter.RevokeAll(cctx)
		res.Cleanup["twd_devices_left_after_down"], res.Cleanup["twd_keys_valid_after_down"] = len(devs), len(keys)
		res.Cleanup["devices_left"], res.Cleanup["untracked_run_devices"] = crep.Left, append(crep.Untracked, trep.Untracked...)
		os.MkdirAll(filepath.Dir(resultsTo), 0o755)
		out, _ := json.MarshalIndent(res, "", "  ")
		out = []byte(netd.Redact(string(out)))
		os.WriteFile(resultsTo, out, 0o644)
		t.Logf("results: %s\n%s", resultsTo, out)
		if len(devs) > 0 || len(keys) > 0 || crep.Left != 0 || len(trep.Untracked) > 0 {
			t.Errorf("leftovers: devices %v keys %d %+v %+v", devs, len(keys), crep, trep)
		}
	})

	// Install + one-time setup.
	r.must(`umask 077; mkdir -p ~/.twd-e2e/work && chmod 700 ~/.twd-e2e`)
	if out, err := exec.Command("limactl", "copy", tarball, vm+":"+R+"/").CombinedOutput(); err != nil {
		t.Fatalf("limactl copy tarball: %v %s", err, out)
	}
	if out, err := exec.Command("limactl", "copy", keyFile, vm+":"+rKey).CombinedOutput(); err != nil {
		t.Fatalf("limactl copy key: %v %s", err, netd.Redact(string(out)))
	}
	r.must(`chmod 600 ~/.twd-e2e/tailnet1_api.env`)
	tgz := filepath.Base(tarball)
	r.must("cd ~/.twd-e2e && tar -xzf " + shq(tgz) + " && ./" + shq(strings.TrimSuffix(tgz, ".tar.gz")) + "/install.sh >/dev/null")
	r.must("sudo " + rTwd + " host setup --microvm >/dev/null")

	// Not --ephemeral: these nodes must outlive the reboots.
	up := func() time.Duration {
		return e.MustJSON(nil, "up", "--tailnet", tailnet, "--api-key-file", rKey, "--yes", "--hostname-prefix", twdPrefix, "--control-url", prodControl)
	}
	res.Numbers["up_first_ms"] = msOf(up())
	agentDir := rWork + "/agent"
	e.MustJSON(nil, "init", agentDir, "--template", "agent")
	r.must(`sed -i '0,/{/s//{\n  "tier": "microvm",/' ` + shq(agentDir+"/twd.jsonc"))
	var dep struct {
		Endpoint struct{ MagicDNS string } `json:"endpoint"`
	}
	e.MustJSON(&dep, "deploy", agentDir)

	newNode := func(role string) *nettest.Node {
		k, err := minter.AuthKey(ctx, netd.KeyRequest{ObjectID: role, Tags: []string{cliflow.TestTag}, Ephemeral: true})
		if err != nil {
			t.Fatalf("mint: %v", netd.RedactError(err))
		}
		n, err := nettest.NewNode(ctx, prodControl, k, testPrefix+role, filepath.Join(work, role))
		if err != nil {
			t.Fatal(netd.RedactError(err))
		}
		nodes = append(nodes, n)
		if st, err := n.LC.StatusWithoutPeers(ctx); err == nil && st.Self != nil {
			tracker.Track(string(st.Self.ID), role)
		}
		return n
	}
	client := newNode("client")
	hc := client.HTTPClient(20 * time.Second)
	agentIP := cliflow.PeerIP(t, client, dep.Endpoint.MagicDNS, 90*time.Second)
	get := func(ip, q string) (int, map[string]any) {
		resp, err := hc.Get("http://" + ip + "/" + q)
		if err != nil {
			return 0, nil
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	callsOf := func(m map[string]any) int { f, _ := m["calls"].(float64); return int(f) }
	for i := 0; i < 10; i++ {
		if code, m := get(agentIP, "?agent=boot1"); code != 200 || callsOf(m) != i+1 {
			t.Fatalf("agent write %d: %d %v", i, code, m)
		}
	}
	obj := e.WaitObjectReady("SupportAgent", "boot1", 90*time.Second)
	objIP := cliflow.PeerIP(t, client, obj.MagicDNS, 90*time.Second)
	if code, m := get(objIP, ""); code != 200 || callsOf(m) != 11 {
		t.Fatalf("object via its own node: %d %v", code, m)
	}
	want := 11
	stable := obj.StableNodeID
	if stable == "" {
		stable = obj.NodeID
	}
	for _, o := range e.Objects() {
		if id := o.StableNodeID; id != "" {
			res.NodeIDs = append(res.NodeIDs, id)
		}
	}
	runDevices := func() []string {
		ds, err := api.ListDevices(ctx)
		if err != nil {
			t.Fatalf("list devices: %v", netd.RedactError(err))
		}
		var out []string
		for _, d := range ds {
			if strings.HasPrefix(d.Hostname, twdPrefix) || strings.HasPrefix(d.Name, twdPrefix) {
				out = append(out, d.NodeID)
			}
		}
		slices.Sort(out)
		return out
	}
	devsBefore := runDevices()
	res.Notes = append(res.Notes, fmt.Sprintf("before reboots: object boot1 node %s (%s, %s), %d twd devices of this run, SQLite calls=%d", stable, obj.MagicDNS, objIP, len(devsBefore), want))

	// Reboot twice: clean, then a power cut.
	for i, force := range []bool{false, true} {
		tag := fmt.Sprintf("reboot%d", i+1)
		args := []string{"stop", vm}
		if force {
			args = []string{"stop", "--force", vm}
		}
		st := time.Now()
		if out, err := exec.Command("limactl", args...).CombinedOutput(); err != nil {
			t.Fatalf("limactl %v: %v %s", args, err, out)
		}
		res.Numbers[tag+"_stop_ms"] = msOf(time.Since(st))
		st = time.Now()
		if out, err := exec.Command("limactl", "start", vm).CombinedOutput(); err != nil {
			t.Fatalf("limactl start: %v %s", err, out)
		}
		res.Numbers[tag+"_limactl_start_ms"] = msOf(time.Since(st))
		// Inside the VM, on its own clock (µs since kernel boot): when the
		// helper came up, and now (ssh usable).
		so := r.must(`systemctl is-active twd-vmhelper; systemctl show -p ActiveEnterTimestampMonotonic --value twd-vmhelper; cut -d' ' -f1 /proc/uptime; ls /run/twd/vmhelper.sock; cat /proc/sys/vm/nr_hugepages`)
		f := strings.Fields(so)
		if len(f) < 5 || f[0] != "active" {
			t.Fatalf("%s: twd-vmhelper not active after boot: %s", tag, so)
		}
		helperUS, _ := strconv.ParseFloat(f[1], 64)
		upNow, _ := strconv.ParseFloat(f[2], 64)
		res.Numbers[tag+"_boot_to_helper_active_ms"] = helperUS / 1000
		res.Numbers[tag+"_boot_to_ssh_ms"] = upNow * 1000
		res.Notes = append(res.Notes, fmt.Sprintf("%s (%s): helper active %.0f ms after kernel boot; huge pages %s", tag, map[bool]string{false: "clean limactl stop", true: "limactl stop --force (power cut)"}[force], helperUS/1000, f[4]))
		// twd up again (as the user).
		d := up()
		res.Numbers[tag+"_twd_up_ms"] = msOf(d)
		sv := time.Now()
		// Where do the 12-17 s until this Mac reaches the rejoined node go?
		// Sample the client's view of the peer (netmap presence, Online,
		// relay / direct path, last handshake) while we poll the object.
		stopSample := make(chan struct{})
		var timeline []string
		go func() {
			last := ""
			for {
				select {
				case <-stopSample:
					return
				case <-time.After(250 * time.Millisecond):
				}
				sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				st, err := client.LC.Status(sctx)
				cancel()
				if err != nil {
					continue
				}
				cur := "peer absent from netmap"
				for _, p := range st.Peer {
					if len(p.TailscaleIPs) > 0 && p.TailscaleIPs[0].String() == objIP {
						hs := "no handshake"
						if !p.LastHandshake.IsZero() {
							hs = "handshake " + time.Since(p.LastHandshake).Round(100*time.Millisecond).String() + " ago"
						}
						cur = fmt.Sprintf("in netmap: online=%v active=%v relay=%q curAddr=%q %s", p.Online, p.Active, p.Relay, p.CurAddr, hs)
						break
					}
				}
				key, _, _ := strings.Cut(cur, " handshake")
				if key != last {
					last = key
					timeline = append(timeline, fmt.Sprintf("+%.1fs %s", time.Since(sv).Seconds(), cur))
				}
			}
		}()
		var m map[string]any
		for {
			var code int
			if code, m = get(objIP, ""); code == 200 {
				break
			}
			if time.Since(sv) > 3*time.Minute {
				close(stopSample)
				t.Fatalf("%s: object not serving on %s 3 min after twd up: %s", tag, objIP, strings.Join(timeline, "; "))
			}
			time.Sleep(100 * time.Millisecond)
		}
		close(stopSample)
		res.Numbers[tag+"_up_to_object_serving_ms"] = msOf(time.Since(sv))
		timeline = append(timeline, fmt.Sprintf("+%.1fs first HTTP 200 from the object", time.Since(sv).Seconds()))
		// The VM side: when hostd said the object's node was ready (VM clock,
		// Lima keeps it in sync), vs. when the Mac client got through.
		ready := strings.TrimSpace(r.must(`grep 'object node ready' ~/.twd-e2e/state/logs/hostd.log | tail -1 | sed 's/.*time=\([^ ]*\).*join_ms=\([^ ]*\).*/\1 join_ms=\2/'; date -u +%Y-%m-%dT%H:%M:%S.%3NZ`))
		res.Notes = append(res.Notes, fmt.Sprintf("%s re-find timeline (client view; twd up returned at %s, Mac clock): %s | VM: last 'object node ready' at %s", tag, sv.Format("15:04:05.000"), strings.Join(timeline, "; "), strings.Join(strings.Fields(ready), " now=")))
		want++
		if callsOf(m) != want {
			t.Fatalf("%s: SQLite calls %d, want %d: data lost? %v", tag, callsOf(m), want, m)
		}
		// Same node, same IP, no new device.
		o, ok := cliflow.Object{}, false
		for _, x := range e.Objects() {
			if x.Namespace == "SupportAgent" && x.Name == "boot1" {
				o, ok = x, true
			}
		}
		id := o.StableNodeID
		if id == "" {
			id = o.NodeID
		}
		if !ok || id != stable || cliflow.PeerIP(t, client, o.MagicDNS, 30*time.Second) != objIP {
			t.Fatalf("%s: object rejoined as %s (%s), want %s (%s)", tag, id, o.MagicDNS, stable, objIP)
		}
		if devs := runDevices(); !slices.Equal(devs, devsBefore) {
			t.Fatalf("%s: devices of this run changed: %v → %v", tag, devsBefore, devs)
		}
		// The volume re-opened (LUKS2 via dm-crypt, not re-formatted).
		var sts struct {
			Workloads []wlStatus
		}
		e.MustJSON(&sts, "status")
		var ws wlStatus
		for _, w := range sts.Workloads {
			if w.Workload == "agent" {
				ws = w
			}
		}
		if ws.VM == nil || ws.VM.Jail == nil || ws.VM.VolumeNew {
			t.Fatalf("%s: agent VM %+v (volume must re-open, not format)", tag, ws.VM)
		}
		cs := r.must("sudo cryptsetup status " + shq(ws.VM.Jail.ID) + " | grep -E 'type|cipher' | tr -s ' '; sudo journalctl -u twd-vmhelper -b --no-pager | grep -o 'volume attached.*formatted=[a-z]*' | tail -1")
		if !strings.Contains(cs, "LUKS2") || !strings.Contains(cs, "formatted=false") {
			t.Fatalf("%s: volume: %s", tag, cs)
		}
		res.Numbers[tag+"_vm_ready_ms"] = msOf(ws.VM.Ready)
		res.Numbers[tag+"_volume_attach_ms"] = ws.VM.VolumeAttachMS
		res.Pass(tag, fmt.Sprintf("helper back via systemd (%.0f ms after kernel boot), twd up %.1f s, object serving %.1f s later on the same node %s / %s (no new device), all %d SQLite writes present, volume re-opened: %s",
			helperUS/1000, msOf(d)/1000, res.Numbers[tag+"_up_to_object_serving_ms"]/1000, stable, objIP, want, strings.Join(strings.Fields(cs), " ")))
		_ = http.StatusOK
	}
}
