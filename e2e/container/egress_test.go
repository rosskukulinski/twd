//go:build e2econtainer && (linux || darwin)

package container

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd"
)

// internetStandIn is the hermetic public internet for E2E-CONTAINER:
// allowed.test resolves to a documentation address that the dialer
// connects to a local server; rebind.test resolves to a private one.
func internetStandIn(t *testing.T) (*hostd.ContainerInternet, func() []string) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "internet stand-in: %s %s", r.Host, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	var mu sync.Mutex
	var dialed []string
	return &hostd.ContainerInternet{
		Resolve: func(_ context.Context, h string) ([]netip.Addr, error) {
			switch h {
			case "allowed.test":
				return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
			case "rebind.test":
				return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
			}
			return nil, fmt.Errorf("no such host %s", h)
		},
		Dial: func(ctx context.Context, n, a string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, a)
			mu.Unlock()
			if a != "203.0.113.7:80" {
				return nil, fmt.Errorf("unreachable")
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
		},
	}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), dialed...)
	}
}

// probeScript tries every target from inside the container: "ok",
// "dns" (no address), or "refused"/"timeout"; UDP targets send a DNS
// query and wait for any answer.
const probeScript = `
import json, socket, sys
out = {}
for t in json.loads(sys.argv[1]):
    proto, host, port = t.split(":")
    try:
        if proto == "udp":
            s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(2)
            s.sendto(b"\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x07example\x03com\x00\x00\x01\x00\x01", (host, int(port)))
            s.recvfrom(512); out[t] = "ok"
        else:
            socket.create_connection((host, int(port)), timeout=3).close(); out[t] = "ok"
    except socket.gaierror: out[t] = "dns"
    except socket.timeout: out[t] = "timeout"
    except OSError as e: out[t] = "refused"
print(json.dumps(out))
`

// fetchScript GETs argv[1] (cafile argv[2] if given) and prints status and body.
const fetchScript = `
import json, ssl, sys, urllib.request
ctx = ssl.create_default_context(cafile=sys.argv[2]) if len(sys.argv) > 2 and sys.argv[2] else None
try:
    r = urllib.request.urlopen(urllib.request.Request(sys.argv[1], headers={"x-from-guest": "1", "x-twd-object": "forged"}), timeout=20, context=ctx)
    print(json.dumps({"status": r.status, "body": r.read().decode()}))
except Exception as e:
    print(json.dumps({"error": type(e).__name__ + ": " + str(e)}))
`

type egressEnv struct {
	call    func(path string, q ...string) map[string]any
	echoIP  string
	tenant  string
	objIP   func(name string) string
	dialed  func() []string
	host    *hostd.Host
	parentW string
}

func (ev *egressEnv) py(t *testing.T, obj, script string, args ...string) map[string]any {
	t.Helper()
	argv, _ := json.Marshal(append([]string{"python3", "-c", script}, args...))
	r := ev.call("/f/"+obj+"/exec", "argv", string(argv))
	if r["exitCode"] != float64(0) {
		t.Fatalf("python in %s: %v", obj, r)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(fmt.Sprint(r["stdout"])), &m); err != nil {
		t.Fatalf("python output %v", r)
	}
	return m
}

