//go:build linux

package main

// The container seccomp filter (ADR-032 D8; security review 2026-11-14):
// every container process (entrypoint and exec) runs under a filter that
// makes socket(AF_VSOCK, …) fail with EPERM. vsock is guestd's channel to
// the host; nothing in a container has any business on it, and the host
// authenticates it anyway (proto.GuestConfig.CtlToken, exec stream tokens).
//
// socketpair is filtered the same way (AF_VSOCK has none today: the kernel
// answers EOPNOTSUPP; the filter keeps it closed if that ever changes).
// io_uring_setup is EPERM too: io_uring's IORING_OP_SOCKET makes sockets
// without the socket syscall, out of seccomp's sight (Docker's default
// profile blocks io_uring as well, so images don't depend on it).
//
// Everything else is allowed: this is not a syscall allowlist, just the
// guest-plumbing cut. 32-bit compat syscalls are covered where the guest
// arch has them (i386 on amd64: socketcall can't be inspected, so it is
// EPERM as a whole; arm on arm64), x32 by masking its syscall bit, and an
// arch the filter doesn't know gets EPERM for everything.

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// seccompArch is one audit arch's syscall numbers the filter checks.
type seccompArch struct {
	audit      uint32
	x32Mask    bool // amd64: clear __X32_SYSCALL_BIT before comparing
	socket     uint32
	socketpair uint32
	socketcall int64 // -1: none
	ioUring    uint32
}

func seccompArches(goarch string) ([]seccompArch, error) {
	switch goarch {
	case "amd64":
		return []seccompArch{
			{audit: unix.AUDIT_ARCH_X86_64, x32Mask: true, socket: 41, socketpair: 53, socketcall: -1, ioUring: 425},
			{audit: unix.AUDIT_ARCH_I386, socket: 359, socketpair: 360, socketcall: 102, ioUring: 425},
		}, nil
	case "arm64":
		return []seccompArch{
			{audit: unix.AUDIT_ARCH_AARCH64, socket: 198, socketpair: 199, socketcall: -1, ioUring: 425},
			{audit: unix.AUDIT_ARCH_ARM, socket: 281, socketpair: 288, socketcall: -1, ioUring: 425},
		}, nil
	}
	return nil, fmt.Errorf("no container seccomp filter for %s", goarch)
}

// seccomp_data offsets (linux/seccomp.h).
const (
	sdNr    = 0
	sdArch  = 4
	sdArg0  = 16 // low 32 bits on little-endian (both guest arches)
	x32Bit  = 0x40000000
	retDeny = unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
)

// bpfProg assembles classic BPF with symbolic jump targets.
type bpfProg struct {
	ins    []unix.SockFilter
	labels map[string]int
	fix    []bpfFix
}

type bpfFix struct {
	at         int
	jt, jf, ja string
}

func (p *bpfProg) label(l string) { p.labels[l] = len(p.ins) }
func (p *bpfProg) stmt(code uint16, k uint32) {
	p.ins = append(p.ins, unix.SockFilter{Code: code, K: k})
}

// jeq: if A == k goto jt else goto jf ("" = the next instruction).
func (p *bpfProg) jeq(k uint32, jt, jf string) {
	p.fix = append(p.fix, bpfFix{at: len(p.ins), jt: jt, jf: jf})
	p.ins = append(p.ins, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: k})
}

func (p *bpfProg) assemble() ([]unix.SockFilter, error) {
	off := func(from int, l string) (int, error) {
		if l == "" {
			return 0, nil
		}
		to, ok := p.labels[l]
		if !ok {
			return 0, fmt.Errorf("bpf: no label %q", l)
		}
		d := to - from - 1
		if d < 0 || d > 255 {
			return 0, fmt.Errorf("bpf: jump to %q out of range", l)
		}
		return d, nil
	}
	for _, f := range p.fix {
		jt, err := off(f.at, f.jt)
		if err != nil {
			return nil, err
		}
		jf, err := off(f.at, f.jf)
		if err != nil {
			return nil, err
		}
		p.ins[f.at].Jt, p.ins[f.at].Jf = uint8(jt), uint8(jf)
	}
	return p.ins, nil
}

// ctrSeccompFilter builds the container filter for goarch.
func ctrSeccompFilter(goarch string) ([]unix.SockFilter, error) {
	arches, err := seccompArches(goarch)
	if err != nil {
		return nil, err
	}
	p := &bpfProg{labels: map[string]int{}}
	const ld = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
	p.stmt(ld, sdArch)
	for i := range arches {
		p.jeq(arches[i].audit, fmt.Sprintf("arch%d", i), "")
	}
	p.stmt(unix.BPF_RET|unix.BPF_K, retDeny) // an arch we don't know
	for i, a := range arches {
		p.label(fmt.Sprintf("arch%d", i))
		p.stmt(ld, sdNr)
		if a.x32Mask {
			p.stmt(unix.BPF_ALU|unix.BPF_AND|unix.BPF_K, ^uint32(x32Bit))
		}
		p.jeq(a.socket, "domain", "")
		p.jeq(a.socketpair, "domain", "")
		p.jeq(a.ioUring, "deny", "")
		if a.socketcall >= 0 {
			p.jeq(uint32(a.socketcall), "deny", "")
		}
		p.stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW)
	}
	p.label("domain")
	p.stmt(ld, sdArg0)
	p.jeq(unix.AF_VSOCK, "deny", "")
	p.stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW)
	p.label("deny")
	p.stmt(unix.BPF_RET|unix.BPF_K, retDeny)
	return p.assemble()
}

// installCtrSeccomp puts the container filter on every thread of this
// process (it survives execve). It needs CAP_SYS_ADMIN or no_new_privs;
// ctr-exec calls it while still root with every capability, so no
// no_new_privs is set (setuid binaries in the image keep working).
func installCtrSeccomp() error {
	f, err := ctrSeccompFilter(runtime.GOARCH)
	if err != nil {
		return err
	}
	prog := unix.SockFprog{Len: uint16(len(f)), Filter: &f[0]}
	r, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&prog)))
	runtime.KeepAlive(f)
	if e != 0 {
		return fmt.Errorf("seccomp: %w", e)
	}
	if r != 0 { // TSYNC: the thread that couldn't be synchronised
		return fmt.Errorf("seccomp: thread %d has a different filter", r)
	}
	return nil
}
