package hostd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// tenantIDRE: tenant IDs name directories, so they must be one safe path
// element.
var tenantIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// tenantDir is the tenant's private state: tenants/<id>/{nodes,workloads}
// (0700, like every level above it). Nothing of one tenant lives in
// another's dir, and no workerd may read any of it except its own
// workload's DO storage (see seatbelt.go).
func (h *Host) tenantDir(id string) string { return filepath.Join(h.cfg.DataDir, "tenants", id) }

func (h *Host) ensureTenantDirs(id string) error {
	td := h.tenantDir(id)
	for _, d := range []string{filepath.Dir(td), td, filepath.Join(td, "nodes"), filepath.Join(td, "workloads")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (h *Host) ensureTenant(ctx context.Context, id string) (*tenant, error) {
	h.mu.Lock()
	t := h.tenants[id]
	h.mu.Unlock()
	if t != nil {
		return t, nil
	}
	if !tenantIDRE.MatchString(id) || strings.Contains(id, "..") {
		return nil, fmt.Errorf("tenant ID %q is not a safe path element", id)
	}
	if err := h.ensureTenantDirs(id); err != nil {
		return nil, err
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
		StateRoot:    filepath.Join(h.tenantDir(id), "nodes"),
		Keys:         &keySource{h: h, tenantID: id},
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
// namespace lease epoch. Keys are never logged. Each tenant's registry has
// its own keySource, which only fetches keys for that tenant's objects: a
// netd registry holds one tenant's keys, never another's.
type keySource struct {
	h        *Host
	tenantID string
}

func (k *keySource) AuthKey(ctx context.Context, req netd.KeyRequest) (string, error) {
	h := k.h
	h.mu.Lock()
	o := h.byNetd[req.ObjectID]
	var w *workload
	if o != nil && o.TenantID == k.tenantID {
		w = h.workloads[o.TenantID+"/"+o.Workload]
	}
	h.mu.Unlock()
	if o != nil && o.TenantID != k.tenantID {
		return "", fmt.Errorf("hostd: tenant %s's registry asked for a key of tenant %s's object %s: refused", k.tenantID, o.TenantID, o.ID)
	}
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
