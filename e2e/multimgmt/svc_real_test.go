//go:build e2emultimgmt && e2ereal

// H12 on a real tailnet: two twd-mgmt replicas on one Postgres, each on its
// own tsnet node (tag:twd-mgmt) on tailnet 1, both hosting the Tailscale
// Service svc:ttest-mgmt (-tsnet-service). A tag:ttest client calls the
// Service name only. Checks:
//
//   - the Service answers with the caller's own WhoIs identity;
//
//   - when the serving replica's store fails, it withdraws (ServiceGate)
//     and fresh connections fail over to the other replica;
//
//   - it re-advertises once its store is back;
//
//   - a SIGTERMed replica withdraws before exiting and traffic moves.
//
//     TWD_E2E_API_KEY_FILE=~/.config/tailnet-agents-design/tailnet1_api.env TWD_E2E_TAILNET=tail7906d5.ts.net \
//     go test -tags 'e2emultimgmt e2ereal' -run TestRealMgmtService -v -timeout 20m ./e2e/multimgmt/
//
// Policy (tailnet 1): autoApprovers.services "svc:ttest-mgmt": ["tag:twd-mgmt"]
// and a grant tag:ttest → svc:ttest-mgmt tcp:80 (with twd.dev/cap/admin).
// Cleanup deletes exactly this run's devices, keys and the Service.
package multimgmt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/netd/nettest"
)

const (
	realMgmtSvc  = "svc:ttest-mgmt"
	prodControl  = "https://controlplane.tailscale.com"
	realTailnet1 = "tail7906d5.ts.net"
)

