//go:build e2ereal

// Real-tailnet check of the tssvc mechanism: a tagged tsnet node hosts a
// Tailscale Service with in-process delivery, and the backend sees the
// caller's real tailnet identity (WhoIs on RemoteAddr).
//
//	TWD_E2E_API_KEY_FILE=~/.config/tailnet-agents-design/tailnet1_api.env \
//	TWD_E2E_TAILNET=tail7906d5.ts.net go test -tags e2ereal -run TestRealService -v ./internal/tssvc/
//
// The tailnet policy needs autoApprovers.services "svc:ttest-tssvc":
// ["tag:ttest"] and a grant tag:ttest → svc:ttest-tssvc tcp:80.
package tssvc_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/internal/tssvc"
	"tailscale.com/tsnet"
)

var allowedTailnets = map[string]bool{"tail7906d5.ts.net": true}

const svcName = "svc:ttest-tssvc"

func TestRealService(t *testing.T) {
	keyFile, tailnet := os.Getenv("TWD_E2E_API_KEY_FILE"), os.Getenv("TWD_E2E_TAILNET")
	if keyFile == "" || tailnet == "" {
		t.Skip("TWD_E2E_API_KEY_FILE / TWD_E2E_TAILNET not set")
	}
	if !allowedTailnets[tailnet] {
		t.Fatalf("refusing to run against tailnet %q", tailnet)
	}
	apiKey, err := tsapi.LoadEnvFile(keyFile, "TS_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	api := &tsapi.Client{Tailnet: tailnet, APIKey: apiKey}
	tracker := tsapi.NewTracker(api)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	b := make([]byte, 3)
	rand.Read(b)
	run := hex.EncodeToString(b)

	if _, err := api.PutService(ctx, tsapi.Service{Name: svcName, Comment: "twd tssvc e2e " + run, Ports: []string{"tcp:80"}}); err != nil {
		t.Fatalf("put service: %v", err)
	}
	t.Cleanup(func() {
		cctx, cc := context.WithTimeout(context.Background(), time.Minute)
		defer cc()
		if err := api.DeleteService(cctx, svcName); err != nil {
			t.Errorf("delete service: %v", err)
		}
		rep := tracker.Cleanup(cctx, "ttest-tssvc-"+run)
		t.Logf("cleanup: %+v", rep)
	})

	node := func(name string) *tsnet.Server {
		k, err := api.CreateKey(ctx, tsapi.KeyOptions{Tags: []string{"tag:ttest"}, Ephemeral: true, Preauthorized: true, ExpirySeconds: 600, Description: "twd tssvc e2e"})
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		t.Cleanup(func() { api.DeleteKey(context.Background(), k.ID) })
		s := &tsnet.Server{Hostname: name, Dir: t.TempDir(), AuthKey: k.Secret, Ephemeral: true,
			ControlURL: "https://controlplane.tailscale.com", Logf: func(string, ...any) {}}
		st, err := s.Up(ctx)
		if err != nil {
			t.Fatalf("up %s: %v", name, err)
		}
		tracker.Track(string(st.Self.ID), name)
		t.Cleanup(func() { s.Close() })
		return s
	}
	host := node("ttest-tssvc-" + run + "-a")
	client := node("ttest-tssvc-" + run + "-b")
	hlc, _ := host.LocalClient()
	hlc.OmitAuth = true
	clc, _ := client.LocalClient()
	cst, _ := clc.StatusWithoutPeers(ctx)

	// Backend: answers with the caller's WhoIs and which path it came in on.
	serve := func(ln net.Listener, via string) {
		go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wi, err := hlc.WhoIs(r.Context(), r.RemoteAddr)
			if err != nil {
				http.Error(w, "whois: "+err.Error(), 500)
				return
			}
			fmt.Fprintf(w, "%s %s %s", via, wi.Node.StableID, r.RemoteAddr)
		}))
	}
	own, err := host.Listen("tcp", ":80")
	if err != nil {
		t.Fatal(err)
	}
	serve(own, "node")
	h := tssvc.NewHost(host, hlc)
	cl := tssvc.NewConnListener(tssvc.Addr{Name: svcName})
	h.Route(80, cl.Deliver)
	serve(cl, "service")

	st := time.Now()
	if err := h.Advertise(ctx, svcName, 80); err != nil {
		t.Fatalf("advertise: %v", err)
	}
	ads, _ := h.Advertised(ctx)
	t.Logf("advertised: %v", ads)

	fqdn := tssvc.Label(svcName) + "." + tailnet
	hc := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DialContext: client.Dial, DisableKeepAlives: true}}
	get := func() (string, error) {
		resp, err := hc.Get("http://" + fqdn + "/")
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("%d %s", resp.StatusCode, body)
		}
		return string(body), nil
	}
	waitFor := func(what string, d time.Duration, ok func(string, error) bool) (string, time.Duration) {
		t0 := time.Now()
		var last string
		var lastErr error
		for time.Since(t0) < d {
			last, lastErr = get()
			if ok(last, lastErr) {
				return last, time.Since(t0)
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("%s: timed out after %v (last %q, %v)", what, d, last, lastErr)
		return "", 0
	}
	body, _ := waitFor("serve via service", 3*time.Minute, func(s string, err error) bool { return err == nil })
	t.Logf("first service response after %v: %q", time.Since(st), body)
	f := strings.Fields(body)
	if len(f) != 3 || f[0] != "service" {
		t.Fatalf("response did not come through the service path: %q", body)
	}
	if f[1] != string(cst.Self.ID) {
		t.Fatalf("WhoIs identity = %s, want the client's node %s", f[1], cst.Self.ID)
	}
	t.Logf("caller identity preserved through the service VIP: %s (src %s)", f[1], f[2])

	// Withdraw: the VIP must stop answering.
	wst := time.Now()
	if err := h.Withdraw(ctx, svcName); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	_, d := waitFor("stop after withdraw", 2*time.Minute, func(s string, err error) bool { return err != nil })
	t.Logf("service stopped answering %v after withdraw (%v total)", d, time.Since(wst))

	// Re-advertise: back again.
	rst := time.Now()
	if err := h.Advertise(ctx, svcName, 80); err != nil {
		t.Fatal(err)
	}
	waitFor("serve after re-advertise", 3*time.Minute, func(s string, err error) bool { return err == nil })
	t.Logf("service answering again %v after re-advertise", time.Since(rst))
	if err := h.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	ads, _ = h.Advertised(ctx)
	if len(ads) != 0 {
		t.Fatalf("advertisements left after Close: %v", ads)
	}
}