func testEgress(t *testing.T, ev *egressEnv, set func(string, any)) {
	ev.call("/f/eg/start")
	ev.call("/f/eg/exec", "argv", `["true"]`)
	parentIP := ev.objIP("eg")

	t.Run("DenyAll", func(t *testing.T) {
		targets := []string{"tcp:1.1.1.1:443", "tcp:1.1.1.1:80", "tcp:8.8.8.8:53", "udp:8.8.8.8:53", "udp:192.168.72.1:53",
			"tcp:" + ev.echoIP + ":80", "tcp:" + parentIP + ":80", "tcp:100.100.100.100:80", "tcp:192.168.72.1:3128",
			"tcp:192.168.72.1:3130", "tcp:192.168.72.1:22", "tcp:169.254.169.254:80", "tcp:10.0.0.1:80", "tcp:172.17.0.1:80",
			"tcp:example.com:443", "tcp:allowed.test:80", "tcp:metadata.google.internal:80"}
		b, _ := json.Marshal(targets)
		r := ev.py(t, "eg", probeScript, string(b))
		special := map[string]string{
			"udp:8.8.8.8:53":                  "timeout", // UDP is not routed at all
			"udp:192.168.72.1:53":             "ok",      // the proxy's DNS answers (NXDOMAIN here)
			"tcp:example.com:443":             "dns",
			"tcp:allowed.test:80":             "dns", // allowlisted, but enableInternet is off
			"tcp:metadata.google.internal:80": "dns",
		}
		for _, tg := range targets {
			got, want := r[tg], "refused"
			if w, ok := special[tg]; ok {
				want = w
			}
			if got != want {
				t.Errorf("%s: %v, want %s", tg, got, want)
			}
		}
		set("egress_deny_all", r)
		if d := ev.dialed(); len(d) != 0 {
			t.Fatalf("internet dialled: %v", d)
		}
	})

	t.Run("InterceptToParentIdentity", func(t *testing.T) {
		ev.call("/f/eg/icept", "addr", "api.internal", "relay", "http://"+ev.echoIP+"/")
		st := time.Now()
		r := ev.py(t, "eg", fetchScript, "http://api.internal/v1?x=1")
		set("intercept_round_trip_ms (guest→proxy→parent DO→echo node→back)", time.Since(st).Milliseconds())
		var body struct {
			FromParent string            `json:"fromParent"`
			URL        string            `json:"url"`
			Headers    map[string]string `json:"headers"`
			Echo       struct {
				Caller struct {
					Addrs []string `json:"addrs"`
				} `json:"caller"`
			} `json:"echo"`
		}
		if r["status"] != float64(200) || json.Unmarshal([]byte(fmt.Sprint(r["body"])), &body) != nil {
			t.Fatalf("%v", r)
		}
		if body.FromParent != "eg" || body.URL != "http://api.internal/v1?x=1" || body.Headers["x-twd-object"] != "" || body.Headers["x-from-guest"] != "1" {
			t.Fatalf("%+v", body)
		}
		ok := false
		for _, a := range body.Echo.Caller.Addrs {
			ok = ok || a == parentIP
		}
		if !ok {
			t.Fatalf("echo node saw %v, want the parent object %s", body.Echo.Caller.Addrs, parentIP)
		}
	})

	t.Run("InterceptHTTPS", func(t *testing.T) {
		ev.call("/f/eg/icept", "kind", "https", "addr", "secure.internal")
		r := ev.py(t, "eg", fetchScript, "https://secure.internal/token", "/etc/cloudflare/certs/cloudflare-containers-ca.crt")
		if r["status"] != float64(200) || !strings.Contains(fmt.Sprint(r["body"]), `"url":"https://secure.internal/token"`) {
			t.Fatalf("with the container CA: %v", r)
		}
		// The image's own trust store has the CA appended (if it has one).
		set("https_intercept_default_trust", ev.py(t, "eg", fetchScript, "https://secure.internal/token", ""))
	})

	t.Run("AllowlistedInternet", func(t *testing.T) {
		if r := ev.py(t, "eg", fetchScript, "http://allowed.test/x"); r["error"] == nil {
			t.Fatalf("enableInternet:false reached the internet: %v", r)
		}
		ev.call("/f/egi/start", "internet", "1")
		ev.call("/f/egi/exec", "argv", `["true"]`)
		if r := ev.py(t, "egi", fetchScript, "http://allowed.test/hello"); r["body"] != "internet stand-in: allowed.test /hello" {
			t.Fatalf("allowlisted: %v", r)
		}
		for _, u := range []string{"http://rebind.test/", "http://example.com/", "http://203.0.113.7/", "http://" + ev.echoIP + "/"} {
			if r := ev.py(t, "egi", fetchScript, u); r["error"] == nil {
				t.Fatalf("%s: %v", u, r)
			}
		}
		for _, d := range ev.dialed() {
			if d != "203.0.113.7:80" {
				t.Fatalf("dialled %s", d)
			}
		}
		ev.call("/f/egi/destroy")
	})

	t.Run("StrictKeyscan", func(t *testing.T) {
		rep, err := ev.host.ContainerCheck(context.Background(), ev.tenant, ev.parentW, "Box", "eg")
		if err != nil {
			t.Fatal(err)
		}
		k := rep.KeyScan
		set("container_keyscan", map[string]any{"needles": k.Needles, "kinds": k.NeedleKinds, "found": k.Found, "disks": k.Disks,
			"guest_files": k.GuestFiles, "ram_mib": k.MemoryMiB, "memdump_ms": k.MemdumpMS, "control": k.Control})
		if len(k.Found) != 0 || len(k.GuestStateF) != 0 {
			t.Fatalf("key material in the container guest: %v %v", k.Found, k.GuestStateF)
		}
		if !memoryDumpRequired && strings.Contains(k.Control, "guest RAM not scanned") {
			t.Logf("%d needles: none in %d guest files or %d disks; guest RAM NOT scanned on this backend: %s", k.Needles, k.GuestFiles, len(k.Disks), k.Control)
			return
		}
		if !strings.HasPrefix(k.Control, "guest canary found") || k.MemoryMiB < 128 {
			t.Fatalf("positive control: %s (%d MiB)", k.Control, k.MemoryMiB)
		}
	})
}