func TestRealMgmtService(t *testing.T) {
	keyFile, tailnet := os.Getenv("TWD_E2E_API_KEY_FILE"), os.Getenv("TWD_E2E_TAILNET")
	if keyFile == "" || tailnet == "" {
		t.Skip("TWD_E2E_API_KEY_FILE / TWD_E2E_TAILNET not set")
	}
	if tailnet != realTailnet1 {
		t.Fatalf("refusing to run against tailnet %q (not in the allow list)", tailnet)
	}
	apiKey, err := tsapi.LoadEnvFile(keyFile, "TS_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	api := &tsapi.Client{Tailnet: tailnet, APIKey: apiKey}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	b := make([]byte, 3)
	rand.Read(b)
	runID := hex.EncodeToString(b)
	prefix := "ttest-mm-" + runID + "-"
	tracker := tsapi.NewTracker(api)
	var keyIDs []string

	r := newRun(t, "svc-real")
	c := newCluster(t, r, 2)

	if _, err := api.PutService(ctx, tsapi.Service{Name: realMgmtSvc, Comment: "twd e2e H12 " + runID, Ports: []string{"tcp:80"}}); err != nil {
		t.Fatalf("put service: %v", err)
	}
	t.Cleanup(func() {
		cctx, cc := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cc()
		for i := range c.replicas {
			c.stop(i, syscall.SIGTERM)
		}
		// This run's nodes, by their unique hostnames, then deleted by ID.
		if devs, err := api.ListDevices(cctx); err == nil {
			for _, d := range devs {
				if strings.HasPrefix(d.Hostname, prefix) || strings.HasPrefix(d.Name, prefix) {
					tracker.Track(d.NodeID, d.Hostname)
				}
			}
		}
		rep := tracker.Cleanup(cctx, prefix)
		t.Logf("cleanup: %+v", rep)
		if rep.Left != 0 {
			t.Errorf("devices left: %+v", rep)
		}
		for _, k := range keyIDs {
			api.DeleteKey(cctx, k)
		}
		if err := api.DeleteService(cctx, realMgmtSvc); err != nil {
			t.Errorf("delete service: %v", err)
		}
	})
	mint := func(tag string) string {
		k, err := api.CreateKey(ctx, tsapi.KeyOptions{Tags: []string{tag}, Ephemeral: true, Preauthorized: true, ExpirySeconds: 1800, Description: "twd e2e H12"})
		if err != nil {
			t.Fatalf("mint %s: %v", tag, err)
		}
		keyIDs = append(keyIDs, k.ID)
		return k.Secret
	}
	c.argsFor = map[int][]string{}
	for i := range c.replicas {
		kf := filepath.Join(r.dir, fmt.Sprintf("r%d.authkey", i))
		if err := os.WriteFile(kf, []byte("TS_AUTHKEY="+mint("tag:twd-mgmt")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c.argsFor[i] = []string{"-tsnet-hostname", fmt.Sprintf("%sr%d", prefix, i), "-tsnet-control-url", prodControl,
			"-tsnet-authkey-file", kf, "-tsnet-service", realMgmtSvc, "-tsnet-tls=false"}
	}
	c.start(0)
	c.start(1)
	client, err := nettest.NewNode(ctx, prodControl, mint("tag:ttest"), prefix+"client", filepath.Join(r.dir, "client"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	cst, _ := client.LC.StatusWithoutPeers(ctx)
	clientID := string(cst.Self.ID)

	replicaID := func(i int) string {
		deadline := time.Now().Add(60 * time.Second)
		for {
			if b, err := os.ReadFile(filepath.Join(c.replicas[i].dir, "replica.id")); err == nil && len(strings.TrimSpace(string(b))) > 0 {
				return strings.TrimSpace(string(b))
			}
			if time.Now().After(deadline) {
				t.Fatalf("replica %d has no replica.id", i)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	ids := []string{replicaID(0), replicaID(1)}
	idx := map[string]int{ids[0]: 0, ids[1]: 1}

	base := "http://" + strings.TrimPrefix(realMgmtSvc, "svc:") + "." + tailnet
	// One request on a fresh connection (no pooling hides a routing change).
	call := func(path string, d time.Duration) (int, map[string]any, error) {
		hc := &http.Client{Timeout: d, Transport: &http.Transport{DialContext: client.Dial, DisableKeepAlives: true}}
		resp, err := hc.Get(base + path)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var m map[string]any
		json.Unmarshal(body, &m)
		return resp.StatusCode, m, nil
	}
	served := func() (int, error) { // which replica answered /v1/status
		code, m, err := call("/v1/status", 4*time.Second)
		if err != nil {
			return -1, err
		}
		if code != 200 {
			return -1, fmt.Errorf("status %d: %v", code, m)
		}
		rp, _ := m["replicas"].(map[string]any)
		id, _ := rp["id"].(string)
		i, ok := idx[id]
		if !ok {
			return -1, fmt.Errorf("unknown replica id %q", id)
		}
		return i, nil
	}
	// until waits for n consecutive answers from replica want (-1: any).
	until := func(what string, want, n int, d time.Duration) (int, time.Duration) {
		t.Helper()
		t0 := time.Now()
		streak, last := 0, -1
		var lastErr error
		for time.Since(t0) < d {
			i, err := served()
			switch {
			case err == nil && (want < 0 || i == want) && (streak == 0 || i == last):
				streak++
				last = i
			case err == nil && (want < 0 || i == want):
				streak, last = 1, i
			default:
				streak, lastErr = 0, err
			}
			if streak >= n {
				return last, time.Since(t0)
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("%s: not %d consecutive answers within %v (last err %v)", what, n, d, lastErr)
		return -1, 0
	}
	// 1. The Service answers, with the caller's identity.
	first, d := until("service up", -1, 3, 4*time.Minute)
	t.Logf("svc %s answering after %v (served by r%d)", realMgmtSvc, d.Round(time.Millisecond), first)
	code, who, err := call("/v1/whoami", 10*time.Second)
	if err != nil || code != 200 || who["nodeID"] != clientID {
		t.Fatalf("whoami via the Service: %d %v %v (want nodeID %s)", code, who, err, clientID)
	}
	t.Logf("whoami via VIP: nodeID %v kind %v admin %v", who["nodeID"], who["kind"], who["admin"])
	for i := range c.replicas {
		if countIn(r, i, "service advertised") == 0 {
			t.Fatalf("replica %d never advertised %s", i, realMgmtSvc)
		}
	}

	// 2. The serving replica's store fails: it withdraws, traffic moves.
	other := 1 - first
	c.proxy(first).set("reset", 0)
	failAt := time.Now()
	_, d = until("failover after store loss", other, 5, 3*time.Minute)
	t.Logf("store loss on r%d: 5 consecutive answers from r%d %v later", first, other, d.Round(time.Millisecond))
	if countIn(r, first, "service withdrawn") == 0 {
		t.Fatalf("r%d did not withdraw %s after losing its store", first, realMgmtSvc)
	}
	_ = failAt

	// 3. Store back: it advertises again.
	c.proxy(first).set("pass", 0)
	back := time.Now()
	for countIn(r, first, "service advertised") < 2 {
		if time.Since(back) > 90*time.Second {
			t.Fatalf("r%d did not re-advertise after its store came back", first)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("r%d re-advertised %v after its store came back", first, time.Since(back).Round(time.Millisecond))

	// 4. SIGTERM the replica serving now: it withdraws on the way out.
	now, _ := until("steady", -1, 3, time.Minute)
	c.stop(now, syscall.SIGTERM)
	stopAt := time.Now()
	_, d = until("failover after SIGTERM", 1-now, 5, 3*time.Minute)
	t.Logf("SIGTERM r%d: 5 consecutive answers from r%d %v later", now, 1-now, d.Round(time.Millisecond))
	_ = stopAt
}

func countIn(r *run, i int, s string) int {
	b, _ := os.ReadFile(filepath.Join(r.dir, fmt.Sprintf("r%d.log", i)))
	return strings.Count(string(b), s)
}
