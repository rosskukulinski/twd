//go:build linux

package vm

import (
	"os"
	"strings"
	"testing"

	"github.com/rosskukulinski/twd/hostd/sandbox"
)

const guestSeesCPUCap = false

// ctrSmokeBackend is Firecracker (needs root and KVM,
// TWD_VM_CONTAINER_KERNEL with erofs, TWD_VM_ROOTFS).
func ctrSmokeBackend(t *testing.T) (sandbox.ContainerBackend, []string) {
	t.Helper()
	kernel, rootfs := os.Getenv("TWD_VM_CONTAINER_KERNEL"), os.Getenv("TWD_VM_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("TWD_VM_CONTAINER_KERNEL / TWD_VM_ROOTFS not set")
	}
	b, err := New(Config{Kernel: kernel, ContainerKernel: kernel, Rootfs: rootfs, BootArgs: os.Getenv("TWD_VM_BOOTARGS"), HugePages: os.Getenv("TWD_VM_HUGEPAGES"), Helper: os.Getenv("TWD_VM_HELPER")})
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { b.Close() })
	return b, nil
}

// checkCtrConfinement: the VMM is jailed (own uid) and capped (cpu.max on
// its cgroup).
func checkCtrConfinement(t *testing.T, info sandbox.Info, cpuMax string) {
	t.Helper()
	if j := info.Jail; j == nil || j.UID < 1000 || !strings.HasPrefix(j.CPUMax, cpuMax) {
		t.Fatalf("jail/cpu.max: %+v", j)
	}
}
