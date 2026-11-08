//go:build linux

package procsb

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// deniedSyscalls fail with EPERM: debugging/introspection of other
// processes, kernel and mount control, BPF, perf, keyrings, namespaces.
var deniedSyscalls = append([]uintptr{
	unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV,
	unix.SYS_PIDFD_GETFD, unix.SYS_KCMP, unix.SYS_PROCESS_MADVISE,
	unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD, unix.SYS_REBOOT,
	unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE,
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT,
	unix.SYS_MOUNT_SETATTR, unix.SYS_OPEN_TREE, unix.SYS_MOVE_MOUNT,
	unix.SYS_FSOPEN, unix.SYS_FSCONFIG, unix.SYS_FSMOUNT, unix.SYS_FSPICK,
	unix.SYS_SWAPON, unix.SYS_SWAPOFF, unix.SYS_ACCT, unix.SYS_QUOTACTL,
	unix.SYS_NFSSERVCTL, unix.SYS_VHANGUP, unix.SYS_SYSLOG, unix.SYS_LOOKUP_DCOOKIE,
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD, unix.SYS_FANOTIFY_INIT,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
	unix.SYS_UNSHARE, unix.SYS_SETNS,
	unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_NAME_TO_HANDLE_AT,
}, archDeniedSyscalls...)

// enosysSyscalls fail with ENOSYS so libraries fall back: clone3 (its flags
// are behind a pointer seccomp can't inspect; glibc falls back to clone,
// whose flags are checked) and io_uring (libuv falls back to epoll/threads).
var enosysSyscalls = []uintptr{
	unix.SYS_CLONE3, unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
}

const cloneNSFlags = unix.CLONE_NEWNS | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWUSER |
	unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWCGROUP | unix.CLONE_NEWTIME

const (
	retAllow = unix.SECCOMP_RET_ALLOW
	retKill  = unix.SECCOMP_RET_KILL_PROCESS
)

func retErrno(e unix.Errno) uint32 { return unix.SECCOMP_RET_ERRNO | uint32(e)&unix.SECCOMP_RET_DATA }

// seccomp_data offsets.
const (
	offNr   = 0
	offArch = 4
)

func offArgLo(i int) uint32 { return 16 + 8*uint32(i) } // little-endian low word

type prog struct{ ins []unix.SockFilter }

func (p *prog) stmt(code uint16, k uint32) int {
	p.ins = append(p.ins, unix.SockFilter{Code: code, K: k})
	return len(p.ins) - 1
}

// jeq appends "if A == k goto <patched later> else fall through" and
// returns its index (Jt patched by target).
func (p *prog) jeq(k uint32) int {
	p.ins = append(p.ins, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: k})
	return len(p.ins) - 1
}

func (p *prog) jsetOrJge(code uint16, k uint32) int {
	p.ins = append(p.ins, unix.SockFilter{Code: unix.BPF_JMP | code | unix.BPF_K, K: k})
	return len(p.ins) - 1
}

// target patches jump i's true branch to land on the next instruction.
func (p *prog) target(i int) {
	d := len(p.ins) - i - 1
	if d > 255 {
		panic("seccomp: jump too far")
	}
	p.ins[i].Jt = uint8(d)
}

func (p *prog) ld(off uint32) { p.stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, off) }
func (p *prog) ret(v uint32)  { p.stmt(unix.BPF_RET|unix.BPF_K, v) }
func (p *prog) and(k uint32)  { p.stmt(unix.BPF_ALU|unix.BPF_AND|unix.BPF_K, k) }
func (p *prog) retIfEq(k, v uint32) {
	j := p.jeq(k)
	p.ins[j].Jt, p.ins[j].Jf = 0, 1
	p.ret(v)
}

// buildFilter returns the seccomp program (see the package comment).
func buildFilter() []unix.SockFilter {
	var p prog
	p.ld(offArch)
	j := p.jeq(auditArch)
	p.ins[j].Jt, p.ins[j].Jf = 1, 0
	p.ret(retKill) // foreign syscall ABI (e.g. 32-bit compat)
	p.ld(offNr)
	archPrologue(&p)
	for _, nr := range deniedSyscalls {
		p.retIfEq(uint32(nr), retErrno(unix.EPERM))
	}
	for _, nr := range enosysSyscalls {
		p.retIfEq(uint32(nr), retErrno(unix.ENOSYS))
	}
	// clone: no new namespaces.
	jClone := p.jeq(uint32(unix.SYS_CLONE))
	jSocket := p.jeq(uint32(unix.SYS_SOCKET))
	p.ret(retAllow)

	// clone(flags, …)
	p.target(jClone)
	p.ld(offArgLo(0))
	js := p.jsetOrJge(unix.BPF_JSET, cloneNSFlags)
	p.ins[js].Jt, p.ins[js].Jf = 1, 0
	p.ret(retAllow)
	p.ret(retErrno(unix.EPERM))

	// socket(domain, type, protocol)
	p.target(jSocket)
	p.ld(offArgLo(0))
	p.retIfEq(unix.AF_UNIX, retAllow)
	jInet := p.jeq(unix.AF_INET)
	jInet6 := p.jeq(unix.AF_INET6)
	jNetlink := p.jeq(unix.AF_NETLINK)
	p.ret(retErrno(unix.EAFNOSUPPORT))
	// AF_INET / AF_INET6: TCP only (Landlock restricts TCP; MPTCP, SCTP,
	// UDP, ICMP and raw sockets would bypass it).
	p.target(jInet)
	p.target(jInet6)
	p.ld(offArgLo(1))
	p.and(0xf) // strip SOCK_NONBLOCK / SOCK_CLOEXEC
	jStream := p.jeq(unix.SOCK_STREAM)
	p.ret(retErrno(unix.EPERM))
	p.target(jStream)
	p.ld(offArgLo(2))
	p.retIfEq(0, retAllow)
	p.retIfEq(unix.IPPROTO_TCP, retAllow)
	p.ret(retErrno(unix.EPERM))
	// AF_NETLINK: NETLINK_ROUTE only (getifaddrs); not sock_diag, which
	// lists every unix socket's path.
	p.target(jNetlink)
	p.ld(offArgLo(2))
	p.retIfEq(unix.NETLINK_ROUTE, retAllow)
	p.ret(retErrno(unix.EPERM))
	return p.ins
}

// installSeccomp sets no_new_privs and installs the filter on every thread
// of this process (TSYNC).
func installSeccomp() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	ins := buildFilter()
	fp := unix.SockFprog{Len: uint16(len(ins)), Filter: &ins[0]}
	_, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&fp)))
	runtime.KeepAlive(ins)
	if e != 0 {
		return fmt.Errorf("seccomp: %w", e)
	}
	return nil
}
