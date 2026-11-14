//go:build darwin

package vm

import (
	"os"
	"strings"
	"testing"

	"github.com/rosskukulinski/twd/hostd/sandbox"
)

const guestSeesCPUCap = true

// ctrSmokeBackend is vz (TWD_VM_KERNEL, TWD_VM_ROOTFS, TWD_VZ_VMM; with
// TWD_VM_ROSETTA the guests run linux/amd64 images under Rosetta, so
// TWD_CTR_IMAGES must then hold the amd64 set).
func ctrSmokeBackend(t *testing.T) (sandbox.ContainerBackend, []string) {
	t.Helper()
	b := vzBackend(t, os.Getenv("TWD_VM_ROSETTA") != "")
	if err := b.ContainersUsable(); err != nil {
		t.Fatalf("vz containers: %v", err)
	}
	return b, b.EmulatedArches()
}

// checkCtrConfinement: vz has no jail; the fractional vCPU is cpu.max on
// the guest's container cgroup (checkCPUShare measures that it holds),
// and the VMM is one process of its own (the VM service holds guest RAM).
func checkCtrConfinement(t *testing.T, info sandbox.Info, cpuMax string) {
	t.Helper()
	if j := info.Jail; j == nil || !strings.HasPrefix(j.CPUMax, cpuMax) {
		t.Fatalf("cpu.max: %+v", j)
	}
	if info.PID <= 0 || info.PID == os.Getpid() || info.ServicePID <= 0 {
		t.Fatalf("VMM / VM service: %+v", info)
	}
}
