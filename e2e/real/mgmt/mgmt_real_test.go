//go:build e2ereal

// Package mgmtreal runs twd-mgmt against tailnet 1 (production control):
// mgmt mints a single-use tag:ttest key → a tsnet node joins with it → a
// cascade kill deletes the device via the API → the device is verified gone.
//
// Safety (BUILD-PLAN): tailnet allow-list; policy is only READ (spawn check);
// node names are ttest-m2-<runid>-*; keys are never printed; cleanup deletes
// only the device and key IDs this run created (never a prefix/tag sweep —
// another engineer runs e2e on the same tailnet).
package mgmtreal

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/mgmt"
	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/tsnet"
)

const prodControl = "https://controlplane.tailscale.com"

var allowedTailnets = map[string]bool{"tail7906d5.ts.net": true}

type results struct {
	Run     string             `json:"run"`
	Suite   string             `json:"suite"`
	Tailnet string             `json:"tailnet"`
	Date    string             `json:"date"`
	Checks  map[string]string  `json:"checks"`
	Numbers map[string]float64 `json:"numbers"`
	Cleanup map[string]any     `json:"cleanup"`
	mu      sync.Mutex
}

func (r *results) check(t *testing.T, name string, ok bool, detail string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ok {
		r.Checks[name] = "PASS"
	} else {
		r.Checks[name] = "FAIL: " + detail
		t.Errorf("%s: %s", name, detail)
	}
	return ok
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func TestMgmtRealMintJoinKill(t *testing.T) {
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
	prefix := "ttest-m2-" + run // class slug → object IDs/hostnames "ttest-m2-<run>-<id10>"
	res := &results{Run: run, Suite: "m2-mgmt", Tailnet: tailnet, Date: time.Now().Format(time.RFC3339),
		Checks: map[string]string{}, Numbers: map[string]float64{}, Cleanup: map[string]any{}}
	root, _ := nettest.FindRepoRoot()
	resultsTo := filepath.Join(root, "e2e", "real", "results", "m2-"+time.Now().Format("20060102-150405")+"-"+run+".json")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("/tmp", "twd-m2-real-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o700)

	var logBuf bytes.Buffer
	log := slog.New(netd.NewRedactingHandler(slog.NewTextHandler(&logBuf, nil)))
	store, err := mgmt.OpenStore(ctx, filepath.Join(dir, "mgmt.db"))
	if err != nil {
		t.Fatal(err)
	}
	custody, err := mgmt.OpenFileCustody(filepath.Join(dir, "custody"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := mgmt.New(mgmt.Config{Store: store, Custody: custody, Broker: &mgmt.Broker{}, IDSecret: bytes.Repeat([]byte{9}, 32),
		ReapInterval: time.Hour, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()

	// Cleanup: exactly the devices and keys this run created.
	var trackedDevices []string
	var nodes []*tsnet.Server
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		for _, n := range nodes {
			n.Close()
		}
		m.Close()
		objs, _ := store.ListObjects(cctx, mgmt.ObjectFilter{})
		var keyIDs []string
		for _, o := range objs {
			if o.StableNodeID != "" && !contains(trackedDevices, o.StableNodeID) {
				trackedDevices = append(trackedDevices, o.StableNodeID)
			}
		}
		recs, _ := store.ListAudit(cctx, mgmt.AuditFilter{Chain: "mgmt"})
		for _, r := range recs {
			if r.Kind == "mint" {
				var d struct{ KeyID string }
				json.Unmarshal(r.Detail, &d)
				keyIDs = append(keyIDs, d.KeyID)
			}
		}
		var errs []string
		for _, id := range trackedDevices {
			if err := api.DeleteDevice(cctx, id); err != nil {
				errs = append(errs, "device "+id+": "+err.Error())
			}
		}
		for _, id := range keyIDs {
			if err := api.DeleteKey(cctx, id); err != nil {
				errs = append(errs, "key "+id+": "+err.Error())
			}
		}
		left := 0
		if ds, err := api.ListDevices(cctx); err == nil {
			for _, d := range ds {
				if contains(trackedDevices, d.NodeID) || strings.HasPrefix(d.Hostname, prefix) {
					left++
				}
			}
		}
		res.Cleanup = map[string]any{"devices_tracked": len(trackedDevices), "keys_minted": len(keyIDs), "devices_left": left, "errors": errs}
		if strings.Contains(logBuf.String(), "tskey-") {
			res.Checks["no_key_in_logs"] = "FAIL"
			t.Error("a key reached the logs")
		} else {
			res.Checks["no_key_in_logs"] = "PASS"
		}
		store.Close()
		os.RemoveAll(dir)
		out, _ := json.MarshalIndent(res, "", "  ")
		os.MkdirAll(filepath.Dir(resultsTo), 0o755)
		os.WriteFile(resultsTo, out, 0o644)
		t.Logf("results: %s\n%s", resultsTo, out)
		if left != 0 {
			t.Errorf("cleanup left %d devices", left)
		}
	}()

	// Serve the real API on a unix socket and drive it like hostd/CLI would.
	sock := filepath.Join(dir, "run", "mgmt.sock")
	ln, err := mgmt.ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	hostToken := mgmt.NewHostToken()
	srv := &http.Server{Handler: mgmt.LocalHandler((&mgmt.Server{M: m}).Handler(), hostToken)}
	go srv.Serve(ln)
	defer srv.Close()
	hc := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	hostID := "h-" + run
	call := func(method, path string, body, out any) int {
		var rd io.Reader
		if body != nil {
			bb, _ := json.Marshal(body)
			rd = bytes.NewReader(bb)
		}
		req, _ := http.NewRequestWithContext(ctx, method, "http://twd-mgmt"+path, rd)
		if rd != nil {
			ct := "application/json"
			if method == "PATCH" {
				ct = "application/merge-patch+json"
			}
			req.Header.Set("Content-Type", ct)
		}
		req.Header.Set("Twd-Host-ID", hostID)
		req.Header.Set(mgmt.HostTokenHeader, hostToken)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			t.Logf("%s %s → %d %s", method, path, resp.StatusCode, rb) // error bodies never contain keys
		} else if out != nil {
			json.Unmarshal(rb, out)
		}
		if resp.StatusCode/100 == 2 {
			return 200 // every success is "ok" to the checks below (201/202 included)
		}
		return resp.StatusCode
	}
	var tn struct{ TenantID string }
	if call("POST", "/v1/tenants", map[string]any{"tailnet": tailnet, "controlURL": prodControl, "mintMode": "apikey", "apiKeyFile": keyFile}, &tn) != 200 {
		t.Fatal("tenant")
	}
	call("PUT", "/v1/hosts/"+hostID, map[string]any{"capacityNodes": 4, "tiers": []string{"process"}}, nil)
	var dep mgmt.DeployResponse
	if call("POST", "/v1/deployments", map[string]any{"tenantID": tn.TenantID, "workload": prefix,
		"namespaces": []mgmt.ClassConfig{{Class: prefix, Tags: []string{"tag:ttest"}, Agent: true}}}, &dep) != 200 {
		t.Fatal("deploy")
	}

	// Root agent (never joins: its unused key must be revoked by the kill)
	// and an ephemeral sub-agent that joins. The spawn reads (never writes)
	// tailnet 1's policy for the ⊆ check.
	var rootObj, child mgmt.CreateObjectResponse
	if call("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "workload": prefix, "namespace": prefix, "objectName": "root"}, &rootObj) != 200 {
		t.Fatal("create root")
	}
	t0 := time.Now()
	st := call("POST", "/v1/objects", map[string]any{"tenantID": tn.TenantID, "workload": prefix, "namespace": prefix, "objectName": "child",
		"parent": map[string]string{"objectID": rootObj.ObjectID}, "ttl": "10m"}, &child)
	res.check(t, "spawn_checked_against_real_policy", st == 200, "spawn status "+itoa(st))
	res.Numbers["spawn_check_ms"] = ms(time.Since(t0))
	if st != 200 {
		return
	}
	res.check(t, "hostname_prefixed", strings.HasPrefix(child.Hostname, prefix+"-"), child.Hostname)

	// Credential release: fenced by epoch.
	var cred mgmt.Credential
	res.check(t, "stale_epoch_rejected", call("POST", "/v1/objects/"+child.ObjectID+"/credentials?epoch=99", nil, nil) == 409, "want 409 lease_lost")
	t1 := time.Now()
	if call("POST", "/v1/objects/"+child.ObjectID+"/credentials?epoch="+itoa(int(dep.Epoch))+"&wait=30", nil, &cred) != 200 || cred.AuthKey == "" {
		t.Fatal("credential")
	}
	res.Numbers["create_to_credential_ms"] = ms(time.Since(t1))
	res.check(t, "key_ephemeral_for_ttl_subagent", cred.Ephemeral, "child key not ephemeral")

	// Join with the minted key.
	node := &tsnet.Server{Hostname: cred.Hostname, Dir: filepath.Join(dir, "node"), ControlURL: prodControl,
		AuthKey: cred.AuthKey, Ephemeral: cred.Ephemeral, Logf: func(string, ...any) {}, UserLogf: func(string, ...any) {}}
	cred.AuthKey = ""
	nodes = append(nodes, node)
	t2 := time.Now()
	stt, err := node.Up(ctx)
	if !res.check(t, "node_joined_with_minted_key", err == nil, errStr(err)) {
		return
	}
	res.Numbers["join_ms"] = ms(time.Since(t2))
	stable := string(stt.Self.ID)
	trackedDevices = append(trackedDevices, stable)
	dev, err := api.GetDevice(ctx, stable)
	res.check(t, "device_tagged_ttest", err == nil && len(dev.Tags) == 1 && dev.Tags[0] == "tag:ttest", errStr(err)+" tags="+strings.Join(dev.Tags, ","))
	if call("PATCH", "/v1/objects/"+child.ObjectID, map[string]any{"status": "ready", "nodeID": stable, "stableNodeID": stable,
		"magicDNS": strings.TrimSuffix(stt.Self.DNSName, "."), "epoch": dep.Epoch}, nil) != 200 {
		t.Fatal("ready")
	}
	// The single-use key is spent: a second credential fetch is refused.
	res.check(t, "credential_not_rereleased", call("POST", "/v1/objects/"+child.ObjectID+"/credentials?epoch="+itoa(int(dep.Epoch)), nil, nil) == 409, "")

	// Cascade kill from the root.
	var kr mgmt.KillResult
	t3 := time.Now()
	call("DELETE", "/v1/objects/"+rootObj.ObjectID+"?cascade=true&reason=e2e", nil, &kr)
	res.Numbers["kill_api_ms"] = ms(time.Since(t3))
	res.check(t, "cascade_killed_both", len(kr.Killed) == 2, strings.Join(kr.Killed, ","))
	res.check(t, "kill_deleted_device", len(kr.DevicesDeleted) == 1 && kr.DevicesDeleted[0] == child.ObjectID, strings.Join(kr.DevicesDeleted, ","))
	gone := false
	for i := 0; i < 20 && !gone; i++ {
		_, err := api.GetDevice(ctx, stable)
		gone = tsapi.IsNotFound(err)
		if !gone {
			time.Sleep(500 * time.Millisecond)
		}
	}
	res.Numbers["kill_to_device_404_ms"] = ms(time.Since(t3))
	res.check(t, "device_verified_gone", gone, "device still present via API")
	if ds, err := api.ListDevices(ctx); err == nil {
		present := false
		for _, d := range ds {
			present = present || d.NodeID == stable
		}
		res.check(t, "device_absent_from_list", !present, "still listed")
	}
	var hb mgmt.HeartbeatResponse
	call("PUT", "/v1/hosts/"+hostID+"/status", map[string]any{"liveNodes": 0, "leases": []any{}}, &hb)
	res.check(t, "revocation_to_host", len(hb.RevokeObjects) == 2, strings.Join(hb.RevokeObjects, ","))

	var aud struct {
		Items    []mgmt.AuditRecord
		Verified bool
	}
	call("GET", "/v1/audit?object="+child.ObjectID, nil, &aud)
	var kinds []string
	for _, r := range aud.Items {
		kinds = append(kinds, r.Kind)
	}
	res.check(t, "audit_chain", aud.Verified && strings.Join(kinds, ",") == "spawn,mint,deny,credential,ready,kill", strings.Join(kinds, ","))
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func itoa(n int) string { return jsonNum(n) }

func jsonNum(n int) string { b, _ := json.Marshal(n); return string(b) }

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
