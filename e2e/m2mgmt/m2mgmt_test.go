//go:build m2mgmt

// Integration test: twd-hostd against the REAL twd-mgmt (built from
// origin/m2-mgmt; `make test-m2mgmt`), hermetic: testcontrol + local DERP,
// and a fake Tailscale API that mints keys testcontrol accepts.
package m2mgmt_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/netd/nettest"
)

type fakeTS struct {
	mu      sync.Mutex
	n       int
	deleted []string
}

func (f *fakeTS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/keys"):
		f.n++
		json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("k%d", f.n), "key": nettest.DummyKey})
	case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/keys/"):
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/v2/device/"):
		f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/api/v2/device/"))
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/acl"):
		w.Header().Set("ETag", `"p1"`)
		w.Write([]byte(`{"tagOwners":{"tag:ttest":["autogroup:admin"]},"grants":[{"src":["tag:ttest"],"dst":["tag:ttest"],"ip":["*"],"app":{"example.com/cap/ttest":[{"role":"tester"}]}}]}`))
	default:
		w.WriteHeader(404)
	}
}

func TestHostdAgainstRealMgmt(t *testing.T) {
	bin := os.Getenv("TWD_MGMT_BIN")
	if bin == "" {
		t.Skip("TWD_MGMT_BIN not set (make test-m2mgmt)")
	}
	wd, err := nettest.WorkerdBinary()
	if err != nil {
		t.Skip(err)
	}
	ctl, err := nettest.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	ts := &fakeTS{}
	tsSrv := httptest.NewServer(ts)
	defer tsSrv.Close()
	dir, _ := hostdtest.ShortDir("twd-m2-")
	defer os.RemoveAll(dir)
	keyFile := filepath.Join(dir, "api.env")
	os.WriteFile(keyFile, []byte("TS_API_KEY=fake\n"), 0o600)
	sock := filepath.Join(dir, "mgmt.sock")
	mlog, _ := os.Create(filepath.Join(dir, "mgmt.log"))
	cmd := exec.Command(bin, "-state-dir", filepath.Join(dir, "mgmt"), "-socket", sock, "-tsapi-url", tsSrv.URL, "-v")
	cmd.Stdout, cmd.Stderr = mlog, mlog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(dir, "mgmt.log"))
			t.Logf("twd-mgmt log (tail):\n%s", hostdtest.Trim(string(b[max(0, len(b)-6000):]), 6000))
		}
	}()
	cli, _ := mgmtapi.NewClient("unix://" + sock)
	ctx := context.Background()
	for i := 0; ; i++ {
		if err := cli.Do(ctx, "GET", "/v0/healthz", nil, nil); err == nil {
			break
		}
		if i > 200 {
			t.Fatal("twd-mgmt did not come up")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var tr struct {
		TenantID string `json:"tenantID"`
	}
	if err := cli.Do(ctx, "POST", "/v0/tenants", map[string]any{"tailnet": "ttest.ts.net", "controlURL": ctl.URL, "mintMode": "apikey", "apiKeyFile": keyFile}, &tr); err != nil {
		t.Fatal(err)
	}
	hc, _ := mgmtapi.NewClient("unix://" + sock)
	h, err := hostd.New(hostd.Config{HostID: "h1", Mgmt: hc, DataDir: filepath.Join(dir, "data"), RunDir: filepath.Join(dir, "run"),
		WorkerdBin: wd, HeartbeatEvery: 300 * time.Millisecond, Ephemeral: true, ControlURL: ctl.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	files, err := hostdtest.Build(hostdtest.Fixture("probe"), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Contract gap: real mgmt knows no workload entry node, so declare the
	// "_workload" namespace explicitly alongside the classes.
	var dep mgmtapi.DeployResponse
	err = cli.Do(ctx, "POST", "/v0/deployments", map[string]any{"tenantID": tr.TenantID, "workload": "probe", "bundle": hostdtest.MustTar(files),
		"namespaces": []map[string]any{
			{"class": mgmtapi.WorkloadNamespace, "tags": []string{"tag:ttest"}},
			{"class": "Probe", "tags": []string{"tag:ttest"}},
			{"class": "Ticker", "tags": []string{"tag:ttest"}},
		}}, &dep)
	if err != nil {
		t.Fatal(err)
	}
	e := &hostdtest.Env{T: t, Host: h, TenantID: tr.TenantID}
	wl := e.WaitActive("probe", dep.DeploymentID, 60*time.Second)
	t.Logf("workload node %s %s (object %s)", wl.Hostname, wl.IP, wl.ID)

	client, err := nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-client", filepath.Join(dir, "client"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	echo, err := nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "ttest-echo", filepath.Join(dir, "echo"))
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	echo.ServeEcho()
	hcl := client.HTTPClient(20 * time.Second)
	getJSON := func(u string) (int, map[string]any) {
		resp, err := hcl.Get(u)
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
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client.WaitPeers(wctx, wl.IP)
	for _, n := range []string{"a", "b", "c"} {
		if code, r := getJSON("http://" + wl.IP + "/probe/" + n + "/"); code != 200 || r["name"] != n {
			t.Fatalf("via workload %s: %d %v", n, code, r)
		}
	}
	ips := map[string]hostd.ObjectStatus{}
	for _, n := range []string{"a", "b", "c"} {
		o := e.WaitObject("probe", "Probe", n, 60*time.Second)
		ips[n] = o
		client.WaitPeers(wctx, o.IP)
		code, r := getJSON("http://" + o.IP + "/egress?url=http://ttest-echo/")
		node, _ := r["caller"].(map[string]any)["node"].(string)
		if code != 200 || !strings.HasPrefix(node, o.Hostname+".") {
			t.Fatalf("%s egress: %d %v (want %s)", n, code, r, o.Hostname)
		}
		t.Logf("object %s → node %s (%s), egress attributed ✓", n, o.Hostname, o.IP)
	}
	// Kill via mgmt; delivery is the heartbeat's revokeObjects.
	resp, err := hcl.Get("http://" + ips["c"].IP + "/stream")
	if err != nil {
		t.Fatal(err)
	}
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
	st := time.Now()
	var kr map[string]any
	if err := cli.Do(ctx, "POST", "/v0/objects/"+ips["c"].ID+"/kill", map[string]any{"reason": "m2 test"}, &kr); err != nil {
		t.Fatal(err)
	}
	select {
	case ft := <-failed:
		t.Logf("streaming caller failed %v after the mgmt kill (heartbeat 300ms) %v", ft.Sub(st), kr)
	case <-time.After(10 * time.Second):
		t.Fatal("stream survived the kill")
	}
	var audit map[string]any
	cli.Do(ctx, "GET", "/v0/audit", nil, &audit)
	if audit["verified"] != true {
		t.Fatalf("audit not verified: %v", hostdtest.Trim(fmt.Sprint(audit), 2000))
	}
	t.Logf("audit verified; %d records", len(audit["records"].([]any)))
}
