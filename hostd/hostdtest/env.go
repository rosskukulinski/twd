package hostdtest

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
)

// Options configure an Env.
type Options struct {
	ControlURL     string // passed to hostd (overrides the tenant's)
	Fake           fakemgmt.Config
	Tenant         mgmtapi.Tenant
	HostnamePrefix string
	Ephemeral      bool
	KillTombstone  time.Duration
	HeartbeatEvery time.Duration
	Verbose        bool
	// VM, DefaultTier and VMDebug are passed to hostd (microVM tier).
	VM          sandbox.Backend
	DefaultTier string
	VMDebug     bool
	// Sandbox is hostd's process-tier sandbox ("" = none unless
	// $TWD_TEST_SANDBOX is set, "auto", "seatbelt", "landlock", "none").
	Sandbox string
	// InsecureSharedHost: see hostd.Config.
	InsecureSharedHost bool
}

// Env is fakemgmt (on a unix socket) plus one running twd-hostd.
type Env struct {
	T        testing.TB
	FM       *fakemgmt.Server
	Mgmt     *mgmtapi.Client // CLI-side client
	Host     *hostd.Host
	TenantID string
	Dir      string
}

// Start starts fakemgmt, creates the tenant and starts hostd.
func Start(t testing.TB, o Options) *Env {
	t.Helper()
	wd, err := nettest.WorkerdBinary()
	if err != nil {
		t.Skip(err)
	}
	if !HasNode() {
		t.Skip("node not available (needed to bundle apps)")
	}
	dir, err := ShortDir("twd-hd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if os.Getenv("TWD_KEEP_INSTANCES") == "" {
			os.RemoveAll(dir)
		}
	})
	var log *slog.Logger
	if o.Verbose || os.Getenv("TWD_TEST_LOG") != "" {
		log = slog.New(netd.NewRedactingHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
		o.Fake.Logger = log
	}
	fm := fakemgmt.New(o.Fake)
	sock := filepath.Join(dir, "mgmt.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: fm}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	cli, _ := mgmtapi.NewClient("unix://" + sock)
	ctx := context.Background()
	var tr struct {
		TenantID string `json:"tenantID"`
	}
	if err := cli.Do(ctx, "POST", "/v0/tenants", o.Tenant, &tr); err != nil {
		t.Fatal(err)
	}
	hc, _ := mgmtapi.NewClient("unix://" + sock)
	if o.HeartbeatEvery == 0 {
		o.HeartbeatEvery = 200 * time.Millisecond
	}
	h, err := hostd.New(hostd.Config{
		HostID: "h1", Mgmt: hc, DataDir: filepath.Join(dir, "data"), RunDir: filepath.Join(dir, "run"),
		WorkerdBin: wd, HeartbeatEvery: o.HeartbeatEvery, Ephemeral: o.Ephemeral, ControlURL: o.ControlURL,
		KillTombstone: o.KillTombstone, HostnamePrefix: o.HostnamePrefix, Logger: log,
		VM: o.VM, DefaultTier: o.DefaultTier, VMDebug: o.VMDebug, Sandbox: testSandbox(o.Sandbox), InsecureSharedHost: o.InsecureSharedHost,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return &Env{T: t, FM: fm, Mgmt: cli, Host: h, TenantID: tr.TenantID, Dir: dir}
}

// AddTenant creates another tenant in fakemgmt and returns an Env view of
// it (same mgmt and host; Deploy/WaitActive/WaitObject act for it).
func (e *Env) AddTenant(t mgmtapi.Tenant) *Env {
	e.T.Helper()
	var tr struct {
		TenantID string `json:"tenantID"`
	}
	if err := e.Mgmt.Do(context.Background(), "POST", "/v0/tenants", t, &tr); err != nil {
		e.T.Fatal(err)
	}
	c := *e
	c.TenantID = tr.TenantID
	return &c
}

// Deploy builds appDir and deploys it as workload; it returns the deployment ID.
func (e *Env) Deploy(appDir, workload string, vars map[string]string) string {
	e.T.Helper()
	files, err := Build(appDir, vars)
	if err != nil {
		e.T.Fatal(err)
	}
	var resp mgmtapi.DeployResponse
	if err := e.Mgmt.Do(context.Background(), "POST", "/v0/deployments",
		mgmtapi.DeployRequest{TenantID: e.TenantID, Workload: workload, Bundle: MustTar(files)}, &resp); err != nil {
		e.T.Fatal(err)
	}
	return resp.DeploymentID
}

// DeployNode builds a runtime "node" app and deploys it as workload.
func (e *Env) DeployNode(appDir, workload string, vars map[string]string) string {
	e.T.Helper()
	files, err := BuildNode(appDir, vars)
	if err != nil {
		e.T.Fatal(err)
	}
	return e.DeployFiles(files, workload)
}

// DeployFiles deploys already-built bundle files as workload.
func (e *Env) DeployFiles(files map[string][]byte, workload string) string {
	e.T.Helper()
	var resp mgmtapi.DeployResponse
	if err := e.Mgmt.Do(context.Background(), "POST", "/v0/deployments",
		mgmtapi.DeployRequest{TenantID: e.TenantID, Workload: workload, Bundle: MustTar(files)}, &resp); err != nil {
		e.T.Fatal(err)
	}
	return resp.DeploymentID
}

// WaitActive waits for workload to run depID and its entry node to be ready.
func (e *Env) WaitActive(workload, depID string, timeout time.Duration) hostd.ObjectStatus {
	e.T.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st := e.Host.Status()
		var ws *hostd.WorkloadStatus
		for i := range st.Workloads {
			if st.Workloads[i].Workload == workload && st.Workloads[i].TenantID == e.TenantID {
				ws = &st.Workloads[i]
			}
		}
		if ws != nil && ws.Status == "failed" {
			e.T.Fatalf("deploy %s failed: %s", workload, ws.Error)
		}
		if ws != nil && ws.Status == "active" && ws.DeploymentID == depID {
			if o, ok := e.Host.Object(e.TenantID, workload, "", workload); ok && o.Status == "ready" {
				return o
			}
		}
		if time.Now().After(deadline) {
			e.T.Fatalf("workload %s not active after %v: %+v", workload, timeout, st.Workloads)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// WaitObject waits for an object's node to be ready.
func (e *Env) WaitObject(workload, class, name string, timeout time.Duration) hostd.ObjectStatus {
	e.T.Helper()
	o, err := e.WaitObjectErr(workload, class, name, timeout)
	if err != nil {
		e.T.Fatal(err)
	}
	return o
}

// WaitObjectErr is WaitObject returning an error.
func (e *Env) WaitObjectErr(workload, class, name string, timeout time.Duration) (hostd.ObjectStatus, error) {
	deadline := time.Now().Add(timeout)
	for {
		o, ok := e.Host.Object(e.TenantID, workload, class, name)
		if ok && o.Status == "ready" && o.IP != "" {
			return o, nil
		}
		if time.Now().After(deadline) {
			return o, fmt.Errorf("object %s/%s/%s not ready after %v: %+v", workload, class, name, timeout, o)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// testSandbox applies $TWD_TEST_SANDBOX (e.g. "auto": run a whole test
// suite with hostd's default sandbox) when a test doesn't pick one.
func testSandbox(s string) string {
	if s == "" {
		return os.Getenv("TWD_TEST_SANDBOX")
	}
	return s
}
