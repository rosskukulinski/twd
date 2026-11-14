package hostd

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/internal/container"
)

// Container admission (ADR-032 D9). ctx.container.start() reaches hostd,
// which, before it boots the container's VM, takes a ContainerSlot:
//
//  1. the host must offer containers (tier_unavailable: never a fallback);
//  2. local capacity (vms, guest memory, vCPUs × overcommit, scratch) must
//     have room: else 503 capacity_exhausted, no spill;
//  3. mgmt must reserve the tenant quota synchronously (429
//     container_quota_exceeded, 409 lease_lost). If mgmt is unreachable it
//     fails closed (503 quota_unavailable), except for a restart of a
//     container whose reservation this host still holds.
//
// Slot.Release (the container stopped) frees the local capacity and
// releases the reservation; a release mgmt doesn't acknowledge is kept
// (it still covers a restart) and retried after each good heartbeat, and
// heartbeats report the running containers so mgmt reconciles either way.

// ContainerKey names one container: one per DO instance (ADR-032 D2).
type ContainerKey struct {
	TenantID, Workload, Class, Object string
}

func (k ContainerKey) String() string {
	return k.TenantID + "/" + k.Workload + "/" + k.Class + "/" + k.Object
}

// ContainerQuota is mgmt's container reservation API as hostd uses it
// (Config.ContainerQuota; default: the mgmt client). Reserve's typed
// refusals are *mgmtapi.Error with a 4xx status; any other error means mgmt
// is unreachable. Release treats an unknown reservation as released.
type ContainerQuota interface {
	Reserve(ctx context.Context, r mgmtapi.ContainerReservationRequest) (*mgmtapi.ContainerReservation, error)
	Release(ctx context.Context, reservationID string) error
}

// mgmtQuota is ContainerQuota over the mgmt client.
type mgmtQuota struct{ c *mgmtapi.Client }

func (q mgmtQuota) Reserve(ctx context.Context, r mgmtapi.ContainerReservationRequest) (*mgmtapi.ContainerReservation, error) {
	return q.c.ReserveContainer(ctx, r)
}
func (q mgmtQuota) Release(ctx context.Context, id string) error {
	return q.c.ReleaseContainer(ctx, id)
}

// QuotaTimeout bounds one reservation call at start().
const QuotaTimeout = 10 * time.Second

type ctrEntry struct {
	key           ContainerKey
	res           container.Resources
	reservationID string
	running       bool // or starting: counts against local capacity
	gen           int  // bumps on every acquire (a stale slot can't release a newer one)
}

// containerLedger is this host's containers and the reservations it holds.
type containerLedger struct {
	mu   sync.Mutex
	held map[ContainerKey]*ctrEntry
}

// usage is the containers' part of HostUsage.
func (l *containerLedger) usage() mgmtapi.HostUsage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.usageLocked()
}

func (l *containerLedger) usageLocked() mgmtapi.HostUsage {
	var u mgmtapi.HostUsage
	for _, e := range l.held {
		if e.running {
			u.VMs++
			u.Containers++
			u.VMMemMiB += e.res.MemMiB
			u.VCPUs += e.res.VCPU
			u.ScratchMiB += e.res.DiskMiB
		}
	}
	return u
}

// running lists the running containers for the heartbeat (never nil).
func (l *containerLedger) running() []mgmtapi.RunningContainer {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []mgmtapi.RunningContainer{}
	for _, e := range l.held {
		if e.running && e.reservationID != "" {
			out = append(out, mgmtapi.RunningContainer{ReservationID: e.reservationID, TenantID: e.key.TenantID, Workload: e.key.Workload,
				Class: e.key.Class, Object: e.key.Object, Resources: e.res})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReservationID < out[j].ReservationID })
	return out
}

// ContainerSlot is an admitted container: local capacity and a quota
// reservation. Release it when the container stops.
type ContainerSlot struct {
	h   *Host
	key ContainerKey
	gen int
	// ReservationID is mgmt's reservation; Restarted is true when mgmt was
	// unreachable and the slot reuses the reservation this host held.
	ReservationID string
	Restarted     bool
	once          sync.Once
}

// containersOffered: the host runs containers (one microVM each): it has a
// container backend and an image store (twd-hostd sets them only when the
// backend is usable: helper, kernel with erofs, mkfs.erofs; see
// cmd/twd-hostd), and isn't forced to the process tier.
func (h *Host) containersOffered() bool {
	return h.cfg.Containers != nil && h.cfg.Images != nil && !h.cfg.ForceProcessTier
}

