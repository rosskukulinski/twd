package hostd_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"syscall"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/netd/nettest"
)

// TestWorkloadService: a deployment with a Tailscale Service. hostd
// advertises it on the workload node once active; callers reach the
// workload through the Service VIP with their own identity; the Service is
// withdrawn while workerd fails health probes (here: SIGSTOPped, the S3
// "stalled backend" case) and comes back when it recovers; a redeploy
// without a Service withdraws it.
func TestWorkloadService(t *testing.T) {
	s := startSuite(t, fakemgmt.Config{})
	e := s.e
	ctx := context.Background()
	const svc, vip = "svc:hello-svc", "100.99.1.10"
	e.FM.SetService("hello", svc)
	dep := e.Deploy(hostdtest.Template("hello"), "hello", map[string]string{"UPSTREAM": "http://ttest-echo/"})
	wl := e.WaitActive("hello", dep, 60*time.Second)

	status := func() hostd.WorkloadStatus {
		for _, w := range e.Host.Status().Workloads {
			if w.Workload == "hello" {
				return w
			}
		}
		t.Fatal("no hello workload")
		return hostd.WorkloadStatus{}
	}
	waitAdvertised := func(want bool, d time.Duration) hostd.WorkloadStatus {
		t.Helper()
		deadline := time.Now().Add(d)
		for {
			ws := status()
			if ws.Service != nil && ws.Service.Advertised == want {
				return ws
			}
			if time.Now().After(deadline) {
				t.Fatalf("service advertised != %v after %v: %+v", want, d, ws.Service)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	ws := waitAdvertised(true, 10*time.Second)
	if ws.Service.Name != svc || !ws.Service.Healthy {
		t.Fatalf("service status %+v", ws.Service)
	}

	// Control approves the workload node as the Service host.
	if err := s.ctl.HostServiceByName(wl.Hostname, svc, vip); err != nil {
		t.Fatal(err)
	}
	if err := nettest.AcceptRoutes(ctx, s.client.LC); err != nil {
		t.Fatal(err)
	}
	if err := nettest.WaitDNSRecord(ctx, s.client.LC, vip); err != nil {
		t.Fatal(err)
	}
	url := "http://hello-svc." + nettest.MagicDNSDomain + "/"
	// dials reports whether a fresh TCP connection to addr opens (no pooled
	// keep-alive connection can hide a withdrawn Service).
	dials := func(addr string) bool {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		c, err := s.client.Dial(dctx, "tcp", addr)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	viaService := func(d time.Duration) map[string]any {
		t.Helper()
		deadline := time.Now().Add(d)
		for {
			code, r := s.getJSON(url)
			if code == 200 {
				return r
			}
			if time.Now().After(deadline) {
				t.Fatalf("GET %s: %d %v", url, code, r)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	t.Run("CallerIdentityThroughVIP", func(t *testing.T) {
		req, _ := http.NewRequest("GET", url, nil)
		var raddr string
		req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
			raddr = i.Conn.RemoteAddr().String()
		}}))
		resp, err := (&http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DialContext: s.client.Dial}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if raddr != vip+":80" {
			t.Fatalf("%s was served from %s, not the Service VIP %s", url, raddr, vip)
		}
		r := viaService(20 * time.Second)
		if get(r, "caller", "nodeId") != s.clientID {
			t.Fatalf("caller via service = %v, want %s", get(r, "caller"), s.clientID)
		}
	})

	t.Run("WithdrawnWhileUnhealthy", func(t *testing.T) {
		pid := status().PID
		if pid == 0 {
			t.Skip("no workerd pid in status (sandboxed launcher)")
		}
		if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		stopped := time.Now()
		cont := func() { syscall.Kill(pid, syscall.SIGCONT) }
		defer cont()
		ws := waitAdvertised(false, 15*time.Second)
		t.Logf("withdrawn %v after workerd stalled (reason %q)", time.Since(stopped).Round(time.Millisecond), ws.Service.Reason)
		if ws.Service.Healthy || ws.Service.Reason != "health probe failing" {
			t.Fatalf("service status %+v", ws.Service)
		}
		if dials(vip + ":80") {
			t.Fatal("a new connection to the Service VIP still reaches the stalled workload node")
		}
		if !dials(wl.IP + ":80") {
			t.Fatal("the workload node's own address stopped accepting (only the Service should be withdrawn)")
		}
		cont()
		recovered := time.Now()
		waitAdvertised(true, 15*time.Second)
		t.Logf("re-advertised %v after workerd resumed", time.Since(recovered).Round(time.Millisecond))
		viaService(20 * time.Second)
	})

	t.Run("RedeployWithoutServiceWithdraws", func(t *testing.T) {
		e.FM.SetService("hello", "")
		dep2 := e.Deploy(hostdtest.Template("hello"), "hello", map[string]string{"UPSTREAM": "http://ttest-echo/"})
		e.WaitActive("hello", dep2, 60*time.Second)
		deadline := time.Now().Add(10 * time.Second)
		for status().Service != nil {
			if time.Now().After(deadline) {
				t.Fatalf("service still reported after redeploy without it: %+v", status().Service)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if dials(vip + ":80") {
			t.Fatal("Service VIP still accepts connections after a redeploy without it")
		}
		// The pooled keep-alive connection from earlier requests was closed
		// by the withdraw, so the shared client can't keep using it.
		short := &http.Client{Timeout: 2 * time.Second, Transport: s.hc.Transport}
		req, _ := http.NewRequest("GET", url, nil)
		var reused bool
		var raddr string
		req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
			reused, raddr = i.Reused, i.Conn.RemoteAddr().String()
		}}))
		if resp, err := short.Do(req); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("service still answering (reused=%v remote=%s) after redeploy without it: %d %.200s", reused, raddr, resp.StatusCode, b)
		}
		// The workload node itself keeps serving.
		if code, _ := s.getJSON("http://" + wl.IP + "/"); code != 200 {
			t.Fatalf("workload node after withdraw: %d", code)
		}
	})
}
