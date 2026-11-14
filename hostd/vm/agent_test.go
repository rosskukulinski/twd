//go:build linux || darwin

package vm

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
)

// vsockPeer is the guest end of one vsock connection to the agent.
type vsockPeer struct {
	c   net.Conn
	enc *json.Encoder
	sc  *bufio.Scanner
}

func dialAgent(t *testing.T, a *agent, first proto.Msg) *vsockPeer {
	t.Helper()
	host, guest := net.Pipe()
	t.Cleanup(func() { host.Close(); guest.Close() })
	go a.route(host)
	g := &vsockPeer{c: guest, enc: json.NewEncoder(guest), sc: bufio.NewScanner(guest)}
	guest.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := g.enc.Encode(first); err != nil {
		t.Fatalf("first line: %v", err)
	}
	guest.SetWriteDeadline(time.Time{})
	return g
}

// closedByHost: the agent hung up on this connection (reads hit EOF, not
// the deadline).
func (g *vsockPeer) closedByHost(t *testing.T) bool {
	t.Helper()
	g.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := g.c.Read(make([]byte, 1))
	g.c.SetReadDeadline(time.Time{})
	return err == io.EOF || err == io.ErrClosedPipe
}

// serve answers the host's ops on this connection with Text = name.
func (g *vsockPeer) serve(name string) {
	go func() {
		for g.sc.Scan() {
			var m proto.Msg
			if json.Unmarshal(g.sc.Bytes(), &m) != nil {
				continue
			}
			g.enc.Encode(proto.Msg{Type: "resp", ID: m.ID, Text: name, Found: []string{name}})
		}
	}()
}

func testAgent() *agent {
	a := newAgent()
	a.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return &a
}

func opText(t *testing.T, a *agent) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := a.Op(ctx, proto.Msg{Op: "fsscan"})
	if err != nil {
		t.Fatalf("op: %v", err)
	}
	return r.Text
}

// TestAgentControlChannelNotHijackable: a guest connection without this
// boot's token (a container process that opened AF_VSOCK to the host) can't
// replace guestd's control channel, forge events, or receive the host's
// ops; guestd's own reconnect (with the token) can.
func TestAgentControlChannelNotHijackable(t *testing.T) {
	a := testAgent()
	if len(a.ctlToken) != 64 || a.ctlToken == newAgent().ctlToken {
		t.Fatalf("ctl token %q: not a fresh 256-bit token", a.ctlToken)
	}
	guestd := dialAgent(t, a, proto.Msg{Type: "hello", Token: a.ctlToken})
	guestd.serve("guestd")
	waitFor(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.enc != nil })
	if got := opText(t, a); got != "guestd" {
		t.Fatalf("op answered by %q, want guestd", got)
	}

	for name, first := range map[string]proto.Msg{
		"no token":        {Type: "hello"},
		"wrong token":     {Type: "hello", Token: newToken()},
		"token prefix":    {Type: "hello", Token: a.ctlToken[:32]},
		"forged ready":    {Type: "ready", Token: a.ctlToken},
		"forged ctr-exit": {Type: "ctr-exit", ExitCode: 0},
		"forged resp":     {Type: "resp", ID: 1, Found: []string{}},
	} {
		h := dialAgent(t, a, first)
		// It would also answer ops and send more forged events, if heard.
		go h.enc.Encode(proto.Msg{Type: "boot"})
		if !h.closedByHost(t) {
			t.Errorf("%s: the agent kept the connection open", name)
		}
		if got := opText(t, a); got != "guestd" {
			t.Fatalf("%s: op answered by %q: the control channel was taken over", name, got)
		}
	}
	if ev := a.eventTypes(); !slices.Equal(ev, []string{"hello"}) {
		t.Fatalf("events %v: forged guest events were accepted", ev)
	}
	if a.readyOK.Load() {
		t.Fatal("a forged ready was accepted")
	}

	// guestd reconnects (its connection died, e.g. a vsock transport
	// reset): the authenticated connection becomes the control channel.
	again := dialAgent(t, a, proto.Msg{Type: "hello", Token: a.ctlToken})
	again.serve("guestd-2")
	if !guestd.closedByHost(t) {
		t.Fatal("the replaced control connection was not closed")
	}
	if got := opText(t, a); got != "guestd-2" {
		t.Fatalf("after guestd's reconnect the op went to %q", got)
	}
}

// TestAgentExecStreamToken: an exec stdio stream is handed to the exec
// only with the token the host issued for it; a connection naming the exec
// with any other token is refused and the exec still gets the real one.
func TestAgentExecStreamToken(t *testing.T) {
	a := testAgent()
	s := newCtrState()
	a.onStream = s.attach
	id, token := newExecID(), newToken()
	ch := make(chan net.Conn, 1)
	s.streams[id] = &pendingStream{token: token, ch: ch}

	for name, tok := range map[string]string{"no token": "", "wrong token": newToken(), "the exec id": id, "token prefix": token[:63]} {
		h := dialAgent(t, a, proto.Msg{Type: proto.ExecStreamHello, Exec: id, Token: tok})
		if !h.closedByHost(t) {
			t.Errorf("%s: the stream was not refused", name)
		}
		select {
		case <-ch:
			t.Fatalf("%s: a stream with a wrong token reached the exec", name)
		default:
		}
	}
	// Nor does an unknown exec id get anywhere.
	if h := dialAgent(t, a, proto.Msg{Type: proto.ExecStreamHello, Exec: newExecID(), Token: token}); !h.closedByHost(t) {
		t.Error("a stream for an unknown exec was not refused")
	}

	g := dialAgent(t, a, proto.Msg{Type: proto.ExecStreamHello, Exec: id, Token: token})
	var c net.Conn
	select {
	case c = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream with the right token never reached the exec")
	}
	go g.c.Write([]byte("frames"))
	b := make([]byte, 6)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "frames" {
		t.Fatalf("stream data %q %v", b, err)
	}
	// Used once: the same token can't open a second stream.
	if h := dialAgent(t, a, proto.Msg{Type: proto.ExecStreamHello, Exec: id, Token: token}); !h.closedByHost(t) {
		t.Error("a second stream with a used token was not refused")
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}
