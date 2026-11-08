//go:build e2ereal && linux

package real

import (
	"os"
	"testing"

	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm"
)

type vmBackendT = sandbox.Backend

func realVMBackend(t *testing.T) sandbox.Backend {
	b, err := vm.New(vm.Config{Kernel: os.Getenv("TWD_VM_KERNEL"), Rootfs: os.Getenv("TWD_VM_ROOTFS")})
	if err != nil {
		t.Fatalf("TWD_E2E_TIER=microvm: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}
