package mgmt

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// TimeSource is the authoritative clock for everything mgmt persists or
// decides by time: lease acquire/renew/expiry, host last_seen and liveness,
// key release windows, revocation windows, TTL reaping (ADR-028 §1).
// Replicas must agree on it, so it is the store's clock, never a replica's
// wall clock.
type TimeSource interface {
	Now() time.Time
}

// StoreClock is implemented by stores that provide the shared time source.
type StoreClock interface {
	TimeSource
	// ClockOffset is the store clock minus this replica's wall clock at the
	// last sync (0 when the store clock is the local clock), and when that
	// sync happened (zero time = never synced).
	ClockOffset() (time.Duration, time.Time)
	// ClockSource is "postgres" (the server clock) or "local" (this
	// machine's clock: SQLite, one replica; or a test override).
	ClockSource() string
}

// dbClock follows the Postgres server clock. It samples clock_timestamp()
// every syncEvery and extrapolates in between with the local monotonic
// clock, so a replica's wall-clock steps (NTP, sleep/wake, a wrong clock)
// never reach lease decisions, and Now() costs no round trip.
type dbClock struct {
	sample func(ctx context.Context) (time.Time, error)

	mu       sync.Mutex
	base     time.Time     // store time at the sample (RTT midpoint)
	at       time.Time     // local time.Now() at the midpoint (carries the monotonic reading)
	offset   time.Duration // base minus local wall time at the midpoint
	syncedAt time.Time
	stop     chan struct{}
	done     chan struct{}
}

const dbClockSyncEvery = 5 * time.Second

func newDBClock(sample func(ctx context.Context) (time.Time, error)) *dbClock {
	return &dbClock{sample: sample, stop: make(chan struct{}), done: make(chan struct{})}
}

// sync takes one sample. On error the clock keeps extrapolating from the
// last good sample (monotonic), which stays correct while the rate drift
// of the local oscillator is small.
func (c *dbClock) sync(ctx context.Context) error {
	t0 := time.Now()
	db, err := c.sample(ctx)
	if err != nil {
		return err
	}
	t1 := time.Now()
	mid := t0.Add(t1.Sub(t0) / 2)
	c.mu.Lock()
	c.base, c.at, c.syncedAt = db, mid, t1
	c.offset = db.Sub(mid.Round(0)) // Round(0) strips the monotonic reading: wall-clock offset
	c.mu.Unlock()
	return nil
}

func (c *dbClock) run() {
	defer close(c.done)
	t := time.NewTicker(dbClockSyncEvery)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			c.sync(ctx)
			cancel()
		}
	}
}

func (c *dbClock) close() {
	close(c.stop)
	<-c.done
}

func (c *dbClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base.IsZero() {
		return time.Now() // never synced (store unreachable at open): best effort
	}
	return c.base.Add(time.Since(c.at))
}

func (c *dbClock) ClockOffset() (time.Duration, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset, c.syncedAt
}

// localClock is the store clock for SQLite: one replica, one machine, so the
// local clock is the shared clock.
type localClock struct{}

func (localClock) Now() time.Time { return time.Now() }

// Skew thresholds for GET /v1/status (ADR-028 §2): beyond these the replica
// reports ok=false and logs a warning. Ownership decisions use the store
// clock regardless, so nothing is refused.
const (
	MaxClockOffset  = 2 * time.Second
	MaxClockSyncAge = 30 * time.Second
)

// ClockStatus is the "clock" object of GET /v1/status.
type ClockStatus struct {
	Source    string `json:"source"`
	OK        bool   `json:"ok"`
	OffsetMs  int64  `json:"offsetMs"`
	SyncAgeMs int64  `json:"syncAgeMs"`
	Message   string `json:"message,omitempty"`
}

// clockStatus reports how this replica's wall clock relates to the shared
// store clock.
func (m *Mgmt) clockStatus() ClockStatus {
	sc, ok := m.store.(StoreClock)
	if !ok {
		return ClockStatus{Source: "local", OK: true}
	}
	if sc.ClockSource() != "postgres" {
		return ClockStatus{Source: "local", OK: true}
	}
	off, at := sc.ClockOffset()
	cs := ClockStatus{Source: "postgres", OK: true, OffsetMs: off.Milliseconds()}
	if at.IsZero() {
		cs.OK, cs.Message = false, "store clock never sampled"
		return cs
	}
	cs.SyncAgeMs = time.Since(at).Milliseconds()
	switch {
	case off.Abs() > MaxClockOffset:
		cs.OK, cs.Message = false, fmt.Sprintf("this replica's wall clock is %v from the store clock (limit %v): fix NTP on this machine", (-off).Round(time.Millisecond), MaxClockOffset)
	case time.Since(at) > MaxClockSyncAge:
		cs.OK, cs.Message = false, fmt.Sprintf("store clock last sampled %v ago (limit %v): is the store reachable?", time.Since(at).Round(time.Second), MaxClockSyncAge)
	}
	return cs
}

// checkClock logs once per change between ok and not ok.
func (m *Mgmt) checkClock() {
	cs := m.clockStatus()
	m.mu.Lock()
	changed := cs.OK != m.clockOK
	m.clockOK = cs.OK
	m.mu.Unlock()
	switch {
	case changed && !cs.OK:
		m.log.Warn("clock skew", "offsetMs", cs.OffsetMs, "syncAgeMs", cs.SyncAgeMs, "message", cs.Message)
	case changed:
		m.log.Info("clock ok", "offsetMs", cs.OffsetMs)
	}
}
