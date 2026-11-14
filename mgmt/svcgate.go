package mgmt

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Advertiser is what a ServiceGate drives (*tssvc.Host implements it).
type Advertiser interface {
	Advertise(ctx context.Context, name string, ports ...uint16) error
	Withdraw(ctx context.Context, name string) error
}

// ServiceGate advertises a Tailscale Service (e.g. svc:twd-mgmt, the VIP
// hosts use to reach any healthy mgmt replica) only while Check passes.
// Tailscale doesn't health-check Service hosts, so each replica gates its
// own advertisement: FailAfter consecutive failures withdraw it, one
// success restores it. Close withdraws it (call it before shutting the
// listeners, so control stops routing callers to a replica that is going
// away).
//
// A replica that is stalled outright (SIGSTOP, a wedged process) can't
// withdraw itself; callers see a timeout and must retry, which a later
// connection may route to another replica (mm-loop S3).
type ServiceGate struct {
	Adv       Advertiser
	Name      string
	Ports     []uint16
	Check     func(ctx context.Context) error
	Every     time.Duration // default 2 s
	Timeout   time.Duration // per check; default 2 s
	FailAfter int           // default 3
	Log       *slog.Logger
	// OnChange, if set, is called after the advertisement changes (e.g. to
	// stop keep-alives on the Service listener once withdrawn, so callers'
	// pooled connections move to another replica).
	OnChange func(advertised bool)

	mu         sync.Mutex
	advertised bool
	closed     bool
	fails      int
	lastErr    error
}

// GateStatus is a ServiceGate snapshot.
type GateStatus struct {
	Name       string `json:"name"`
	Advertised bool   `json:"advertised"`
	Fails      int    `json:"fails"`
	LastError  string `json:"lastError,omitempty"`
}

func (g *ServiceGate) defaults() {
	if g.Every <= 0 {
		g.Every = 2 * time.Second
	}
	if g.Timeout <= 0 {
		g.Timeout = 2 * time.Second
	}
	if g.FailAfter <= 0 {
		g.FailAfter = 3
	}
	if g.Log == nil {
		g.Log = slog.New(slog.DiscardHandler)
	}
}

// Run checks every Every until ctx is done. The first check runs at once.
func (g *ServiceGate) Run(ctx context.Context) {
	g.defaults()
	t := time.NewTicker(g.Every)
	defer t.Stop()
	for {
		g.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Step runs one check and reconciles the advertisement.
func (g *ServiceGate) Step(ctx context.Context) {
	g.defaults()
	cctx, cancel := context.WithTimeout(ctx, g.Timeout)
	err := g.Check(cctx)
	cancel()
	if ctx.Err() != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.lastErr = err
	if err != nil {
		g.fails++
	} else {
		g.fails = 0
	}
	want := g.fails < g.FailAfter && (g.advertised || err == nil)
	actx, acancel := context.WithTimeout(ctx, 5*time.Second)
	defer acancel()
	switch {
	case want && !g.advertised:
		if aerr := g.Adv.Advertise(actx, g.Name, g.Ports...); aerr != nil {
			g.Log.Warn("service advertise failed", "service", g.Name, "err", aerr)
			return
		}
		g.advertised = true
		g.Log.Info("service advertised", "service", g.Name)
		g.changed(true)
	case !want && g.advertised:
		if werr := g.Adv.Withdraw(actx, g.Name); werr != nil {
			g.Log.Warn("service withdraw failed", "service", g.Name, "err", werr)
			return
		}
		g.advertised = false
		g.Log.Warn("service withdrawn: health check failing", "service", g.Name, "fails", g.fails, "err", err)
		g.changed(false)
	}
}

// Close withdraws the advertisement.
func (g *ServiceGate) Close(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true // Run won't advertise again
	if !g.advertised {
		return nil
	}
	g.advertised = false
	err := g.Adv.Withdraw(ctx, g.Name)
	g.changed(false)
	return err
}

func (g *ServiceGate) changed(advertised bool) {
	if g.OnChange != nil {
		g.OnChange(advertised)
	}
}

// Status is a snapshot.
func (g *ServiceGate) Status() GateStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := GateStatus{Name: g.Name, Advertised: g.advertised, Fails: g.fails}
	if g.lastErr != nil {
		st.LastError = g.lastErr.Error()
	}
	return st
}
