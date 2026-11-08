//go:build e2ereal

package real

// M3 on tailnet 1: twd-hostd + fakemgmt (apikey mode via internal/tsapi)
// on production control. hello: verified caller + workload egress; probe:
// 3 DO objects, each with its own node and attributed egress; kill. Every
// node is ephemeral tag:ttest named ttest-m3-<run>-*; cleanup deletes exactly
// the device IDs and key IDs this run created. No policy is read or written.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
)

func TestRealHostd(t *testing.T) {
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
	b := make([]byte, 3)
	rand.Read(b)
	run := hex.EncodeToString(b)
	prefix := "ttest-m3-" + run + "-"
	root, _ := nettest.FindRepoRoot()
	res := &results{Run: "m3-" + run, Tailnet: tailnet, Date: time.Now().Format(time.RFC3339), Checks: map[string]string{},
		Numbers: map[string]float64{}, Cleanup: map[string]any{}}
	res.resultsTo = filepath.Join(root, "e2e", "real", "results", "m3-"+time.Now().Format("20060102-150405")+"-"+run+".json")
	quiet := slog.New(netd.NewRedactingHandler(slog.NewTextHandler(io.Discard, nil)))
	testMinter := tsapi.MintSource(api, quiet)
	tracker := tsapi.NewTracker(api)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	e := hostdtest.Start(t, hostdtest.Options{
		ControlURL: prodControl, Ephemeral: true, HostnamePrefix: prefix, HeartbeatEvery: time.Second,
		KillTombstone: 10 * time.Second,
		Fake:          fakemgmt.Config{ForceTags: []string{testTag}},
		Tenant:        mgmtapi.Tenant{Tailnet: tailnet, ControlURL: prodControl, MintMode: "apikey", APIKeyFile: keyFile},
	})
	var nodes []*nettest.Node
	// Cleanup (registered after hostdtest's, so it runs FIRST): track every
	// node this run created, close everything, delete exactly those devices,
	// revoke exactly the minted keys.
	t.Cleanup(func() {
		for _, o := range e.Host.Status().Objects {
			tracker.Track(o.NodeID, o.Hostname)
		}
		for id, obj := range e.FM.NodeIDs() {
			tracker.Track(id, obj)
		}
		e.Host.Close()
		for _, n := range nodes {
			n.Close()
		}
		cctx, ccancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer ccancel()
		crep := tracker.Cleanup(cctx, prefix)
		var revErrs []string
		minted := len(testMinter.KeyIDs())
		if err := testMinter.RevokeAll(cctx); err != nil {
			revErrs = append(revErrs, netd.Redact(err.Error()))
		}
		for _, m := range e.FM.Minters() {
			minted += len(m.KeyIDs())
			if err := m.RevokeAll(cctx); err != nil {
				revErrs = append(revErrs, netd.Redact(err.Error()))
			}
		}
		res.Cleanup["devices_tracked"] = crep.Tracked
		res.Cleanup["devices_deleted"] = crep.Deleted
		res.Cleanup["devices_left"] = crep.Left
		res.Cleanup["untracked_run_devices"] = crep.Untracked
		res.Cleanup["delete_errors"] = crep.Errors
		res.Cleanup["keys_minted"] = minted
		res.Cleanup["keys_revoke_errors"] = revErrs
		os.MkdirAll(filepath.Dir(res.resultsTo), 0o755)
		out, _ := json.MarshalIndent(res, "", "  ")
		out = []byte(netd.Redact(string(out)))
		os.WriteFile(res.resultsTo, out, 0o644)
		t.Logf("results: %s\n%s", res.resultsTo, out)
		if crep.Left != 0 || len(crep.Untracked) > 0 {
			t.Errorf("cleanup left devices: %+v", crep)
		}
	})

	mkNode := func(role string) *nettest.Node {
		k, err := testMinter.AuthKey(ctx, netd.KeyRequest{ObjectID: role, Tags: []string{testTag}, Ephemeral: true})
		if err != nil {
			t.Fatalf("mint %s: %v", role, netd.RedactError(err))
		}
		n, err := nettest.NewNode(ctx, prodControl, k, prefix+role, filepath.Join(e.Dir, role))
		if err != nil {
			t.Fatalf("%s: %v", role, netd.RedactError(err))
		}
		nodes = append(nodes, n)
		if st, err := n.LC.StatusWithoutPeers(ctx); err == nil && st.Self != nil {
			tracker.Track(string(st.Self.ID), role)
		}
		return n
	}
	client := mkNode("client")
	echo := mkNode("echo")
	if err := echo.ServeEcho(); err != nil {
		t.Fatal(err)
	}
	st, _ := client.LC.StatusWithoutPeers(ctx)
	clientID := string(st.Self.ID)
	hc := client.HTTPClient(30 * time.Second)
	getJSON := func(u string) (int, map[string]any, time.Duration) {
		s := time.Now()
		resp, err := hc.Get(u)
		if err != nil {
			return 0, map[string]any{"err": err.Error()}, time.Since(s)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			m = map[string]any{"raw": string(b)}
		}
		return resp.StatusCode, m, time.Since(s)
	}
	waitPeer := func(ip string) {
		wctx, c := context.WithTimeout(ctx, 60*time.Second)
		defer c()
		if err := client.WaitPeers(wctx, ip); err != nil {
			t.Fatal(err)
		}
	}
	echoURL := "http://" + prefix + "echo/"
	nodeOf := func(r map[string]any, path ...string) string {
		var v any = r
		for _, p := range path {
			m, _ := v.(map[string]any)
			v = m[p]
		}
		s, _ := v.(string)
		return s
	}

	st0 := time.Now()
	dHello := e.Deploy(hostdtest.Template("hello"), "hello", map[string]string{"UPSTREAM": echoURL})
	dProbe := e.Deploy(hostdtest.Fixture("probe"), "probe", nil)
	hello := e.WaitActive("hello", dHello, 2*time.Minute)
	res.num("hello_deploy_to_active_ms", msOf(time.Since(st0)))
	probe := e.WaitActive("probe", dProbe, 2*time.Minute)
	res.num("probe_deploy_to_active_ms", msOf(time.Since(st0)))
	waitPeer(hello.IP)
	waitPeer(probe.IP)

	t.Run("HelloVerifiedCaller", func(t *testing.T) {
		code, r, d := getJSON("http://" + hello.IP + "/")
		callerOK := code == 200 && nodeOf(r, "caller", "nodeId") == clientID
		egressOK := strings.HasPrefix(nodeOf(r, "upstream", "body", "caller", "node"), hello.Hostname+".")
		res.check("hello_verified_caller", callerOK, fmt.Sprint(code, r))
		res.check("hello_egress_is_workload_node", egressOK, fmt.Sprint(r))
		res.num("hello_first_request_ms", msOf(d))
		if !callerOK || !egressOK {
			t.Fatalf("%d %v", code, r)
		}
	})

	t.Run("ThreeObjectsOwnNodesAndEgress", func(t *testing.T) {
		names := []string{"a", "b", "c"}
		for _, n := range names {
			code, r, d := getJSON("http://" + probe.IP + "/probe/" + n + "/")
			ok := code == 200 && r["name"] == n && nodeOf(r, "caller", "nodeId") == clientID
			res.check("via_workload_before_node_"+n, ok, fmt.Sprint(code, r))
			res.num("via_workload_first_request_"+n+"_ms", msOf(d))
		}
		cst := time.Now()
		for _, n := range names {
			o, err := e.WaitObjectErr("probe", "Probe", n, 2*time.Minute)
			if err != nil {
				res.check("object_node_ready_"+n, false, err.Error())
				t.Fatal(err)
			}
			res.num("object_node_ready_"+n+"_ms_since_first_request", msOf(time.Since(cst)))
			tracker.Track(o.NodeID, o.Hostname)
			waitPeer(o.IP)
			code, r, _ := getJSON("http://" + o.IP + "/")
			ok := code == 200 && r["name"] == n && nodeOf(r, "caller", "nodeId") == clientID
			res.check("object_direct_ingress_"+n, ok, fmt.Sprint(code, r))
			code, r, d := getJSON("http://" + o.IP + "/egress?url=" + echoURL)
			eok := code == 200 && strings.HasPrefix(nodeOf(r, "caller", "node"), o.Hostname+".")
			res.check("object_egress_attributed_"+n, eok, fmt.Sprint(code, r))
			res.num("object_egress_"+n+"_ms", msOf(d))
			if !ok || !eok {
				t.Errorf("%s: ingress %v egress %v", n, ok, eok)
			}
		}
	})

	t.Run("KillFailsFast", func(t *testing.T) {
		o, ok := e.Host.Object(e.TenantID, "probe", "Probe", "c")
		if !ok {
			t.Skip("object c missing")
		}
		resp, err := hc.Get("http://" + o.IP + "/stream")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		br.ReadString('\n')
		failed := make(chan time.Time, 1)
		go func() {
			for {
				if _, err := br.ReadString('\n'); err != nil {
					failed <- time.Now()
					return
				}
			}
		}()
		ks := time.Now()
		if _, err := e.FM.Kill(ctx, o.ID, false, "e2e-real"); err != nil {
			t.Fatal(err)
		}
		res.num("kill_mgmt_call_ms", msOf(time.Since(ks)))
		select {
		case ft := <-failed:
			ttf := ft.Sub(ks)
			res.num("kill_streaming_caller_ttf_ms", msOf(ttf))
			res.check("kill_caller_fails_fast", ttf < time.Second, ttf.String())
		case <-time.After(10 * time.Second):
			res.check("kill_caller_fails_fast", false, "stream survived 10s")
			t.Error("stream survived the kill")
		}
		qc := client.HTTPClient(5 * time.Second)
		rs := time.Now()
		_, err = qc.Get("http://" + o.IP + "/")
		res.num("kill_new_request_fail_ms", msOf(time.Since(rs)))
		res.Notes = append(res.Notes, "new request after kill: "+fmt.Sprint(err))
		res.check("kill_new_request_fails", err != nil, "succeeded")
		code, r, _ := getJSON("http://" + probe.IP + "/probe/c/")
		res.check("kill_via_workload_typed_killed", code == 410 && r["code"] == "killed", fmt.Sprint(code, r))
	})

	for k, v := range res.Checks {
		if strings.HasPrefix(v, "FAIL") {
			t.Errorf("%s: %s", k, v)
		}
	}
}
