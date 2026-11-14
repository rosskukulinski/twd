//go:build e2evm && darwin

package vm

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm"
)

// Virtualization.framework (ADR-032 D7). There is no jailer: each guest
// runs in its own twd-vz process and its own Virtualization.framework VM
// service (Apple's sandboxed XPC process). The framework refuses to save
// this VM's state (see vm.Machine.MemoryDump), so the no-keys scan covers
// the disks and the guest's files, not its RAM.
const (
	isolationTest      = "OwnVMMAndVMService"
	memoryDumpRequired = false
	resultsPrefix      = "vz-"
)

type testBackend interface {
	sandbox.Backend
	Close() error
}

func newBackend(kernel, rootfs string, vcpus int) (testBackend, map[string]any, error) {
	be, err := vm.New(vm.Config{VMM: os.Getenv("TWD_VZ_VMM"), Kernel: kernel, Rootfs: rootfs, MemMiB: 512, VCPUs: vcpus,
		BootArgs: os.Getenv("TWD_VM_BOOTARGS"), Rosetta: os.Getenv("TWD_VM_ROSETTA") != ""})
	if err != nil {
		return nil, nil, err
	}
	return be, map[string]any{"via": be.Via()}, nil
}

var seenPIDs = map[int]string{}

func checkIsolated(t *testing.T, w string, info *sandbox.Info) map[string]any {
	t.Helper()
	if info.PID <= 0 || info.PID == os.Getpid() || info.ServicePID <= 0 || info.ServicePID == info.PID {
		t.Fatalf("%s: want its own VMM and VM service process: %+v", w, info)
	}
	out := map[string]any{"vmm_pid": info.PID, "vm_service_pid": info.ServicePID}
	for _, p := range []struct {
		pid  int
		want string
		key  string
	}{{info.PID, "twd-vz", "vmm"}, {info.ServicePID, "com.apple.Virtualization.VirtualMachine", "vm_service"}} {
		b, _ := exec.Command("/bin/ps", "-o", "comm=", "-p", strconv.Itoa(p.pid)).Output()
		comm := strings.TrimSpace(string(b))
		if !strings.Contains(comm, p.want) {
			t.Fatalf("%s: %s pid %d is %q, want %s", w, p.key, p.pid, comm, p.want)
		}
		if other, ok := seenPIDs[p.pid]; ok && other != w {
			t.Fatalf("%s shares %s pid %d with %s", w, p.key, p.pid, other)
		}
		seenPIDs[p.pid] = w
		out[p.key] = comm
	}
	return out
}
