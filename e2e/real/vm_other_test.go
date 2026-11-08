//go:build e2ereal && !linux

package real

import (
	"testing"

	"github.com/rosskukulinski/twd/hostd/sandbox"
)

type vmBackendT = sandbox.Backend

func realVMBackend(t *testing.T) sandbox.Backend {
	t.Fatal("TWD_E2E_TIER=microvm needs Linux + KVM")
	return nil
}
