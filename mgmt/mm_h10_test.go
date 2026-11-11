//go:build e2emultimgmt

package mgmt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// skewClock is one replica's view of a shared base time, offset by skew.
type skewClock struct {
	base *baseTime
	skew time.Duration
}

type baseTime struct {
	mu  sync.Mutex
	now time.Time
}

func (b *baseTime) advance(d time.Duration) { b.mu.Lock(); b.now = b.now.Add(d); b.mu.Unlock() }

func (c skewClock) Now() time.Time {
	c.base.mu.Lock()
	defer c.base.mu.Unlock()
	return c.base.now.Add(c.skew)
}
func (c skewClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// h10World is two Mgmt replicas (A, B) on one store with skewed clocks.
type h10World struct {
	a, b   *Mgmt
	base   *baseTime
	tenant string
}

func newH10World(t *testing.T, skewB time.Duration) *h10World {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "mgmt.db")
	if pg := os.Getenv("TWD_TEST_POSTGRES_DSN"); pg != "" {
		dsn = pg
	}
	st, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cust, err := OpenFileCustody(filepath.Join(dir, "custody"))
	if err != nil {
		t.Fatal(err)
	}
	base := &baseTime{now: time.Now()}
	secret := []byte("0123456789abcdef0123456789abcdef")
	mk := func(skew time.Duration) *Mgmt {
		m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{BaseURL: "http://127.0.0.1:1"}, IDSecret: secret,
			Clock: skewClock{base, skew}, ReapInterval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
		return m
	}
	w := &h10World{a: mk(0), b: mk(skewB), base: base}
	keyFile := filepath.Join(dir, "api.env")
	os.WriteFile(keyFile, []byte("TS_API_KEY=x\n"), 0o600)
	tn, err := w.a.CreateTenant(ctx, TenantRequest{Tailnet: fmt.Sprintf("t%d.ts.net", time.Now().UnixNano()), MintMode: "apikey", APIKeyFile: keyFile})
	if err != nil {
		t.Fatal(err)
	}
	w.tenant = tn.ID
	return w
}

func (w *h10World) deploy(t *testing.T, m *Mgmt, host string) (DeployResponse, error) {
	t.Helper()
	return m.Deploy(context.Background(), DeployRequest{TenantID: w.tenant, Workload: "w", Host: host,
		Bundle: makeBundle(t, map[string]string{"twd.jsonc": testTwdJSONC, "dist/index.js": "export default {}"})})
}

// TestMM_H10_LeaseClockSkew probes H10: lease expiry and host liveness are
// computed from each replica's own clock (AcquireLease/RenewLease/TouchHost
// take the replica's now). Invariant I1: a healthy lease holder that
// heartbeats normally must never be displaced.
//
// Case "ahead": h1 heartbeats through replica A (correct clock) every 2 s;
// h2 through replica B. A redeploy of the same workload is served by B,
// whose clock runs ahead by skew. Expected: the workload stays on h1.
//
// Case "behind": h1 heartbeats through replica B, whose clock runs behind
// by skew (so it writes early lease expiries and old last_seen values).
// A redeploy is served by A. Expected: the workload stays on h1.
func TestMM_H10_LeaseClockSkew(t *testing.T) {
	ctx := context.Background()
	skews := []time.Duration{0, 1 * time.Second, 2 * time.Second, 5 * time.Second, 7 * time.Second, 8 * time.Second,
		9 * time.Second, 10 * time.Second, 12 * time.Second, 15 * time.Second, 20 * time.Second}
	for _, tc := range []struct {
		name string
		sign time.Duration
	}{{"ahead", 1}, {"behind", -1}} {
		minBad, minRefused := time.Duration(-1), time.Duration(-1)
		for _, s := range skews {
			w := newH10World(t, tc.sign*s)
			hbH1, other := w.a, w.b // which replica h1 heartbeats through / which serves the redeploy
			if tc.name == "behind" {
				hbH1, other = w.b, w.a
			}
			// h1 registers and heartbeats through hbH1; h2 through the other.
			if _, _, err := hbH1.RegisterHost(ctx, RegisterRequest{HostID: "h1", CapacityNodes: 100, Tiers: []string{"process"}}, "", ""); err != nil {
				t.Fatal(err)
			}
			if _, _, err := other.RegisterHost(ctx, RegisterRequest{HostID: "h2", CapacityNodes: 1000, Tiers: []string{"process"}}, "", ""); err != nil {
				t.Fatal(err)
			}
			d1, err := w.deploy(t, hbH1, "h1")
			if err != nil {
				t.Fatal(err)
			}
			// Steady state: 2 s heartbeats for 10 s.
			for range 5 {
				w.base.advance(2 * time.Second)
				hb, err := hbH1.Heartbeat(ctx, "h1", 1, []HeartbeatLease{{Namespace: "w", Epoch: d1.Epoch}})
				if err != nil || len(hb.Revoke) > 0 { // steady state: h1 must keep its lease
					t.Fatalf("%s skew %v: h1 heartbeat: %v revoke=%v", tc.name, s, err, hb.Revoke)
				}
				other.Heartbeat(ctx, "h2", 0, nil)
			}
			// 1 s after h1's last heartbeat, a redeploy through the other replica.
			w.base.advance(1 * time.Second)
			d2, err := w.deploy(t, other, "")
			switch {
			case err != nil:
				t.Logf("%-6s skew %4v: redeploy REFUSED: %v (h1 is healthy)", tc.name, s, err)
				if minRefused < 0 {
					minRefused = s
				}
			case d2.Host != "h1":
				t.Logf("%-6s skew %4v: redeploy MOVED the workload to %s (epoch %d→%d) although h1 is healthy", tc.name, s, d2.Host, d1.Epoch, d2.Epoch)
				if minBad < 0 {
					minBad = s
				}
			default:
				t.Logf("%-6s skew %4v: redeploy stays on h1 (epoch %d→%d)", tc.name, s, d1.Epoch, d2.Epoch)
			}
		}
		if minRefused >= 0 {
			t.Errorf("I8 violated (%s): redeploys of a healthy workload are refused at clock skew ≥ %v", tc.name, minRefused)
		}
		if minBad >= 0 {
			t.Errorf("I1 violated (%s): a healthy, heartbeating lease holder was displaced at clock skew ≥ %v", tc.name, minBad)
		} else {
			t.Logf("%s: no displacement up to %v", tc.name, skews[len(skews)-1])
		}
	}
}
