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

// vmBackend: the microVM tier needs Linux + KVM or macOS; this OS offers the
// process tier only, and microvm deployments are refused (tier_unavailable),
// never downgraded.
func vmBackend(vmOptions, *slog.Logger) sandbox.Backend { return nil }
