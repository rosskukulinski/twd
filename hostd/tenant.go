package hostd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/netd"
)

// tenant is one tenant's netd registry on this host.
type tenant struct {
	id     string
	info   mgmtapi.Tenant
	reg    *netd.Registry
	egress http.Handler
}

func (h *Host) ensureTenant(ctx context.Context, id string) (*tenant, error) {
	h.mu.Lock()
	t := h.tenants[id]
	h.mu.Unlock()
	if t != nil {
		return t, nil
	}
	info, err := h.mgmt.Tenant(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("tenant %s: %w", id, err)
	}
	control := h.cfg.ControlURL
	if control == "" {
		control = info.ControlURL
	}
	if control == "" {
		control = DefaultControlURL
	}
	t = &tenant{id: id, info: *info}
	reg, err := netd.New(netd.Config{
		ControlURL:   control,
		StateRoot:    filepath.Join(h.cfg.DataDir, "tenants", id, "nodes"),
		Keys:         &keySource{h: h},
		Upstream:     h.ingressHandler(t),
		Ephemeral:    h.cfg.Ephemeral,
		Hostname:     func(oid string) string { return oid },
		ReadyTimeout: h.cfg.ReadyTimeout,
		JoinParallel: h.cfg.JoinParallel,
		Logger:       h.log.With("tenant", id),
		TSNetLogs:    h.cfg.TSNetLogs,
	})
	if err != nil {
		return nil, err
	}
	t.reg = reg
	t.egress = reg.EgressHandler()
	h.mu.Lock()
	if old := h.tenants[id]; old != nil {
		h.mu.Unlock()
		reg.Close()
		return old, nil
	}
	h.tenants[id] = t
	h.mu.Unlock()
	h.log.Info("tenant registry ready", "tenant", id, "tailnet", info.Tailnet, "control", control)
	return t, nil
}

// keySource fetches a single-use credential from mgmt, fenced by the
// namespace lease epoch. Keys are never logged.
type keySource struct{ h *Host }

func (k *keySource) AuthKey(ctx context.Context, req netd.KeyRequest) (string, error) {
	h := k.h
	h.mu.Lock()
	o := h.byNetd[req.ObjectID]
	var w *workload
	if o != nil {
		w = h.workloads[o.TenantID+"/"+o.Workload]
	}
	h.mu.Unlock()
	if o == nil || w == nil {
		return "", errors.New("hostd: no object for node " + req.ObjectID)
	}
	backoff := 100 * time.Millisecond
	for {
		c, err := h.mgmt.Credential(ctx, o.ID, w.epochNow())
		if err == nil {
			if c.AuthKey == "" {
				return "", errors.New("hostd: mgmt returned an empty credential")
			}
			return c.AuthKey, nil
		}
		if e, ok := mgmtapi.AsError(err); ok {
			switch {
			case e.Code == mgmtapi.CodeIdentityPending || e.Status == 202 || e.Status == 425 || e.Status == 429 || e.Status == 503:
				// queued behind the registration throttle: retry
			default:
				return "", err
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}
