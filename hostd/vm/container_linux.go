//go:build linux

package vm

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sync"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm/helperapi"
)

// ContainersUsable reports whether BootContainer can work on this host: the
// helper speaks container guests and the container kernel has erofs (the
// image format). twd-hostd offers containers only then (a host that can't
// run them doesn't advertise them, so placement never sends it any).
func (b *Backend) ContainersUsable() error {
	if err := b.helperContainers(); err != nil {
		return err
	}
	kernel := b.cfg.ContainerKernel
	if kernel == "" {
		kernel = b.cfg.Kernel
	}
	return KernelHasErofs(kernel)
}

// helperContainers checks that the helper service supports container guests
// (the in-process helper always does).
func (b *Backend) helperContainers() error {
	if b.sock == "" || b.srv != nil {
		return nil
	}
	r, err := helperapi.Ping(b.sock)
	if err != nil {
		return err
	}
	for _, f := range r.Features {
		if f == helperapi.FeatureContainer {
			return nil
		}
	}
	return errors.New("the microVM helper predates container guests (re-run `sudo twd host setup --microvm`)")
}

// BootContainer implements sandbox.ContainerBackend: one Firecracker VM
// per container, in its own jail (uid, netns, cgroup), with the image as a
// read-only drive and a scratch disk the helper makes fresh. The guest's
// NIC goes to a userspace stack with no handlers at all, so it has no
// egress of any kind (ADR-032 D4 deny-all; the egress proxy is phase B);
// hostd reaches the container's ports through that stack (DialPort).
func (b *Backend) BootContainer(ctx context.Context, s sandbox.ContainerSpec) (sandbox.Container, error) {
	if err := b.helperContainers(); err != nil {
		return nil, err
	}
	if s.VCPUs < 1 || s.MemMiB < 128 || len(s.Argv) == 0 || s.Image == "" {
		return nil, fmt.Errorf("bad container spec (vcpus %d, mem %d MiB, argv %q)", s.VCPUs, s.MemMiB, s.Argv)
	}
	quota := 0
	if s.CPUMax > 0 && s.CPUMax < float64(s.VCPUs) {
		quota = int(math.Round(s.CPUMax * 100000))
		if quota < 1000 {
			quota = 1000 // the kernel's floor for cpu.max is 1 ms
		}
	}
	pids := s.Pids
	if pids <= 0 {
		pids = 4096
	}
	req := &helperapi.StartRequest{Name: s.ID, MemMiB: s.MemMiB, VCPUs: s.VCPUs, HugePages: b.cfg.HugePages,
		BootArgs: b.cfg.BootArgs, MTU: b.cfg.MTU, GuestMAC: guestMAC, GuestCID: 3, VsockPort: proto.VsockPort,
		Image: true, ScratchMiB: s.ScratchMiB, Pids: pids + 64}
	kernel := b.cfg.ContainerKernel
	if kernel == "" {
		kernel = b.cfg.Kernel
	}
	cs := &ctrState{exited: make(chan struct{}), streams: map[string]chan net.Conn{}, log: &tailBuf{max: 64 << 10}}
	gc := proto.GuestConfig{UnixNano: time.Now().UnixNano(), IP: proto.GuestIP, Gateway: proto.GatewayIP, Prefix: proto.PrefixLen,
		MTU: b.cfg.MTU, Container: &proto.ContainerConfig{Image: proto.ImageDev, Argv: s.Argv, Env: s.Env, Cwd: s.Cwd, User: s.User,
			Pids: pids, Hostname: s.Hostname}}
	if s.ScratchMiB > 0 {
		gc.Container.Scratch = proto.ScratchDev
	}
	l := launch{name: s.ID, log: s.Log, kernel: kernel, req: req, ctr: cs,
		handlers: nil, // no egress: every guest flow is refused at the host stack
		drive:    func(p string) error { return writeGuestJSON(p, gc) },
		extra: func(m *Machine) (*os.File, error) {
			f, err := os.Open(s.Image)
			if err != nil {
				return nil, fmt.Errorf("image: %w", err)
			}
			m.disks = append(m.disks, s.Image)
			return f, nil
		}}
	m, err := b.launch(ctx, l)
	if err != nil {
		return nil, err
	}
	// The guest boots on its whole vCPUs (a 1/16 vCPU kernel boot takes
	// seconds) and is capped to its fraction once the entrypoint runs. If
	// the cap can't be applied, the VM doesn't run uncapped.
	if quota > 0 {
		resp, _, err := m.conn.Do(helperapi.Request{Op: "cpu", CPUQuotaUS: quota}, nil, 10*time.Second)
		if err != nil {
			c := &ctrVM{m: m, s: cs}
			c.Stop(0)
			return nil, fmt.Errorf("cap the container's CPU: %w", err)
		}
		if m.info.Jail != nil && resp.Jail != nil {
			m.info.Jail.CPUMax = resp.Jail.CPUMax
		}
	}
	return &ctrVM{m: m, s: cs}, nil
}

