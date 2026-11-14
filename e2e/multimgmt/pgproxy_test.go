//go:build e2emultimgmt

package multimgmt

import (
	"io"
	"net"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pgProxy sits between one replica and Postgres and injects faults:
// pass, delay (each forwarded read waits d, both directions), blackhole
// (accept and hold connections, forward nothing) and reset (close every
// connection now; new ones are refused while the mode lasts).
type pgProxy struct {
	ln     net.Listener
	target string
	mode   atomic.Value // string
	delay  atomic.Int64 // ns
	mu     sync.Mutex
	conns  map[net.Conn]bool
}

func newPGProxy(t *testing.T, target string) *pgProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &pgProxy{ln: ln, target: target, conns: map[net.Conn]bool{}}
	p.mode.Store("pass")
	go p.serve()
	t.Cleanup(func() { ln.Close(); p.closeAll() })
	return p
}

func (p *pgProxy) addr() string { return p.ln.Addr().String() }

// set changes the mode; "reset" also drops every open connection.
func (p *pgProxy) set(mode string, d time.Duration) {
	p.delay.Store(int64(d))
	p.mode.Store(mode)
	if mode == "reset" {
		p.closeAll()
	}
}

func (p *pgProxy) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		c.Close()
		delete(p.conns, c)
	}
}

func (p *pgProxy) track(c net.Conn) {
	p.mu.Lock()
	p.conns[c] = true
	p.mu.Unlock()
}

func (p *pgProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if p.mode.Load() == "reset" {
			c.Close()
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			c.Close()
			continue
		}
		p.track(c)
		p.track(up)
		go p.pipe(up, c)
		go p.pipe(c, up)
	}
}

func (p *pgProxy) pipe(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			for p.mode.Load() == "blackhole" {
				time.Sleep(50 * time.Millisecond)
			}
			if d := time.Duration(p.delay.Load()); p.mode.Load() == "delay" && d > 0 {
				time.Sleep(d)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				return
			}
			return
		}
	}
}

// viaProxy rewrites a postgres:// DSN to go through addr.
func viaProxy(t *testing.T, dsn, addr string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = addr
	out := u.String()
	if extra := os.Getenv("TWD_MM_DSN_SUFFIX"); extra != "" { // e.g. &twd_max_conns=2
		out += extra
	}
	return out
}
