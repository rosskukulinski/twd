package mgmt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/internal/container"
)

// Containers and code sandboxes (ADR-032 D9): quota reservations at
// ctx.container.start(), released on stop and reconciled from heartbeats,
// all on the store clock (ADR-028 §1). Containers are not objects: they
// count against neither maxObjects nor a host's capacityNodes.

const (
	// ContainerReleaseAfter: a host's reservation that no heartbeat reported
	// for this long is released (a stop whose DELETE was lost, a start that
	// never booted).
	ContainerReleaseAfter = 30 * time.Second
	// ContainerStaleAfter: a reservation not renewed for this long (its host
	// stopped heartbeating) no longer counts against the quota. Its row stays
	// until the host reports again (renewed, or released) or is removed.
	ContainerStaleAfter = 2 * time.Minute
)

// ContainerReservation is one running (or starting) container's claim on
// its tenant's container quota.
type ContainerReservation struct {
	ID        string              `json:"reservationID"`
	TenantID  string              `json:"tenantID"`
	Workload  string              `json:"workload"`
	Class     string              `json:"class"`
	Object    string              `json:"object"`
	HostID    string              `json:"host"`
	Resources container.Resources `json:"resources"`
	CreatedAt int64               `json:"createdAt"`
	RenewedAt int64               `json:"renewedAt"`
}

func (r ContainerReservation) key() string {
	return r.TenantID + "|" + r.Workload + "|" + r.Class + "|" + r.Object
}

// ContainerQuota is what the store checks a new (or grown) reservation
// against, atomically with the insert.
type ContainerQuota struct {
	MaxContainers int
	MaxMemMiB     int
	MaxMicroVCPU  int64
	// ClassMax is the class's max_instances in this workload (0 = none).
	ClassMax int
	// StaleBefore (unix ms, store clock): reservations renewed earlier don't
	// count.
	StaleBefore int64
}

// ContainerQuotaError is the limit a reservation would exceed.
type ContainerQuotaError struct {
	Quota     string // maxContainers | maxContainerMemMiB | maxContainerVCPU | max_instances
	Limit     float64
	Used      float64
	Requested float64
}

func (e *ContainerQuotaError) Error() string {
	return fmt.Sprintf("%s: %g used + %g requested > %g", e.Quota, e.Used, e.Requested, e.Limit)
}

// ContainerFilter selects reservations; empty fields match everything.
type ContainerFilter struct {
	TenantID string
	HostID   string
}

// RunningContainer is one container a heartbeat reports.
type RunningContainer struct {
	ReservationID string              `json:"reservationID,omitempty"`
	TenantID      string              `json:"tenantID"`
	Workload      string              `json:"workload"`
	Class         string              `json:"class"`
	Object        string              `json:"object"`
	Resources     container.Resources `json:"resources"`
}

// containerDefaults fills unset container quotas (tenants created before
// they existed store 0).
func (t *Tenant) containerDefaults() {
	if t.MaxContainers <= 0 {
		t.MaxContainers = DefaultMaxContainers
	}
	if t.MaxContainerMemMiB <= 0 {
		t.MaxContainerMemMiB = DefaultMaxContainerMemMiB
	}
	if t.MaxContainerVCPU <= 0 {
		t.MaxContainerVCPU = DefaultMaxContainerVCPU
	}
}

// ReserveRequest is POST /v1/container-reservations.
type ReserveRequest struct {
	TenantID  string              `json:"tenantID"`
	Workload  string              `json:"workload"`
	Class     string              `json:"class"`
	Object    string              `json:"object"`
	Epoch     int64               `json:"epoch"`
	Resources container.Resources `json:"resources"`
}

