package gen_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/hostd/gen"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
)

func sched(cron string, at time.Time) map[string]any {
	return map[string]any{"kind": "scheduled", "cron": cron, "scheduledTime": at.UnixMilli()}
}

// TestGeneratedCron runs the cron fixture on real workerd and drives the
// platform's "scheduled" route as hostd does.
func TestGeneratedCron(t *testing.T) {
	in := start(t, hostdtest.Fixture("cron"), map[string]string{"SLEEP_MS": "10"})
	at := time.Date(2026, 11, 13, 10, 5, 0, 0, time.UTC)

	list := func() []map[string]any {
		code, _, raw := in.getRaw("/runs", wl)
		if code != 200 {
			t.Fatalf("/runs: %d %s", code, raw)
		}
		var out []map[string]any
		mustJSON(t, raw, &out)
		return out
	}

	t.Run("RunsScheduledHandler", func(t *testing.T) {
		code, _, r := in.get("/", sched("*/5 * * * *", at))
		if code != 200 || r["outcome"] != "ok" {
			t.Fatalf("%d %v", code, r)
		}
		l := list()
		if len(l) != 1 {
			t.Fatalf("runs %v", l)
		}
		run := l[0]
		if run["cron"] != "*/5 * * * *" || run["scheduledTime"] != float64(at.UnixMilli()) || run["hasTailnet"] != true {
			t.Fatalf("run %v", run)
		}
		if run["type"] != nil && run["type"] != "scheduled" {
			t.Fatalf("controller.type %v", run["type"])
		}
	})
	t.Run("ExceptionIsReported", func(t *testing.T) {
		code, _, r := in.get("/", sched("0 0 1 1 *", at))
		if code != 200 || r["outcome"] != "exception" {
			t.Fatalf("%d %v", code, r)
		}
	})
	t.Run("EgressIsWorkload", func(t *testing.T) {
		code, _, r := in.get("/", sched("0 0 3 1 *", at))
		if code != 200 || r["outcome"] != "ok" {
			t.Fatalf("%d %v", code, r)
		}
		l := list()
		e, _ := l[len(l)-1]["egress"].(map[string]any)
		if e == nil || e["status"] != float64(200) || !strings.Contains(e["body"].(string), `"as":"workload"`) {
			t.Fatalf("egress %v", l[len(l)-1])
		}
	})
	t.Run("UndeclaredCronRefused", func(t *testing.T) {
		before := len(list())
		for _, c := range []any{"* * * * *", "", 5, nil, "*/5 * * * * "} {
			code, h, r := in.get("/", map[string]any{"kind": "scheduled", "cron": c, "scheduledTime": at.UnixMilli()})
			if code != 400 || h.Get("twd-error") != "bad_request" {
				t.Fatalf("cron %v: %d %v", c, code, r)
			}
		}
		if n := len(list()); n != before {
			t.Fatalf("an undeclared cron ran: %d → %d", before, n)
		}
	})
	t.Run("BadScheduledTimeRefused", func(t *testing.T) {
		for _, st := range []any{"x", nil, map[string]any{}} {
			code, _, r := in.get("/", map[string]any{"kind": "scheduled", "cron": "*/5 * * * *", "scheduledTime": st})
			if code != 400 {
				t.Fatalf("scheduledTime %v: %d %v", st, code, r)
			}
		}
	})
	t.Run("ClientCannotReachScheduledViaWorkload", func(t *testing.T) {
		// A workload request can't become a scheduled one: the path and
		// headers are the tenant's, the route is cf.twd (hostd's).
		before := len(list())
		code, _, _ := in.get("/__scheduled?cron=*/5+*+*+*+*", wl)
		if code != 200 { // the fixture's fetch answers everything
			t.Fatalf("%d", code)
		}
		code, _, _ = in.get("/cdn-cgi/handler/scheduled?cron=*/5+*+*+*+*", wl)
		if code != 200 {
			t.Fatalf("%d", code)
		}
		if n := len(list()); n != before {
			t.Fatalf("a workload request ran scheduled(): %d → %d", before, n)
		}
	})
}

// TestGeneratedCronFlagOnlyWithCrons: the platform's extra-handlers flag
// is enabled only for deployments that declare crons, and the route
// refuses for the others.
func TestGeneratedCronFlagOnlyWithCrons(t *testing.T) {
	gen1 := func(crons []string) string {
		app := &bundle.App{Name: "w", Main: "index.js", CompatDate: "2026-04-01", Classes: map[string]*bundle.Class{},
			Modules: map[string][]byte{"index.js": []byte("export default {}")}, Crons: crons}
		dir := t.TempDir()
		res, err := gen.Generate(app, gen.Options{Dir: filepath.Join(dir, "i"), DOStorage: filepath.Join(dir, "do"), DeploymentID: "d", Workload: "w"})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(res.Config)
		cfg, _ := os.ReadFile(filepath.Join(dir, "i", "twd-config.json"))
		return string(b) + "\n" + string(cfg)
	}
	if s := gen1(nil); strings.Contains(s, "service_binding_extra_handlers") || !strings.Contains(s, `"crons": []`) {
		t.Fatalf("no crons:\n%s", s)
	}
	if s := gen1([]string{"* * * * *"}); !strings.Contains(s, "service_binding_extra_handlers") || !strings.Contains(s, `"* * * * *"`) {
		t.Fatalf("crons:\n%s", s)
	}
}

func TestGeneratedNoCronsRefusesScheduled(t *testing.T) {
	in := start(t, hostdtest.Fixture("probe"), nil)
	code, h, r := in.get("/", sched("* * * * *", time.Now()))
	if code != 400 || h.Get("twd-error") != "bad_request" {
		t.Fatalf("%d %v", code, r)
	}
}

func (in *inst) getRaw(path string, twd map[string]any) (int, http.Header, []byte) {
	req, _ := http.NewRequest("GET", "http://app.local"+path, nil)
	b, _ := json.Marshal(map[string]any{"tailscale": map[string]any{"caller": testCaller}, "objectId": "o1", "twd": twd})
	req.Header.Set("x-twd-cf", string(b))
	resp, err := in.hc.Do(req)
	if err != nil {
		in.t.Fatal(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, rb
}

func mustJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
}

// TestGeneratedCronCancelledOnDisconnect: when hostd gives up on a run
// (CronTimeout) and drops the connection, workerd cancels the tenant's
// scheduled(): it does not keep running unseen past its time limit, where
// the overlap guard could no longer see it.
func TestGeneratedCronCancelledOnDisconnect(t *testing.T) {
	in := start(t, hostdtest.Fixture("cron"), map[string]string{"SLEEP_MS": "1500"})
	b, _ := json.Marshal(map[string]any{"twd": sched("0 0 2 1 *", time.Now())})
	req, _ := http.NewRequest("POST", "http://app.local/", nil)
	req.Header.Set("x-twd-cf", string(b))
	short := &http.Client{Timeout: 400 * time.Millisecond, Transport: in.hc.Transport}
	if resp, err := short.Do(req); err == nil {
		resp.Body.Close()
		t.Fatalf("the sleeping run answered early: %d", resp.StatusCode)
	}
	time.Sleep(2500 * time.Millisecond)
	_, _, raw := in.getRaw("/runs", wl)
	var l []map[string]any
	mustJSON(t, raw, &l)
	var phases []any
	for _, r := range l {
		phases = append(phases, r["phase"])
	}
	if len(l) != 1 || l[0]["phase"] != "start" {
		t.Fatalf("want only the start of the cancelled run, got phases %v", phases)
	}
}
