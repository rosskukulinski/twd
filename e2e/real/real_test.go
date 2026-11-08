//go:build e2ereal

// Package real runs netd's identity / egress / kill suite on a real tailnet
// (Tailscale production control). It mints ephemeral, pre-authorized,
// single-use tag:ttest keys via the API, never prints them, and deletes every
// device and key it created afterwards. Dials go via tsnet only (this host's
// system DNS can't resolve the test tailnet's names).
//
//	TWD_E2E_API_KEY_FILE=~/.config/tailnet-agents-design/tailnet1_api.env \
//	TWD_E2E_TAILNET=tail7906d5.ts.net go test -tags e2ereal ./e2e/real/
package real

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
)

const (
	prodControl = "https://controlplane.tailscale.com"
	testTag     = "tag:ttest"
	testCap     = "example.com/cap/ttest"
)

// allowedTailnets are the only tailnets this suite may touch (BUILD-PLAN safety rule 2).
var allowedTailnets = map[string]bool{"tail7906d5.ts.net": true}

type results struct {
	Run       string             `json:"run"`
	Tailnet   string             `json:"tailnet"`
	Date      string             `json:"date"`
	Checks    map[string]string  `json:"checks"`
	Numbers   map[string]float64 `json:"numbers"`
	Notes     []string           `json:"notes"`
	Cleanup   map[string]any     `json:"cleanup"`
	mu        sync.Mutex
	resultsTo string
}

func (r *results) check(name string, ok bool, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := "PASS"
	if !ok {
		v = "FAIL: " + detail
	}
	r.Checks[name] = v
}

func (r *results) num(name string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Numbers[name] = v
}

