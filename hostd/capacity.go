package hostd

import (
	"runtime"
	"syscall"

	"github.com/rosskukulinski/twd/hostd/mgmtapi"
)

// The capacity vector (ADR-032 D9): what this host registers and what its
// heartbeats report as used. Tailnet nodes are one dimension; the rest bound
// microVMs, DO instances and containers alike.

// Defaults for the measured capacity.
const (
	DefaultMaxVMs         = 64
	DefaultVCPUOvercommit = 1.0
	// defaultVMMemShare of the host's RAM is offered to guests; the rest
	// stays with the host (hostd, netd, workerd, the page cache).
	defaultVMMemShare = 0.75
	// defaultScratchShare of the free space under DataDir is offered as
	// container scratch disks (sparse files; this bounds their total size).
	defaultScratchShare = 0.9
)

// capacity is the vector this host registers: Config.Capacity when set
// (its nodes replaced by CapacityNodes), else measured. A host without a
// microVM backend has no VM capacity.
func (h *Host) capacity() mgmtapi.HostCapacity {
	h.capOnce.Do(func() {
		c := mgmtapi.HostCapacity{}
		switch {
		case h.cfg.Capacity != nil:
			c = *h.cfg.Capacity
		case h.hasVMs():
			total, huge := memInfo()
			c = mgmtapi.HostCapacity{VMs: DefaultMaxVMs, VMMemMiB: int(float64(total) * defaultVMMemShare), HugeMemMiB: huge,
				VCPUs: runtime.NumCPU(), VCPUOvercommit: DefaultVCPUOvercommit, ScratchMiB: int(float64(freeMiB(h.cfg.DataDir)) * defaultScratchShare)}
			if c.HugeMemMiB > c.VMMemMiB {
				c.VMMemMiB = c.HugeMemMiB // huge pages are reserved for guests already
			}
		}
		if !h.hasVMs() {
			c = mgmtapi.HostCapacity{}
		}
		if c.VCPUOvercommit == 0 && c.VCPUs > 0 {
			c.VCPUOvercommit = DefaultVCPUOvercommit
		}
		c.Nodes = h.cfg.CapacityNodes
		h.capVec = c
	})
	return h.capVec
}

// hasVMs: this host boots microVMs (DO instances or containers; both are
// Firecracker or vz guests sharing the VM dimensions).
func (h *Host) hasVMs() bool {
	return (h.cfg.VM != nil || h.cfg.Containers != nil) && !h.cfg.ForceProcessTier
}

// usage is the live usage: ready nodes, microVM DO instances (at the
// configured guest size) and running containers.
func (h *Host) usage() mgmtapi.HostUsage {
	u := h.vmUsage()
	cu := h.containers.usage()
	u.VMs += cu.VMs
	u.VMMemMiB += cu.VMMemMiB
	u.VCPUs += cu.VCPUs
	u.ScratchMiB += cu.ScratchMiB
	u.Containers = cu.Containers
	if c := h.capacity(); c.HugeMemMiB > 0 {
		u.HugeMemMiB = min(u.VMMemMiB, c.HugeMemMiB) // guests are backed by huge pages first
	}
	return u
}

// vmUsage is the usage outside the container ledger: ready nodes and the
// microVM DO instances.
func (h *Host) vmUsage() mgmtapi.HostUsage {
	var u mgmtapi.HostUsage
	h.mu.Lock()
	for _, o := range h.objects {
		if o.Status == statusReady {
			u.Nodes++
		}
	}
	wls := make([]*workload, 0, len(h.workloads))
	for _, w := range h.workloads {
		wls = append(wls, w)
	}
	h.mu.Unlock()
	for _, w := range wls {
		w.mu.Lock()
		vm := w.active != nil && w.active.tier == "microvm"
		w.mu.Unlock()
		if vm {
			u.VMs++
			u.VMMemMiB += h.cfg.VMMemMiB
			u.VCPUs += float64(h.cfg.VMVCPUs)
		}
	}
	return u
}

// memInfo returns the host's RAM and huge page pool in MiB (per OS:
// capacity_linux.go, capacity_other.go).
func memInfo() (totalMiB, hugeMiB int) { return osMemInfo() }

// freeMiB is the space available to this user under dir.
func freeMiB(dir string) int {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return 0
	}
	return int(uint64(st.Bavail) * uint64(st.Bsize) >> 20)
}
