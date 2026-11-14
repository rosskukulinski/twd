package vm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/netd"
)

// agent is the host end of one guest: the twd-guestd control channel
// (guest/proto NDJSON over vsock), the host-side network stack and the
// ingress proxy through it. Both backends (Firecracker, vz) embed it; they
// differ only in how the VM, its vsock and its NIC come to exist.
type agent struct {
	log     *slog.Logger
	vn      *vnet
	ingress http.Handler

	info    sandbox.Info
	start   time.Time // the VM's start: Boot and Ready are measured from it
	done    chan struct{}
	ready   chan struct{}
	readyOK atomic.Bool
	// onBoot, if set, is called with the count of "boot" reports (a guest
	// that boots again was restarted by its hypervisor).
	onBoot func(n int)
	boots  int

	mu      sync.Mutex
	ctl     net.Conn
	enc     *json.Encoder
	pending map[int64]chan proto.Msg
	nextID  int64
	events  []proto.Msg
	lastErr string
}

func newAgent() agent {
	return agent{done: make(chan struct{}), ready: make(chan struct{}), pending: map[int64]chan proto.Msg{}}
}

// ---- control channel ----

func (m *agent) acceptCtl(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		m.mu.Lock()
		if m.ctl != nil {
			m.ctl.Close()
		}
		m.ctl, m.enc = c, json.NewEncoder(c)
		m.mu.Unlock()
		go m.readCtl(c)
	}
}

func (m *agent) readCtl(c net.Conn) {
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var msg proto.Msg
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		m.mu.Lock()
		if msg.Type == "resp" {
			if ch := m.pending[msg.ID]; ch != nil {
				delete(m.pending, msg.ID)
				ch <- msg
			}
			m.mu.Unlock()
			continue
		}
		if len(m.events) < 256 {
			m.events = append(m.events, msg)
		}
		m.mu.Unlock()
		switch msg.Type {
		case "boot":
			m.boots++
			if m.boots == 1 {
				m.info.Boot = time.Since(m.start)
			}
			if m.onBoot != nil {
				m.onBoot(m.boots)
			}
		case "ready":
			m.info.Ready = time.Since(m.start)
			m.info.GuestUptimeAtReady = time.Duration(msg.UptimeMS) * time.Millisecond
			if m.readyOK.CompareAndSwap(false, true) {
				close(m.ready)
			}
		case "workerd-exit":
			m.log.Warn("guest workerd exited", "code", msg.ExitCode, "restarts", msg.Restarts, "err", msg.Error)
			m.mu.Lock()
			m.lastErr = fmt.Sprintf("workerd exit %d %s", msg.ExitCode, msg.Error)
			m.mu.Unlock()
		}
	}
}

func (m *agent) eventTypes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s []string
	for _, e := range m.events {
		s = append(s, e.Type)
	}
	return s
}

// Op implements sandbox.VM.
func (m *agent) Op(ctx context.Context, msg proto.Msg) (proto.Msg, error) {
	ch := make(chan proto.Msg, 1)
	m.mu.Lock()
	if m.enc == nil {
		m.mu.Unlock()
		return proto.Msg{}, errors.New("guest agent not connected")
	}
	m.nextID++
	msg.ID = m.nextID
	m.pending[msg.ID] = ch
	err := m.enc.Encode(msg)
	m.mu.Unlock()
	if err != nil {
		return proto.Msg{}, err
	}
	select {
	case r := <-ch:
		if r.Error != "" {
			return r, errors.New(r.Error)
		}
		return r, nil
	case <-ctx.Done():
		return proto.Msg{}, ctx.Err()
	case <-m.done:
		return proto.Msg{}, errors.New("microVM exited")
	}
}

// ---- ingress ----

func (m *agent) ingressProxy() http.Handler {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return m.vn.dialGuest(ctx, proto.IngressPort)
		},
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     30 * time.Second,
	}
	target := &url.URL{Scheme: "http", Host: "guest"}
	return &httputil.ReverseProxy{
		Transport:     tr,
		FlushInterval: -1,
		ErrorLog:      nil,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			netd.WriteError(w, &netd.Error{Code: netd.CodeUpstreamUnavailable, Detail: "microVM: " + err.Error()})
		},
	}
}

// Ingress implements sandbox.VM.
func (m *agent) Ingress() http.Handler { return m.ingress }

// DialGuest implements sandbox.VM.
func (m *agent) DialGuest(ctx context.Context) (net.Conn, error) {
	return m.vn.dialGuest(ctx, proto.IngressPort)
}

// Done implements sandbox.VM.
func (m *agent) Done() <-chan struct{} { return m.done }

// Info implements sandbox.VM.
func (m *agent) Info() sandbox.Info { return m.info }

// Refused implements sandbox.VM.
func (m *agent) Refused() []string { return m.vn.refusedFlows() }

// lastError is the guest's last reported workerd failure.
func (m *agent) lastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}
