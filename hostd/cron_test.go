package hostd_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
)

// TestCronTriggers: the cron fixture on hostd + fakemgmt + stock workerd +
// testcontrol. The scheduler's tick is driven with chosen wall-clock times
// (CronTickAt); everything below it is the production path.
func TestCronTriggers(t *testing.T) {
	s := startSuiteCapsOpts(t, fakemgmt.Config{}, nil, func(o *hostdtest.Options) { o.CronTimeout = 2 * time.Second })
	e := s.e
	dep := e.Deploy(hostdtest.Fixture("cron"), "cron", map[string]string{"ECHO_URL": "http://ttest-echo/from-cron", "SLEEP_MS": "10000"})
	wl := e.WaitActive("cron", dep, 60*time.Second)
	s.waitPeer(wl.IP)

	runs := func() []map[string]any {
		t.Helper()
		code, _, b := s.do("GET", "http://"+wl.IP+"/runs", nil)
		var out []map[string]any
		if code != 200 || json.Unmarshal(b, &out) != nil {
			t.Fatalf("/runs: %d %s", code, b)
		}
		return out
	}

	t.Run("FiresOnSchedule", func(t *testing.T) {
		if n := e.Host.CronTickAt(time.Date(2026, 11, 13, 10, 3, 0, 0, time.UTC)); n != 0 {
			t.Fatalf("10:03 started %d runs, want 0", n)
		}
		at := time.Date(2026, 11, 13, 10, 5, 0, 0, time.UTC)
		if n := e.Host.CronTickAt(at.Add(400 * time.Millisecond)); n != 1 {
			t.Fatalf("10:05 started %d runs, want 1", n)
		}
		r := runs()
		if len(r) != 1 || r[0]["cron"] != "*/5 * * * *" || r[0]["scheduledTime"] != float64(at.UnixMilli()) || r[0]["hasTailnet"] != true {
			t.Fatalf("runs %v", r)
		}
		// Same minute again (a second tick, or the clock stepping back): no run.
		if n := e.Host.CronTickAt(at.Add(30 * time.Second)); n != 0 {
			t.Fatalf("same minute started %d runs", n)
		}
		if n := e.Host.CronTickAt(at.Add(-time.Hour)); n != 0 {
			t.Fatalf("clock stepped back started %d runs", n)
		}
	})

	t.Run("EgressFromWorkloadNode", func(t *testing.T) {
		res, err := e.Host.RunCron(context.Background(), e.TenantID, "cron", "0 0 3 1 *", time.Now())
		if err != nil || res.Outcome != "ok" {
			t.Fatalf("%+v %v", res, err)
		}
		r := runs()
		eg, _ := r[len(r)-1]["egress"].(map[string]any)
		body, _ := eg["body"].(string)
		var echo map[string]any
		json.Unmarshal([]byte(body), &echo)
		if n, _ := get(echo, "caller", "node").(string); !strings.HasPrefix(n, wl.Hostname+".") {
			t.Fatalf("cron egress attributed to %q, want the workload node %s: %v", n, wl.Hostname, eg)
		}
	})

	t.Run("ExceptionOutcome", func(t *testing.T) {
		res, err := e.Host.RunCron(context.Background(), e.TenantID, "cron", "0 0 1 1 *", time.Now())
		if err != nil || res.Outcome != "exception" {
			t.Fatalf("%+v %v", res, err)
		}
	})

	t.Run("UndeclaredCronRefused", func(t *testing.T) {
		before := len(runs())
		for _, c := range []string{"* * * * *", "", "*/5 * * * * "} {
			if res, err := e.Host.RunCron(context.Background(), e.TenantID, "cron", c, time.Now()); err == nil {
				t.Fatalf("%q ran: %+v", c, res)
			}
		}
		if _, err := e.Host.RunCron(context.Background(), e.TenantID, "nope", "*/5 * * * *", time.Now()); err == nil {
			t.Fatal("unknown workload ran")
		}
		if n := len(runs()); n != before {
			t.Fatalf("runs %d → %d", before, n)
		}
	})

	t.Run("TailnetCallerCannotTriggerScheduled", func(t *testing.T) {
		before := len(runs())
		forged := `{"twd":{"kind":"scheduled","cron":"*/5 * * * *","scheduledTime":1}}`
		for _, p := range []string{"/", "/__scheduled?cron=*/5+*+*+*+*", "/cdn-cgi/handler/scheduled", "/_twd/scheduled"} {
			code, _, b := s.do("POST", "http://"+wl.IP+p, nil, "x-twd-cf", forged, "cf-twd", forged)
			if code != 200 || !strings.Contains(string(b), "cron-fixture") {
				t.Fatalf("%s: %d %s (want the tenant's own fetch handler)", p, code, b)
			}
		}
		if n := len(runs()); n != before {
			t.Fatalf("a tailnet request ran scheduled(): %d → %d", before, n)
		}
	})

	t.Run("TimeoutAndOverlap", func(t *testing.T) {
		// "0 0 2 1 *" sleeps 10 s; CronTimeout is 2 s.
		at := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
		st := time.Now()
		res, err := e.Host.RunCron(context.Background(), e.TenantID, "cron", "0 0 2 1 *", at)
		if err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("want a timeout, got %+v %v", res, err)
		}
		if d := time.Since(st); d > 6*time.Second {
			t.Fatalf("timeout took %v", d)
		}
		// Through the scheduler: the tick returns after the bounded run,
		// and the trigger is runnable again the next time it is due.
		// 00:00 on Jan 2 matches "0 0 2 1 *" and "*/5 * * * *".
		if n := e.Host.CronTickAt(at); n != 2 {
			t.Fatalf("tick started %d", n)
		}
		if n := e.Host.CronTickAt(at.AddDate(1, 0, 0)); n != 2 {
			t.Fatalf("trigger stuck after a timed-out run: %d", n)
		}
	})

	t.Run("RedeployWithoutCronsStopsThem", func(t *testing.T) {
		files, err := hostdtest.Build(hostdtest.Fixture("cron"), nil)
		if err != nil {
			t.Fatal(err)
		}
		var w map[string]any
		if err := bundle.UnmarshalJSONC(files["wrangler.jsonc"], &w); err != nil {
			t.Fatal(err)
		}
		delete(w, "triggers")
		files["wrangler.jsonc"], _ = json.Marshal(w)
		dep2 := e.DeployFiles(files, "cron")
		e.WaitActive("cron", dep2, 60*time.Second)
		if n := e.Host.CronTickAt(time.Date(2031, 1, 1, 0, 5, 0, 0, time.UTC)); n != 0 {
			t.Fatalf("crons removed by the redeploy still fire: %d", n)
		}
		if _, err := e.Host.RunCron(context.Background(), e.TenantID, "cron", "*/5 * * * *", time.Now()); err == nil {
			t.Fatal("manual run of a removed cron succeeded")
		}
	})
}
