//go:build e2esvc

// E2E-SVC on tailnet 1, driven only through the twd CLI: a workload with
// twd.jsonc tailnet.service. twd up (apikey) → deploy → mgmt created the
// Service definition (owned by twd) → a tag:ttest client reaches the
// workload through the Service VIP by name, with its own identity →
// redeploy keeps the same name and VIP → twd kill of the workload node
// stops the Service → twd down → zero leftover devices and keys.
//
//	TWD_E2E_API_KEY_FILE=~/.config/tailnet-agents-design/tailnet1_api.env TWD_E2E_TAILNET=tail7906d5.ts.net \
//	  go test -tags e2esvc -count=1 -v -timeout 20m ./e2e/svc/
//
// Policy (tailnet 1, added by hand; twd never writes autoApprovers):
// tagOwners tag:twd-wl-svc; autoApprovers.services "svc:ttest-svc-hello":
// ["tag:twd-wl-svc"]; grant tag:ttest → svc:ttest-svc-hello, tag:twd-wl-svc
// tcp:80. Cleanup deletes exactly this run's IDs and the Service.
package svc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/e2e/cliflow"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
)

const (
	prodControl = "https://controlplane.tailscale.com"
	svcName     = "svc:ttest-svc-hello"
	wlTag       = "tag:twd-wl-svc"
)

var allowedTailnets = map[string]bool{"tail7906d5.ts.net": true}

type depRes struct {
	DeploymentID string `json:"deploymentID"`
	Endpoint     struct {
		Node     string `json:"node"`
		MagicDNS string `json:"magicDNS"`
		Service  *struct {
			Name     string `json:"name"`
			MagicDNS string `json:"magicDNS"`
		} `json:"service"`
	} `json:"endpoint"`
	Preflight struct {
		OK      bool   `json:"ok"`
		Service string `json:"service"`
	} `json:"preflight"`
	Timings map[string]float64 `json:"timingsMs"`
}

