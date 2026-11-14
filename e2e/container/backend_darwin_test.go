//go:build e2econtainer && darwin

package container

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm"
)

// newBackend is vz (Virtualization.framework, no sudo): one twd-vz VMM
// per container. TWD_VM_ROSETTA shares Rosetta for Linux into the guests,
// and then TWD_CTR_IMAGES is the stock amd64 set.
func newBackend(t *testing.T) (sandbox.ContainerBackend, []string) {
	t.Helper()
	kernel, rootfs := os.Getenv("TWD_VM_KERNEL"), os.Getenv("TWD_VM_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("TWD_VM_KERNEL and TWD_VM_ROOTFS must be set (make e2e-vz-container)")
	}
	be, err := vm.New(vm.Config{VMM: os.Getenv("TWD_VZ_VMM"), Kernel: kernel, Rootfs: rootfs, Rosetta: os.Getenv("TWD_VM_ROSETTA") != ""})
	if err != nil {
		t.Fatalf("vz backend unavailable: %v", err)
	}
	t.Cleanup(func() { be.Close() })
	if err := be.ContainersUsable(); err != nil {
		t.Fatalf("vz containers: %v", err)
	}
	return be, be.EmulatedArches()
}

// checkConfinement: no jail on macOS; the fractional vCPU is cpu.max on
// the guest's container cgroup (hostd/vm TestContainerSmoke measures it).
func checkConfinement(t *testing.T, j *sandbox.Jail, cpuMax string) {
	t.Helper()
	if j == nil || !strings.HasPrefix(j.CPUMax, cpuMax) {
		t.Fatalf("container cpu.max %s: %+v", cpuMax, j)
	}
}

// hostPids counts the host's processes (a container's fork bomb must not
// move it).
func hostPids() int {
	out, err := exec.Command("ps", "-A", "-o", "pid=").Output()
	if err != nil {
		return -1
	}
	return len(strings.Fields(string(out)))
}

func processGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
