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
	"sort"
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
	if cfg.Volume != "" {
		if err := mountVolume(cfg.Volume, cfg.VolumeFormat); err != nil {
			return fmt.Errorf("DO volume: %w", err)
		}
	}
	if cfg.Rosetta != "" {
		if err := rosetta(cfg.Rosetta); err != nil {
			logf("rosetta: %v", err) // x86_64 binaries won't run; the guest still serves
		}
	}

	g := &guest{cfg: cfg, out: make(chan proto.Msg, 64)}
	go g.vsockLoop()
	go g.reap()
	g.send(proto.Msg{Type: "boot", UptimeMS: time.Since(start).Milliseconds()})
	g.superviseWorkerd()
	return nil
}

// mountVolume mounts the persistent DO volume (ext4 made on the host, or
// here if format is set and the device is blank) at DODir. The host
// decrypts it (dm-crypt, or an encrypted disk image on macOS); there is no
// key in here. data=ordered plus the host's flush handling make workerd's
// fsyncs reach the host disk.
func mountVolume(dev string, format bool) error {
	var err error
	for i := 0; i < 100; i++ { // devtmpfs creates the node asynchronously
		if _, err = os.Stat(dev); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	if format {
		if err := formatVolume(dev); err != nil {
			return err
		}
	}
	if err := unix.Mount(dev, proto.DODir, "ext4", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC|unix.MS_NOATIME, "errors=remount-ro,data=ordered"); err != nil {
		return fmt.Errorf("mount %s: %w", dev, err)
	}
	os.Chmod(proto.DODir, 0o700)
	return nil
}

// formatVolume makes the DO filesystem on a new volume. It refuses a
// device that is not blank: the host asks only for new volumes, and a
// wrong answer must not destroy data.
func formatVolume(dev string) error {
	f, err := os.Open(dev)
	if err != nil {
		return err
	}
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	f.Close()
	if !bytes.Equal(head[:n], make([]byte, n)) {
		return fmt.Errorf("refusing to format %s: not blank", dev)
	}
	st := time.Now()
	out, err := exec.Command("/usr/sbin/mke2fs", "-q", "-F", "-t", "ext4", "-L", "twd-do", "-m", "0",
		"-E", "lazy_itable_init=0,lazy_journal_init=0,nodiscard", dev).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mke2fs: %v: %s", err, bytes.TrimSpace(out))
	}
	logf("formatted %s in %v", dev, time.Since(st).Round(time.Millisecond))
	return nil
}

// rosettaBinfmt is Apple's documented binfmt_misc entry for Rosetta for
// Linux (x86_64 ELF executables), with the interpreter path left out. The
// escapes are parsed by the kernel, not by Go.
const rosettaBinfmt = `:rosetta:M::\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:` +
	`\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:`

// rosetta mounts the host's Rosetta for Linux share (virtiofs tag) and
// registers it for x86_64 executables. Flags: F opens the interpreter now
// (it keeps working whatever mount namespace a later process runs in), C
// computes credentials from the binary, not the interpreter.
func rosetta(tag string) error {
	if err := os.MkdirAll(proto.RosettaDir, 0o755); err != nil {
		return err
	}
	if err := unix.Mount(tag, proto.RosettaDir, "virtiofs", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mount virtiofs %s: %w", tag, err)
	}
	const bf = "/proc/sys/fs/binfmt_misc"
	if _, err := os.Stat(bf + "/register"); err != nil {
		if err := unix.Mount("binfmt_misc", bf, "binfmt_misc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			return fmt.Errorf("mount binfmt_misc: %w", err)
		}
	}
	if err := os.WriteFile(bf+"/register", []byte(rosettaBinfmt+proto.RosettaDir+"/rosetta:CF"), 0); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("register binfmt: %w", err)
	}
	logf("rosetta registered for x86_64 binaries")
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
			mode := os.FileMode(0o600)
			if h.Mode&0o100 != 0 {
				mode = 0o700
			}
			if err := os.WriteFile(p, b, mode); err != nil {
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
	stopping   bool     // shutdown: don't restart workerd
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
	case "shutdown":
		if err := g.shutdown(); err != nil {
			r.Error = err.Error()
		}
	case "fsbench":
		b, err := fsbench(m.N)
		if err != nil {
			r.Error = err.Error()
		}
		r.Bench = b
	case "probe":
		if !g.cfg.Debug {
			r.Error = "probe needs debug mode"
			break
		}
		r.Results = probe(m.Targets)
	case "run":
		if !g.cfg.Debug {
			r.Error = "run needs debug mode"
			break
		}
		r.Text, r.ExitCode, r.Error = runDebug(m.Argv)
	default:
		r.Error = "unknown op " + m.Op
	}
	return r
}

