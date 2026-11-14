package sandbox

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// ContainerSpec describes one container guest (ADR-032): a microVM that
// runs an OCI image instead of workerd. It belongs to exactly one Durable
// Object and is a lower trust domain than it: nothing of the parent (its
// DO volume, volume key, egress MAC key, tailnet identity) is ever part of
// a ContainerSpec.
type ContainerSpec struct {
	// ID is short and unique on this host (logs, run dir).
	ID string
	// Image is the host path of the image's erofs (hostd/image).
	Image string
	// ScratchMiB sizes the instance's ephemeral scratch disk (overlay upper).
	ScratchMiB int
	// VCPUs is the VM's vCPU count (≥ 1); CPUMax, if in (0, VCPUs), caps
	// it to a fraction of vCPUs (cgroup cpu.max on the VMM), e.g. 0.0625.
	VCPUs  int
	CPUMax float64
	MemMiB int
	// Pids is pids.max inside the guest (and on the VMM's cgroup).
	Pids int
	// Argv, Env, Cwd, User: the process, after the image config and the
	// start() overrides are merged (see ContainerProcess).
	Argv     []string
	Env      []string
	Cwd      string
	User     string
	Hostname string
	// EnableInternet is start({enableInternet}) (recorded; Net enforces it).
	EnableInternet bool
	// Net is the host side of the guest's NIC (ADR-032 D4: the sandbox
	// egress proxy, netd/sbxnet). Nil: no egress of any kind, and no DNS.
	Net ContainerNet
	// CACert is the PEM certificate of the container's HTTPS-intercept CA,
	// installed in the guest (its key never is).
	CACert []byte
	// Canary, if set, is put into the guest's memory (an env var of the
	// guest agent's container config) as the no-keys scan's positive
	// control: a RAM dump that doesn't contain it isn't a real dump.
	Canary string
	Log    *slog.Logger
}

// ContainerNet is the host side of a container guest's network. The
// backend hands it every TCP flow and DNS query the guest makes; nothing
// else leaves the guest.
type ContainerNet interface {
	// Accept decides a TCP flow to dst before its handshake completes:
	// false resets it.
	Accept(dst netip.AddrPort) bool
	// Serve owns an accepted flow until it ends.
	Serve(c net.Conn, dst netip.AddrPort)
	// DNS answers one query to the gateway's port 53 (wire format); nil
	// drops it.
	DNS(query []byte) []byte
}

// ExecSpec is one exec in a container.
type ExecSpec struct {
	Argv  []string
	Env   []string
	Cwd   string
	User  string
	Stdin bool
	// Stdout: "pipe" (default) | "ignore"; Stderr: "pipe" (default) |
	// "ignore" | "combined".
	Stdout, Stderr string
	PTY            bool
	Cols, Rows     int
}

// Exec is a running exec.
type Exec interface {
	ID() string
	PID() int
	// Stdin is nil unless ExecSpec.Stdin; Close is the half-close.
	Stdin() io.WriteCloser
	// Stdout and Stderr are nil when not piped. They end when the process
	// has exited (and its output is drained).
	Stdout() io.Reader
	Stderr() io.Reader
	// Wait returns the exit code (128+n for signal n).
	Wait(ctx context.Context) (int, error)
	Signal(sig int) error
	Resize(cols, rows int) error
}

// Container is one running container guest.
type Container interface {
	Exec(ctx context.Context, s ExecSpec) (Exec, error)
	// Signal signals the entrypoint process.
	Signal(sig int) error
	// DialPort opens a TCP connection to a port in the guest (getTcpPort).
	DialPort(ctx context.Context, port uint16) (net.Conn, error)
	// Exited is closed when the entrypoint has exited; ExitCode is then
	// its status.
	Exited() <-chan struct{}
	ExitCode() int
	// Done is closed when the VM is gone (exit, Stop, Kill, crash).
	Done() <-chan struct{}
	// Stop sends SIGTERM to the entrypoint, waits up to grace for it to
	// exit, then kills the VM; it returns when the VM is gone.
	Stop(grace time.Duration)
	Kill()
	Info() Info
	RSSKB() int64
	// Log is the tail of the entrypoint's output.
	Log() string
}

// ContainerBackend boots container guests.
type ContainerBackend interface {
	BootContainer(ctx context.Context, s ContainerSpec) (Container, error)
}

