//go:build e2evm && linux

// Package vm is E2E-VM (M8): the same apps as the process tier, run in a
// Firecracker microVM in Lima, with host-side netd (one tsnet.Server per
// object, outside the guest) as the guest's only network. Hermetic:
// testcontrol + local DERP/STUN, fakemgmt, real twd-hostd, real Firecracker.
//
// Run with `make e2e-vm` (inside Lima; needs root, KVM, the guest image).
package vm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm"
	"github.com/rosskukulinski/twd/netd/nettest"
)

type results struct {
	mu sync.Mutex
	m  map[string]any
}

func (r *results) set(k string, v any) {
	r.mu.Lock()
	r.m[k] = v
	r.mu.Unlock()
}

func TestE2EVM(t *testing.T) {
	kernel, rootfs := os.Getenv("TWD_VM_KERNEL"), os.Getenv("TWD_VM_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("TWD_VM_KERNEL and TWD_VM_ROOTFS must be set (make e2e-vm)")
	}
	vcpus := 2
	if v := os.Getenv("TWD_VM_VCPUS"); v != "" {
		fmt.Sscan(v, &vcpus)
	}
	be, err := vm.New(vm.Config{Kernel: kernel, Rootfs: rootfs, MemMiB: 512, VCPUs: vcpus, BootArgs: os.Getenv("TWD_VM_BOOTARGS"), HugePages: os.Getenv("TWD_VM_HUGEPAGES")})
	if err != nil {
		t.Fatalf("microVM backend unavailable: %v", err)
	}
	t.Cleanup(func() { be.Close() })
	res := &results{m: map[string]any{"started": time.Now().UTC().Format(time.RFC3339), "kernel": kernel, "rootfs": rootfs, "vcpus": vcpus, "mem_mib": 512,
		"hugepages": os.Getenv("TWD_VM_HUGEPAGES"), "free_hugepages_2m_at_start": vm.FreeHugePages2M()}}
	if mb, err := os.ReadFile(filepath.Join(filepath.Dir(rootfs), "manifest.json")); err == nil {
		var man map[string]any
		json.Unmarshal(mb, &man)
		res.set("guest_image", man)
	}
	defer writeResults(t, res)

	ctl, err := nettest.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctl.Close)
	e := hostdtest.Start(t, hostdtest.Options{ControlURL: ctl.URL, Fake: fakeStatic(), Ephemeral: true, KillTombstone: 5 * time.Second,
		VM: be, VMDebug: true,
		Tenant: mgmtapi.Tenant{Tailnet: "ttest.ts.net", ControlURL: ctl.URL, MintMode: "static", Tier: bundle.TierMicroVM}})
	ctx := context.Background()
	var client, echo *nettest.Node
	var wg sync.WaitGroup
	var cerr, eerr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		client, cerr = nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-client", filepath.Join(e.Dir, "client"))
	}()
	go func() {
		defer wg.Done()
		echo, eerr = nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-echo", filepath.Join(e.Dir, "echo"))
	}()
	wg.Wait()
	if cerr != nil || eerr != nil {
		t.Fatal(cerr, eerr)
	}
	t.Cleanup(func() { client.Close(); echo.Close() })
	if err := echo.ServeEcho(); err != nil {
		t.Fatal(err)
	}
	st, _ := client.LC.StatusWithoutPeers(ctx)
	clientID := string(st.Self.ID)
	hc := client.HTTPClient(30 * time.Second)
	getJSON := func(url string) (int, map[string]any) {
		resp, err := hc.Get(url)
		if err != nil {
			return 0, map[string]any{"err": err.Error()}
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			m = map[string]any{"raw": string(b)}
		}
		return resp.StatusCode, m
	}
	waitPeer := func(ip string) {
		t.Helper()
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := client.WaitPeers(c, ip); err != nil {
			t.Fatal(err)
		}
	}
	echoURL := "http://ttest-echo/"

	// Deploy hello + probe in the microVM tier (the tenant's tier), and a
	// process-tier copy of probe for the latency comparison.
	tDeploy := time.Now()
	dHello := e.Deploy(hostdtest.Template("hello"), "hello", map[string]string{"UPSTREAM": echoURL})
	dProbe := e.Deploy(hostdtest.Fixture("probe"), "probe", nil)
	pfiles, err := hostdtest.Build(hostdtest.Fixture("probe"), nil)
	if err != nil {
		t.Fatal(err)
	}
	pfiles["twd.jsonc"] = withTier(t, pfiles["twd.jsonc"], bundle.TierProcess)
	dProc := e.DeployFiles(pfiles, "probep")
	hello := e.WaitActive("hello", dHello, 90*time.Second)
	probe := e.WaitActive("probe", dProbe, 90*time.Second)
	probep := e.WaitActive("probep", dProc, 90*time.Second)
	res.set("deploy_to_all_active_ms", time.Since(tDeploy).Milliseconds())
	for _, ip := range []string{hello.IP, probe.IP, probep.IP} {
		waitPeer(ip)
	}

	wstatus := func(name string) hostdtestWS {
		for _, w := range e.Host.Status().Workloads {
			if w.Workload == name {
				return hostdtestWS{Tier: w.Tier, VM: w.VM}
			}
		}
		return hostdtestWS{}
	}

	t.Run("TierSelection", func(t *testing.T) {
		for name, want := range map[string]string{"hello": "microvm", "probe": "microvm", "probep": "process"} {
			ws := wstatus(name)
			if ws.Tier != want {
				t.Fatalf("%s: tier %q, want %q", name, ws.Tier, want)
			}
			if want == "microvm" {
				if ws.VM == nil || ws.VM.PID == 0 {
					t.Fatalf("%s: no VM info", name)
				}
				res.set("boot_"+name, map[string]any{"instance_start_to_guest_init_ms": ws.VM.Boot.Milliseconds(),
					"instance_start_to_workerd_ready_ms": ws.VM.Ready.Milliseconds(), "boot_call_total_ms": ws.VM.Total.Milliseconds(),
					"guest_uptime_at_ready_ms": ws.VM.GuestUptimeAtReady.Milliseconds()})
				t.Logf("%s VM: InstanceStart→init %v, →workerd ready %v, whole Boot() %v", name, ws.VM.Boot, ws.VM.Ready, ws.VM.Total)
			}
		}
	})

	t.Run("HelloVerifiedCaller", func(t *testing.T) {
		code, r := getJSON("http://" + hello.IP + "/")
		if code != 200 || get(r, "caller", "nodeId") != clientID {
			t.Fatalf("%d %v", code, r)
		}
		if n, _ := get(r, "upstream", "body", "caller", "node").(string); !strings.HasPrefix(n, hello.Hostname+".") {
			t.Fatalf("hello egress (from inside the guest) attributed to %q, want %s: %v", n, hello.Hostname, r)
		}
		res.set("hello_verified_caller", true)
	})

	objs := []string{"a", "b", "c"}
	objIP := map[string]string{}
	t.Run("ObjectsOwnNodesAndAttributedEgress", func(t *testing.T) {
		for _, n := range objs {
			code, r := getJSON("http://" + probe.IP + "/probe/" + n + "/")
			if code != 200 || r["name"] != n || get(r, "caller", "nodeId") != clientID {
				t.Fatalf("via workload %s: %d %v", n, code, r)
			}
		}
		attributed := 0
		for _, n := range objs {
			o := e.WaitObject("probe", "Probe", n, 60*time.Second)
			objIP[n] = o.IP
			waitPeer(o.IP)
			code, r := getJSON("http://" + o.IP + "/")
			if code != 200 || r["name"] != n || get(r, "caller", "nodeId") != clientID {
				t.Fatalf("direct %s: %d %v", n, code, r)
			}
			code, r = getJSON("http://" + o.IP + "/egress?url=" + echoURL)
			node, _ := get(r, "caller", "node").(string)
			if code != 200 || !strings.HasPrefix(node, o.Hostname+".") {
				t.Fatalf("%s egress from the guest: %d %v (want %s)", n, code, r, o.Hostname)
			}
			attributed++
		}
		// One object cannot claim another's identity from inside the VM.
		code, r := getJSON("http://" + objIP["a"] + "/forge-egress?as=b&url=" + echoURL)
		oa, _ := e.Host.Object(e.TenantID, "probe", "Probe", "a")
		if node, _ := get(r, "caller", "node").(string); code != 200 || !strings.HasPrefix(node, oa.Hostname+".") {
			t.Fatalf("forged egress: %d %v", code, r)
		}
		res.set("egress_attributed_per_object", fmt.Sprintf("%d/%d", attributed, len(objs)))
	})

	t.Run("RSSPerVM" /* before the memory dump, which faults in all guest RAM */, func(t *testing.T) {
		m := map[string]any{}
		for _, w := range []string{"hello", "probe"} {
			v := e.Host.Sandbox(e.TenantID, w)
			r, err := v.Op(ctx, proto.Msg{Op: "stats"})
			if err != nil {
				t.Fatal(err)
			}
			m[w] = map[string]any{"firecracker_rss_mb": v.RSSKB() / 1024, "guest_mem_total_mb": r.Stats.MemTotalKB / 1024,
				"guest_mem_used_mb": (r.Stats.MemTotalKB - r.Stats.MemAvailableKB) / 1024, "guest_workerd_rss_mb": r.Stats.WorkerdRSSKB / 1024}
		}
		res.set("rss", m)
		t.Logf("RSS: %v", m)
	})

	vmProbe := e.Host.Sandbox(e.TenantID, "probe")
	if vmProbe == nil {
		t.Fatal("no sandbox for probe")
	}

	t.Run("NoKeysInGuest", func(t *testing.T) {
		needles := keyNeedles(t, filepath.Join(e.Dir, "data"))
		res.set("key_needles", len(needles))
		// Disks: the rootfs image and the config drive, byte for byte.
		for _, d := range vmProbe.Disks() {
			if hit := scanFile(t, d, needles); hit != "" {
				t.Fatalf("key material (%s) on guest disk %s", hit, d)
			}
		}
		// The guest's filesystem tree (tmpfs included): nothing that looks
		// like tailscale state.
		r, err := vmProbe.Op(ctx, proto.Msg{Op: "fsscan"})
		if err != nil || len(r.Found) > 0 {
			t.Fatalf("fsscan: %v %v", r.Found, err)
		}
		// Guest memory: a full Firecracker snapshot of RAM (workerd heap,
		// page cache, tmpfs). Positive control: the instance's egress MAC key
		// is in there, so the dump is real.
		dir := t.TempDir()
		st := time.Now()
		mem, err := vmProbe.MemoryDump(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		dumpMS := time.Since(st).Milliseconds()
		mac := macKey(t, e, "probe")
		if hit := scanFile(t, mem, map[string][]byte{"mac-key (positive control)": []byte(mac)}); hit == "" {
			t.Fatal("positive control failed: the MAC key is not in the memory dump")
		}
		if hit := scanFile(t, mem, needles); hit != "" {
			t.Fatalf("key material (%s) in guest memory", hit)
		}
		fi, _ := os.Stat(mem)
		os.Remove(mem)
		res.set("no_keys_in_guest", map[string]any{"needles": len(needles), "fsscan_files": r.Scanned, "memdump_mb": fi.Size() >> 20,
			"memdump_ms": dumpMS, "disks": vmProbe.Disks()})
		t.Logf("%d needles (tskey-, the test auth key, %d node/machine private keys as text and raw bytes): none in %d guest files, 2 disks or %d MiB of guest RAM",
			len(needles), (len(needles)-2)/2, r.Scanned, fi.Size()>>20)
		// After the pause/resume the guest still serves.
		if code, _ := getJSON("http://" + objIP["a"] + "/"); code != 200 {
			t.Fatalf("after snapshot: %d", code)
		}
	})

	t.Run("GuestCannotReachHostNetwork", func(t *testing.T) {
		// A canary on every host interface: nothing from the guest may reach it.
		canary, err := net.Listen("tcp", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		defer canary.Close()
		var hits atomic.Int64
		go func() {
			for {
				c, err := canary.Accept()
				if err != nil {
					return
				}
				hits.Add(1)
				c.Close()
			}
		}()
		cport := canary.Addr().(*net.TCPAddr).Port
		targets := []string{
			fmt.Sprintf("%s:22", proto.GatewayIP), fmt.Sprintf("%s:80", proto.GatewayIP), fmt.Sprintf("%s:%d", proto.GatewayIP, cport),
			fmt.Sprintf("127.0.0.1:%d", cport), "10.0.2.2:22", "169.254.169.254:80", "1.1.1.1:443", "192.168.72.3:80",
			echo.IP4 + ":80",
		}
		addrs, _ := net.InterfaceAddrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
				targets = append(targets, fmt.Sprintf("%s:%d", ipn.IP, cport), fmt.Sprintf("%s:22", ipn.IP))
			}
		}
		r, err := vmProbe.Op(ctx, proto.Msg{Op: "probe", Targets: targets})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, p := range r.Results {
			out[p.Target] = p.Result
			if p.Result == "connected" {
				t.Errorf("guest reached %s", p.Target)
			}
		}
		if hits.Load() != 0 {
			t.Errorf("host canary got %d connections from the guest", hits.Load())
		}
		// Through the platform path too: netd's egress guard refuses the
		// host and loopback with a typed error (SEC-4 from inside a VM).
		typed := map[string]string{}
		for _, u := range []string{fmt.Sprintf("http://127.0.0.1:%d/", cport), fmt.Sprintf("http://10.0.2.15:%d/", cport), "http://169.254.169.254/", "http://example.com/"} {
			code, rr := getJSON("http://" + objIP["a"] + "/egress?url=" + u)
			typed[u] = fmt.Sprintf("%d %v", code, rr["code"])
			if code == 200 {
				t.Errorf("egress to %s allowed: %v", u, rr)
			}
		}
		res.set("guest_network_probes", map[string]any{"raw_tcp": out, "refused_at_gateway": len(vmProbe.Refused()), "canary_hits": hits.Load(), "via_egress_proxy": typed})
		t.Logf("raw TCP from the guest: %v; via the egress proxy: %v", out, typed)
	})

	t.Run("LatencyVsProcessTier", func(t *testing.T) {
		// Object names are per tenant and class (fakemgmt namespace = class),
		// so the process-tier copy uses its own name.
		pa := map[string]string{}
		for _, n := range []string{"pa"} {
			code, r := getJSON("http://" + probep.IP + "/probe/" + n + "/")
			if code != 200 {
				t.Fatalf("process tier probe: %d %v", code, r)
			}
			o, err := e.WaitObjectErr("probep", "Probe", n, 60*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			waitPeer(o.IP)
			pa["a"] = o.IP
		}
		lat := map[string]any{}
		for name, url := range map[string]string{
			"vm_object":             "http://" + objIP["a"] + "/",
			"process_object":        "http://" + pa["a"] + "/",
			"vm_object_egress":      "http://" + objIP["a"] + "/egress?url=" + echoURL,
			"process_object_egress": "http://" + pa["a"] + "/egress?url=" + echoURL,
			"vm_workload_hello":     "http://" + hello.IP + "/",
		} {
			lat[name] = measure(t, hc, url, 200)
		}
		res.set("latency_ms", lat)
		t.Logf("latency: %v", lat)
	})

	t.Run("KillObjectFailsFast", func(t *testing.T) {
		o, _ := e.Host.Object(e.TenantID, "probe", "Probe", "c")
		br, closeFn := openStream(t, hc, "http://"+objIP["c"]+"/stream")
		defer closeFn()
		failed := watchFail(br)
		st := time.Now()
		if killed, err := e.FM.Kill(ctx, o.ID, true, "test kill"); err != nil || len(killed) != 1 {
			t.Fatal(killed, err)
		}
		select {
		case ft := <-failed:
			ttf := ft.Sub(st)
			res.set("kill_object_stream_ttf_ms", float64(ttf.Microseconds())/1000)
			t.Logf("streaming caller (into the VM) failed %v after the mgmt kill", ttf)
			if ttf > 500*time.Millisecond {
				t.Fatalf("time to failure %v", ttf)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stream survived the kill")
		}
		qc := client.HTTPClient(5 * time.Second)
		rs := time.Now()
		_, err := qc.Get("http://" + objIP["c"] + "/")
		d := time.Since(rs)
		res.set("kill_object_new_request_ms", float64(d.Microseconds())/1000)
		if err == nil || d > time.Second {
			t.Fatalf("new request after kill: err=%v after %v", err, d)
		}
	})

	t.Run("KillVMFailsFast", func(t *testing.T) {
		old := e.Host.Sandbox(e.TenantID, "probe")
		br, closeFn := openStream(t, hc, "http://"+objIP["b"]+"/stream")
		defer closeFn()
		failed := watchFail(br)
		st := time.Now()
		old.Kill() // SIGKILL the VMM: a guest crash
		select {
		case ft := <-failed:
			ttf := ft.Sub(st)
			res.set("kill_vm_stream_ttf_ms", float64(ttf.Microseconds())/1000)
			t.Logf("streaming caller failed %v after the VMM was killed", ttf)
			if ttf > 500*time.Millisecond {
				t.Fatalf("time to failure %v", ttf)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stream survived the VM kill")
		}
		// While the VM restarts, requests fail fast with a typed error
		// (or already succeed on the new VM), never hang.
		rs := time.Now()
		code, r := getJSON("http://" + objIP["b"] + "/")
		d := time.Since(rs)
		res.set("kill_vm_next_request", map[string]any{"ms": d.Milliseconds(), "status": code, "code": r["code"]})
		t.Logf("request right after the VM kill: %d %v in %v", code, r["code"], d)
		if code != 200 && d > 2*time.Second {
			t.Fatalf("request after VM kill took %v", d)
		}
		// hostd restarts the VM; the object's node (and its identity) is
		// unchanged and serves again.
		deadline := time.Now().Add(60 * time.Second)
		for {
			nv := e.Host.Sandbox(e.TenantID, "probe")
			if nv != nil && nv != old {
				if code, r := getJSON("http://" + objIP["b"] + "/"); code == 200 && r["name"] == "b" {
					res.set("vm_restart_to_serving_ms", time.Since(st).Milliseconds())
					t.Logf("VM restarted and serving %v after the kill", time.Since(st))
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("VM did not come back")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

type hostdtestWS struct {
	Tier string
	VM   *sandbox.Info
}

func fakeStatic() (c fakemgmt.Config) {
	c.StaticKey = nettest.DummyKey
	return c
}

func get(m map[string]any, path ...string) any {
	var v any = m
	for _, p := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[p]
	}
	return v
}

func withTier(t *testing.T, twd []byte, tier string) []byte {
	var m map[string]any
	if err := bundle.UnmarshalJSONC(twd, &m); err != nil {
		t.Fatal(err)
	}
	m["tier"] = tier
	b, _ := json.Marshal(m)
	return b
}

func openStream(t *testing.T, hc *http.Client, url string) (*bufio.Reader, func()) {
	req, _ := http.NewRequest("GET", url, nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	return br, func() { resp.Body.Close() }
}

func watchFail(br *bufio.Reader) <-chan time.Time {
	failed := make(chan time.Time, 1)
	go func() {
		for {
			if _, err := br.ReadString('\n'); err != nil {
				failed <- time.Now()
				return
			}
		}
	}()
	return failed
}

func measure(t *testing.T, hc *http.Client, url string, n int) map[string]float64 {
	for i := 0; i < 10; i++ { // warm up
		if r, err := hc.Get(url); err == nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
	}
	ds := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		st := time.Now()
		r, err := hc.Get(url)
		if err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatalf("%s: %d", url, r.StatusCode)
		}
		ds = append(ds, float64(time.Since(st).Microseconds())/1000)
	}
	sort.Float64s(ds)
	return map[string]float64{"p50": ds[n/2], "p95": ds[n*95/100], "p99": ds[n*99/100], "n": float64(n)}
}

// keyNeedles collects what must never appear in a guest: the "tskey-"
// prefix, the test auth key, and every node and machine private key that
// host netd holds (from its tsnet state files), as text and as raw bytes.
func keyNeedles(t *testing.T, dataDir string) map[string][]byte {
	n := map[string][]byte{"tskey- prefix": []byte("tskey-"), "test auth key": []byte(nettest.DummyKey)}
	re := regexp.MustCompile(`privkey:[0-9a-f]{64}`)
	var add func(b []byte, depth int)
	add = func(b []byte, depth int) {
		for _, k := range re.FindAll(b, -1) {
			raw, _ := hex.DecodeString(string(k[8:]))
			if bytes.Equal(raw, make([]byte, len(raw))) {
				continue // an unset (zero) key slot is not a secret
			}
			n["privkey text "+string(k[8:16])] = append([]byte(nil), k...)
			n["privkey raw "+string(k[8:16])] = raw
		}
		if depth > 2 {
			return
		}
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			for _, v := range m {
				if s, ok := v.(string); ok {
					if d, err := base64.StdEncoding.DecodeString(s); err == nil {
						add(d, depth+1)
					}
				}
			}
		}
	}
	files := 0
	filepath.WalkDir(dataDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && strings.HasSuffix(p, ".state") {
			if b, err := os.ReadFile(p); err == nil {
				files++
				add(b, 0)
			}
		}
		return nil
	})
	if len(n) < 4 {
		t.Fatalf("positive control failed: found no private keys in %d host state files under %s", files, dataDir)
	}
	return n
}

// scanFile reports the first needle found in path ("" if none).
func scanFile(t *testing.T, path string, needles map[string][]byte) string {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const chunk = 16 << 20
	const overlap = 128
	buf := make([]byte, chunk+overlap)
	keep := 0
	for {
		n, err := io.ReadFull(f, buf[keep:])
		data := buf[:keep+n]
		for name, nd := range needles {
			if len(nd) > 0 && bytes.Contains(data, nd) {
				return name
			}
		}
		if err != nil {
			return ""
		}
		keep = copy(buf, data[len(data)-overlap:])
	}
}

func macKey(t *testing.T, e *hostdtest.Env, workload string) string {
	for _, w := range e.Host.Status().Workloads {
		if w.Workload == workload {
			b, err := os.ReadFile(filepath.Join(w.InstanceDir, "config.capnp"))
			if err != nil {
				t.Fatal(err)
			}
			m := regexp.MustCompile(`name = "MAC_KEY", text = "([0-9a-f]+)"`).FindSubmatch(b)
			if m == nil {
				t.Fatal("no MAC_KEY in config")
			}
			return string(m[1])
		}
	}
	t.Fatal("no workload " + workload)
	return ""
}

func writeResults(t *testing.T, r *results) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m["passed"] = !t.Failed()
	b, _ := json.MarshalIndent(r.m, "", "  ")
	os.MkdirAll("results", 0o755)
	p := filepath.Join("results", "m8-"+time.Now().UTC().Format("20060102T150405Z")+".json")
	os.WriteFile(p, b, 0o644)
	t.Logf("results: %s\n%s", p, b)
}
