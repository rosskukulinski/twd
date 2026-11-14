package hostd_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/sandbox/sandboxtest"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/tailcfg"
)

// TestContainerFenceInheritance (ADR-032 D2, ADR-028 §3): a container
// inherits its parent's fence. Partitioned from mgmt, the host stops a
// fenced workload's containers within FenceAfter (two live copies of one
// object's sandbox must not outlive the lease); a workload whose classes
// are all "fence": "never" keeps its containers, and they keep working.
func TestContainerFenceInheritance(t *testing.T) {
	const fenceAfter = time.Second
	procs := &sandboxtest.Procs{}
	st, err := sandboxtest.ImageStore(t.TempDir(), []string{"sleep", "3600"}, "twd.test/box:1")
	if err != nil {
		t.Fatal(err)
	}
	s := startSuiteCapsOpts(t, fakemgmt.Config{}, tailcfg.PeerCapMap{nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue}}, func(o *hostdtest.Options) {
		o.Containers, o.Images, o.FenceAfter, o.HeartbeatEvery = procs, st, fenceAfter, 100*time.Millisecond
	})
	e := s.e
	files, err := hostdtest.Build(hostdtest.Fixture("ctr"), nil)
	if err != nil {
		t.Fatal(err)
	}
	never := map[string][]byte{}
	for k, v := range files {
		never[k] = v
	}
	tj := string(files["twd.jsonc"])
	for _, c := range []string{`"Box": { "tags"`, `"PBox": { "tags"`, `"Plain": { "egress"`} {
		if !strings.Contains(tj, c) {
			t.Fatalf("fixture twd.jsonc changed: no %s", c)
		}
		tj = strings.Replace(tj, c, strings.Replace(c, "{ ", `{ "fence": "never", `, 1), 1)
	}
	never["twd.jsonc"] = []byte(tj)
	dF := e.DeployFiles(files, "fenced")
	dN := e.DeployFiles(never, "nofence")
	wF, wN := e.WaitActive("fenced", dF, 60*time.Second), e.WaitActive("nofence", dN, 60*time.Second)
	s.waitPeer(wF.IP)
	s.waitPeer(wN.IP)
	call := func(ip, path string, q ...string) map[string]any {
		t.Helper()
		v := url.Values{}
		for i := 0; i+1 < len(q); i += 2 {
			v.Set(q[i], q[i+1])
		}
		_, m := s.getJSON("http://" + ip + path + "?" + v.Encode())
		return m
	}
	ctrOf := func(wl string) hostd.ContainerStatus {
		for _, c := range e.Host.Containers() {
			if c.Workload == wl && c.Class == "Box" && c.Object == "a" {
				return c
			}
		}
		return hostd.ContainerStatus{}
	}
	for _, ip := range []string{wF.IP, wN.IP} {
		call(ip, "/f/a/start")
		if r := call(ip, "/f/a/exec", "argv", `["true"]`); r["exitCode"] != float64(0) {
			t.Fatalf("exec: %v", r)
		}
	}
	if ctrOf("fenced").State != "running" || ctrOf("nofence").State != "running" || procs.Live() != 2 {
		t.Fatalf("before: %+v %+v live %d", ctrOf("fenced"), ctrOf("nofence"), procs.Live())
	}

	e.FM.SetHeartbeatsDown(true)
	defer e.FM.SetHeartbeatsDown(false)
	t0 := time.Now()
	for ctrOf("fenced").State == "running" && time.Since(t0) < 10*time.Second {
		time.Sleep(20 * time.Millisecond)
	}
	d := time.Since(t0)
	c := ctrOf("fenced")
	if c.State != "stopped" || !strings.Contains(c.ExitReason, "heartbeat lost") {
		t.Fatalf("fenced workload's container after %v: %+v", d, c)
	}
	// FenceAfter, plus one heartbeat interval to notice, plus the kill.
	if d > fenceAfter+time.Second {
		t.Fatalf("fenced container stopped %v after the partition (FenceAfter %v)", d, fenceAfter)
	}
	t.Logf("fenced container stopped %v after the partition began (FenceAfter %v)", d.Round(time.Millisecond), fenceAfter)
	time.Sleep(3 * fenceAfter)
	if c := ctrOf("nofence"); c.State != "running" {
		t.Fatalf(`"fence": "never" workload's container was stopped: %+v`, c)
	}
	if procs.Live() != 1 {
		t.Fatalf("live fake containers %d, want 1 (the fence:never one)", procs.Live())
	}
	if r := call(wN.IP, "/f/a/exec", "argv", `["echo","still here"]`); r["stdout"] != "still here\n" {
		t.Fatalf("fence:never container unusable during the partition: %v", r)
	}
}
