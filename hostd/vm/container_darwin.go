//go:build darwin

package vm

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm/vzapi"
)

// ContainersUsable reports whether BootContainer can work on this Mac: the
// guest kernel has erofs (the image format; guest/build-kernel-vz.sh
// builds it in). twd-hostd offers containers only then.
func (b *Backend) ContainersUsable() error {
	return KernelHasErofs(b.cfg.Kernel)
}

// EmulatedArches are the image architectures this backend runs besides
// its own: linux/amd64 under Rosetta for Linux when it is shared into the
// guests (Config.Rosetta on Apple silicon). The image store accepts them,
// so the stock amd64-only images (docker.io/cloudflare/sandbox) run as
// they are, without the arm64 alias substitution.
func (b *Backend) EmulatedArches() []string {
	if b.cfg.Rosetta && runtime.GOARCH == "arm64" {
		return []string{"amd64"}
	}
	return nil
}

// BootContainer implements sandbox.ContainerBackend on vz: one VM (one
// twd-vz process) per container, booting the same guest image and
// container mode as Firecracker (guestd: erofs image read-only on
// /dev/vdc, a fresh scratch disk on /dev/vdd as the overlay upper, exec
// stdio on its own vsock connections). Differences from Firecracker, all
// because macOS has no jailer, cgroups or mkfs.ext4:
//   - the scratch is a sparse file of ScratchMiB that the guest formats;
//   - a fractional vCPU (CPUMax) is applied as cpu.max on the guest's
//     container cgroup (the VM's whole vCPU count is the hard cap);
//   - pids.max is the guest's only (there is one VMM process, no cgroup).
//
// The NIC is the same userspace stack with containerHandlers (deny-all in
// v1), so the guest has no network path except the ones hostd serves.
func (b *Backend) BootContainer(ctx context.Context, s sandbox.ContainerSpec) (sandbox.Container, error) {
	if s.VCPUs < 1 || s.MemMiB < 128 || len(s.Argv) == 0 || s.Image == "" {
		return nil, fmt.Errorf("bad container spec (vcpus %d, mem %d MiB, argv %q)", s.VCPUs, s.MemMiB, s.Argv)
	}
	if s.ScratchMiB < 0 || s.ScratchMiB > 1<<20 {
		return nil, fmt.Errorf("bad container scratch size %d MiB", s.ScratchMiB)
	}
	quota := 0
	if s.CPUMax > 0 && s.CPUMax < float64(s.VCPUs) {
		quota = int(math.Round(s.CPUMax * 100000))
		if quota < 1000 {
			quota = 1000 // the kernel's floor for cpu.max is 1 ms
		}
	}
	pids := s.Pids
	if pids <= 0 {
		pids = 4096
	}
	cs := &ctrState{exited: make(chan struct{}), streams: map[string]chan net.Conn{}, log: &tailBuf{max: 64 << 10}}
	gc := proto.GuestConfig{UnixNano: time.Now().UnixNano(), IP: proto.GuestIP, Gateway: proto.GatewayIP, Prefix: proto.PrefixLen,
		MTU: b.cfg.MTU, Container: &proto.ContainerConfig{Image: proto.ImageDev, Argv: s.Argv, Env: s.Env, Cwd: s.Cwd, User: s.User,
			Pids: pids, Hostname: s.Hostname, CPUQuotaUS: quota}}
	if b.cfg.Rosetta {
		gc.Rosetta = vzapi.RosettaTag
	}
	m, err := b.launch(ctx, vzLaunch{name: s.ID, log: s.Log, cpus: s.VCPUs, memMiB: s.MemMiB, ctr: cs,
		handlers: containerHandlers(s),
		prepare: func(m *Machine, drive string) ([]vzapi.Disk, error) {
			if _, err := os.Stat(s.Image); err != nil {
				return nil, fmt.Errorf("image: %w", err)
			}
			disks := []vzapi.Disk{{Path: s.Image, ReadOnly: true}} // /dev/vdc
			if s.ScratchMiB > 0 {
				p := filepath.Join(m.dir, "scratch.img") // /dev/vdd: sparse, blank, gone with the VM
				f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
				if err != nil {
					return nil, fmt.Errorf("scratch: %w", err)
				}
				err = f.Truncate(int64(s.ScratchMiB) << 20)
				f.Close()
				if err != nil {
					return nil, fmt.Errorf("scratch: %w", err)
				}
				disks = append(disks, vzapi.Disk{Path: p})
				gc.Container.Scratch, gc.Container.ScratchFormat = proto.ScratchDev, true
			}
			return disks, writeGuestJSON(drive, gc)
		}})
	if err != nil {
		return nil, err
	}
	if quota > 0 {
		m.info.Jail = &sandbox.Jail{CPUMax: fmt.Sprintf("%d 100000 (guest cgroup)", quota)}
	}
	return &ctrVM{m: m, s: cs}, nil
}
