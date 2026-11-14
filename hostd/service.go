package hostd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/rosskukulinski/twd/netd"
)

// Tailscale Services (twd.jsonc tailnet.service): the workload entry node
// advertises the Service only while this host should be serving it. That is
// the Service's health gate: Tailscale itself doesn't health-check Service
// hosts, so a host that keeps advertising while its workload is down or
// stalled black-holes the callers control routes to it.
//
// The advertisement is wanted while the deployment is active, its instance
// is running and answering health probes, and the workload node is ready.
// It is withdrawn on undeploy (and so on fencing and when mgmt moves the
// workload to another host), when the instance fails serviceUnhealthyAfter
// consecutive probes, and with the node (kill, close).

// DefaultServiceCheckEvery is how often hostd probes the workload instance
// of every workload that has a Service.
const DefaultServiceCheckEvery = 2 * time.Second

// serviceUnhealthyAfter consecutive failed probes withdraw the Service; one
// good probe brings it back.
const serviceUnhealthyAfter = 2

// serviceProbeTimeout bounds one probe.
const serviceProbeTimeout = 1500 * time.Millisecond

// ServiceStatus is the Tailscale Service view of a workload (status API).
type ServiceStatus struct {
	Name       string `json:"name"`
	Advertised bool   `json:"advertised"`
	// Healthy is the last probe result; Reason says why the Service isn't
	// advertised.
	Healthy bool   `json:"healthy"`
	Reason  string `json:"reason,omitempty"`
}

func (h *Host) serviceLoop() {
	defer h.wg.Done()
	every := h.cfg.ServiceCheckEvery
	if every <= 0 {
		every = DefaultServiceCheckEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
		}
		h.mu.Lock()
		wls := make([]*workload, 0, len(h.workloads))
		for _, w := range h.workloads {
			wls = append(wls, w)
		}
		h.mu.Unlock()
		for _, w := range wls {
			h.syncService(w, true)
		}
	}
}

// syncService reconciles a workload's Service advertisement with whether it
// should be served here. probe runs a health probe first (the periodic
// loop); event-driven calls (deploy, undeploy) use the last probe result.
func (h *Host) syncService(w *workload, probe bool) {
	w.svcMu.Lock()
	defer w.svcMu.Unlock()
	w.mu.Lock()
	svc, inst, status, wl, t := w.service, w.active, w.status, w.wl, w.tenant
	adv, advNetd := w.svcAdvertised, w.svcNetd
	w.mu.Unlock()
	if svc == "" && adv == "" {
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	defer cancel()

	want, reason := true, ""
	var netdID string
	switch {
	case svc == "":
		want, reason = false, "no service"
	case status != "active" || inst == nil || inst.stopping.Load():
		want, reason = false, "deployment "+status
	case wl == nil || t == nil:
		want, reason = false, "no workload node"
	default:
		h.mu.Lock()
		ready := wl.live && wl.Status == statusReady
		netdID = wl.NetdID
		h.mu.Unlock()
		if !ready {
			want, reason = false, "workload node "+wl.Status
		}
	}
	if want && probe {
		if inst.probe(ctx) {
			w.mu.Lock()
			w.svcFails, w.svcHealthy = 0, true
			w.mu.Unlock()
		} else {
			w.mu.Lock()
			w.svcFails++
			w.svcHealthy = false
			w.mu.Unlock()
		}
	}
	w.mu.Lock()
	fails := w.svcFails
	w.mu.Unlock()
	if want && fails >= serviceUnhealthyAfter {
		want, reason = false, "health probe failing"
	}

	// The node is the source of truth: a kill, a node close or a rejoin
	// drops the advertisement without telling us.
	if adv != "" && t != nil {
		if o := t.reg.Get(advNetd); o == nil || !slices.Contains(o.Info().Services, adv) {
			adv, advNetd = "", ""
		}
	}
	if adv != "" && (!want || adv != svc || advNetd != netdID) {
		if err := t.reg.WithdrawService(ctx, advNetd, adv); err != nil {
			h.log.Warn("service withdraw failed", "workload", w.name, "service", adv, "err", err)
		} else {
			h.log.Info("service withdrawn", "workload", w.name, "service", adv, "reason", reason)
		}
		adv, advNetd = "", ""
	}
	if want && adv == "" {
		if err := t.reg.AdvertiseService(ctx, netdID, svc); err != nil {
			reason = "advertise failed: " + netd.Redact(err.Error())
			h.log.Warn("service advertise failed", "workload", w.name, "service", svc, "err", err)
		} else {
			adv, advNetd, reason = svc, netdID, ""
			h.log.Info("service advertised", "workload", w.name, "service", svc, "node", wl.Hostname)
		}
	}
	w.mu.Lock()
	w.svcAdvertised, w.svcNetd, w.svcReason = adv, advNetd, reason
	w.mu.Unlock()
}

// serviceStatus is the status view (nil without a Service).
func (w *workload) serviceStatus() *ServiceStatus {
	if w.service == "" && w.svcAdvertised == "" {
		return nil
	}
	name := w.service
	if name == "" {
		name = w.svcAdvertised
	}
	return &ServiceStatus{Name: name, Advertised: w.svcAdvertised != "" && w.svcAdvertised == name,
		Healthy: w.svcHealthy, Reason: w.svcReason}
}

// probe reports whether the instance answers a health check: workerd's
// platform admin "health" op, or the Node app's health path.
func (inst *instance) probe(ctx context.Context) bool {
	if inst.stopping.Load() {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, serviceProbeTimeout)
	defer cancel()
	var req *http.Request
	if inst.node != nil {
		req, _ = http.NewRequestWithContext(ctx, "GET", "http://app"+inst.app.Node.Health, nil)
	} else {
		blob, _ := json.Marshal(map[string]any{"twd": map[string]any{"kind": "admin", "op": "health"}})
		req, _ = http.NewRequestWithContext(ctx, "POST", "http://platform/_twd/admin", nil)
		req.Header.Set(netd.HeaderCF, string(blob))
	}
	resp, err := inst.direct.Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if inst.node != nil {
		return true // any answer, as waitNodeHealthy
	}
	return resp.StatusCode == 200
}