// shutdown stops workerd for good, then syncs and unmounts the DO volume,
// so the host can hand it to the next VM with nothing in flight.
func (g *guest) shutdown() error {
	g.mu.Lock()
	g.stopping = true
	pid := g.workerdPID
	g.mu.Unlock()
	if pid > 0 {
		unix.Kill(pid, unix.SIGTERM)
		for i := 0; i < 200; i++ { // ≤ 2 s
			if unix.Kill(pid, 0) != nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		unix.Kill(pid, unix.SIGKILL)
		for i := 0; i < 100 && unix.Kill(pid, 0) == nil; i++ {
			time.Sleep(10 * time.Millisecond)
		}
	}
	unix.Sync()
	if g.cfg.Volume != "" {
		if err := unix.Unmount(proto.DODir, 0); err != nil {
			// Something still holds it: force it read-only (flushes).
			unix.Mount("", proto.DODir, "", unix.MS_REMOUNT|unix.MS_RDONLY, "")
			return fmt.Errorf("unmount %s: %w (remounted read-only)", proto.DODir, err)
		}
	}
	return nil
}

func doFS() string {
	b, _ := os.ReadFile("/proc/mounts")
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) > 2 && f[1] == proto.DODir {
			return f[0] + " " + f[2]
		}
	}
	return "tmpfs (/run)"
}

// fsbench measures 4 KiB write + fdatasync latency on the DO volume.
func fsbench(n int) (*proto.Bench, error) {
	if n <= 0 || n > 2000 {
		n = 200
	}
	p := filepath.Join(proto.DODir, ".twd-fsbench")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer os.Remove(p)
	defer f.Close()
	buf := bytes.Repeat([]byte{0x5a}, 4096)
	var w, s []int64
	for i := 0; i < n; i++ {
		t := time.Now()
		if _, err := f.WriteAt(buf, int64(i%256)*4096); err != nil {
			return nil, err
		}
		t2 := time.Now()
		if err := unix.Fdatasync(int(f.Fd())); err != nil {
			return nil, err
		}
		w = append(w, t2.Sub(t).Microseconds())
		s = append(s, time.Since(t2).Microseconds())
	}
	pct := func(xs []int64, q float64) int64 {
		c := append([]int64(nil), xs...)
		sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
		return c[int(float64(len(c)-1)*q)]
	}
	return &proto.Bench{N: n, FS: doFS(), WriteP50US: pct(w, 0.5), WriteP95US: pct(w, 0.95), SyncP50US: pct(s, 0.5), SyncP95US: pct(s, 0.95)}, nil
}

func (g *guest) stats() *proto.Stats {
	s := &proto.Stats{UptimeMS: time.Since(start).Milliseconds(), DOFS: doFS()}
	var sf unix.Statfs_t
	if unix.Statfs(proto.DODir, &sf) == nil {
		s.DOUsedKB = int64(sf.Blocks-sf.Bfree) * sf.Bsize / 1024
	}
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

// runDebug runs argv (relative to AppDir) for a platform test and returns its
// combined output (≤ 64 KiB) and exit code.
func runDebug(argv []string) (string, int, string) {
	if len(argv) == 0 {
		return "", -1, "no argv"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = proto.AppDir
	cmd.Env = []string{"HOME=/tmp", "PATH=/usr/bin:/bin", "TZ=UTC"}
	out, err := cmd.CombinedOutput()
	if len(out) > 64<<10 {
		out = out[:64<<10]
	}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return string(out), ee.ExitCode(), ""
	case err != nil:
		return string(out), -1, err.Error()
	}
	return string(out), 0, ""
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
		g.mu.Lock()
		stopping := g.stopping
		g.mu.Unlock()
		if stopping {
			logf("workerd stopped for shutdown")
			select {}
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
