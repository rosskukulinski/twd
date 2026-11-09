//go:build e2ereal

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

// publicHTTP resolves names via 1.1.1.1 (this Mac's resolver can't see
// *.ts.net Funnel names) and fetches over the public internet.
func publicHTTP() *http.Client {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", "1.1.1.1:53")
	}}
	d := &net.Dialer{Timeout: 10 * time.Second, Resolver: r}
	return &http.Client{Timeout: 70 * time.Second, Transport: &http.Transport{DialContext: d.DialContext}}
}

// TestMgmtRealWIF: Funnel-served issuer → federated trust credentials →
// token exchange → mint → join → kill deletes the device with the
// lifecycle credential. Set TWD_E2E_SKIP_WIF=1 to skip (it takes ~1–2 min
// on a fresh Funnel cert).
func TestMgmtRealWIF(t *testing.T) {
	keyFile, tailnet := os.Getenv("TWD_E2E_API_KEY_FILE"), os.Getenv("TWD_E2E_TAILNET")
	if keyFile == "" || tailnet == "" {
		t.Skip("TWD_E2E_API_KEY_FILE / TWD_E2E_TAILNET not set")
	}
	if os.Getenv("TWD_E2E_SKIP_WIF") != "" {
		t.Skip("TWD_E2E_SKIP_WIF set")
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
	prefix := "ttest-m2-" + run
	tenantID := "tn_m2" + run
	res := &results{Run: run, Suite: "m2-mgmt-wif", Tailnet: tailnet, Date: time.Now().Format(time.RFC3339),
		Checks: map[string]string{}, Numbers: map[string]float64{}, Cleanup: map[string]any{}}
	root, _ := nettest.FindRepoRoot()
	resultsTo := filepath.Join(root, "e2e", "real", "results", "m2wif-"+time.Now().Format("20060102-150405")+"-"+run+".json")
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("/tmp", "twd-m2-wif-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o700)
	var logBuf bytes.Buffer
	log := slog.New(netd.NewRedactingHandler(slog.NewTextHandler(&lockedBuf{b: &logBuf}, nil)))

	// Everything this run creates, for cleanup.
	var keyIDs, fedIDs, devices []string
	var nodes []*tsnet.Server
	var m *mgmt.Mgmt
	var store *mgmt.SQLStore
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer ccancel()
		if m != nil {
			m.Close()
		}
		if store != nil {
			recs, _ := store.ListAudit(cctx, mgmt.AuditFilter{Chain: "mgmt"})
			for _, r := range recs {
				if r.Kind == "mint" {
					var d struct{ KeyID string }
					json.Unmarshal(r.Detail, &d)
					keyIDs = append(keyIDs, d.KeyID)
				}
			}
			store.Close()
		}
		for _, n := range nodes {
			n.Close()
		}
		var errs []string
		for _, id := range devices {
			if err := api.DeleteDevice(cctx, id); err != nil {
				errs = append(errs, "device "+id+": "+err.Error())
			}
		}
		for _, id := range append(keyIDs, fedIDs...) {
			if err := api.DeleteKey(cctx, id); err != nil {
				errs = append(errs, "key "+id+": "+err.Error())
			}
		}
		left := 0
		if ds, err := api.ListDevices(cctx); err == nil {
			for _, d := range ds {
				if contains(devices, d.NodeID) || strings.HasPrefix(d.Hostname, prefix) {
					left++
				}
			}
		}
		res.Cleanup = map[string]any{"devices_tracked": len(devices), "auth_keys": len(keyIDs), "federated_credentials_deleted": len(fedIDs),
			"devices_left": left, "errors": errs}
		res.Checks["no_key_in_logs"] = "PASS"
		if strings.Contains(logBuf.String(), "tskey-") {
			res.Checks["no_key_in_logs"] = "FAIL"
			t.Error("a key reached the logs")
		}
		os.RemoveAll(dir)
		out, _ := json.MarshalIndent(res, "", "  ")
		os.WriteFile(resultsTo, out, 0o644)
		t.Logf("results: %s\n%s", resultsTo, out)
		if left != 0 || len(errs) > 0 {
			t.Errorf("cleanup: %d devices left, errors %v", left, errs)
		}
	}()

	// 1. mgmt's own node on the operator tailnet (tag:twd-mgmt has the funnel attr).
	mk, err := api.CreateKey(ctx, tsapi.KeyOptions{Tags: []string{"tag:twd-mgmt"}, Ephemeral: true, Preauthorized: true,
		ExpirySeconds: 300, Description: prefix + " mgmt"})
	if err != nil {
		t.Fatal(err)
	}
	keyIDs = append(keyIDs, mk.ID)
	mnode := &tsnet.Server{Hostname: prefix + "-mgmt", Dir: filepath.Join(dir, "mgmt-node"), ControlURL: prodControl,
		AuthKey: mk.Secret, Ephemeral: true, Logf: func(string, ...any) {}, UserLogf: func(string, ...any) {}}
	nodes = append(nodes, mnode)
	st, err := mnode.Up(ctx)
	if err != nil {
		t.Fatalf("mgmt node up: %v", err)
	}
	devices = append(devices, string(st.Self.ID))
	dns := strings.TrimSuffix(st.Self.DNSName, ".")
	issuer, err := mgmt.LoadOrCreateIssuer("https://"+dns, filepath.Join(dir, "issuer.key"))
	if err != nil {
		t.Fatal(err)
	}
	fl, err := mnode.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())
	if err != nil {
		t.Fatalf("funnel: %v", err)
	}
	go http.Serve(fl, issuer.Handler())

	// 2. The issuer is publicly reachable (first fetch waits for the cert).
	t0 := time.Now()
	pub := publicHTTP()
	discURL := issuer.TenantIssuer(tenantID) + "/.well-known/openid-configuration"
	var disc map[string]any
	for time.Since(t0) < 3*time.Minute {
		resp, err := pub.Get(discURL)
		if err == nil && resp.StatusCode == 200 {
			json.NewDecoder(resp.Body).Decode(&disc)
			resp.Body.Close()
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(3 * time.Second)
	}
	res.Numbers["funnel_first_fetch_ms"] = ms(time.Since(t0))
	if !res.check(t, "issuer_public_via_funnel", disc["issuer"] == issuer.TenantIssuer(tenantID), "discovery not fetched") {
		return
	}
	resp, err := pub.Get(issuer.TenantIssuer(tenantID) + "/jwks")
	jwksOK := false
	if err == nil {
		jb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		tok, _ := issuer.Sign(tenantID, "probe", "probe")
		_, verr := mgmt.VerifyJWT(tok, jb, time.Now())
		jwksOK = verr == nil
	}
	res.check(t, "public_jwks_verifies_issued_jwt", jwksOK, errStr(err))

	// 3. Per-tenant trust credentials (subjects pinned, no wildcards).
	mint, err := api.CreateFederated(ctx, tsapi.FederatedOptions{Scopes: []string{"auth_keys"}, Tags: []string{"tag:ttest"},
		Issuer: issuer.TenantIssuer(tenantID), Subject: mgmt.MintSubject(tenantID), Description: prefix + " mint"})
	if !res.check(t, "create_mint_trust_credential", err == nil, errStr(err)) {
		return
	}
	fedIDs = append(fedIDs, mint.ID)
	life, err := api.CreateFederated(ctx, tsapi.FederatedOptions{Scopes: []string{"devices:core"}, Tags: []string{"tag:ttest"},
		Issuer: issuer.TenantIssuer(tenantID), Subject: mgmt.LifecycleSubject(tenantID), Description: prefix + " lifecycle"})
	if !res.check(t, "create_lifecycle_trust_credential", err == nil, errStr(err)) {
		return
	}
	fedIDs = append(fedIDs, life.ID)
	res.check(t, "audience_is_default_generated", mint.Audience == "" || mint.Audience == mgmt.WIFAudience(mint.ID), "audience "+mint.Audience)

	// 4. mgmt in WIF mode.
	store, err = mgmt.OpenStore(ctx, filepath.Join(dir, "mgmt.db"))
	if err != nil {
		t.Fatal(err)
	}
	custody, _ := mgmt.OpenFileCustody(filepath.Join(dir, "custody"))
	m, err = mgmt.New(mgmt.Config{Store: store, Custody: custody, Broker: &mgmt.Broker{Issuer: issuer},
		IDSecret: bytes.Repeat([]byte{3}, 32), ReapInterval: time.Hour, MintRetries: 3, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	if _, err := m.CreateTenant(ctx, mgmt.TenantRequest{ID: tenantID, Tailnet: tailnet, ControlURL: prodControl, MintMode: mgmt.MintWIF,
		WIFClientID: mint.ID, WIFLifecycleClientID: life.ID}); err != nil {
		t.Fatal(err)
	}
	hostID := "h-" + run
	m.RegisterHost(ctx, mgmt.RegisterRequest{HostID: hostID, CapacityNodes: 4}, "", "")
	dep, err := m.Deploy(ctx, mgmt.DeployRequest{TenantID: tenantID, Workload: prefix, Namespaces: []mgmt.ClassConfig{{Class: prefix, Tags: []string{"tag:ttest"}}}})
	if err != nil {
		t.Fatal(err)
	}
	obj, err := m.CreateObject(ctx, mgmt.CreateObjectRequest{TenantID: tenantID, Namespace: prefix, ObjectName: "wif"})
	if err != nil {
		t.Fatal(err)
	}
	t1 := time.Now()
	cred, err := m.Credential(ctx, obj.ObjectID, hostID, dep.Epoch, 60*time.Second)
	res.Numbers["wif_exchange_and_mint_ms"] = ms(time.Since(t1))
	if !res.check(t, "mint_via_token_exchange", err == nil && cred.AuthKey != "", errStr(err)+" "+lastDeny(ctx, store)) {
		return
	}
	onode := &tsnet.Server{Hostname: cred.Hostname, Dir: filepath.Join(dir, "obj-node"), ControlURL: prodControl,
		AuthKey: cred.AuthKey, Ephemeral: cred.Ephemeral, Logf: func(string, ...any) {}, UserLogf: func(string, ...any) {}}
	cred.AuthKey = ""
	nodes = append(nodes, onode)
	ost, err := onode.Up(ctx)
	if !res.check(t, "node_joined_with_wif_minted_key", err == nil, errStr(err)) {
		return
	}
	stable := string(ost.Self.ID)
	devices = append(devices, stable)
	if dev, err := api.GetDevice(ctx, stable); err == nil {
		res.check(t, "device_tagged_ttest", len(dev.Tags) == 1 && dev.Tags[0] == "tag:ttest", strings.Join(dev.Tags, ","))
	}
	if _, err := m.Ready(ctx, obj.ObjectID, hostID, mgmt.ReadyRequest{NodeID: stable, StableNodeID: stable, Epoch: dep.Epoch}); err != nil {
		t.Fatal(err)
	}
	t2 := time.Now()
	kr, err := m.Kill(ctx, obj.ObjectID, mgmt.KillRequest{Reason: "e2e wif"})
	res.Numbers["kill_ms"] = ms(time.Since(t2))
	res.check(t, "kill_deleted_device_via_lifecycle_credential", err == nil && len(kr.DevicesDeleted) == 1, errStr(err)+" "+jsonStr(kr))
	gone := false
	for i := 0; i < 20 && !gone; i++ {
		_, err := api.GetDevice(ctx, stable)
		if gone = tsapi.IsNotFound(err); !gone {
			time.Sleep(500 * time.Millisecond)
		}
	}
	res.check(t, "device_verified_gone", gone, "still present")
	sts, ok, _ := m.VerifyAll(ctx)
	res.check(t, "audit_chain_verifies", ok, jsonStr(sts))
}

func lastDeny(ctx context.Context, s *mgmt.SQLStore) string {
	recs, _ := s.ListAudit(ctx, mgmt.AuditFilter{Chain: "mgmt"})
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Kind == "deny" {
			return string(recs[i].Detail)
		}
	}
	return ""
}

func jsonStr(v any) string { b, _ := json.Marshal(v); return string(b) }

type lockedBuf struct {
	mu sync.Mutex
	b  *bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
