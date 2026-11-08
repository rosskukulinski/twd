//go:build linux

// twd-guestd is PID 1 of a twd microVM guest. It:
//
//  1. mounts the pseudo filesystems and a tmpfs for runtime state;
//  2. sets the clock from the config drive (the platform's egress MACs are
//     timestamped);
//  3. extracts the config drive (/dev/vdb, a plain tar) into /run/twd/app;
//  4. brings up lo and eth0 with the static address from guest.json;
//  5. starts the pinned workerd on the delivered config and restarts it if
//     it exits;
//  6. reports boot, readiness and workerd exits to the host over vsock, and
//     answers the host's ops (stats, fsscan, and in debug mode probe).
//
// The guest never holds tailnet keys: there is no tailscale here at all.
// All tailnet identity lives in host netd, on the other side of the TAP.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jsimonetti/rtnetlink"
	"github.com/rosskukulinski/twd/guest/proto"
	"golang.org/x/sys/unix"
)

const workerdBin = "/usr/bin/workerd"

var start = time.Now()

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "twd-guestd: "+format+"\n", args...)
}

func main() {
	if os.Getpid() != 1 {
		logf("not PID 1; refusing to run")
		os.Exit(2)
	}
	if err := run(); err != nil {
		logf("fatal: %v", err)
	}
	// PID 1 must never exit (the kernel panics; panic=1 reboots, which
	// Firecracker turns into a VM exit). Power off cleanly instead.
	unix.Sync()
	unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
	select {}
}

func run() error {
	if err := mounts(); err != nil {
		return err
	}
	cfg, err := readConfigDrive("/dev/vdb")
	if err != nil {
		return fmt.Errorf("config drive: %w", err)
	}
	if cfg.UnixNano > 0 {
		tv := unix.NsecToTimeval(cfg.UnixNano + time.Since(start).Nanoseconds())
		if err := unix.Settimeofday(&tv); err != nil {
			logf("settimeofday: %v", err)
		}
	}
	if err := netUp(cfg); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	os.MkdirAll(proto.DODir, 0o700)

	g := &guest{cfg: cfg, out: make(chan proto.Msg, 64)}
	go g.vsockLoop()
	go g.reap()
	g.send(proto.Msg{Type: "boot", UptimeMS: time.Since(start).Milliseconds()})
	g.superviseWorkerd()
	return nil
}

func mounts() error {
	for _, m := range []struct {
		src, dst, typ string
		flags         uintptr
		data          string
	}{
		{"proc", "/proc", "proc", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"sysfs", "/sys", "sysfs", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755"},
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755,size=50%"},
		{"tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777,size=64m"},
	} {
		os.MkdirAll(m.dst, 0o755)
		if err := unix.Mount(m.src, m.dst, m.typ, m.flags, m.data); err != nil && !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("mount %s: %w", m.dst, err)
		}
	}
	os.MkdirAll("/dev/shm", 0o1777)
	unix.Mount("tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777")
	return nil
}

