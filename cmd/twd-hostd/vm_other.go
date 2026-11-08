//go:build !linux

package main

import (
	"log/slog"

	"github.com/rosskukulinski/twd/hostd/sandbox"
)

type vmOptions struct {
	kernel, rootfs, firecracker, hugePages string
	memMiB, vcpus                          int
}

// vmBackend: the microVM tier needs Linux + KVM; on this OS every workload
// runs in the process tier.
func vmBackend(vmOptions, *slog.Logger) sandbox.Backend { return nil }
