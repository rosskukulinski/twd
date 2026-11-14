package mgmt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The dbClock extrapolates from the last sample with the local monotonic
// clock, and keeps doing so when a sample fails.
func TestDBClockExtrapolates(t *testing.T) {
	server := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) // far from the local wall clock
	fail := false
	c := newDBClock(func(ctx context.Context) (time.Time, error) {
		if fail {
			return time.Time{}, errors.New("store down")
		}
		return server, nil
	})
	if got := c.Now(); got.Year() == 2030 {
		t.Fatalf("unsynced clock should fall back to local time, got %v", got)
	}
	if err := c.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	d := c.Now().Sub(server)
	if d < 50*time.Millisecond || d > 500*time.Millisecond {
		t.Fatalf("Now() should be the server time plus elapsed (≈50 ms), got +%v", d)
	}
	off, at := c.ClockOffset()
	if off < 24*time.Hour || at.IsZero() {
		t.Fatalf("offset should reflect a server clock years ahead, got %v (synced %v)", off, at)
	}
	fail = true
	if err := c.sync(context.Background()); err == nil {
		t.Fatal("sync should report the failure")
	}
	if d2 := c.Now().Sub(server); d2 < d {
		t.Fatalf("after a failed sync the clock must keep extrapolating, got +%v (was +%v)", d2, d)
	}
}

// On Postgres, the store clock follows the server: Now() matches
// clock_timestamp() closely, and Mgmt.now() uses it.
func TestStoreClockPostgres(t *testing.T) {
	dsn := os.Getenv("TWD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TWD_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	st, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var us int64
	if err := st.db.QueryRowContext(ctx, "SELECT (extract(epoch from clock_timestamp())*1000000)::bigint").Scan(&us); err != nil {
		t.Fatal(err)
	}
	if d := st.Now().Sub(time.UnixMicro(us)).Abs(); d > 200*time.Millisecond {
		t.Fatalf("store clock is %v away from the server clock", d)
	}
	if _, at := st.ClockOffset(); at.IsZero() {
		t.Fatal("ClockOffset: never synced")
	}
	cust, _ := OpenFileCustody(filepath.Join(t.TempDir(), "c"))
	m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"),
		Clock: skewedReal{time.Hour}, ReapInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if d := m.now().Sub(st.Now()).Abs(); d > time.Second {
		t.Fatalf("Mgmt.now() must use the store clock, not the replica clock (an hour off): differs by %v", d)
	}
}

// SQLite: the store clock is the local clock, and Mgmt.now() uses it.
func TestStoreClockSQLite(t *testing.T) {
	st, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if d := st.Now().Sub(time.Now()).Abs(); d > time.Second {
		t.Fatalf("SQLite store clock should be local time, off by %v", d)
	}
	if off, _ := st.ClockOffset(); off != 0 {
		t.Fatalf("SQLite offset = %v, want 0", off)
	}
}

type skewedReal struct{ d time.Duration }

func (s skewedReal) Now() time.Time                         { return time.Now().Add(s.d) }
func (s skewedReal) After(d time.Duration) <-chan time.Time { return time.After(d) }

// fakeClockStore wraps a store with a controllable store-clock report.
type fakeClockStore struct {
	*SQLStore
	off    time.Duration
	synced time.Time
}

func (f *fakeClockStore) ClockOffset() (time.Duration, time.Time) { return f.off, f.synced }
func (f *fakeClockStore) ClockSource() string                     { return "postgres" }

// TestMM_F2_SkewGate: GET /v1/status reports the replica's offset from the
// store clock and turns ok=false beyond MaxClockOffset or a stale sample
// (ADR-028 §2). It warns; it does not refuse.
func TestMM_F2_SkewGate(t *testing.T) {
	sq, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	st := &fakeClockStore{SQLStore: sq, synced: time.Now()}
	cust, _ := OpenFileCustody(filepath.Join(t.TempDir(), "c"))
	m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, tc := range []struct {
		name   string
		off    time.Duration
		synced time.Time
		ok     bool
	}{
		{"in sync", 40 * time.Millisecond, time.Now(), true},
		{"replica 1.9 s behind", 1900 * time.Millisecond, time.Now(), true},
		{"replica 3 s behind", 3 * time.Second, time.Now(), false},
		{"replica 3 s ahead", -3 * time.Second, time.Now(), false},
		{"stale sample", 0, time.Now().Add(-45 * time.Second), false},
		{"never sampled", 0, time.Time{}, false},
	} {
		st.off, st.synced = tc.off, tc.synced
		s, err := m.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if s.Clock.OK != tc.ok || s.Clock.Source != "postgres" || s.Clock.OffsetMs != tc.off.Milliseconds() {
			t.Errorf("%s: clock = %+v, want ok=%v", tc.name, s.Clock, tc.ok)
		}
		if !tc.ok && s.Clock.Message == "" {
			t.Errorf("%s: a not-ok clock needs a message", tc.name)
		}
	}
	// SQLite (no server clock): always local and ok.
	m2, _ := New(Config{Store: sq, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour})
	defer m2.Close()
	if s, _ := m2.Status(context.Background()); s.Clock.Source != "local" || !s.Clock.OK {
		t.Errorf("sqlite: clock = %+v, want local ok", s.Clock)
	}
}