// readConfigDrive extracts the tar on dev into proto.AppDir and parses
// guest.json.
func readConfigDrive(dev string) (*proto.GuestConfig, error) {
	f, err := os.Open(dev)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := os.MkdirAll(proto.AppDir, 0o700); err != nil {
		return nil, err
	}
	tr := tar.NewReader(bufio.NewReaderSize(f, 1<<20))
	var cfgb []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := filepath.Clean("/" + h.Name)
		if name == "/" || strings.Contains(h.Name, "..") {
			continue
		}
		p := filepath.Join(proto.AppDir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(p, 0o700)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(p), 0o700)
			b, err := io.ReadAll(io.LimitReader(tr, 256<<20))
			if err != nil {
				return nil, err
			}
			if name == "/"+proto.GuestConfigFile {
				cfgb = b
				continue
			}
			if err := os.WriteFile(p, b, 0o600); err != nil {
				return nil, err
			}
		}
	}
	if cfgb == nil {
		return nil, errors.New(proto.GuestConfigFile + " missing")
	}
	var cfg proto.GuestConfig
	if err := json.Unmarshal(cfgb, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func netUp(cfg *proto.GuestConfig) error {
	c, err := rtnetlink.Dial(nil)
	if err != nil {
		return err
	}
	defer c.Close()
	links, err := c.Link.List()
	if err != nil {
		return err
	}
	var eth, lo uint32
	for _, l := range links {
		if l.Attributes == nil {
			continue
		}
		switch l.Attributes.Name {
		case "eth0":
			eth = l.Index
		case "lo":
			lo = l.Index
		}
	}
	if eth == 0 {
		return errors.New("no eth0")
	}
	ip := net.ParseIP(cfg.IP).To4()
	gw := net.ParseIP(cfg.Gateway).To4()
	if ip == nil || gw == nil || cfg.Prefix <= 0 || cfg.Prefix > 30 {
		return fmt.Errorf("bad address config %q/%d via %q", cfg.IP, cfg.Prefix, cfg.Gateway)
	}
	if lo != 0 {
		if err := c.Address.New(&rtnetlink.AddressMessage{Family: unix.AF_INET, PrefixLength: 8, Index: lo,
			Attributes: &rtnetlink.AddressAttributes{Address: net.IPv4(127, 0, 0, 1).To4(), Local: net.IPv4(127, 0, 0, 1).To4()}}); err != nil && !errors.Is(err, unix.EEXIST) {
			logf("lo address: %v", err)
		}
		c.Link.Set(&rtnetlink.LinkMessage{Family: unix.AF_UNSPEC, Index: lo, Flags: unix.IFF_UP, Change: unix.IFF_UP})
	}
	if cfg.MTU > 0 {
		mtu := uint32(cfg.MTU)
		c.Link.Set(&rtnetlink.LinkMessage{Family: unix.AF_UNSPEC, Index: eth, Attributes: &rtnetlink.LinkAttributes{MTU: mtu}})
	}
	if err := c.Address.New(&rtnetlink.AddressMessage{Family: unix.AF_INET, PrefixLength: uint8(cfg.Prefix), Index: eth,
		Attributes: &rtnetlink.AddressAttributes{Address: ip, Local: ip}}); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("eth0 address: %w", err)
	}
	if err := c.Link.Set(&rtnetlink.LinkMessage{Family: unix.AF_UNSPEC, Index: eth, Flags: unix.IFF_UP, Change: unix.IFF_UP}); err != nil {
		return fmt.Errorf("eth0 up: %w", err)
	}
	// A default route, so that the ADR-019 probes below test the host side
	// (every non-allowed flow must die at host netd), not a missing route.
	if err := c.Route.Add(&rtnetlink.RouteMessage{Family: unix.AF_INET, Table: unix.RT_TABLE_MAIN, Protocol: unix.RTPROT_BOOT,
		Scope: unix.RT_SCOPE_UNIVERSE, Type: unix.RTN_UNICAST,
		Attributes: rtnetlink.RouteAttributes{Gateway: gw, OutIface: eth}}); err != nil && !errors.Is(err, unix.EEXIST) {
		logf("default route: %v", err)
	}
	return nil
}

// ---- guest state + vsock control channel ----

type guest struct {
	cfg *proto.GuestConfig
	out chan proto.Msg

	mu         sync.Mutex
	workerdPID int
	restarts   int
	exits      chan int // workerd exit statuses, fed by reap()
}

func (g *guest) send(m proto.Msg) {
	select {
	case g.out <- m:
	default: // the host is not reading; drop rather than block PID 1
	}
}

// vsockLoop keeps a connection to the host (CID 2) open, writing queued
// messages and serving the host's ops. It reconnects if the host restarts.
func (g *guest) vsockLoop() {
	for {
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err == nil {
			err = unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: proto.VsockPort})
			if err == nil {
				f := os.NewFile(uintptr(fd), "vsock")
				g.serveHost(f)
				f.Close()
			} else {
				unix.Close(fd)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (g *guest) serveHost(f *os.File) {
	done := make(chan struct{})
	var wmu sync.Mutex
	enc := json.NewEncoder(f)
	write := func(m proto.Msg) error {
		wmu.Lock()
		defer wmu.Unlock()
		return enc.Encode(m)
	}
	go func() {
		defer close(done)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var m proto.Msg
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			go func() { write(g.handle(m)) }()
		}
	}()
	for {
		select {
		case m := <-g.out:
			if err := write(m); err != nil {
				g.send(m) // retry on the next connection
				return
			}
		case <-done:
			return
		}
	}
}

func (g *guest) handle(m proto.Msg) proto.Msg {
	r := proto.Msg{Type: "resp", ID: m.ID}
	switch m.Op {
	case "ping":
		r.UptimeMS = time.Since(start).Milliseconds()
	case "stats":
		r.Stats = g.stats()
	case "fsscan":
		r.Found, r.Scanned = fsscan("/")
	case "probe":
		if !g.cfg.Debug {
			r.Error = "probe needs debug mode"
			break
		}
		r.Results = probe(m.Targets)
	default:
		r.Error = "unknown op " + m.Op
	}
	return r
}

func (g *guest) stats() *proto.Stats {
	s := &proto.Stats{UptimeMS: time.Since(start).Milliseconds()}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		s.MemTotalKB = kv(b, "MemTotal:")
		s.MemAvailableKB = kv(b, "MemAvailable:")
	}
	g.mu.Lock()
	pid := g.workerdPID
	g.mu.Unlock()
	if pid > 0 {
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
			s.WorkerdRSSKB = kv(b, "VmRSS:")
		}
	}
	return s
}

