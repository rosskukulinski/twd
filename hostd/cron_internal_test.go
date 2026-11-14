package hostd

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/netd"
)

func cronHost(crons ...string) (*Host, *workload) {
	h := &Host{cfg: Config{FenceAfter: DefaultFenceAfter}, log: slog.New(slog.DiscardHandler), workloads: map[string]*workload{}}
	w := &workload{h: h, tenantID: "tn", name: "w", status: "active"}
	w.active = &instance{h: h, w: w, depID: "d1", app: &bundle.App{Crons: crons}}
	h.workloads["tn/w"] = w
	h.lastHB = time.Now()
	return h, w
}

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		panic(err)
	}
	return t
}

func names(rs []cronRun) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.w.name+":"+r.cron+"@"+r.at.Format("15:04"))
	}
	return strings.Join(out, ",")
}

// finish clears the running marks, as runScheduled does when a run ends.
func finish(rs []cronRun) {
	for _, r := range rs {
		r.w.mu.Lock()
		delete(r.w.cronRunning, r.cron)
		r.w.mu.Unlock()
	}
}

func TestCronDueMatchesSchedule(t *testing.T) {
	h, _ := cronHost("*/5 * * * *", "0 * * * *")
	rs := h.cronDue(at("2026-11-13 10:05:00"))
	if got := names(rs); got != "w:*/5 * * * *@10:05" {
		t.Fatalf("10:05: %s", got)
	}
	finish(rs)
	finish(h.cronDue(at("2026-11-13 10:06:00")))
	rs = h.cronDue(at("2026-11-13 11:00:00"))
	if got := names(rs); got != "w:*/5 * * * *@11:00,w:0 * * * *@11:00" {
		t.Fatalf("11:00: %s", got)
	}
	// scheduledTime is the whole minute, not the wake-up time.
	finish(rs)
	rs = h.cronDue(at("2026-11-13 11:05:00").Add(830 * time.Millisecond))
	if len(rs) != 1 || !rs[0].at.Equal(at("2026-11-13 11:05:00")) {
		t.Fatalf("scheduledTime %v", rs)
	}
}

func TestCronDueOncePerMinute(t *testing.T) {
	h, _ := cronHost("* * * * *")
	finish(h.cronDue(at("2026-11-13 10:05:00")))
	if rs := h.cronDue(at("2026-11-13 10:05:59")); len(rs) != 0 {
		t.Fatalf("same minute ran twice: %s", names(rs))
	}
	// The wall clock steps back: minutes that already ran don't run again.
	if rs := h.cronDue(at("2026-11-13 10:01:00")); len(rs) != 0 {
		t.Fatalf("clock step back re-ran: %s", names(rs))
	}
	if rs := h.cronDue(at("2026-11-13 10:06:00")); len(rs) != 1 {
		t.Fatalf("next minute: %s", names(rs))
	}
}

func TestCronDueNoBackfill(t *testing.T) {
	h, _ := cronHost("* * * * *")
	finish(h.cronDue(at("2026-11-13 10:00:00")))
	// The host slept (or the clock jumped) for 3 hours: one run, for now.
	rs := h.cronDue(at("2026-11-13 13:00:00"))
	if got := names(rs); got != "w:* * * * *@13:00" {
		t.Fatalf("after a jump: %s", got)
	}
}

func TestCronDueSkipsOverlap(t *testing.T) {
	h, _ := cronHost("* * * * *", "*/2 * * * *")
	first := h.cronDue(at("2026-11-13 10:00:00"))
	if len(first) != 2 {
		t.Fatal(names(first))
	}
	// Both still running at 10:01 and 10:02: no new runs, no queue.
	if rs := h.cronDue(at("2026-11-13 10:01:00")); len(rs) != 0 {
		t.Fatalf("overlapping run started: %s", names(rs))
	}
	finish(first[:1]) // "* * * * *" finished; "*/2" still going
	if got := names(h.cronDue(at("2026-11-13 10:02:00"))); got != "w:* * * * *@10:02" {
		t.Fatalf("10:02: %s", got)
	}
}

func TestCronDueNeedsFreshHeartbeat(t *testing.T) {
	h, w := cronHost("* * * * *")
	w.noFence = true // stateless: keeps serving through a partition, but no crons
	h.lastHB = time.Now().Add(-DefaultFenceAfter - time.Second)
	if rs := h.cronDue(at("2026-11-13 10:00:00")); len(rs) != 0 {
		t.Fatalf("stale heartbeat ran crons: %s", names(rs))
	}
	h.lastHB = time.Now()
	h.fenced = true
	if rs := h.cronDue(at("2026-11-13 10:00:00")); len(rs) != 0 {
		t.Fatalf("fenced host ran crons: %s", names(rs))
	}
	h.fenced = false
	h.lastHB = time.Time{}
	if rs := h.cronDue(at("2026-11-13 10:00:00")); len(rs) != 0 {
		t.Fatalf("never-heartbeated host ran crons: %s", names(rs))
	}
	// Fencing disabled (FenceAfter < 0): the default window still applies.
	h.cfg.FenceAfter = -1
	h.lastHB = time.Now().Add(-DefaultFenceAfter - time.Second)
	if rs := h.cronDue(at("2026-11-13 10:00:00")); len(rs) != 0 {
		t.Fatalf("stale heartbeat with fencing off ran crons: %s", names(rs))
	}
	h.lastHB = time.Now()
	if rs := h.cronDue(at("2026-11-13 10:00:00")); len(rs) != 1 {
		t.Fatalf("fresh heartbeat: %s", names(rs))
	}
}

