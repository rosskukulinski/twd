//go:build linux && arm64

package procsb

import "golang.org/x/sys/unix"

const auditArch = unix.AUDIT_ARCH_AARCH64

var archDeniedSyscalls []uintptr

func archPrologue(*prog) {}
