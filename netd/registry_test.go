package netd_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
)

func startControl(t *testing.T) *nettest.Control {
	t.Helper()
	ctl, err := nettest.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctl.Close)
	return ctl
}

// countingMint is a MintFunc KeySource that records calls and concurrency.
type countingMint struct {
	calls, inflight, maxInflight atomic.Int32
	delay                        time.Duration
}

func (m *countingMint) source() netd.KeySource {
	return netd.MintFunc(func(ctx context.Context, req netd.KeyRequest) (string, error) {
		m.calls.Add(1)
		n := m.inflight.Add(1)
		defer m.inflight.Add(-1)
		for {
			cur := m.maxInflight.Load()
			if n <= cur || m.maxInflight.CompareAndSwap(cur, n) {
				break
			}
		}
		time.Sleep(m.delay)
		return nettest.DummyKey, nil
	})
}

func TestParallelJoinQueueAndClose(t *testing.T) {
	ctl := startControl(t)
	mint := &countingMint{delay: 100 * time.Millisecond}
	const n, par = 12, 4
	reg, err := netd.New(netd.Config{ControlURL: ctl.URL, StateRoot: filepath.Join(newRundir(t), "state"),
		Keys: mint.source(), JoinParallel: par})
	if err != nil {
		t.Fatal(err)
	}
	st := time.Now()
	for i := range n {
		if _, err := reg.Create(fmt.Sprintf("p%02d", i), netd.ObjectOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(st); d > 100*time.Millisecond {
		t.Errorf("Create is not async: %v for %d objects", d, n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for i := range n {
		if err := reg.Get(fmt.Sprintf("p%02d", i)).Ready(ctx); err != nil {
			t.Fatal(err)
		}
	}
	allUp := time.Since(st)
	if got := mint.maxInflight.Load(); got > par {
		t.Errorf("join queue not bounded: %d concurrent mints > %d", got, par)
	}
	if got := mint.calls.Load(); got != n {
		t.Errorf("mint called %d times, want %d", got, n)
	}
	cs := time.Now()
	reg.Close()
	closeDur := time.Since(cs)
	t.Logf("N=%d par=%d: all up %v; parallel Close %v", n, par, allUp.Round(time.Millisecond), closeDur.Round(time.Millisecond))
	for _, in := range reg.Objects() {
		if in.State != netd.StateClosed {
			t.Errorf("%s: %s after Close", in.ID, in.State)
		}
	}
	// tsnet Close is ~0.3 s per node serially on real control; in parallel the
	// whole set should take about one node's worth.
	if closeDur > 5*time.Second {
		t.Errorf("Close of %d nodes took %v (not parallel?)", n, closeDur)
	}
	if _, err := reg.Create("late", netd.ObjectOptions{}); !errors.Is(err, netd.ErrClosed) {
		t.Errorf("Create after Close: %v", err)
	}
}

func TestRejoinFromSavedState(t *testing.T) {
	ctl := startControl(t)
	root := filepath.Join(newRundir(t), "state")
	persistent := false
	mint := &countingMint{}
	cfg := netd.Config{ControlURL: ctl.URL, StateRoot: root, Keys: mint.source(), Ephemeral: persistent}
	reg, err := netd.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, id := range []string{"r1", "r2"} {
		o, _ := reg.Create(id, netd.ObjectOptions{})
		if err := o.Ready(ctx); err != nil {
			t.Fatal(err)
		}
	}
	before := map[string]netd.ObjectInfo{}
	for _, in := range reg.Objects() {
		before[in.ID] = in
	}
	reg.Close()

	// "Restart": a new registry over the same state root, and no key source.
	cfg.Keys = netd.MintFunc(func(context.Context, netd.KeyRequest) (string, error) {
		return "", errors.New("rejoin must not request a key")
	})
	reg2, err := netd.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reg2.Close()
	ids, err := reg2.Rejoin()
	if err != nil || len(ids) != 2 {
		t.Fatalf("Rejoin: %v %v", ids, err)
	}
	for _, id := range ids {
		o := reg2.Get(id)
		if err := o.Ready(ctx); err != nil {
			t.Fatal(err)
		}
		in := o.Info()
		if !in.Rejoined || in.NodeID != before[id].NodeID || in.IPs[0] != before[id].IPs[0] {
			t.Errorf("%s: rejoined=%v node %s ip %v; before node %s ip %v", id, in.Rejoined, in.NodeID, in.IPs, before[id].NodeID, before[id].IPs)
		}
		t.Logf("%s rejoined as %s %v in %.0f ms", id, in.NodeID, in.IPs, in.JoinMS)
	}
	if mint.calls.Load() != 2 {
		t.Errorf("first run minted %d keys, want 2", mint.calls.Load())
	}
}

// TestRejoinRejectedRemints: an ephemeral node's saved state is rejected by
// control after a "restart" (control expired/removed the device, which is
// what happens to an ephemeral node that is offline for a while). tsnet.Up
// would wait forever; netd must notice NeedsLogin, ask the KeySource for a
// key with Remint set, and join again as the SAME hostname (a new node ID).
// A persistent node whose state is accepted rejoins as before, with no key.
//
// testcontrol never refuses a node key by itself (an unknown key is simply
// re-registered), so rejection is forced with SetExpireAllNodes(true), and
// lifted inside the Remint callback, which only runs once NeedsLogin was
// detected. Joins are serialized so the persistent node registers after.
func TestRejoinRejectedRemints(t *testing.T) {
	ctl := startControl(t)
	root := filepath.Join(newRundir(t), "state")
	mint := &countingMint{}
	cfg := netd.Config{ControlURL: ctl.URL, StateRoot: root, Keys: mint.source(), Ephemeral: false, JoinParallel: 1}
	reg, err := netd.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	eph := true
	for id, opts := range map[string]netd.ObjectOptions{"gone": {Ephemeral: &eph}, "kept": {}} {
		o, _ := reg.Create(id, opts)
		if err := o.Ready(ctx); err != nil {
			t.Fatal(err)
		}
	}
	before := map[string]netd.ObjectInfo{}
	for _, in := range reg.Objects() {
		before[in.ID] = in
	}
	reg.Close()
	ctl.Server.SetExpireAllNodes(true)

	var mu sync.Mutex
	var remints []netd.KeyRequest
	cfg.Keys = netd.MintFunc(func(ctx context.Context, req netd.KeyRequest) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if !req.Remint {
			return "", errors.New("a plain rejoin must not request a key")
		}
		remints = append(remints, req)
		ctl.Server.SetExpireAllNodes(false) // the device is "back" for the fresh registration
		return nettest.DummyKey, nil
	})
	reg2, err := netd.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reg2.Close()
	ids, err := reg2.Rejoin() // ReadDir order: gone, then kept (JoinParallel 1)
	if err != nil || len(ids) != 2 {
		t.Fatalf("Rejoin: %v %v", ids, err)
	}
	st := time.Now()
	for _, id := range ids {
		if err := reg2.Get(id).Ready(ctx); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	t.Logf("both ready %v after Rejoin", time.Since(st).Round(time.Millisecond))
	gone, kept := reg2.Get("gone").Info(), reg2.Get("kept").Info()
	if !gone.Reminted || gone.Rejoined || gone.Hostname != before["gone"].Hostname || gone.NodeID == before["gone"].NodeID || !gone.Ephemeral {
		t.Errorf("gone: %+v (before node %s)", gone, before["gone"].NodeID)
	}
	if kept.Reminted || !kept.Rejoined || kept.NodeID != before["kept"].NodeID || kept.IPs[0] != before["kept"].IPs[0] {
		t.Errorf("kept: %+v (before %+v)", kept, before["kept"])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(remints) != 1 || remints[0].ObjectID != "gone" || remints[0].Hostname != before["gone"].Hostname || !remints[0].Ephemeral {
		t.Errorf("remint requests: %+v", remints)
	}
	if mint.calls.Load() != 2 {
		t.Errorf("first run minted %d keys, want 2", mint.calls.Load())
	}
}

func TestKillPendingObject(t *testing.T) {
	ctl := startControl(t)
	release := make(chan struct{})
	reg, err := netd.New(netd.Config{ControlURL: ctl.URL, StateRoot: filepath.Join(newRundir(t), "state"),
		Keys: netd.MintFunc(func(ctx context.Context, _ netd.KeyRequest) (string, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nettest.DummyKey, nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	o, _ := reg.Create("pend", netd.ObjectOptions{})
	if o.State() != netd.StatePending {
		t.Fatalf("state %s", o.State())
	}
	rep, err := reg.Kill(context.Background(), "pend", netd.KillOptions{})
	if err != nil || rep.PrevState != netd.StatePending {
		t.Fatalf("%+v %v", rep, err)
	}
	close(release)
	err = o.Ready(context.Background())
	if e, ok := netd.AsError(err); !ok || e.Code != netd.CodeKilled {
		t.Fatalf("Ready after kill: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if o.State() != netd.StateKilled || o.Info().NodeID != "" {
		t.Fatalf("killed-while-pending object came up: %+v", o.Info())
	}
}

func TestJoinFailureIsTypedAndRedacted(t *testing.T) {
	secret := "tskey-" + "auth-kFAKE123CNTRL-" + strings.Repeat("x", 24)
	var buf bytes.Buffer
	reg, err := netd.New(netd.Config{ControlURL: "http://127.0.0.1:1", StateRoot: filepath.Join(newRundir(t), "state"),
		Keys: netd.MintFunc(func(context.Context, netd.KeyRequest) (string, error) {
			return "", errors.New("mint failed for " + secret)
		}),
		Logger:      slog.New(slog.NewTextHandler(&buf, nil)),
		JoinTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	o, _ := reg.Create("bad", netd.ObjectOptions{})
	err = o.Ready(context.Background())
	e, ok := netd.AsError(err)
	if !ok || e.Code != netd.CodeBootstrapFailed {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(o.Info().Error, secret) || strings.Contains(buf.String(), secret) {
		t.Fatalf("secret leaked: err=%q info=%q log=%q", err, o.Info().Error, buf.String())
	}
	if !strings.Contains(buf.String(), "tskey-[REDACTED]") {
		t.Errorf("log lacks redaction marker: %s", buf.String())
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := netd.New(netd.Config{StateRoot: t.TempDir()}); err == nil {
		t.Error("empty ControlURL accepted (would silently join production control)")
	}
	if _, err := netd.New(netd.Config{ControlURL: "http://x"}); err == nil {
		t.Error("empty StateRoot accepted")
	}
	reg, _ := netd.New(netd.Config{ControlURL: "http://127.0.0.1:1", StateRoot: t.TempDir()})
	defer reg.Close()
	for _, id := range []string{"", "UPPER", "../x", "a/b", "a.b", strings.Repeat("a", 64)} {
		if _, err := reg.Create(id, netd.ObjectOptions{}); err == nil {
			t.Errorf("invalid id %q accepted", id)
		}
	}
	if _, err := reg.Kill(context.Background(), "nope", netd.KillOptions{}); !errors.Is(err, netd.ErrNotFound) {
		t.Errorf("Kill unknown: %v", err)
	}
}

func TestCreateIdempotent(t *testing.T) {
	reg, _ := netd.New(netd.Config{ControlURL: "http://127.0.0.1:1", StateRoot: t.TempDir(), JoinTimeout: time.Second,
		Keys: netd.StaticKey(nettest.DummyKey)})
	defer reg.Close()
	var wg sync.WaitGroup
	objs := make([]*netd.Object, 8)
	for i := range objs {
		wg.Add(1)
		go func() { defer wg.Done(); objs[i], _ = reg.Create("same", netd.ObjectOptions{}) }()
	}
	wg.Wait()
	for _, o := range objs {
		if o != objs[0] {
			t.Fatal("concurrent Create returned different objects")
		}
	}
}
