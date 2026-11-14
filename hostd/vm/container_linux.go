//go:build linux

package vm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm/helperapi"
)

// ContainersUsable reports whether BootContainer can work on this host: the
// helper speaks container guests and the container kernel has erofs (the
// image format). twd-hostd offers containers only then (a host that can't
// run them doesn't advertise them, so placement never sends it any).
func (b *Backend) ContainersUsable() error {
	if err := b.helperContainers(); err != nil {
		return err
	}
	kernel := b.cfg.ContainerKernel
	if kernel == "" {
		kernel = b.cfg.Kernel
	}
	return KernelHasErofs(kernel)
}

// helperContainers checks that the helper service supports container guests
// (the in-process helper always does).
func (b *Backend) helperContainers() error {
	if b.sock == "" || b.srv != nil {
		return nil
	}
	r, err := helperapi.Ping(b.sock)
	if err != nil {
		return err
	}
	for _, f := range r.Features {
		if f == helperapi.FeatureContainer {
			return nil
		}
	}
	return errors.New("the microVM helper predates container guests (re-run `sudo twd host setup --microvm`)")
}

// BootContainer implements sandbox.ContainerBackend: one Firecracker VM
// per container, in its own jail (uid, netns, cgroup), with the image as a
// read-only drive and a scratch disk the helper makes fresh. The guest's
// NIC goes to a userspace stack with no handlers at all, so it has no
// egress of any kind (ADR-032 D4 deny-all; the egress proxy is phase B);
// hostd reaches the container's ports through that stack (DialPort).
func (b *Backend) BootContainer(ctx context.Context, s sandbox.ContainerSpec) (sandbox.Container, error) {
	if err := b.helperContainers(); err != nil {
		return nil, err
	}
	if s.VCPUs < 1 || s.MemMiB < 128 || len(s.Argv) == 0 || s.Image == "" {
		return nil, fmt.Errorf("bad container spec (vcpus %d, mem %d MiB, argv %q)", s.VCPUs, s.MemMiB, s.Argv)
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
	req := &helperapi.StartRequest{Name: s.ID, MemMiB: s.MemMiB, VCPUs: s.VCPUs, HugePages: b.cfg.HugePages,
		BootArgs: b.cfg.BootArgs, MTU: b.cfg.MTU, GuestMAC: guestMAC, GuestCID: 3, VsockPort: proto.VsockPort,
		Image: true, ScratchMiB: s.ScratchMiB, Pids: pids + 64}
	kernel := b.cfg.ContainerKernel
	if kernel == "" {
		kernel = b.cfg.Kernel
	}
	cs := &ctrState{exited: make(chan struct{}), streams: map[string]chan net.Conn{}, log: &tailBuf{max: 64 << 10}}
	gc := proto.GuestConfig{UnixNano: time.Now().UnixNano(), IP: proto.GuestIP, Gateway: proto.GatewayIP, Prefix: proto.PrefixLen,
		MTU: b.cfg.MTU, Container: &proto.ContainerConfig{Image: proto.ImageDev, Argv: s.Argv, Env: s.Env, Cwd: s.Cwd, User: s.User,
			Pids: pids, Hostname: s.Hostname}}
	if s.ScratchMiB > 0 {
		gc.Container.Scratch = proto.ScratchDev
	}
	l := launch{name: s.ID, log: s.Log, kernel: kernel, req: req, ctr: cs,
		handlers: containerHandlers(s), // v1: none, so every guest flow is refused at the host stack
		drive:    func(p string) error { return writeGuestJSON(p, gc) },
		extra: func(m *Machine) (*os.File, error) {
			f, err := os.Open(s.Image)
			if err != nil {
				return nil, fmt.Errorf("image: %w", err)
			}
			m.disks = append(m.disks, s.Image)
			return f, nil
		}}
	m, err := b.launch(ctx, l)
	if err != nil {
		return nil, err
	}
	// The guest boots on its whole vCPUs (a 1/16 vCPU kernel boot takes
	// seconds) and is capped to its fraction once the entrypoint runs. If
	// the cap can't be applied, the VM doesn't run uncapped.
	if quota > 0 {
		resp, _, err := m.conn.Do(helperapi.Request{Op: "cpu", CPUQuotaUS: quota}, nil, 10*time.Second)
		if err != nil {
			c := &ctrVM{m: m, s: cs}
			c.Stop(0)
			return nil, fmt.Errorf("cap the container's CPU: %w", err)
		}
		if m.info.Jail != nil && resp.Jail != nil {
			m.info.Jail.CPUMax = resp.Jail.CPUMax
		}
	}
	return &ctrVM{m: m, s: cs}, nil
}