// AcquireContainer admits one container of key with resources res (what
// ctx.container.start() does before it boots the VM). See the package
// comment above for the order of checks and their errors.
func (h *Host) AcquireContainer(ctx context.Context, key ContainerKey, res container.Resources) (*ContainerSlot, error) {
	if !h.containersOffered() {
		return nil, &apiError{Code: "tier_unavailable", Detail: "this host has no container backend (containers run in microVMs: Linux with KVM and twd-vmhelper); ctx.container.start() is refused, not run another way"}
	}
	if key.TenantID == "" || key.Workload == "" || key.Class == "" || key.Object == "" || !res.Valid() {
		return nil, &apiError{Code: "bad_request", Detail: fmt.Sprintf("container %s: a key and positive resources are required", key)}
	}
	h.mu.Lock()
	w := h.workloads[key.TenantID+"/"+key.Workload]
	h.mu.Unlock()
	if w == nil {
		return nil, &apiError{Code: "lease_lost", Detail: fmt.Sprintf("workload %s/%s is not assigned to this host", key.TenantID, key.Workload)}
	}
	epoch := w.epochNow()
	base := h.vmUsage() // nodes and microVM DO instances

	l := &h.containers
	l.mu.Lock()
	prev := l.held[key]
	if prev != nil && prev.running {
		l.mu.Unlock()
		return nil, &apiError{Code: "container_running", Detail: fmt.Sprintf("container %s is already running", key)}
	}
	if err := h.checkCapacityLocked(base, res); err != nil {
		l.mu.Unlock()
		return nil, err
	}
	var prevCopy *ctrEntry
	if prev != nil {
		c := *prev
		prevCopy = &c
	}
	e := &ctrEntry{key: key, res: res, running: true}
	if prev != nil {
		e.gen = prev.gen + 1
	}
	l.held[key] = e // counts against capacity while mgmt answers
	gen := e.gen
	l.mu.Unlock()

	rctx, cancel := context.WithTimeout(ctx, QuotaTimeout)
	defer cancel()
	r, err := h.quota.Reserve(rctx, mgmtapi.ContainerReservationRequest{TenantID: key.TenantID, Workload: key.Workload,
		Class: key.Class, Object: key.Object, Epoch: epoch, Resources: res})
	slot := &ContainerSlot{h: h, key: key, gen: gen}
	l.mu.Lock()
	defer l.mu.Unlock()
	cur := l.held[key]
	if cur == nil || cur.gen != gen {
		return nil, &apiError{Code: "container_running", Detail: fmt.Sprintf("container %s was started concurrently", key)}
	}
	switch {
	case err == nil:
		cur.reservationID = r.ReservationID
		slot.ReservationID = r.ReservationID
		return slot, nil
	case quotaRefused(err):
		l.restoreLocked(key, prevCopy, gen)
		return nil, asAPIError(err)
	case prevCopy != nil && prevCopy.reservationID != "" && res.Fits(prevCopy.res):
		// mgmt unreachable, but this host still holds the key's
		// reservation (the container stopped and mgmt never acknowledged
		// the release): a restart within it is allowed.
		cur.reservationID, cur.res = prevCopy.reservationID, prevCopy.res
		slot.ReservationID, slot.Restarted = prevCopy.reservationID, true
		h.log.Warn("mgmt unreachable: restarting a container under the reservation this host holds", "container", key.String(),
			"reservation", prevCopy.reservationID, "err", err)
		return slot, nil
	default:
		l.restoreLocked(key, prevCopy, gen)
		h.log.Warn("mgmt unreachable: container start refused (fail closed)", "container", key.String(), "err", err)
		return nil, &apiError{Code: "quota_unavailable", Detail: fmt.Sprintf(
			"container %s: mgmt can't reserve its quota right now (%v); containers fail closed while mgmt is unreachable (only a restart within a reservation this host holds proceeds). Retry", key, err)}
	}
}

// restoreLocked undoes a failed acquire: the key goes back to what it was
// (a held, stopped reservation, or nothing).
func (l *containerLedger) restoreLocked(key ContainerKey, prev *ctrEntry, gen int) {
	if prev == nil {
		delete(l.held, key)
		return
	}
	p := *prev
	p.gen = gen
	l.held[key] = &p
}

// quotaRefused reports a typed refusal from mgmt (as opposed to mgmt being
// unreachable or unavailable, 5xx).
func quotaRefused(err error) bool {
	me, ok := mgmtapi.AsError(err)
	return ok && me.Status >= 400 && me.Status < 500
}