// writeGuestJSON writes a config drive holding only guest.json.
func writeGuestJSON(path string, gc proto.GuestConfig) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	gb, _ := json.Marshal(gc)
	if err := tw.WriteHeader(&tar.Header{Name: proto.GuestConfigFile, Mode: 0o600, Size: int64(len(gb)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	tw.Write(gb)
	if err := tw.Close(); err != nil {
		return err
	}
	if pad := buf.Len() % 4096; pad != 0 {
		buf.Write(make([]byte, 4096-pad))
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

// ctrState is a container guest's host-side state (Machine.ctr).
type ctrState struct {
	m *Machine

	mu       sync.Mutex
	mainPID  int
	exited   chan struct{}
	exitOnce sync.Once
	exitCode int
	streams  map[string]chan net.Conn
	log      *tailBuf
}

// event consumes container messages from the control channel.
func (s *ctrState) event(msg proto.Msg) bool {
	switch msg.Type {
	case "ctr-ready":
		s.mu.Lock()
		s.mainPID = msg.PID
		s.mu.Unlock()
		m := s.m
		m.info.Ready = time.Since(m.start)
		m.info.GuestUptimeAtReady = time.Duration(msg.UptimeMS) * time.Millisecond
		if m.readyOK.CompareAndSwap(false, true) {
			close(m.ready)
		}
		return true
	case "ctr-exit":
		s.exitOnce.Do(func() {
			s.mu.Lock()
			s.exitCode = msg.ExitCode
			s.mu.Unlock()
			if msg.Error != "" {
				s.log.Write([]byte("twd: " + msg.Error + "\n"))
				s.m.mu.Lock()
				s.m.lastErr = msg.Error
				s.m.mu.Unlock()
			}
			close(s.exited)
		})
		// A start failure ends the boot wait at once (Boot returns the
		// error) instead of waiting for the ready timeout.
		if !s.m.readyOK.Load() {
			s.m.log.Warn("container entrypoint failed to start", "err", msg.Error)
			s.m.stopped.Store(true)
			go s.m.Kill()
		}
		return true
	case "log":
		s.log.Write([]byte(msg.Text + "\n"))
		s.m.log.Debug("container output", "line", msg.Text)
		return true
	case "exec-exit", "hello":
		return true
	}
	return false
}

func (m *Machine) attachStream(id string, c net.Conn) {
	if m.ctr == nil {
		c.Close()
		return
	}
	s := m.ctr
	s.mu.Lock()
	ch := s.streams[id]
	delete(s.streams, id)
	s.mu.Unlock()
	if ch == nil {
		c.Close() // nobody asked for this exec
		return
	}
	ch <- c
}

// ctrVM implements sandbox.Container.
type ctrVM struct {
	m *Machine
	s *ctrState
}

func newExecID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (c *ctrVM) Exec(ctx context.Context, es sandbox.ExecSpec) (sandbox.Exec, error) {
	select {
	case <-c.s.exited:
		return nil, errors.New("the container is not running")
	default:
	}
	id := newExecID()
	ch := make(chan net.Conn, 1)
	c.s.mu.Lock()
	c.s.streams[id] = ch
	c.s.mu.Unlock()
	drop := func() {
		c.s.mu.Lock()
		delete(c.s.streams, id)
		c.s.mu.Unlock()
		select {
		case cn := <-ch:
			cn.Close()
		default:
		}
	}
	r, err := c.m.Op(ctx, proto.Msg{Op: "exec", Exec: id, ExecSpec: &proto.ExecSpec{Argv: es.Argv, Env: es.Env, Cwd: es.Cwd, User: es.User,
		Stdin: es.Stdin, Stdout: es.Stdout, Stderr: es.Stderr, PTY: es.PTY, Cols: es.Cols, Rows: es.Rows}})
	if err != nil {
		drop()
		return nil, err
	}
	var conn net.Conn
	select {
	case conn = <-ch:
	case <-time.After(10 * time.Second):
		drop()
		return nil, errors.New("exec stream did not arrive")
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
	e := &fcExec{c: c, id: id, pid: r.PID, conn: conn, done: make(chan struct{})}
	if es.Stdout != "ignore" || es.PTY {
		e.outR, e.outW = io.Pipe()
	}
	if !es.PTY && es.Stderr != "ignore" && es.Stderr != "combined" {
		e.errR, e.errW = io.Pipe()
	}
	if es.Stdin {
		e.stdin = &execStdin{e: e}
	}
	go e.demux()
	return e, nil
}

// fcExec is one exec's host side: frames on its vsock stream.
type fcExec struct {
	c    *ctrVM
	id   string
	pid  int
	conn net.Conn

	wmu        sync.Mutex
	stdin      *execStdin
	outR, errR *io.PipeReader
	outW, errW *io.PipeWriter
	done       chan struct{}
	code       int
	err        error
}

func (e *fcExec) ID() string { return e.id }
func (e *fcExec) PID() int   { return e.pid }
func (e *fcExec) Stdin() io.WriteCloser {
	if e.stdin == nil {
		return nil
	}
	return e.stdin
}

func (e *fcExec) Stdout() io.Reader {
	if e.outR == nil {
		return nil
	}
	return e.outR
}

func (e *fcExec) Stderr() io.Reader {
	if e.errR == nil {
		return nil
	}
	return e.errR
}

func (e *fcExec) demux() {
	defer close(e.done)
	finish := func(err error) {
		for _, w := range []*io.PipeWriter{e.outW, e.errW} {
			if w != nil {
				w.CloseWithError(err)
			}
		}
		e.conn.Close()
	}
	for {
		kind, p, err := proto.ReadFrame(e.conn)
		if err != nil {
			e.code, e.err = -1, fmt.Errorf("exec stream: %w", err)
			finish(e.err)
			return
		}
		switch kind {
		case proto.FrameStdout:
			if e.outW != nil {
				e.outW.Write(p)
			}
		case proto.FrameStderr:
			if e.errW != nil {
				e.errW.Write(p)
			}
		case proto.FrameExit:
			if len(p) == 4 {
				e.code = int(int32(binary.BigEndian.Uint32(p)))
			}
			finish(nil)
			return
		}
	}
}

func (e *fcExec) Wait(ctx context.Context) (int, error) {
	select {
	case <-e.done:
		return e.code, e.err
	case <-ctx.Done():
		return -1, ctx.Err()
	case <-e.c.m.done:
		<-e.done // the stream dies with the VM
		return e.code, e.err
	}
}

func (e *fcExec) Signal(sig int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := e.c.m.Op(ctx, proto.Msg{Op: "signal", Exec: e.id, Signal: sig})
	return err
}

func (e *fcExec) Resize(cols, rows int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := e.c.m.Op(ctx, proto.Msg{Op: "resize", Exec: e.id, Cols: cols, Rows: rows})
	return err
}

type execStdin struct {
	e      *fcExec
	closed bool
}

func (s *execStdin) Write(p []byte) (int, error) {
	s.e.wmu.Lock()
	defer s.e.wmu.Unlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	n := 0
	for len(p) > 0 {
		k := min(len(p), 64<<10)
		if err := proto.WriteFrame(s.e.conn, proto.FrameStdin, p[:k]); err != nil {
			return n, err
		}
		n += k
		p = p[k:]
	}
	return n, nil
}

func (s *execStdin) Close() error {
	s.e.wmu.Lock()
	defer s.e.wmu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return proto.WriteFrame(s.e.conn, proto.FrameStdinEOF, nil)
}

func (c *ctrVM) Signal(sig int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.m.Op(ctx, proto.Msg{Op: "signal", Signal: sig})
	return err
}

func (c *ctrVM) DialPort(ctx context.Context, port uint16) (net.Conn, error) {
	return c.m.vn.dialGuest(ctx, port)
}

func (c *ctrVM) Exited() <-chan struct{} { return c.s.exited }

func (c *ctrVM) ExitCode() int {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return c.s.exitCode
}

func (c *ctrVM) Done() <-chan struct{} { return c.m.done }

// Stop: SIGTERM to the entrypoint, up to grace for it to exit, then the
// VMM is SIGKILLed and the jail torn down (the helper session closes).
func (c *ctrVM) Stop(grace time.Duration) {
	select {
	case <-c.s.exited:
	case <-c.m.done:
	default:
		if grace > 0 && c.Signal(15) == nil {
			select {
			case <-c.s.exited:
			case <-c.m.done:
			case <-time.After(grace):
			}
		}
	}
	c.m.stopped.Store(true)
	c.m.Kill()
	select {
	case <-c.m.done:
	case <-time.After(15 * time.Second):
		c.m.log.Warn("container VM did not exit after kill")
	}
	c.m.teardown()
	if os.Getenv("TWD_KEEP_INSTANCES") == "" {
		os.RemoveAll(c.m.dir)
	}
}

func (c *ctrVM) Kill() {
	c.m.stopped.Store(true)
	c.m.Kill()
}

func (c *ctrVM) Info() sandbox.Info { return c.m.info }
func (c *ctrVM) RSSKB() int64       { return c.m.RSSKB() }
func (c *ctrVM) Log() string        { return c.s.log.String() }

// tailBuf keeps the last max bytes written.
type tailBuf struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
