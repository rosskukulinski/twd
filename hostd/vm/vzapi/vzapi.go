// Package vzapi is the contract between twd-hostd and twd-vz, the
// per-VM Virtualization.framework VMM on macOS (ADR-032 D7).
//
// twd-hostd stays a pure-Go (CGO_ENABLED=0) binary. Every microVM is one
// twd-vz process: it is the only code that links Virtualization.framework,
// it carries the com.apple.security.virtualization entitlement, and it is
// spawned by hostd with everything it needs as inherited descriptors:
//
//	fd 3  NIC: one end of an AF_UNIX SOCK_DGRAM socketpair; each datagram
//	      is one ethernet frame (VZFileHandleNetworkDeviceAttachment).
//	      hostd's userspace netstack holds the other end: it is the guest's
//	      only network (ADR-019).
//	fd 4  control: AF_UNIX SOCK_STREAM, newline-delimited JSON (Ctl).
//	fd 5  vsock: AF_UNIX SOCK_STREAM; for every connection the guest opens
//	      to the host on Config.VsockPort, twd-vz sends one byte with the
//	      connection's descriptor attached (SCM_RIGHTS). The VMM is not in
//	      the data path of the guest agent's channel.
//	fd 6  (optional) the decrypted DO volume block device (/dev/rdiskN,
//	      opened by hostd), attached as the third virtio-blk disk.
//
// The VMM's config is one JSON document on stdin. Nothing secret crosses
// this interface: the volume's key stays in hostd and the disk-image
// framework (hdiutil); the VMM gets an already-decrypted block device.
//
// Lifetime: the VMM stops the VM and exits when its control connection
// closes (hostd died or dropped it), so a VM never outlives its hostd.
package vzapi

// Inherited descriptor numbers in the VMM.
const (
	FDNet    = 3
	FDCtl    = 4
	FDVsock  = 5
	FDVolume = 6
)

// Config is the VMM's boot configuration (stdin, JSON).
type Config struct {
	Kernel  string `json:"kernel"`
	Initrd  string `json:"initrd,omitempty"`
	Cmdline string `json:"cmdline"`
	CPUs    int    `json:"cpus"`
	MemMiB  int    `json:"memMiB"`
	// Disks are attached in order as virtio-blk (/dev/vda, vdb, ...):
	// the rootfs, the per-VM config drive (Disks[ConfigDisk]), then a
	// container guest's image (vdc) and scratch (vdd).
	Disks []Disk `json:"disks"`
	// Volume attaches fd FDVolume as the next disk.
	Volume bool `json:"volume,omitempty"`
	// Console is the file the guest's virtio console (hvc0) is appended to.
	Console   string `json:"console"`
	MAC       string `json:"mac"`
	MTU       int    `json:"mtu,omitempty"`
	VsockPort uint32 `json:"vsockPort"`
	// Rosetta shares Rosetta for Linux into the guest as the virtiofs tag
	// RosettaTag (Apple silicon only; the guest registers it with
	// binfmt_misc to run x86_64 binaries).
	Rosetta bool `json:"rosetta,omitempty"`
}

// ConfigDisk is the index in Config.Disks of the VM's config drive: the
// one disk no other VM has open (the VMM finds its VM service by it; a
// container image is shared read-only by every VM that runs it).
const ConfigDisk = 1

// RosettaTag is the virtiofs tag of the Rosetta directory share.
const RosettaTag = "rosetta"

// Disk is one disk image attached by path (read by the VMM itself).
type Disk struct {
	Path     string `json:"path"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// Ctl is one control message, either direction.
//
// VMM → hostd: Event "started" (the VM is running; PID is the VMM's),
// "stopped" (the guest powered off or the VM failed: Error says why; the
// VMM exits right after), "resp" (answer to an Op, same ID).
// hostd → VMM: Op "pause", "resume", "save" (pause, write the machine
// state to Path, resume: macOS 14+), "stop" (hard stop, then exit).
type Ctl struct {
	Event string `json:"event,omitempty"`
	Op    string `json:"op,omitempty"`
	ID    int64  `json:"id,omitempty"`
	Path  string `json:"path,omitempty"`
	Error string `json:"error,omitempty"`
	PID   int    `json:"pid,omitempty"`
	// ServicePID is the Virtualization.framework XPC process that runs the
	// guest (its memory and vCPUs): Virtualization.framework never runs a
	// VM inside the calling process. 0 if it could not be identified.
	ServicePID int `json:"servicePid,omitempty"`
	// StartMS is the VMM's config → VM running time.
	StartMS int64 `json:"startMs,omitempty"`
	// Rosetta reports the host's Rosetta for Linux availability on
	// "started" (installed | not-installed | not-supported) when asked.
	Rosetta string `json:"rosetta,omitempty"`
}
