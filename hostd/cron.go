package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/rosskukulinski/twd/internal/cron"
	"github.com/rosskukulinski/twd/netd"
)

// Cron triggers (wrangler.jsonc "triggers": {"crons": [...]}).
//
// hostd runs each active workload's cron triggers against its default
// export's scheduled() handler, in UTC, at minute granularity. The rules:
//
//   - Only the active instance of a workload this host serves runs them,
//     and only while this host's heartbeat is fresh (within FenceAfter).
//     That holds for stateless workloads too, which otherwise keep serving
//     through a partition: mgmt may have moved the workload by then, and
//     its new host runs the triggers.
//   - Each (workload, trigger) runs at most once per minute. A wall clock
//     stepped backwards does not re-run minutes that already ran; one
//     stepped forwards (or a host that slept) does not backfill the missed
//     minutes. Like Cloudflare's, delivery is best effort, not exactly once.
//   - A trigger whose previous run is still going is skipped, not queued.
//   - A run is bounded by Config.CronTimeout (wall clock) and counts as an
//     in-flight request, so a blue/green switch drains it like any other.
//   - The invocation reaches workerd only over the instance's private
//     ingress, as cf.twd {kind: "scheduled"}, which only hostd can set (it
//     overwrites cf.twd on every tailnet request). The platform wrapper
//     accepts only crons the deployment declares.

// DefaultCronTimeout bounds one scheduled() run (Cloudflare's limit for
// cron triggers is 15 minutes of wall time).
const DefaultCronTimeout = 15 * time.Minute

// CronResult is one scheduled() run's outcome, as workerd reports it.
type CronResult struct {
	Outcome string `json:"outcome"` // "ok", "exception", "exceededCpu", ...
	NoRetry bool   `json:"noRetry"`
	Error   string `json:"error,omitempty"`
}

// cronRun is one due trigger.
type cronRun struct {
	w    *workload
	cron string
	at   time.Time
}

