//go:build linux && amd64

package procsb

import "golang.org/x/sys/unix"

const auditArch = unix.AUDIT_ARCH_X86_64

var archDeniedSyscalls = []uintptr{unix.SYS_IOPL, unix.SYS_IOPERM, unix.SYS_MODIFY_LDT, unix.SYS_USELIB, unix.SYS_CREATE_MODULE}

// archPrologue refuses the x32 ABI (syscall numbers with bit 30 set).
func archPrologue(p *prog) {
	j := p.jsetOrJge(unix.BPF_JGE, 0x40000000)
	p.ins[j].Jt, p.ins[j].Jf = 0, 1
	p.ret(retErrno(unix.EPERM))
}
