package mgmt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time                         { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *stepClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (c *stepClock) advance(d time.Duration)                { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

// TestMM_F5_LeaseHeldExplains: the workload's host stopped heartbeating 11 s
// ago (dead to placement after 10 s) but its 15 s lease has not expired. A
// confirmed takeover (ForceTakeover, F4: the workload is stateful) must say who holds it, how long ago it was seen and when the
// lease frees up (with Retry-After), not just "lease held by another
// holder"; once the lease expires the redeploy goes to a live host.
func TestMM_F5_LeaseHeldExplains(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := OpenStore(ctx, filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	clk := &stepClock{now: time.Unix(1_800_000_000, 0)}
	st.SetClock(clk)
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	kf := filepath.Join(dir, "api.env")
	os.WriteFile(kf, []byte("TS_API_KEY=x\n"), 0o600)
	tn, err := m.CreateTenant(ctx, TenantRequest{Tailnet: "t.ts.net", MintMode: "apikey", APIKeyFile: kf})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"h1", "h2"} {
		if _, _, err := m.RegisterHost(ctx, RegisterRequest{HostID: h, CapacityNodes: 10, Tiers: []string{"process"}}, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	deploy := func(host string) (DeployResponse, error) {
		return m.Deploy(ctx, DeployRequest{TenantID: tn.ID, Workload: "w", Host: host, ForceTakeover: host == "",
			Bundle: makeBundle(t, map[string]string{"twd.jsonc": testTwdJSONC, "dist/index.js": "export default {}"})})
	}
	if _, err := deploy("h1"); err != nil {
		t.Fatal(err)
	}
	clk.advance(11 * time.Second) // h1 silent for 11 s; h2 keeps heartbeating
	m.Heartbeat(ctx, "h2", 0, nil)
	_, err = deploy("")
	var e *Error
	if !errors.As(err, &e) || e.Code != "lease_held" || e.Status != 409 {
		t.Fatalf("want a typed 409 lease_held, got %v", err)
	}
	for _, want := range []string{"h1", "last saw", "expires in"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("message should mention %q: %q", want, e.Message)
		}
	}
	if e.RetryAfter < 1 || e.RetryAfter > 5 {
		t.Errorf("Retry-After = %d s, want the remaining ~4 s of the lease", e.RetryAfter)
	}
	clk.advance(5 * time.Second) // lease expired
	m.Heartbeat(ctx, "h2", 0, nil)
	if r, err := deploy(""); err != nil || r.Host != "h2" {
		t.Fatalf("after the lease expires the redeploy should land on h2: %+v %v", r, err)
	}
}