// checkCapacityLocked refuses a container that doesn't fit the host's
// remaining capacity (503 capacity_exhausted; no spill to another host).
// Called with h.containers.mu held (so concurrent starts can't both take
// the last room); base is the usage outside the ledger.
func (h *Host) checkCapacityLocked(base mgmtapi.HostUsage, res container.Resources) error {
	c := h.capacity()
	cu := h.containers.usageLocked()
	u := mgmtapi.HostUsage{VMs: base.VMs + cu.VMs, VMMemMiB: base.VMMemMiB + cu.VMMemMiB, VCPUs: base.VCPUs + cu.VCPUs, ScratchMiB: cu.ScratchMiB}
	type dim struct {
		name                      string
		capacity, used, requested float64
	}
	dims := []dim{
		{"vms", float64(c.VMs), float64(u.VMs), 1},
		{"vmMemMiB", float64(c.VMMemMiB), float64(u.VMMemMiB), float64(res.MemMiB)},
		{"vcpus", float64(c.VCPUs) * c.VCPUOvercommit, u.VCPUs, res.VCPU},
		{"scratchMiB", float64(c.ScratchMiB), float64(u.ScratchMiB), float64(res.DiskMiB)},
	}
	// The operator's optional container-only bounds (Config.ContainerCapacity).
	if cc := h.cfg.ContainerCapacity; cc.Max > 0 || cc.MemMiB > 0 || cc.VCPU > 0 {
		if cc.Max > 0 {
			dims = append(dims, dim{"containers", float64(cc.Max), float64(cu.Containers), 1})
		}
		if cc.MemMiB > 0 {
			dims = append(dims, dim{"containerMemMiB", float64(cc.MemMiB), float64(cu.VMMemMiB), float64(res.MemMiB)})
		}
		if cc.VCPU > 0 {
			dims = append(dims, dim{"containerVCPU", cc.VCPU, cu.VCPUs, res.VCPU})
		}
	}
	for _, d := range dims {
		if d.used+d.requested > d.capacity+1e-9 {
			return &apiError{Code: "capacity_exhausted", Detail: fmt.Sprintf("this host has no room for the container: %s %g used + %g requested > %g (containers stay with their Durable Object's host; nothing spills elsewhere)",
				d.name, d.used, d.requested, d.capacity), Extra: map[string]any{"missing": map[string]any{"capacity": d.name, "limit": d.capacity, "used": d.used, "requested": d.requested}}}
		}
	}
	return nil
}

// Release frees the slot's capacity and releases its reservation (the
// container stopped). Idempotent. A release mgmt doesn't acknowledge keeps
// the reservation held (a restart may reuse it) and is retried after the
// next good heartbeat.
func (s *ContainerSlot) Release(ctx context.Context) {
	s.once.Do(func() {
		l := &s.h.containers
		l.mu.Lock()
		e := l.held[s.key]
		if e == nil || e.gen != s.gen {
			l.mu.Unlock()
			return
		}
		e.running = false
		id := e.reservationID
		l.mu.Unlock()
		s.h.releaseReservation(ctx, s.key, s.gen, id)
	})
}

// releaseReservation tells mgmt; on success the stopped entry goes.
func (h *Host) releaseReservation(ctx context.Context, key ContainerKey, gen int, id string) {
	if id != "" {
		rctx, cancel := context.WithTimeout(ctx, QuotaTimeout)
		err := h.quota.Release(rctx, id)
		cancel()
		if err != nil {
			h.log.Warn("container reservation release failed (kept; retried after the next heartbeat)", "container", key.String(), "reservation", id, "err", err)
			return
		}
	}
	l := &h.containers
	l.mu.Lock()
	if e := l.held[key]; e != nil && e.gen == gen && !e.running {
		delete(l.held, key)
	}
	l.mu.Unlock()
}

// retryReleases re-sends the releases mgmt hasn't acknowledged (after a
// good heartbeat).
func (h *Host) retryReleases(ctx context.Context) {
	type rel struct {
		key ContainerKey
		gen int
		id  string
	}
	var todo []rel
	l := &h.containers
	l.mu.Lock()
	for k, e := range l.held {
		if !e.running {
			todo = append(todo, rel{k, e.gen, e.reservationID})
		}
	}
	l.mu.Unlock()
	for _, r := range todo {
		h.releaseReservation(ctx, r.key, r.gen, r.id)
	}
}
