//go:build linux

package main

import (
	"log/slog"
	"os"

	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm"
)

type vmOptions struct {
	kernel, rootfs, firecracker, jailer, helper, hugePages, containerKernel, vzVMM string
	memMiB, vcpus                                                                  int
	rosetta                                                                        bool
}

// vmBackend enables the microVM tier when this host can run it: KVM, the
// guest kernel and image, and the twd-vmhelper service (`sudo twd host
// setup --microvm`; as root without it, an in-process helper). Otherwise
// microvm deployments are refused (tier_unavailable), never downgraded.
func vmBackend(o vmOptions, log *slog.Logger) sandbox.Backend {
	if os.Geteuid() == 0 {
		if _, err := os.Stat(o.helper); err != nil {
			o.helper = "" // root without the service: in-process helper
		}
	}
	b, err := vm.New(vm.Config{Helper: o.helper, Firecracker: o.firecracker, Jailer: o.jailer, Kernel: o.kernel, Rootfs: o.rootfs,
		MemMiB: o.memMiB, VCPUs: o.vcpus, HugePages: o.hugePages, ContainerKernel: o.containerKernel, Log: log})
	if err != nil {
		log.Info("microVM tier unavailable; process tier only", "reason", err)
		return nil
	}
	log.Info("microVM tier available", "backend", b.Name(), "via", b.Via(), "kernel", o.kernel, "rootfs", o.rootfs)
	return b
}