// cronLeaseFresh reports whether this host may run cron triggers now: it
// has heartbeated successfully within FenceAfter (DefaultFenceAfter when
// fencing is disabled) and is not self-fenced.
func (h *Host) cronLeaseFresh() bool {
	window := h.cfg.FenceAfter
	if window <= 0 {
		window = DefaultFenceAfter
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.fenced && !h.lastHB.IsZero() && time.Since(h.lastHB) <= window
}

// cronDue decides which triggers run for the minute containing now, marks
// them running and returns them. It is the whole scheduling policy; the
// caller only starts the runs.
func (h *Host) cronDue(now time.Time) []cronRun {
	minute := now.UTC().Truncate(time.Minute)
	if !h.cronLeaseFresh() {
		return nil
	}
	h.mu.Lock()
	wls := make([]*workload, 0, len(h.workloads))
	for _, w := range h.workloads {
		wls = append(wls, w)
	}
	h.mu.Unlock()
	sort.Slice(wls, func(i, j int) bool { return wls[i].tenantID+"/"+wls[i].name < wls[j].tenantID+"/"+wls[j].name })
	var due []cronRun
	for _, w := range wls {
		w.mu.Lock()
		inst := w.active
		if w.status != "active" || inst == nil || inst.app == nil || len(inst.app.Crons) == 0 || inst.stopping.Load() {
			w.mu.Unlock()
			continue
		}
		if !minute.After(w.cronLast) {
			w.mu.Unlock() // this minute (or a later one, before a clock step back) already ran
			continue
		}
		w.cronLast = minute
		for _, expr := range inst.app.Crons {
			s, err := cron.Parse(expr)
			if err != nil || !s.Matches(minute) {
				continue // validated at deploy; an error here cannot happen
			}
			if w.cronRunning[expr] {
				h.log.Warn("cron trigger skipped: previous run still in progress", "workload", w.name, "cron", expr, "scheduledTime", minute)
				continue
			}
			if w.cronRunning == nil {
				w.cronRunning = map[string]bool{}
			}
			w.cronRunning[expr] = true
			due = append(due, cronRun{w: w, cron: expr, at: minute})
		}
		w.mu.Unlock()
	}
	return due
}

// cronLoop wakes at each minute boundary and starts the due triggers.
func (h *Host) cronLoop() {
	defer h.wg.Done()
	for {
		now := h.now()
		next := now.UTC().Truncate(time.Minute).Add(time.Minute)
		t := time.NewTimer(next.Sub(now) + 50*time.Millisecond)
		select {
		case <-h.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		for _, r := range h.cronDue(h.now()) {
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				h.runScheduled(r)
			}()
		}
	}
}

func (h *Host) now() time.Time {
	if h.nowFn != nil {
		return h.nowFn()
	}
	return time.Now()
}

// runScheduled runs one due trigger and clears its running mark.
func (h *Host) runScheduled(r cronRun) {
	defer func() {
		r.w.mu.Lock()
		delete(r.w.cronRunning, r.cron)
		r.w.mu.Unlock()
	}()
	st := time.Now()
	res, err := h.invokeCron(h.ctx, r.w, r.cron, r.at)
	log := h.log.With("workload", r.w.name, "tenant", r.w.tenantID, "cron", r.cron, "scheduledTime", r.at, "ms", time.Since(st).Milliseconds())
	switch {
	case err != nil:
		log.Warn("cron trigger failed", "err", err)
	case res.Outcome != "ok":
		log.Warn("cron trigger ran", "outcome", res.Outcome, "error", res.Error)
	default:
		log.Info("cron trigger ran", "outcome", res.Outcome)
	}
}

// RunCron runs one of a workload's declared cron triggers now, outside its
// schedule (twd dev --test-scheduled, tests). at is the controller's
// scheduledTime. It skips the schedule and lease checks but not the
// declared-cron check.
func (h *Host) RunCron(ctx context.Context, tenantID, workload, cronExpr string, at time.Time) (CronResult, error) {
	h.mu.Lock()
	w := h.workloads[tenantID+"/"+workload]
	h.mu.Unlock()
	if w == nil {
		return CronResult{}, &apiError{Code: "unknown_object", Detail: "workload " + workload + " is not hosted here"}
	}
	return h.invokeCron(ctx, w, cronExpr, at)
}

// invokeCron sends one scheduled() invocation to the workload's active
// instance (cf.twd.kind = scheduled) and waits for its outcome.
func (h *Host) invokeCron(ctx context.Context, w *workload, cronExpr string, at time.Time) (CronResult, error) {
	var res CronResult
	actx, cancel := context.WithTimeout(ctx, h.cfg.DrainTimeout+5*time.Second)
	inst, release := w.acquire(actx)
	cancel()
	if inst == nil {
		return res, &apiError{Code: "upstream_unavailable", Detail: "workload " + w.name + " has no running instance"}
	}
	defer release()
	if inst.app == nil || !slices.Contains(inst.app.Crons, cronExpr) {
		// The deployment changed under the scheduler, or a manual run named
		// a cron the app does not declare.
		return res, &apiError{Code: "bad_request", Detail: fmt.Sprintf("deployment %s has no cron trigger %q", inst.depID, cronExpr)}
	}
	timeout := h.cfg.CronTimeout
	if timeout <= 0 {
		timeout = DefaultCronTimeout
	}
	ctx, cancel = context.WithTimeout(ctx, timeout)
	defer cancel()
	blob, _ := json.Marshal(map[string]any{
		"twd":     map[string]any{"kind": "scheduled", "cron": cronExpr, "scheduledTime": at.UnixMilli(), "workload": w.name},
		"traceId": newTraceID(),
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://platform/_twd/scheduled", nil)
	req.Header.Set(netd.HeaderCF, string(blob))
	// inst.direct's client timeout is for short admin calls; a run may take
	// up to CronTimeout, bounded by ctx instead.
	resp, err := (&http.Client{Transport: inst.direct.Transport}).Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, fmt.Errorf("scheduled() exceeded the %v cron timeout", timeout)
		}
		return res, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != 200 {
		return res, fmt.Errorf("platform refused the cron run: %d %s", resp.StatusCode, netd.Redact(string(b)))
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return res, fmt.Errorf("bad platform response: %w", err)
	}
	res.Error = netd.Redact(res.Error)
	return res, nil
}