func TestCronDueOnlyActiveInstances(t *testing.T) {
	for name, mod := range map[string]func(w *workload){
		"stopped":  func(w *workload) { w.status = "stopped" },
		"starting": func(w *workload) { w.status = "starting" },
		"failed":   func(w *workload) { w.status = "failed" },
		"no instance": func(w *workload) {
			w.active = nil
		},
		"stopping": func(w *workload) { w.active.stopping.Store(true) },
		"no crons": func(w *workload) { w.active.app = &bundle.App{} },
	} {
		h, w := cronHost("* * * * *")
		mod(w)
		if rs := h.cronDue(at("2026-11-13 10:00:00")); len(rs) != 0 {
			t.Errorf("%s: ran %s", name, names(rs))
		}
	}
}

// The schedule follows the active instance's deployment: a redeploy that
// changes the crons takes effect at the next minute, and a minute that ran
// under the old deployment does not run again under the new one.
func TestCronDueFollowsDeployment(t *testing.T) {
	h, w := cronHost("* * * * *")
	finish(h.cronDue(at("2026-11-13 10:00:00")))
	w.mu.Lock()
	w.active = &instance{h: h, w: w, depID: "d2", app: &bundle.App{Crons: []string{"*/10 * * * *"}}}
	w.mu.Unlock()
	if rs := h.cronDue(at("2026-11-13 10:00:30")); len(rs) != 0 {
		t.Fatalf("same minute re-ran after redeploy: %s", names(rs))
	}
	if rs := h.cronDue(at("2026-11-13 10:01:00")); len(rs) != 0 {
		t.Fatalf("old cron ran after redeploy: %s", names(rs))
	}
	if got := names(h.cronDue(at("2026-11-13 10:10:00"))); got != "w:*/10 * * * *@10:10" {
		t.Fatalf("new cron: %s", got)
	}
}

func TestCronDueIsolatesWorkloads(t *testing.T) {
	h, _ := cronHost("* * * * *")
	o := &workload{h: h, tenantID: "tn2", name: "other", status: "active"}
	o.active = &instance{h: h, w: o, app: &bundle.App{Crons: []string{"*/2 * * * *"}}}
	h.workloads["tn2/other"] = o
	rs := h.cronDue(at("2026-11-13 10:00:00"))
	if got := names(rs); got != "w:* * * * *@10:00,other:*/2 * * * *@10:00" { // sorted by tenant/workload
		t.Fatalf("%s", got)
	}
	// One workload's long run does not hold up another's.
	finish(rs[:1])
	if got := names(h.cronDue(at("2026-11-13 10:01:00"))); got != "w:* * * * *@10:01" {
		t.Fatalf("%s", got)
	}
}

// TestCronLoopFiresAtMinuteBoundary runs the real cronLoop with a clock
// shifted to just before a minute boundary, against a fake workerd ingress,
// and checks the invocation hostd sends.
func TestCronLoopFiresAtMinuteBoundary(t *testing.T) {
	got := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var blob map[string]any
		json.Unmarshal([]byte(r.Header.Get(netd.HeaderCF)), &blob)
		blob["_path"] = r.URL.Path
		got <- blob
		w.Write([]byte(`{"outcome":"ok","noRetry":false}`))
	}))
	defer srv.Close()
	h, w := cronHost("* * * * *")
	addr := srv.Listener.Addr().String()
	w.active.direct = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}}
	boundary := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	offset := boundary.Add(-300 * time.Millisecond).Sub(time.Now())
	h.nowFn = func() time.Time { return time.Now().Add(offset) }
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.wg.Add(1)
	go h.cronLoop()
	defer func() { h.cancel(); h.wg.Wait() }()
	select {
	case blob := <-got:
		twd, _ := blob["twd"].(map[string]any)
		if blob["_path"] != "/_twd/scheduled" || twd["kind"] != "scheduled" || twd["cron"] != "* * * * *" ||
			twd["scheduledTime"] != float64(boundary.UnixMilli()) || twd["workload"] != "w" || blob["traceId"] == "" {
			t.Fatalf("invocation %v", blob)
		}
		if _, ok := blob["tailscale"]; ok {
			t.Fatalf("a cron run must carry no caller identity: %v", blob)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cronLoop did not fire at the minute boundary")
	}
	select {
	case b := <-got:
		t.Fatalf("fired twice for one minute: %v", b)
	case <-time.After(500 * time.Millisecond):
	}
}
