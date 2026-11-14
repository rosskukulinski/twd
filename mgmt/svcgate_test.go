package mgmt

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type fakeAdv struct {
	mu    sync.Mutex
	on    bool
	calls []string
	fail  error
}

func (f *fakeAdv) Advertise(_ context.Context, name string, _ ...uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "adv")
	if f.fail != nil {
		return f.fail
	}
	f.on = true
	return nil
}

func (f *fakeAdv) Withdraw(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "wd")
	f.on = false
	return nil
}

// TestServiceGate: a replica advertises svc:twd-mgmt only while its store
// answers; FailAfter consecutive failures withdraw it, one success restores
// it, a failing first check never advertises, Close withdraws.
func TestServiceGate(t *testing.T) {
	ctx := context.Background()
	adv := &fakeAdv{}
	var healthy = true
	var changes []bool
	g := &ServiceGate{Adv: adv, Name: "svc:twd-mgmt", Ports: []uint16{80}, FailAfter: 2,
		Check: func(context.Context) error {
			if healthy {
				return nil
			}
			return errors.New("store unavailable")
		},
		OnChange: func(a bool) { changes = append(changes, a) }}
	step := func(h bool, wantOn bool) {
		t.Helper()
		healthy = h
		g.Step(ctx)
		if adv.on != wantOn || g.Status().Advertised != wantOn {
			t.Fatalf("after check healthy=%v: advertised %v (status %+v), want %v", h, adv.on, g.Status(), wantOn)
		}
	}
	step(true, true)
	step(true, true)
	step(false, true) // one failure: keep serving (a blip)
	step(false, false)
	if st := g.Status(); st.LastError != "store unavailable" || st.Fails != 2 {
		t.Fatalf("status %+v", st)
	}
	step(false, false)
	step(true, true)
	if err := g.Close(ctx); err != nil || adv.on {
		t.Fatalf("close: %v, advertised %v", err, adv.on)
	}
	if want := []bool{true, false, true, false}; len(changes) != len(want) {
		t.Fatalf("OnChange calls %v, want %v", changes, want)
	}

	// Unhealthy from the start: never advertised.
	adv2 := &fakeAdv{}
	g2 := &ServiceGate{Adv: adv2, Name: "svc:twd-mgmt", Ports: []uint16{80},
		Check: func(context.Context) error { return errors.New("down") }}
	g2.Step(ctx)
	if len(adv2.calls) != 0 {
		t.Fatalf("advertised while unhealthy: %v", adv2.calls)
	}
	// A failed advertise is retried next step.
	adv3 := &fakeAdv{fail: errors.New("localapi")}
	g3 := &ServiceGate{Adv: adv3, Name: "svc:twd-mgmt", Ports: []uint16{80}, Check: func(context.Context) error { return nil }}
	g3.Step(ctx)
	adv3.fail = nil
	g3.Step(ctx)
	if !adv3.on || len(adv3.calls) != 2 {
		t.Fatalf("advertise retry: on=%v calls=%v", adv3.on, adv3.calls)
	}
}

func TestServiceGateClosedStaysWithdrawn(t *testing.T) {
	adv := &fakeAdv{}
	g := &ServiceGate{Adv: adv, Name: "svc:twd-mgmt", Ports: []uint16{80}, Check: func(context.Context) error { return nil }}
	g.Step(context.Background())
	g.Close(context.Background())
	g.Step(context.Background())
	if adv.on {
		t.Fatal("re-advertised after Close")
	}
}
