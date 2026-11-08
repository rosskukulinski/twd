package hostd

import (
	"context"
	"log/slog"
	"testing"

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/sandbox"
)

type fakeBackend struct{}

func (fakeBackend) Boot(context.Context, sandbox.Spec) (sandbox.VM, error) { return nil, nil }
func (fakeBackend) Name() string                                           { return "fake" }

// TestTierFor: deployment tier > tenant tier > host default; microvm falls
// back to process on a host without a microVM backend (macOS).
func TestTierFor(t *testing.T) {
	ten := func(tier string) *tenant { return &tenant{info: mgmtapi.Tenant{Tier: tier}} }
	cases := []struct {
		vm               bool
		def, tenant, app string
		want             string
	}{
		{false, "", "", "", "process"},
		{false, "", "", "microvm", "process"}, // no backend: fallback
		{false, "microvm", "microvm", "microvm", "process"},
		{true, "", "", "", "process"},
		{true, "", "", "microvm", "microvm"},
		{true, "", "microvm", "", "microvm"},
		{true, "", "microvm", "process", "process"}, // deployment overrides tenant
		{true, "microvm", "", "", "microvm"},
		{true, "microvm", "process", "", "process"},
	}
	for _, c := range cases {
		h := &Host{log: slog.New(slog.DiscardHandler), cfg: Config{DefaultTier: c.def}}
		if c.vm {
			h.cfg.VM = fakeBackend{}
		}
		got := h.tierFor(&bundle.App{Name: "x", Tier: c.app}, ten(c.tenant))
		if got != c.want {
			t.Errorf("%+v: got %s", c, got)
		}
	}
}