func TestE2EService(t *testing.T) {
	keyFile, tailnet := os.Getenv("TWD_E2E_API_KEY_FILE"), os.Getenv("TWD_E2E_TAILNET")
	if keyFile == "" || tailnet == "" {
		t.Skip("TWD_E2E_API_KEY_FILE / TWD_E2E_TAILNET not set")
	}
	if !allowedTailnets[tailnet] {
		t.Fatalf("refusing to run against tailnet %q (not in the allow list)", tailnet)
	}
	wd, err := nettest.WorkerdBinary()
	if err != nil {
		t.Skip(err)
	}
	t.Setenv("TWD_WORKERD", wd)
	apiKey, err := tsapi.LoadEnvFile(keyFile, "TS_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	api := &tsapi.Client{Tailnet: tailnet, APIKey: apiKey}
	b := make([]byte, 3)
	rand.Read(b)
	run := hex.EncodeToString(b)
	testPrefix, prefix, _ := cliflow.RunPrefixes("svc", run)
	res := cliflow.NewResults("svc-"+run, tailnet)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if _, err := api.GetService(ctx, svcName); err == nil {
		t.Fatalf("%s already exists on %s (a previous run's leftover?); delete it first", svcName, tailnet)
	}

	quiet := slog.New(netd.NewRedactingHandler(slog.NewTextHandler(io.Discard, nil)))
	minter := tsapi.MintSource(api, quiet)
	tracker := tsapi.NewTracker(api)
	bin := cliflow.BuildBinaries(t)
	state := filepath.Join("/tmp", "twd-e2e-svc-"+run)
	work := t.TempDir()
	e := &cliflow.Env{T: t, BinDir: bin, StateDir: state, WorkDir: work, Tailnet: tailnet, APIKeyFile: keyFile,
		ControlURL: prodControl, Prefix: prefix, Res: res, ClientTagged: true}
	var client *nettest.Node
	downDone := false
	t.Cleanup(func() {
		if !downDone {
			e.Twd("down", "--purge")
		}
		if t.Failed() {
			for _, c := range []string{"mgmt", "hostd"} {
				b, _ := os.ReadFile(filepath.Join(state, "logs", c+".log"))
				t.Logf("%s log (tail):\n%s", c, netd.Redact(string(b[max(0, len(b)-6000):])))
			}
		}
		if client != nil {
			client.Close()
		}
		cctx, cc := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cc()
		devs, keys := cliflow.Leftovers(cctx, api, res)
		for _, id := range res.NodeIDs {
			tracker.Track(id, "twd")
		}
		for _, id := range res.InfraNodeIDs {
			tracker.Track(id, "twd-infra")
		}
		crep := tracker.Cleanup(cctx, testPrefix)
		for _, k := range keys {
			api.DeleteKey(cctx, k)
		}
		minter.RevokeAll(cctx)
		// twd keeps the Service definition across undeploys (a stable VIP);
		// this run created it, so it removes it.
		if err := api.DeleteService(cctx, svcName); err != nil {
			t.Errorf("delete service: %v", err)
		}
		os.RemoveAll(state)
		out, _ := json.MarshalIndent(res, "", "  ")
		t.Logf("results:\n%s", netd.Redact(string(out)))
		if len(devs) > 0 || len(keys) > 0 {
			t.Errorf("after twd down: leftover twd devices %v, valid keys %d", devs, len(keys))
		}
		if crep.Left != 0 || len(crep.Untracked) > 0 {
			t.Errorf("cleanup left devices: %+v", crep)
		}
	})

	// twd up (apikey mode), like E2E-MAC.
	var up struct {
		TenantID   string `json:"tenantID"`
		MgmtNodeID string `json:"mgmtNodeID"`
	}
	e.MustJSON(&up, "up", "--tailnet", tailnet, "--api-key-file", keyFile, "--yes", "--hostname-prefix", prefix,
		"--control-url", prodControl, "--mgmt-hostname", prefix+"mgmt", "--host-id", strings.TrimSuffix(prefix, "-"), "--ephemeral")
	e.TrackInfra(up.MgmtNodeID)

	// The app: hello with its own tag and a Service.
	dir := filepath.Join(work, "svchello")
	e.MustJSON(nil, "init", dir, "--template", "hello")
	tj := filepath.Join(dir, "twd.jsonc")
	cfg := []byte(`{
  "name": "svchello",
  "tailnet": {
    "tags": ["` + wlTag + `"],
    // the Tailscale Service the workload node hosts
    "service": "` + strings.TrimPrefix(svcName, "svc:") + `"
  }
}
`)
	if err := os.WriteFile(tj, cfg, 0o644); err != nil {
		t.Fatal(err)
	}

	var dep depRes
	d := e.MustJSON(&dep, "deploy", dir)
	res.Numbers["deploy_ms"] = float64(d.Milliseconds())
	if !dep.Preflight.OK || dep.Preflight.Service != svcName {
		t.Fatalf("preflight %+v", dep.Preflight)
	}
	res.Pass("preflight_service_approved", svcName+" in autoApprovers for "+wlTag)
	if dep.Endpoint.Service == nil || dep.Endpoint.Service.Name != svcName {
		t.Fatalf("deploy endpoint has no service: %+v", dep.Endpoint)
	}
	e.TrackObjectNodes()
	def, err := api.GetService(ctx, svcName)
	if err != nil {
		t.Fatalf("mgmt did not create %s: %v", svcName, err)
	}
	if !strings.HasPrefix(def.Comment, "managed by twd: tenant=") || !slices.Contains(def.Ports, "tcp:80") || len(def.Addrs) == 0 {
		t.Fatalf("service definition %+v", def)
	}
	res.Pass("service_definition", def.Comment+" "+strings.Join(def.Addrs, ","))

	// A tag:ttest client.
	k, err := minter.AuthKey(ctx, netd.KeyRequest{ObjectID: "client", Tags: []string{cliflow.TestTag}, Ephemeral: true})
	if err != nil {
		t.Fatal(netd.RedactError(err))
	}
	if client, err = nettest.NewNode(ctx, prodControl, k, testPrefix+"client", filepath.Join(work, "client")); err != nil {
		t.Fatal(err)
	}
	cst, _ := client.LC.StatusWithoutPeers(ctx)
	tracker.Track(string(cst.Self.ID), "client")
	clientID := string(cst.Self.ID)

	url := "http://" + dep.Endpoint.Service.MagicDNS + "/"
	get := func(d time.Duration) (code int, raddr string, body map[string]any, err error) {
		req, _ := http.NewRequest("GET", url, nil)
		req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
			raddr = i.Conn.RemoteAddr().String()
		}}))
		hc := &http.Client{Timeout: d, Transport: &http.Transport{DialContext: client.Dial, DisableKeepAlives: true}}
		resp, err := hc.Do(req)
		if err != nil {
			return 0, raddr, nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		json.Unmarshal(b, &body)
		return resp.StatusCode, raddr, body, nil
	}
	isVIP := func(raddr string) bool {
		for _, a := range def.Addrs {
			if strings.HasPrefix(raddr, a+":") || strings.HasPrefix(raddr, "["+a+"]:") {
				return true
			}
		}
		return false
	}
	waitServed := func(what string, d time.Duration) (time.Duration, map[string]any, string) {
		t.Helper()
		t0 := time.Now()
		var lastErr error
		var lastCode int
		for time.Since(t0) < d {
			code, raddr, body, err := get(5 * time.Second)
			if err == nil && code == 200 {
				return time.Since(t0), body, raddr
			}
			lastErr, lastCode = err, code
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("%s: %s not served within %v (last %d %v)", what, url, d, lastCode, lastErr)
		return 0, nil, ""
	}

	// 1. Through the VIP, with the caller's identity.
	wait, body, raddr := waitServed("first call", 3*time.Minute)
	res.Numbers["service_first_ok_ms"] = float64(wait.Milliseconds())
	if !isVIP(raddr) {
		t.Fatalf("%s answered from %s, not a Service VIP %v", url, raddr, def.Addrs)
	}
	caller, _ := body["caller"].(map[string]any)
	if caller["nodeId"] != clientID {
		t.Fatalf("caller via the Service = %v, want %s", caller, clientID)
	}
	res.Pass("service_caller_identity", "VIP "+raddr+" caller "+clientID)
	t.Logf("served through %s (VIP %s) %v after deploy, caller %s", svcName, raddr, wait.Round(time.Millisecond), clientID)

	// 2. Redeploy: same name, same VIP, still served.
	var dep2 depRes
	e.MustJSON(&dep2, "deploy", dir)
	e.TrackObjectNodes()
	def2, err := api.GetService(ctx, svcName)
	if err != nil || !slices.Equal(def2.Addrs, def.Addrs) {
		t.Fatalf("VIPs changed across redeploy: %v → %v (%v)", def.Addrs, def2.Addrs, err)
	}
	wait, _, raddr = waitServed("after redeploy", 2*time.Minute)
	if !isVIP(raddr) {
		t.Fatalf("after redeploy answered from %s", raddr)
	}
	res.Pass("redeploy_stable_vip", strings.Join(def2.Addrs, ","))
	t.Logf("redeploy: same VIPs, served again %v after the deploy returned", wait.Round(time.Millisecond))

	// 3. twd kill of the workload node: the Service stops.
	var kr map[string]any
	e.MustJSON(&kr, "kill", dep2.Endpoint.Node, "--reason", "e2e-svc")
	killAt := time.Now()
	stopped := false
	for time.Since(killAt) < 60*time.Second {
		if _, _, _, err := get(3 * time.Second); err != nil {
			stopped = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !stopped {
		t.Fatalf("%s still served 60 s after twd kill of its workload node", url)
	}
	res.Numbers["service_stopped_after_kill_ms"] = float64(time.Since(killAt).Milliseconds())
	res.Pass("kill_stops_service", time.Since(killAt).Round(time.Millisecond).String())
	t.Logf("service stopped %v after twd kill", time.Since(killAt).Round(time.Millisecond))

	e.Down()
	downDone = true
	time.Sleep(2 * time.Second)
	if _, err := api.GetService(ctx, svcName); err != nil {
		t.Fatalf("service definition gone after twd down (it should be kept: stable VIP): %v", err)
	}
}