// testLimits: the instance limits hold inside the guest and the host
// doesn't notice (ADR-032 D5): memory exhaustion is an OOM in the guest,
// a full scratch disk is ENOSPC, a fork bomb stays in the VM.
func testLimits(t *testing.T, ev *egressEnv, vmAlive func(string) (int, bool), hostPids func() int, set func(string, any)) {
	ev.call("/f/lim/start", "instance", `{"vcpu":0.25,"memoryMib":256,"diskMb":256}`)
	if r := ev.call("/f/lim/exec", "argv", `["true"]`); r["exitCode"] != float64(0) {
		t.Fatalf("start: %v", r)
	}
	pid, _ := vmAlive("lim")
	alive := func(what string) {
		t.Helper()
		if p, ok := vmAlive("lim"); !ok || p != pid {
			t.Fatalf("after %s: the container VM is gone (%d → %d)", what, pid, p)
		}
		if r := ev.call("/f/lim/exec", "argv", `["echo","ok"]`); r["stdout"] != "ok\n" {
			t.Fatalf("after %s: container unusable: %v", what, r)
		}
	}
	t.Run("MemoryOOMInGuestOnly", func(t *testing.T) {
		r := ev.call("/f/lim/exec", "argv", `["python3","-c","b = bytearray(700 * 1024 * 1024); print(len(b))"]`)
		if r["exitCode"] == float64(0) {
			t.Fatalf("700 MiB allocated in a 256 MiB container: %v", r)
		}
		set("limit_memory_700mib_in_256mib", map[string]any{"exitCode": r["exitCode"], "stderr_tail": tail(fmt.Sprint(r["stderr"]), 120)})
		alive("OOM")
	})
	t.Run("DiskENOSPC", func(t *testing.T) {
		r := ev.call("/f/lim/exec", "argv", `["sh","-c","dd if=/dev/zero of=/workspace/fill bs=1M count=600 2>&1; df -m /workspace | tail -1"]`)
		out := fmt.Sprint(r["stdout"])
		if !strings.Contains(out, "No space left") {
			t.Fatalf("600 MB written to a 256 MB scratch disk: %v", r)
		}
		set("limit_disk_600mb_in_256mb", tail(out, 200))
		ev.call("/f/lim/exec", "argv", `["rm","-f","/workspace/fill"]`)
		alive("ENOSPC")
	})
	t.Run("ForkBombContained", func(t *testing.T) {
		before := hostPids()
		done := make(chan map[string]any, 1)
		go func() {
			done <- ev.call("/f/lim/exec", "argv", `["timeout","8","sh","-c","f() { f | f & }; f; sleep 10"]`)
		}()
		time.Sleep(3 * time.Second)
		during := hostPids()
		st := time.Now()
		r := ev.call("/f/eg/exec", "argv", `["echo","neighbour ok"]`)
		lat := time.Since(st)
		if r["stdout"] != "neighbour ok\n" || lat > 5*time.Second {
			t.Fatalf("another container during the fork bomb: %v in %v", r, lat)
		}
		<-done
		set("fork_bomb", map[string]any{"host_pids_before": before, "host_pids_during": during, "neighbour_exec_ms": lat.Milliseconds()})
		if during-before > 50 {
			t.Fatalf("host process count grew by %d during the guest fork bomb", during-before)
		}
		ev.call("/f/lim/destroy")
	})
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