func kv(b []byte, key string) int64 {
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, key) {
			f := strings.Fields(strings.TrimPrefix(ln, key))
			if len(f) > 0 {
				n, _ := strconv.ParseInt(f[0], 10, 64)
				return n
			}
		}
	}
	return 0
}

// fsscan lists files whose names look like tailscale/tsnet state. It does
// not grep contents for key material: the host scans the guest's disks and
// a full memory snapshot for that, so the needles never enter the guest.
func fsscan(root string) (found []string, n int) {
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (p == "/proc" || p == "/sys" || p == "/dev") {
			return filepath.SkipDir
		}
		n++
		b := strings.ToLower(d.Name())
		if strings.Contains(b, "tailscale") || strings.HasPrefix(b, "tsnet") || strings.HasSuffix(b, ".state") ||
			b == "derpmap.cached.json" || strings.Contains(b, "machinekey") || strings.Contains(b, "nodekey") {
			found = append(found, p)
		}
		return nil
	})
	return found, n
}

func probe(targets []string) []proto.ProbeResult {
	out := make([]proto.ProbeResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := time.Now()
			c, err := net.DialTimeout("tcp", t, 1500*time.Millisecond)
			r := proto.ProbeResult{Target: t, MS: time.Since(st).Milliseconds()}
			switch {
			case err == nil:
				// A connect can succeed against a TCP stack that accepts
				// then resets; count it only if the peer actually talks
				// or holds the connection open.
				c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
				_, rerr := c.Read(make([]byte, 1))
				c.Close()
				var ne net.Error
				if rerr == nil || errors.As(rerr, &ne) && ne.Timeout() {
					r.Result = "connected"
				} else {
					r.Result, r.Detail = "reset", rerr.Error()
				}
			case errors.Is(err, unix.ECONNREFUSED) || errors.Is(err, unix.ECONNRESET):
				r.Result = "refused"
			case errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH):
				r.Result = "unreachable"
			default:
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					r.Result = "timeout"
				} else {
					r.Result = "error"
				}
				r.Detail = err.Error()
			}
			out[i] = r
		}()
	}
	wg.Wait()
	return out
}

// ---- workerd supervision ----

// reap collects every child (PID 1 inherits orphans) and reports workerd's
// exit status.
func (g *guest) reap() {
	sig := make(chan os.Signal, 8)
	signal.Notify(sig, syscall.SIGCHLD)
	for range sig {
		for {
			var ws unix.WaitStatus
			pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			g.mu.Lock()
			if pid == g.workerdPID && g.exits != nil {
				g.workerdPID = 0
				select {
				case g.exits <- ws.ExitStatus():
				default:
				}
			}
			g.mu.Unlock()
		}
	}
}

func (g *guest) superviseWorkerd() {
	g.mu.Lock()
	g.exits = make(chan int, 1)
	g.mu.Unlock()
	for attempt := 0; ; attempt++ {
		st := time.Now()
		cmd := exec.Command(workerdBin, g.cfg.WorkerdArgs...)
		cmd.Dir = proto.AppDir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Env = []string{"HOME=/tmp", "PATH=/usr/bin:/bin", "TZ=UTC"}
		g.mu.Lock()
		err := cmd.Start()
		if err == nil {
			g.workerdPID = cmd.Process.Pid
		}
		g.mu.Unlock()
		if err != nil {
			logf("start workerd: %v", err)
			g.send(proto.Msg{Type: "workerd-exit", ExitCode: -1, Error: err.Error(), Restarts: attempt})
			if attempt >= 5 {
				select {}
			}
			time.Sleep(time.Second)
			continue
		}
		healthy := make(chan struct{})
		go func() {
			if g.waitHealthy(20 * time.Second) {
				close(healthy)
			}
		}()
		select {
		case <-healthy:
			g.send(proto.Msg{Type: "ready", UptimeMS: time.Since(start).Milliseconds(), WorkerdMS: time.Since(st).Milliseconds(), Restarts: attempt})
			code := <-g.exits
			g.send(proto.Msg{Type: "workerd-exit", ExitCode: code, Restarts: attempt})
		case code := <-g.exits:
			g.send(proto.Msg{Type: "workerd-exit", ExitCode: code, Restarts: attempt, Error: "exited before healthy"})
		}
		if attempt >= 5 {
			logf("workerd keeps exiting; giving up")
			select {}
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
}

func (g *guest) waitHealthy(d time.Duration) bool {
	cl := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(d)
	addr := fmt.Sprintf("http://127.0.0.1:%d/_twd/admin", proto.IngressPort)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(ctx, "POST", addr, bytes.NewReader(nil))
		req.Header.Set("x-twd-cf", g.cfg.HealthCF)
		resp, err := cl.Do(req)
		cancel()
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
