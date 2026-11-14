//go:build darwin

package main

import (
	"log/slog"

	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm"
)

type vmOptions struct {
	kernel, rootfs, firecracker, jailer, helper, hugePages, vzVMM string
	memMiB, vcpus                                                 int
	rosetta                                                       bool
}

// vmBackend enables the microVM tier on macOS when this host can run it:
// Virtualization.framework, the guest kernel and image, and a twd-vz that
// carries the virtualization entitlement (ADR-032 D7). No root, no helper
// service: each VM is one twd-vz process.
func vmBackend(o vmOptions, log *slog.Logger) sandbox.Backend {
	b, err := vm.New(vm.Config{VMM: o.vzVMM, Kernel: o.kernel, Rootfs: o.rootfs, MemMiB: o.memMiB, VCPUs: o.vcpus,
		Rosetta: o.rosetta, Log: log})
	if err != nil {
		log.Info("microVM tier unavailable; process tier only", "reason", err)
		return nil
	}
	log.Info("microVM tier available", "backend", b.Name(), "via", b.Via(), "kernel", o.kernel, "rootfs", o.rootfs)
	return b
}
