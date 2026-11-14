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
	certs  *certSource // node certificate resolver (nil: fail closed)
}

// failClosedCerts is the CertSource of a tenant whose OBO public key mgmt
// did not publish: every twd caller's caps are withheld.
type failClosedCerts struct{}

func (failClosedCerts) Lookup(context.Context, string) (*netd.CallerObject, error) {
	return nil, errors.New("tenant has no pinned OBO public key; certificates can't be verified")
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
	// RT-01: pin the tenant's OBO root public key now; every inbound
	// caller's node certificate is verified against it. Without a key,
	// certificates can't be verified and netd withholds twd callers' caps.
	certs, err := newCertSource(h, id, info.OBOPublicKey)
	if err != nil {
		h.log.Error("node certificates unavailable for tenant; twd callers' caps will be withheld (fail closed)", "tenant", id, "err", err)
		certs = nil
	}
	var certSrc netd.CertSource
	if certs != nil {
		certSrc = certs
		t.certs = certs
	} else {
		certSrc = failClosedCerts{}
	}
	reg, err := netd.New(netd.Config{
		ControlURL:    control,
		StateRoot:     filepath.Join(h.tenantDir(id), "nodes"),
		Keys:          &keySource{h: h, tenantID: id},
		Upstream:      h.ingressHandler(t),
		CallerCerts:   certSrc,
		AllowSiblings: h.cfg.AllowSiblingEgress,
		Ephemeral:     h.cfg.Ephemeral,
		Hostname:      func(oid string) string { return oid },
		ReadyTimeout:  h.cfg.ReadyTimeout,
		JoinParallel:  h.cfg.JoinParallel,
		Logger:        h.log.With("tenant", id),
		TSNetLogs:     h.cfg.TSNetLogs,
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
	if req.Remint {
		// netd's rejoin from saved state landed in NeedsLogin: control no
		// longer knows the device. mgmt moves the object back to pending
		// (clearing its node IDs) and queues a mint under the same hostname;
		// the credential below is that new key, and awaitNode reports the
		// new node ID with /ready.
		h.log.Warn("saved node state rejected by control; re-minting under the same hostname", "object", o.ID, "hostname", o.Hostname)
		rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
		c, err := h.mgmt.Remint(rctx, o.ID, mgmtapi.RemintRequest{Epoch: w.epochNow(), Reason: "saved node state rejected by control (NeedsLogin)"})
		rcancel()
		if err == nil && c.AuthKey != "" {
			return c.AuthKey, nil // the re-mint answers with the new key
		}
		if err != nil {
			if e, ok := mgmtapi.AsError(err); !ok || e.Code != mgmtapi.CodeIdentityPending {
				return "", fmt.Errorf("hostd: remint %s: %w", o.ID, err)
			}
		}
	}
	backoff := 100 * time.Millisecond
	moved := false
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
			case e.Code == "already_joined" && !req.Remint && !moved:
				// The node joined from another host (the workload moved here:
				// `twd deploy --replace`, or a confirmed takeover) and its saved
				// state is on that host's disk. Re-mint under the same hostname,
				// fenced by this host's lease epoch (only the current holder can);
				// mgmt deletes the previous device. Costs one registration
				// (ADR-017 throttle) per moved node.
				moved = true
				h.log.Info("node joined from another host; re-minting here", "object", o.ID, "hostname", o.Hostname, "epoch", w.epochNow())
				rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
				c, rerr := h.mgmt.Remint(rctx, o.ID, mgmtapi.RemintRequest{Epoch: w.epochNow(), Reason: "workload moved to host " + h.cfg.HostID + ": no saved node state here"})
				rcancel()
				if rerr == nil && c.AuthKey != "" {
					return c.AuthKey, nil
				}
				if rerr != nil {
					if re, ok := mgmtapi.AsError(rerr); !ok || re.Code != mgmtapi.CodeIdentityPending {
						return "", fmt.Errorf("hostd: remint %s: %w", o.ID, rerr)
					}
				}
				continue
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
