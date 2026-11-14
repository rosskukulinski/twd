// Package sandboxtest has a fake sandbox.ContainerBackend for tests that
// can't boot microVMs (macOS, CI without KVM): each "container" is a plain
// host process group. It isolates NOTHING; it exists so hostd's container
// manager and the ctx.container polyfill can be tested against real
// workerd everywhere. The microVM backend is hostd/vm.
package sandboxtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/hostd/sandbox"
)

// Procs is the fake backend.
type Procs struct {
	// Ports maps container ports to host addresses (DialPort); an
	// unmapped port is "not listening".
	Ports map[uint16]string
	// Boots counts BootContainer calls; Specs keeps them.
	Boots atomic.Int64
	mu    sync.Mutex
	Specs []sandbox.ContainerSpec
	live  map[*proc]bool
}

// Live is the number of running fake containers.
func (b *Procs) Live() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.live)
}

// BootContainer implements sandbox.ContainerBackend.
func (b *Procs) BootContainer(ctx context.Context, s sandbox.ContainerSpec) (sandbox.Container, error) {
	b.Boots.Add(1)
	b.mu.Lock()
	b.Specs = append(b.Specs, s)
	if b.live == nil {
		b.live = map[*proc]bool{}
	}
	b.mu.Unlock()
	cmd := exec.Command(s.Argv[0], s.Argv[1:]...)
	cmd.Env = s.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("entrypoint: %w", err)
	}
	p := &proc{b: b, cmd: cmd, exited: make(chan struct{}), done: make(chan struct{}), started: time.Now()}
	b.mu.Lock()
	b.live[p] = true
	b.mu.Unlock()
	go func() {
		err := cmd.Wait()
		p.code = exitCode(cmd, err)
		close(p.exited)
		p.kill()
	}()
	return p, nil
}

func exitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState == nil {
		return -1
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	if err != nil && cmd.ProcessState.ExitCode() < 0 {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}

type proc struct {
	b       *Procs
	cmd     *exec.Cmd
	exited  chan struct{}
	done    chan struct{}
	code    int
	started time.Time
	once    sync.Once
	n       atomic.Int64
}

func (p *proc) kill() {
	p.once.Do(func() {
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		p.b.mu.Lock()
		delete(p.b.live, p)
		p.b.mu.Unlock()
		close(p.done)
	})
}

func (p *proc) Exec(ctx context.Context, s sandbox.ExecSpec) (sandbox.Exec, error) {
	select {
	case <-p.done:
		return nil, errors.New("the container is not running")
	default:
	}
	cmd := exec.Command(s.Argv[0], s.Argv[1:]...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, s.Env...)
	cmd.Dir = s.Cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	e := &pexec{id: fmt.Sprintf("x%d", p.n.Add(1)), cmd: cmd, done: make(chan struct{})}
	var ow, ew *os.File
	if s.Stdout != "ignore" {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		e.out, ow = r, w
		cmd.Stdout = w
	}
	switch s.Stderr {
	case "ignore":
	case "combined":
		cmd.Stderr = ow
	default:
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		e.err, ew = r, w
		cmd.Stderr = w
	}
	if s.Stdin {
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		e.in = in
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	for _, f := range []*os.File{ow, ew} {
		if f != nil {
			f.Close()
		}
	}
	go func() {
		err := cmd.Wait()
		e.code = exitCode(cmd, err)
		close(e.done)
	}()
	go func() {
		select {
		case <-p.done:
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-e.done:
		}
	}()
	return e, nil
}

func (p *proc) Signal(sig int) error {
	return syscall.Kill(p.cmd.Process.Pid, syscall.Signal(sig))
}

func (p *proc) DialPort(ctx context.Context, port uint16) (net.Conn, error) {
	a, ok := p.b.Ports[port]
	if !ok {
		return nil, errors.New("connection refused")
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", a)
}

func (p *proc) Exited() <-chan struct{} { return p.exited }
func (p *proc) ExitCode() int           { return p.code }
func (p *proc) Done() <-chan struct{}   { return p.done }

func (p *proc) Stop(grace time.Duration) {
	select {
	case <-p.exited:
	default:
		if grace > 0 {
			p.Signal(15)
			select {
			case <-p.exited:
			case <-time.After(grace):
			}
		}
	}
	p.kill()
	<-p.exited
}

func (p *proc) Kill()        { p.kill() }
func (p *proc) RSSKB() int64 { return 0 }
func (p *proc) Log() string  { return "" }
func (p *proc) Info() sandbox.Info {
	return sandbox.Info{PID: p.cmd.Process.Pid}
}

type pexec struct {
	id       string
	cmd      *exec.Cmd
	in       io.WriteCloser
	out, err *os.File
	done     chan struct{}
	code     int
}

func (e *pexec) ID() string            { return e.id }
func (e *pexec) PID() int              { return e.cmd.Process.Pid }
func (e *pexec) Stdin() io.WriteCloser { return e.in }
func (e *pexec) Stdout() io.Reader {
	if e.out == nil {
		return nil
	}
	return e.out
}
func (e *pexec) Stderr() io.Reader {
	if e.err == nil {
		return nil
	}
	return e.err
}

func (e *pexec) Wait(ctx context.Context) (int, error) {
	select {
	case <-e.done:
		return e.code, nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (e *pexec) Signal(sig int) error        { return syscall.Kill(e.cmd.Process.Pid, syscall.Signal(sig)) }
func (e *pexec) Resize(cols, rows int) error { return errors.New("no PTY in the fake backend") }
