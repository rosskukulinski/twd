//go:build e2evm && linux

package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm"
)

// Firecracker (Lima twd-dev): jailed VMMs, a full memory snapshot for the
// no-keys scan.
const (
	isolationTest      = "Jailed"
	memoryDumpRequired = true
	resultsPrefix      = "l3-"
)

type testBackend interface {
	sandbox.Backend
	Close() error
}

func newBackend(kernel, rootfs string, vcpus int) (testBackend, map[string]any, error) {
	be, err := vm.New(vm.Config{Kernel: kernel, Rootfs: rootfs, MemMiB: 512, VCPUs: vcpus, BootArgs: os.Getenv("TWD_VM_BOOTARGS"), HugePages: os.Getenv("TWD_VM_HUGEPAGES"),
		Helper: os.Getenv("TWD_VM_HELPER")})
	if err != nil {
		return nil, nil, err
	}
	return be, map[string]any{"hugepages": os.Getenv("TWD_VM_HUGEPAGES"), "free_hugepages_2m_at_start": vm.FreeHugePages2M()}, nil
}

func checkIsolated(t *testing.T, w string, info *sandbox.Info) map[string]any {
	t.Helper()
	j := info.Jail
	if j == nil || j.UID < 1000 || j.Chroot == "" || j.Cgroup == "" {
		t.Fatalf("%s: not jailed: %+v", w, j)
	}
	return checkJailed(t, info.PID, j)
}

// checkJailed: the VMM runs as its jail uid, chrooted, under seccomp, in
// its cgroup with cpu.max and memory.max set.
func checkJailed(t *testing.T, pid int, j *sandbox.Jail) map[string]any {
	t.Helper()
	st, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{"uid": j.UID, "chroot": j.Chroot, "cgroup": j.Cgroup}
	for _, ln := range strings.Split(string(st), "\n") {
		f := strings.Fields(ln)
		if len(f) > 1 && f[0] == "Uid:" && f[1] != fmt.Sprint(j.UID) {
			t.Fatalf("VMM uid %s, want %d", f[1], j.UID)
		}
		if len(f) > 1 && f[0] == "Seccomp:" {
			if f[1] != "2" {
				t.Fatalf("VMM seccomp mode %s, want 2", f[1])
			}
			out["seccomp"] = "filter (mode 2)"
		}
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d/root/api.sock", pid)); err != nil {
		t.Fatalf("VMM not in its jail: %v", err)
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d/root/etc/passwd", pid)); err == nil {
		t.Fatal("VMM sees the host's /etc")
	}
	cg, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if !strings.Contains(string(cg), j.Cgroup) {
		t.Fatalf("VMM cgroup %s, want %s", cg, j.Cgroup)
	}
	cpu, _ := os.ReadFile(filepath.Join("/sys/fs/cgroup", j.Cgroup, "cpu.max"))
	mem, _ := os.ReadFile(filepath.Join("/sys/fs/cgroup", j.Cgroup, "memory.max"))
	out["cpu.max"], out["memory.max"] = strings.TrimSpace(string(cpu)), strings.TrimSpace(string(mem))
	if out["cpu.max"] == "max 100000" || out["memory.max"] == "max" {
		t.Fatalf("cgroup limits not set: %v", out)
	}
	return out
}