// ReserveContainer reserves quota for one container of (tenant, workload,
// class, object), for the host holding the workload's lease at r.Epoch. A
// key that already holds a reservation is renewed and returned (created
// false): a restart needs no new headroom (only a larger container is
// re-checked for the difference), and the lease holder adopts a key another
// host held. A new key is checked against the tenant's container quotas and
// the class's max_instances in the store transaction that inserts it.
func (m *Mgmt) ReserveContainer(ctx context.Context, hostID string, r ReserveRequest) (ContainerReservation, bool, error) {
	if hostID == "" {
		return ContainerReservation{}, false, errf(400, "bad_request", "container reservations need a host (Twd-Host-ID header on the local-dev socket)")
	}
	if r.TenantID == "" || r.Workload == "" || r.Class == "" || r.Object == "" || len(r.Object) > 256 || strings.ContainsRune(r.Object, 0) {
		return ContainerReservation{}, false, errf(400, "bad_request", "tenantID, workload, class and object (≤256 bytes) are required")
	}
	if !r.Resources.Valid() {
		return ContainerReservation{}, false, errf(400, "bad_request", "resources: vcpu and memMiB must be positive (vcpu ≤ 64), diskMiB ≥ 0")
	}
	t, err := m.store.GetTenant(ctx, r.TenantID)
	if err != nil {
		return ContainerReservation{}, false, lookupErr(err, errf(404, "unknown_tenant", "tenant %q not found", r.TenantID))
	}
	lease := LeaseName(r.TenantID, r.Workload)
	now := m.now()
	l, err := m.store.GetLease(ctx, lease)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ContainerReservation{}, false, err
	}
	if err != nil || l.Epoch != r.Epoch || l.Holder != hostID || l.ExpiresAt < now.UnixMilli() {
		m.audit(ctx, AuditRecord{TenantID: r.TenantID, Kind: "deny", Code: "lease_lost", ActingFor: "host:" + hostID},
			map[string]any{"host": hostID, "epoch": r.Epoch, "container": r.Class + "/" + r.Object, "workload": r.Workload})
		return ContainerReservation{}, false, errf(409, "lease_lost", "the lease on %s is not held by %q at epoch %d; only the workload's lease holder may start its containers", lease, hostID, r.Epoch)
	}
	q := ContainerQuota{MaxContainers: t.MaxContainers, MaxMemMiB: t.MaxContainerMemMiB, MaxMicroVCPU: micro(t.MaxContainerVCPU),
		StaleBefore: now.Add(-ContainerStaleAfter).UnixMilli()}
	if d, err := m.latestDeployment(ctx, r.TenantID, r.Workload); err == nil {
		for _, c := range d.Containers {
			if c.Class == r.Class {
				q.ClassMax = c.MaxInstances
			}
		}
	}
	res := ContainerReservation{ID: newID("cr_"), TenantID: r.TenantID, Workload: r.Workload, Class: r.Class, Object: r.Object,
		HostID: hostID, Resources: r.Resources, CreatedAt: now.UnixMilli(), RenewedAt: now.UnixMilli()}
	detail := map[string]any{"host": hostID, "workload": r.Workload, "class": r.Class, "object": r.Object,
		"vcpu": r.Resources.VCPU, "memMiB": r.Resources.MemMiB, "diskMiB": r.Resources.DiskMiB}
	actx, ai := m.intend(ctx, AuditRecord{TenantID: r.TenantID, Kind: "container_reserve", ActingFor: "host:" + hostID}, detail)
	got, created, err := m.store.ReserveContainer(actx, res, q, &Fence{Lease: lease, Holder: hostID, Epoch: r.Epoch})
	var qe *ContainerQuotaError
	switch {
	case errors.As(err, &qe):
		m.audit(ctx, AuditRecord{TenantID: r.TenantID, Kind: "deny", Code: "container_quota_exceeded", ActingFor: "host:" + hostID},
			map[string]any{"host": hostID, "workload": r.Workload, "class": r.Class, "object": r.Object, "quota": qe.Quota, "limit": qe.Limit, "used": qe.Used})
		e := errf(429, "container_quota_exceeded", "tenant %s: %s %g reached (%g in use, %g requested by %s/%s); stop a container, or raise the limit (PATCH /v1/tenants/%s)",
			r.TenantID, qe.Quota, qe.Limit, qe.Used, qe.Requested, r.Class, r.Object, r.TenantID)
		if qe.Quota == "max_instances" {
			e.Message = fmt.Sprintf("class %s of workload %s runs %g containers (max_instances %g in its containers config)", r.Class, r.Workload, qe.Used, qe.Limit)
		}
		e.Missing = map[string]any{"quota": qe.Quota, "limit": qe.Limit, "used": qe.Used, "requested": qe.Requested}
		return ContainerReservation{}, false, e
	case errors.Is(err, ErrLeaseLost):
		return ContainerReservation{}, false, errf(409, "lease_lost", "%v", err)
	case err != nil:
		return ContainerReservation{}, false, err
	}
	m.recordIntent(ctx, ai, map[string]any{"reservationID": got.ID, "existing": !created})
	return got, created, nil
}