// Resources are a container's limits.
type Resources struct {
	VCPU      float64 `json:"vcpu"`
	MemoryMiB int     `json:"memoryMib"`
	DiskMB    int     `json:"diskMb"`
}

// InstanceTypes are Cloudflare's container instance types
// (developers.cloudflare.com/containers/platform-details/limits, checked
// 2026-11-13).
var InstanceTypes = map[string]Resources{
	"lite":       {VCPU: 1.0 / 16, MemoryMiB: 256, DiskMB: 2000},
	"basic":      {VCPU: 0.25, MemoryMiB: 1024, DiskMB: 4000},
	"standard-1": {VCPU: 0.5, MemoryMiB: 4096, DiskMB: 8000},
	"standard-2": {VCPU: 1, MemoryMiB: 6144, DiskMB: 12000},
	"standard-3": {VCPU: 2, MemoryMiB: 8192, DiskMB: 16000},
	"standard-4": {VCPU: 4, MemoryMiB: 12288, DiskMB: 20000},
}

// Custom instance bounds (Cloudflare: up to 4 vCPU, 12 GiB, 20 GB; at
// least 3 GiB of memory per vCPU is not enforced here).
const (
	MaxVCPU      = 4.0
	MaxMemoryMiB = 12288
	MaxDiskMB    = 20000
	MinMemoryMiB = 128
)

// ResolveInstance turns start({instance}) into resources: a type name,
// a custom {vcpu, memoryMib, diskMb}, or "" (the class's instance_type,
// else lite).
func ResolveInstance(name string, custom *Resources, classDefault string) (Resources, error) {
	if custom != nil {
		r := *custom
		if !(r.VCPU > 0) || r.VCPU > MaxVCPU || math.IsNaN(r.VCPU) || r.MemoryMiB < MinMemoryMiB || r.MemoryMiB > MaxMemoryMiB || r.DiskMB < 64 || r.DiskMB > MaxDiskMB {
			return Resources{}, fmt.Errorf("instance %+v out of bounds (vcpu ≤ %v, %d ≤ memoryMib ≤ %d, 64 ≤ diskMb ≤ %d)", r, MaxVCPU, MinMemoryMiB, MaxMemoryMiB, MaxDiskMB)
		}
		return r, nil
	}
	if name == "" {
		name = classDefault
	}
	if name == "" {
		name = "lite"
	}
	r, ok := InstanceTypes[name]
	if !ok {
		return Resources{}, fmt.Errorf("unknown instance type %q (lite, basic, standard-1 … standard-4, or {vcpu, memoryMib, diskMb})", name)
	}
	return r, nil
}

// VMShape maps resources onto a Firecracker VM (ADR-032 D5): whole vCPUs
// (≥ 1: Firecracker can't do less) plus cpu.max for a fraction, guest
// memory, and the scratch disk.
func (r Resources) VMShape() (vcpus int, cpuMax float64, memMiB, scratchMiB int) {
	vcpus = int(math.Ceil(r.VCPU))
	if vcpus < 1 {
		vcpus = 1
	}
	if r.VCPU < float64(vcpus) {
		cpuMax = r.VCPU
	}
	scratchMiB = int(math.Ceil(float64(r.DiskMB) * 1e6 / (1 << 20)))
	return vcpus, cpuMax, r.MemoryMiB, scratchMiB
}

// ContainerProcess merges an image config with start() overrides into the
// entrypoint process: start({entrypoint}) replaces ENTRYPOINT and CMD both
// (Cloudflare and `docker run --entrypoint` semantics); start({env}) is
// layered over the image's Env.
func ContainerProcess(imgEntrypoint, imgCmd, imgEnv []string, override []string, env map[string]string) (argv, outEnv []string, err error) {
	argv = append(append([]string{}, imgEntrypoint...), imgCmd...)
	if len(override) > 0 {
		argv = append([]string{}, override...)
	}
	if len(argv) == 0 {
		return nil, nil, fmt.Errorf("the image has no ENTRYPOINT or CMD, and start() named no entrypoint")
	}
	outEnv = append([]string{}, imgEnv...)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.Contains(env[k], "\x00") {
			return nil, nil, fmt.Errorf("bad env name %q", k)
		}
		outEnv = setEnv(outEnv, k, env[k])
	}
	return argv, outEnv, nil
}

func setEnv(env []string, k, v string) []string {
	for i, kv := range env {
		if strings.HasPrefix(kv, k+"=") {
			env[i] = k + "=" + v
			return env
		}
	}
	return append(env, k+"="+v)
}
