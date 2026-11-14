//go:build !linux && !darwin

package main

import (
	"log/slog"

	"github.com/rosskukulinski/twd/hostd/sandbox"
)

type vmOptions struct {
	kernel, rootfs, firecracker, jailer, helper, hugePages, containerKernel, vzVMM string
	memMiB, vcpus                                                                  int
	rosetta                                                                        bool
}

// vmBackend: the microVM tier needs Linux + KVM or macOS; on this OS every
// workload runs in the process tier.
func vmBackend(vmOptions, *slog.Logger) sandbox.Backend { return nil }