func msOf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func pct(ds []time.Duration) (p50, p95, max float64) {
	if len(ds) == 0 {
		return
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return msOf(s[len(s)/2]), msOf(s[len(s)*95/100]), msOf(s[len(s)-1])
}

func TestRealTailnet(t *testing.T) {
	keyFile, tailnet := os.Getenv("TWD_E2E_API_KEY_FILE"), os.Getenv("TWD_E2E_TAILNET")
	if keyFile == "" || tailnet == "" {
		t.Skip("TWD_E2E_API_KEY_FILE / TWD_E2E_TAILNET not set")
	}
	if !allowedTailnets[tailnet] {
		t.Fatalf("refusing to run against tailnet %q (not in the allow list)", tailnet)
	}
	apiKey, err := tsapi.LoadEnvFile(keyFile, "TS_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	api := &tsapi.Client{Tailnet: tailnet, APIKey: apiKey}
	wdBin, err := nettest.WorkerdBinary()
	if err != nil {
		t.Fatal(err)
	}
	root, _ := nettest.FindRepoRoot()
	b := make([]byte, 3)
	rand.Read(b)
	run := hex.EncodeToString(b)
	prefix := "ttest-" + run
	res := &results{Run: run, Tailnet: tailnet, Date: time.Now().Format(time.RFC3339), Checks: map[string]string{},
		Numbers: map[string]float64{}, Cleanup: map[string]any{}}
	res.resultsTo = filepath.Join(root, "e2e", "real", "results", time.Now().Format("20060102-150405")+"-"+run+".json")
	minter := tsapi.MintSource(api, slog.New(netd.NewRedactingHandler(slog.NewTextHandler(io.Discard, nil))))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dir, err := os.MkdirTemp("/tmp", "twd-real-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o700)
	var closers []func()
	// Cleanup runs even on failure: close nodes, delete every device of this
	// run, revoke every minted key, write the results file.
	t.Cleanup(func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		devs, err := api.ListDevices(cctx)
		deleted := 0
		var delErrs []string
		if err != nil {
			delErrs = append(delErrs, "list: "+err.Error())
		}
		for _, d := range devs {
			if strings.HasPrefix(d.Hostname, prefix) || strings.HasPrefix(d.Name, prefix) {
				if err := api.DeleteDevice(cctx, d.NodeID); err != nil {
					delErrs = append(delErrs, d.Hostname+": "+err.Error())
				} else {
					deleted++
				}
			}
		}
		// Verify nothing of this run is left.
		left := 0
		if devs, err := api.ListDevices(cctx); err == nil {
			for _, d := range devs {
				if strings.HasPrefix(d.Hostname, prefix) {
					left++
				}
			}
		}
		revErr := minter.RevokeAll(cctx)
		res.Cleanup["devices_deleted_at_cleanup"] = deleted
		res.Cleanup["devices_left"] = left
		res.Cleanup["keys_minted"] = len(minter.KeyIDs())
		res.Cleanup["keys_revoke_error"] = fmt.Sprint(revErr)
		res.Cleanup["delete_errors"] = delErrs
		os.RemoveAll(dir)
		os.MkdirAll(filepath.Dir(res.resultsTo), 0o755)
		out, _ := json.MarshalIndent(res, "", "  ")
		out = []byte(netd.Redact(string(out)))
		os.WriteFile(res.resultsTo, out, 0o644)
		t.Logf("results: %s\n%s", res.resultsTo, out)
		if left > 0 {
			t.Errorf("%d devices of run %s left on the tailnet", left, run)
		}
	})

	// workerd + netd registry on production control.
	runDir := filepath.Join(dir, "run")
	os.MkdirAll(runDir, 0o700)
	egressSock := filepath.Join(runDir, "egress.sock")
	eln, err := net.Listen("unix", egressSock)
	if err != nil {
		t.Fatal(err)
	}
	wd, err := nettest.StartWorkerd(wdBin, filepath.Join(root, "netd", "testdata", "workerd", "config.capnp"), runDir, egressSock)
	if err != nil {
		t.Fatal(err)
	}
	closers = append(closers, wd.Stop)
	type delEv struct {
		id string
		at time.Time
	}
	deletes := make(chan delEv, 8)
	reg, err := netd.New(netd.Config{
		ControlURL: prodControl,
		StateRoot:  filepath.Join(dir, "state"),
		Keys:       minter,
		Tags:       []string{testTag},
		Ephemeral:  true,
		Hostname:   func(id string) string { return prefix + "-obj-" + id },
		Upstream:   netd.UnixUpstream(wd.IngressSock),
		OnDelete: func(ctx context.Context, in netd.ObjectInfo) error {
			err := api.DeleteDevice(ctx, in.NodeID)
			deletes <- delEv{in.ID, time.Now()}
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	closers = append(closers, func() {
		st := time.Now()
		reg.Close()
		res.num("registry_close_all_ms", msOf(time.Since(st)))
	})
	go http.Serve(eln, reg.EgressHandler())
	closers = append(closers, func() { eln.Close() })

	objects := []string{"a", "b", "c", "d", "e"}
	joinStart := time.Now()
	for _, id := range objects {
		if _, err := reg.Create(id, netd.ObjectOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// Client and echo nodes, also ephemeral tag:ttest.
	mkNode := func(role string) *nettest.Node {
		k, err := minter.AuthKey(ctx, netd.KeyRequest{ObjectID: role, Tags: []string{testTag}, Ephemeral: true})
		if err != nil {
			t.Fatalf("mint %s: %v", role, netd.RedactError(err))
		}
		st := time.Now()
		n, err := nettest.NewNode(ctx, prodControl, k, prefix+"-"+role, filepath.Join(dir, role))
		if err != nil {
			t.Fatalf("%s: %v", role, netd.RedactError(err))
		}
		res.num("join_"+role+"_ms", msOf(time.Since(st)))
		closers = append(closers, func() { n.Close() })
		return n
	}
	client := mkNode("client")
	echo := mkNode("echo")
	if err := echo.ServeEcho(); err != nil {
		t.Fatal(err)
	}
	ips := map[string]string{}
	var joinMS []time.Duration
	for _, id := range objects {
		o := reg.Get(id)
		if err := o.Ready(ctx); err != nil {
			t.Fatalf("object %s: %v", id, err)
		}
		ips[id] = o.IPv4().String()
		joinMS = append(joinMS, time.Duration(o.Info().JoinMS*float64(time.Millisecond)))
	}
	res.num("objects_all_ready_ms", msOf(time.Since(joinStart)))
	p50, _, mx := pct(joinMS)
	res.num("object_join_p50_ms", p50)
	res.num("object_join_max_ms", mx)
	all := []string{echo.IP4}
	for _, id := range objects {
		all = append(all, ips[id])
	}
	ws := time.Now()
	if err := client.WaitPeers(ctx, all...); err != nil {
		t.Fatal(err)
	}
	if err := echo.WaitPeers(ctx, ips["a"]); err != nil {
		t.Fatal(err)
	}
	res.num("client_sees_all_peers_ms", msOf(time.Since(ws)))
	st, _ := client.LC.StatusWithoutPeers(ctx)
	clientID := string(st.Self.ID)
	hc := client.HTTPClient(30 * time.Second)
	get := func(u string, hdr map[string]string) (int, http.Header, []byte, time.Duration) {
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		s := time.Now()
		resp, err := hc.Do(req)
		if err != nil {
			return 0, nil, []byte(err.Error()), time.Since(s)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, b, time.Since(s)
	}
	type objResp struct {
		Caller     *netd.Caller      `json:"caller"`
		ObjectID   string            `json:"objectId"`
		SawHeaders map[string]string `json:"sawHeaders"`
	}

	t.Run("IngressIdentity", func(t *testing.T) {
		for _, id := range objects {
			code, _, b, d := get("http://"+ips[id]+"/", nil)
			var r objResp
			json.Unmarshal(b, &r)
			ok := code == 200 && r.Caller != nil && r.Caller.NodeID == clientID && r.ObjectID == id &&
				contains(r.Caller.Tags, testTag) && len(r.Caller.CapMap[testCap]) > 0 && r.Caller.User == ""
			res.check("ingress_identity_"+id, ok, string(b))
			res.num("first_request_"+id+"_ms", msOf(d))
			if !ok {
				t.Errorf("%s: %d %s", id, code, b)
			} else if id == "a" {
				res.Notes = append(res.Notes, "caller capMap["+testCap+"] = "+string(r.Caller.CapMap[testCap][0]))
			}
		}
	})

	t.Run("ForgedHeadersStripped", func(t *testing.T) {
		code, _, b, _ := get("http://"+ips["a"]+"/", map[string]string{
			"x-twd-cf":     `{"tailscale":{"caller":{"nodeId":"nEVIL","tags":["tag:admin"]}},"objectId":"b"}`,
			"x-twd-object": "b", "cf-connecting-ip": "1.2.3.4", "tailnet-assertion": "x",
		})
		var r objResp
		json.Unmarshal(b, &r)
		ok := code == 200 && r.Caller != nil && r.Caller.NodeID == clientID && r.ObjectID == "a" && len(r.SawHeaders) == 1
		res.check("forged_headers_stripped", ok, string(b))
		if !ok {
			t.Error(string(b))
		}
	})

	t.Run("EgressAttribution", func(t *testing.T) {
		for _, id := range objects {
			code, _, b, _ := get("http://"+ips[id]+"/egress?url=http://"+prefix+"-echo/", nil)
			var r nettest.EchoReply
			json.Unmarshal(b, &r)
			ok := code == 200 && strings.HasPrefix(r.Caller.Node, prefix+"-obj-"+id+".") && len(r.Caller.Addrs) > 0 && r.Caller.Addrs[0] == ips[id]
			res.check("egress_attribution_"+id, ok, string(b))
			if !ok {
				t.Errorf("%s: %d %s", id, code, b)
			}
		}
		victim := map[string]string{"a": "b", "b": "a"}
		for id, v := range victim {
			code, _, b, _ := get("http://"+ips[id]+"/forge-egress?as="+v+"&url=http://"+echo.IP4+"/", nil)
			var r nettest.EchoReply
			json.Unmarshal(b, &r)
			ok := code == 200 && strings.HasPrefix(r.Caller.Node, prefix+"-obj-"+id+".")
			for k := range r.Headers {
				if strings.HasPrefix(strings.ToLower(k), "x-twd") {
					ok = false
				}
			}
			res.check("tenant_cannot_forge_egress_"+id, ok, string(b))
			if !ok {
				t.Errorf("forge %s as %s: %s", id, v, b)
			}
		}
	})

	t.Run("SelfSSRF", func(t *testing.T) {
		cases := map[string][2]string{
			"own_ip":        {"http://" + ips["a"] + "/", netd.CodeForbiddenDestination},
			"sibling_ip":    {"http://" + ips["b"] + "/", netd.CodeForbiddenDestination},
			"sibling_name":  {"http://" + prefix + "-obj-b/", netd.CodeForbiddenDestination},
			"sibling_fqdn":  {"http://" + prefix + "-obj-c." + tailnet + "/", netd.CodeForbiddenDestination},
			"loopback":      {"http://127.0.0.1/", netd.CodeForbiddenDestination},
			"loopback_v6":   {"http://[::1]/", netd.CodeForbiddenDestination},
			"link_local":    {"http://169.254.169.254/", netd.CodeForbiddenDestination},
			"quad100":       {"http://100.100.100.100/", netd.CodeForbiddenDestination},
			"public_ip":     {"http://1.1.1.1/", netd.CodeForbiddenDestination},
			"localhost":     {"http://localhost/", netd.CodePeerNotVisible},
			"public_name":   {"http://example.com/", netd.CodePeerNotVisible},
			"nonexistent":   {"http://" + prefix + "-nonexistent/", netd.CodePeerNotVisible},
			"unknown_cgnat": {"http://100.127.255.254/", netd.CodePeerNotVisible},
			"echo_short_ok": {"http://" + prefix + "-echo/", ""},
			"echo_fqdn_ok":  {"http://" + prefix + "-echo." + tailnet + "/", ""},
			"echo_by_ip_ok": {"http://" + echo.IP4 + "/", ""},
		}
		for name, c := range cases {
			code, h, b, _ := get("http://"+ips["a"]+"/egress?url="+c[0], nil)
			var ok bool
			if c[1] == "" {
				ok = code == 200
			} else {
				ok = code == 403 && h.Get(netd.HeaderError) == c[1]
			}
			res.check("ssrf_"+name, ok, fmt.Sprintf("%d %s", code, b))
			if !ok {
				t.Errorf("%s (%s): %d %s", name, c[0], code, b)
			}
		}
	})

	t.Run("Latency", func(t *testing.T) {
		series := func(name, u string) {
			for i := 0; i < 10; i++ {
				get(u, nil)
			}
			var ds []time.Duration
			for i := 0; i < 100; i++ {
				_, _, _, d := get(u, nil)
				ds = append(ds, d)
			}
			p50, p95, mx := pct(ds)
			res.num(name+"_p50_ms", p50)
			res.num(name+"_p95_ms", p95)
			res.num(name+"_max_ms", mx)
		}
		series("client_to_echo", "http://"+echo.IP4+"/")
		series("client_to_obj", "http://"+ips["a"]+"/")
		series("client_to_obj_to_echo", "http://"+ips["a"]+"/egress?url=http://"+echo.IP4+"/")
		cs, _ := client.LC.Status(ctx)
		for _, p := range cs.Peer {
			for _, a := range p.TailscaleIPs {
				if a.String() == ips["a"] || a.String() == echo.IP4 {
					path := "DERP(" + p.Relay + ")"
					if p.CurAddr != "" {
						path = "direct"
					}
					res.Notes = append(res.Notes, "client->"+p.HostName+" path: "+path)
				}
			}
		}
	})

	// Kill with logout (ephemeral: the device is removed at once), streaming caller.
	killAndMeasure := func(t *testing.T, id string, opts netd.KillOptions, label string) {
		resp, err := hc.Get("http://" + ips[id] + "/stream")
		if err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(resp.Body)
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		failed := make(chan time.Time, 1)
		go func() {
			for {
				if _, err := br.ReadString('\n'); err != nil {
					failed <- time.Now()
					return
				}
			}
		}()
		// Poll the client's netmap concurrently with the kill (10 ms) so the
		// removal time is measured from kill start, not from Kill returning.
		removed := make(chan time.Time, 1)
		killAt := time.Now()
		go func() {
			for time.Since(killAt) < 3*time.Minute {
				if !client.InNetmap(ctx, ips[id]) {
					removed <- time.Now()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			close(removed)
		}()
		opts.Reason = "e2e-real"
		rep, err := reg.Kill(ctx, id, opts)
		killDone := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		var ttf time.Duration = -1
		select {
		case ft := <-failed:
			ttf = ft.Sub(killAt)
		case <-time.After(10 * time.Second):
		}
		res.num(label+"_streaming_caller_ttf_ms", msOf(ttf))
		res.num(label+"_kill_total_ms", rep.TotalMS)
		res.num(label+"_kill_abort_ms", rep.AbortMS)
		res.num(label+"_kill_logout_ms", rep.LogoutMS)
		res.num(label+"_kill_close_ms", rep.CloseMS)
		res.num(label+"_kill_device_delete_ms", rep.DeleteMS)
		ok := ttf >= 0 && ttf < 500*time.Millisecond
		res.check(label+"_caller_fails_fast", ok, fmt.Sprintf("ttf %v report %+v", ttf, rep))
		if !ok {
			t.Errorf("time to failure %v (report %+v)", ttf, rep)
		}
		if rep.DeleteErr != "" {
			t.Errorf("device delete failed: %+v", rep)
		}
		if rep.LogoutErr != "" {
			res.Notes = append(res.Notes, label+": logout error (kill proceeded): "+rep.LogoutErr)
		}
		// Real control: time until the killed node leaves the client's netmap.
		if rt, ok := <-removed; ok {
			res.num(label+"_netmap_removal_ms", msOf(rt.Sub(killAt)))
			// Negative: removal was visible before Kill returned (it waits for
			// the device-delete API response).
			res.num(label+"_netmap_removal_after_kill_returned_ms", msOf(rt.Sub(killDone)))
		} else {
			res.num(label+"_netmap_removal_ms", -1)
			t.Errorf("node %s still in client netmap 3 min after kill", id)
		}
		// An object calling the killed node through netd egress gets a typed,
		// immediate peer_not_visible once its own netmap drops the node.
		qc := client.HTTPClient(3 * time.Second)
		for s := time.Now(); ; {
			var code int
			var h http.Header
			var b []byte
			rs := time.Now()
			resp, err := qc.Get("http://" + ips["a"] + "/egress?url=http://" + ips[id] + "/")
			d := time.Since(rs)
			if err == nil {
				code, h = resp.StatusCode, resp.Header
				b, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
			} else {
				b = []byte(err.Error())
				h = http.Header{}
			}
			if code == 403 && h.Get(netd.HeaderError) == netd.CodePeerNotVisible {
				res.num(label+"_object_egress_peer_not_visible_after_kill_ms", msOf(time.Since(killAt)))
				res.num(label+"_object_egress_refusal_latency_ms", msOf(d))
				res.check(label+"_object_egress_to_killed_is_peer_not_visible", true, "")
				break
			}
			if time.Since(s) > 30*time.Second {
				res.check(label+"_object_egress_to_killed_is_peer_not_visible", false, fmt.Sprintf("%d %s", code, b))
				t.Errorf("egress to killed %s: %d %s", id, code, b)
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		// A brand-new request after removal: how fast does it fail?
		sc := client.HTTPClient(5 * time.Second)
		s := time.Now()
		_, err = sc.Get("http://" + ips[id] + "/")
		res.num(label+"_new_request_after_removal_fail_ms", msOf(time.Since(s)))
		res.Notes = append(res.Notes, label+": new request after netmap removal: "+fmt.Sprint(err))
		if err == nil {
			t.Error("request to killed object succeeded")
		}
	}
	t.Run("KillWithLogout", func(t *testing.T) { killAndMeasure(t, "c", netd.KillOptions{Logout: true}, "kill_logout") })
	t.Run("KillDefault", func(t *testing.T) { killAndMeasure(t, "d", netd.KillOptions{}, "kill_default") })
	t.Run("KillCloseFirst", func(t *testing.T) { killAndMeasure(t, "e", netd.KillOptions{CloseFirst: true}, "kill_closefirst") })
	close(deletes)
	for ev := range deletes {
		res.Notes = append(res.Notes, "device delete hook ran for "+ev.id)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
