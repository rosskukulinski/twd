package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
)

// twd dev: the same twd-hostd (process tier, netd, workerd, per-object
// nodes) as production, against an embedded control plane on 127.0.0.1
// (Tailscale's testcontrol + local DERP/STUN) and an in-memory mgmt. No
// tailnet, no keys. A local proxy on --port forwards to the workload node
// through a dev client node, so getCaller() sees a real (dev) caller.
//
// Not built yet (STATUS.md): the embedded policy evaluator (every dev node
// sees every other and gets the same test caps) and mock peers from
// twd.dev.jsonc.

type devResult struct {
	URL      string             `json:"url"`
	Workload string             `json:"workload"`
	Node     string             `json:"node"`
	As       string             `json:"as"`
	Smoke    map[string]any     `json:"smoke,omitempty"`
	Timings  map[string]float64 `json:"timingsMs"`
}

func cmdDev(args []string) error {
	f := newFlags("dev")
	port := f.Int("port", 8787, "local port (127.0.0.1)")
	once := f.Bool("once", false, "start, make one request to /, print it and exit (smoke test)")
	workerdFlag := f.String("workerd", "", "workerd binary")
	verbose := f.Bool("v", false, "hostd logs to stderr")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	dir := "."
	if len(pos) > 0 {
		dir = pos[0]
	}
	a, err := loadApp(dir, "")
	if err != nil {
		return err
	}
	wd, err := findWorkerd(*workerdFlag)
	if err != nil {
		return err
	}
	t0 := time.Now()
	res := devResult{Workload: a.Name, Timings: map[string]float64{}}
	var log *slog.Logger
	if *verbose {
		log = slog.New(netd.NewRedactingHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	}
	tmp, err := os.MkdirTemp("/tmp", "twd-dev-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	os.Chmod(tmp, 0o700)

	ctl, err := nettest.StartControl()
	if err != nil {
		return err
	}
	defer ctl.Close()
	fm := fakemgmt.New(fakemgmt.Config{StaticKey: nettest.DummyKey, Logger: log})
	sock := filepath.Join(tmp, "mgmt.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	msrv := &http.Server{Handler: fm}
	go msrv.Serve(ln)
	defer msrv.Close()
	mc, _ := mgmtapi.NewClient("unix://" + sock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var tr struct {
		TenantID string `json:"tenantID"`
	}
	if err := mc.Do(ctx, "POST", "/v1/tenants", mgmtapi.Tenant{Tailnet: nettest.MagicDNSDomain, ControlURL: ctl.URL, MintMode: "static"}, &tr); err != nil {
		return err
	}
	hc, _ := mgmtapi.NewClient("unix://" + sock)
	h, err := hostd.New(hostd.Config{HostID: "dev", Mgmt: hc, DataDir: filepath.Join(tmp, "data"), RunDir: filepath.Join(tmp, "run"),
		WorkerdBin: wd, HeartbeatEvery: 300 * time.Millisecond, Ephemeral: true, ControlURL: ctl.URL, Logger: log, HostnamePrefix: "dev-",
		// twd dev is explicitly process-tier: a "tier": "microvm" app runs
		// in a local process here (never on a host serving tenants).
		ForceProcessTier: true})
	if err != nil {
		return err
	}
	if err := h.Start(ctx); err != nil {
		return err
	}
	defer h.Close()
	client, err := nettest.NewNode(ctx, ctl.URL, nettest.DummyKey, "dev-you", filepath.Join(tmp, "you"))
	if err != nil {
		return err
	}
	defer client.Close()
	if st, err := client.LC.Status(ctx); err == nil && st.Self != nil {
		if u, ok := st.User[st.Self.UserID]; ok {
			res.As = u.LoginName
		}
	}

	deploy := func() (hostd.ObjectStatus, error) {
		tb, _, err := a.build(nil)
		if err != nil {
			return hostd.ObjectStatus{}, err
		}
		var dr mgmtapi.DeployResponse
		if err := mc.Do(ctx, "POST", "/v1/deployments", mgmtapi.DeployRequest{TenantID: tr.TenantID, Workload: a.Name, Bundle: tb}, &dr); err != nil {
			return hostd.ObjectStatus{}, err
		}
		dl := time.Now().Add(60 * time.Second)
		for time.Now().Before(dl) {
			for _, w := range h.Status().Workloads {
				if w.Workload == a.Name && w.Status == "failed" {
					return hostd.ObjectStatus{}, fail("deploy_failed", "workerd could not start "+a.Name, strings.Split(w.Error, "\n")...)
				}
				if w.Workload == a.Name && w.Status == "active" && w.DeploymentID == dr.DeploymentID {
					if o, ok := h.Object(tr.TenantID, a.Name, "", a.Name); ok && o.Status == "ready" && o.IP != "" {
						return o, nil
					}
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		return hostd.ObjectStatus{}, fail("identity_pending", "dev deploy not ready after 60s")
	}
	wl, err := deploy()
	if err != nil {
		return err
	}
	res.Node = wl.Hostname
	res.Timings["ready"] = ms(time.Since(t0))
	var cur atomic.Value
	cur.Store(wl.IP)
	rp := &httputil.ReverseProxy{
		Transport: client.HTTPClient(0).Transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			ip := cur.Load().(string)
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", ip, ip
		},
	}
	pl, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return fail("bad_request", fmt.Sprintf("can't listen on 127.0.0.1:%d", *port), err.Error()).withFix("twd dev --port <other>")
	}
	psrv := &http.Server{Handler: rp, ReadHeaderTimeout: 10 * time.Second}
	go psrv.Serve(pl)
	defer psrv.Close()
	res.URL = "http://" + pl.Addr().String()
	if *once {
		res.Smoke = devGet(res.URL + "/")
		emit(f, res, func() { fmt.Printf("twd dev · %s → %v\n", res.URL, res.Smoke) })
		return nil
	}
	fmt.Printf("twd dev · process tier · embedded control plane (127.0.0.1) · no policy evaluator yet: every dev node sees every other\n")
	if a.Cfg.Tier == "microvm" {
		fmt.Printf("  twd.jsonc asks for \"tier\": \"microvm\"; twd dev runs it in the process tier (deployed hosts refuse it without a microVM backend)\n")
	}
	fmt.Printf("  node %-28s %s\n", wl.Hostname, strings.Join(a.Cfg.Tailnet.Tags, ","))
	for _, c := range a.Cfg.Classes() {
		if c.Class != "_workload" {
			fmt.Printf("  class %-27s %s (a node per object, on first use)\n", c.Class, strings.Join(c.Tags, ","))
		}
	}
	fmt.Printf("ready on %s  (as user %s) in %.1fs · edits reload · Ctrl-C to stop\n", res.URL, res.As, res.Timings["ready"]/1000)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	last := snapshot(a.Dir)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-sig:
			fmt.Println("\nstopping")
			return nil
		case <-tick.C:
			now := snapshot(a.Dir)
			if now == last {
				continue
			}
			last = now
			rs := time.Now()
			if na, err := loadApp(a.Dir, a.Name); err == nil {
				a = na
			}
			o, err := deploy()
			if err != nil {
				printErr(err)
				continue
			}
			cur.Store(o.IP)
			fmt.Printf("↻ reloaded in %.0f ms\n", ms(time.Since(rs)))
		}
	}
}

func devGet(u string) map[string]any {
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Get(u)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	m := map[string]any{}
	if json.Unmarshal(b, &m) != nil {
		m = map[string]any{"body": string(b)}
	}
	m["status"] = resp.StatusCode
	return m
}

// snapshot is a cheap change detector: newest mtime + file count of the
// app's sources (node_modules, dist and dot-dirs excluded).
func snapshot(dir string) string {
	var newest time.Time
	n := 0
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && p != dir && (d.Name() == "node_modules" || d.Name() == "dist" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if info, err := d.Info(); err == nil && !d.IsDir() {
			n++
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		return nil
	})
	return fmt.Sprintf("%d/%d", n, newest.UnixNano())
}