// latestDeployment is the newest deployment of (tenant, workload).
func (m *Mgmt) latestDeployment(ctx context.Context, tenantID, workload string) (Deployment, error) {
	ds, err := m.store.ListDeployments(ctx, DeploymentFilter{TenantID: tenantID, Workload: workload, Latest: true}, Page{})
	if err != nil {
		return Deployment{}, err
	}
	if len(ds) == 0 {
		return Deployment{}, ErrNotFound
	}
	return ds[len(ds)-1], nil
}

// ReleaseContainer drops a reservation (its container stopped). hostID ""
// (an admin) releases any; a host only its own (403).
func (m *Mgmt) ReleaseContainer(ctx context.Context, id, hostID, by string) error {
	notFound := errf(404, "not_found", "no container reservation %q (already released?)", id)
	r, err := m.store.GetContainerReservation(ctx, id)
	if err != nil {
		return lookupErr(err, notFound)
	}
	if hostID != "" && r.HostID != hostID {
		return errf(403, "forbidden", "reservation %s is held by host %s", id, r.HostID)
	}
	actx, ai := m.intend(ctx, AuditRecord{TenantID: r.TenantID, Kind: "container_release", ActingFor: by},
		map[string]any{"reservationID": id, "host": r.HostID, "workload": r.Workload, "class": r.Class, "object": r.Object, "reason": "stopped"})
	switch err := m.store.ReleaseContainer(actx, id, r.HostID); {
	case errors.Is(err, ErrNotFound): // released (or taken over) meanwhile
		return notFound
	case err != nil:
		return err
	}
	m.recordIntent(ctx, ai, nil)
	return nil
}

// reconcileContainers applies a heartbeat's running containers: reported
// reservations of this host are renewed, unreported ones older than
// ContainerReleaseAfter released, and reported containers without one
// adopted when this host holds the workload's lease.
func (m *Mgmt) reconcileContainers(ctx context.Context, hostID string, running []RunningContainer) {
	now := m.now()
	held := map[string]bool{}
	for _, rc := range running {
		ln := LeaseName(rc.TenantID, rc.Workload)
		if _, ok := held[ln]; ok {
			continue
		}
		l, err := m.store.GetLease(ctx, ln)
		held[ln] = err == nil && l.Holder == hostID && l.ExpiresAt >= now.UnixMilli()
	}
	rs := make([]ContainerReservation, 0, len(running))
	for _, rc := range running {
		if rc.TenantID == "" || rc.Workload == "" || rc.Class == "" || rc.Object == "" || !rc.Resources.Valid() {
			continue
		}
		rs = append(rs, ContainerReservation{ID: newID("cr_"), TenantID: rc.TenantID, Workload: rc.Workload, Class: rc.Class, Object: rc.Object,
			HostID: hostID, Resources: rc.Resources, CreatedAt: now.UnixMilli(), RenewedAt: now.UnixMilli()})
	}
	adopted, released, err := m.store.ReconcileContainers(ctx, hostID, rs, now.UnixMilli(), now.Add(-ContainerReleaseAfter).UnixMilli(),
		func(r ContainerReservation) bool { return held[LeaseName(r.TenantID, r.Workload)] })
	if err != nil {
		m.log.Warn("container reconcile", "host", hostID, "err", err)
		return
	}
	for _, r := range adopted {
		m.audit(ctx, AuditRecord{TenantID: r.TenantID, Kind: "container_reserve", ActingFor: "host:" + hostID},
			map[string]any{"reservationID": r.ID, "host": hostID, "workload": r.Workload, "class": r.Class, "object": r.Object, "adopted": true,
				"vcpu": r.Resources.VCPU, "memMiB": r.Resources.MemMiB, "diskMiB": r.Resources.DiskMiB})
	}
	for _, r := range released {
		m.audit(ctx, AuditRecord{TenantID: r.TenantID, Kind: "container_release", ActingFor: "host:" + hostID},
			map[string]any{"reservationID": r.ID, "host": hostID, "workload": r.Workload, "class": r.Class, "object": r.Object,
				"reason": fmt.Sprintf("not reported by its host for %v", ContainerReleaseAfter)})
	}
}
