// Package sandbox is the portable interface between twd-hostd and an
// isolation backend that runs one workerd instance somewhere other than a
// host process. The microVM tier (hostd/vm, Linux + KVM) implements it; hostd
// itself never imports Linux-only code, so the process tier keeps building
// and testing on macOS.
//
// The contract mirrors the process tier's four sockets:
//
//	hostd ──Ingress()──▶ guest workerd ingress (cfBlobHeader x-twd-cf)
//	guest ──gateway:port──▶ Spec.Handlers[port]  (facet egress, workload egress, host API)
//
// The guest reaches nothing else: every other flow is refused at the
// host-side network stack (ADR-019), and no tailnet key ever crosses it.
package sandbox

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
)

// Spec describes one sandboxed workerd instance.
type Spec struct {
	// ID is short and unique on this host (it names the VM's network
	// namespace and run directory).
	ID string
	// AppDir is the generated workerd instance directory; it is delivered
	// to the guest whole (config drive) and becomes its working directory.
	AppDir string
	// WorkerdArgs is workerd's argv after the binary.
	WorkerdArgs []string
	// HealthCF is the x-twd-cf blob of the platform health request.
	HealthCF string
	// Handlers are the host services the guest may reach, by gateway TCP port.
	Handlers map[uint16]http.Handler
	// Debug enables guest diagnostics (connectivity probes).
	Debug bool
	// Volume, if set, is the workload's persistent DO storage: attached
	// to the guest as a block device and mounted at proto.DODir.
	Volume *Volume
	Log    *slog.Logger
}

// Volume is a persistent, encrypted-at-rest block volume for one
// (tenant, workload). The image lives on the host (Path); the backend
// creates it (sparse, SizeMiB) if it is missing. Key is the volume's data
// key from mgmt custody: it stays on the host (dm-crypt there); the guest
// sees only plaintext ext4.
type Volume struct {
	Path    string
	Name    string // "tenant/workload"
	Epoch   int64  // the namespace lease epoch (fencing, ADR-011)
	Key     []byte
	SizeMiB int
}

// String never prints the key.
func (v *Volume) String() string {
	if v == nil {
		return "<nil>"
	}
	return "sandbox.Volume{" + v.Name + "}"
}

// Info is a sandbox's boot accounting.
type Info struct {
	PID int // VMM process
	// ServicePID is the hypervisor process that holds the guest's memory and
	// vCPUs when that is not the VMM itself (vz: Virtualization.framework's
	// per-VM XPC service). 0 under Firecracker.
	ServicePID int `json:",omitempty"`
	// Boot is InstanceStart → guest init reported in; Ready is
	// InstanceStart → workerd healthy in the guest; Total is the whole Boot()
	// call (netns, TAP, stack, VMM, API calls, boot, workerd).
	Boot, Ready, Total time.Duration
	GuestUptimeAtReady time.Duration
	// Jail is how the VMM is confined (microVM tier: jailer chroot,
	// uid/gid, cgroup limits, seccomp).
	Jail *Jail `json:",omitempty"`
	// Console is the host file with the guest's serial console (guestd and
	// workerd output).
	Console string `json:",omitempty"`
	// HugePages: guest RAM is backed by 2 MiB pages.
	HugePages bool
	// Volume: the persistent DO volume ("" if none); VolumeNew if it was
	// created (formatted) by this boot; VolumeAttachMS is the host-side
	// attach time (loop + dm-crypt open [+ format]).
	Volume         string  `json:",omitempty"`
	VolumeNew      bool    `json:",omitempty"`
	VolumeAttachMS float64 `json:",omitempty"`
}

// Jail describes the VMM's confinement.
type Jail struct {
	ID, Chroot, Cgroup, CPUMax, Seccomp, Netns string
	UID, GID                                   int
	MemMax                                     int64
}

// VM is one running sandbox.
type VM interface {
	// Ingress forwards an (already attributed) request to the guest's workerd.
	Ingress() http.Handler
	// DialGuest opens a TCP connection to the guest's workerd ingress.
	DialGuest(ctx context.Context) (net.Conn, error)
	// Done is closed when the sandbox has exited (crash, kill or Stop); by
	// then every flow into it has been aborted.
	Done() <-chan struct{}
	// Err is why it exited (nil after Stop).
	Err() error
	Stop()
	Info() Info
	// Op sends a control op to the guest agent and waits for its answer.
	Op(ctx context.Context, m proto.Msg) (proto.Msg, error)
	// Refused lists flows the guest tried that the host stack refused.
	Refused() []string
	// RSSKB is the VMM process's resident set (guest memory it touched,
	// plus the VMM itself).
	RSSKB() int64
	// Kill terminates the VMM abruptly (tests: crash → fast failure).
	Kill()
	// MemoryDump pauses the VM, writes a full snapshot (guest memory file)
	// into dir, resumes it and returns the memory file path.
	MemoryDump(ctx context.Context, dir string) (string, error)
	// Disks are the host paths of the guest's block devices.
	Disks() []string
}

// Backend boots sandboxes.
type Backend interface {
	Boot(ctx context.Context, s Spec) (VM, error)
	Name() string
}
