//go:build linux

package vm

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm/helper"
	"github.com/rosskukulinski/twd/hostd/vm/helperapi"
	"github.com/rosskukulinski/twd/netd"
	"golang.org/x/sys/unix"
)

// Config configures the Firecracker backend.
type Config struct {
	// Helper is twd-vmhelper's socket (installed by `sudo twd host setup
	// --microvm`): the privileged netns/TAP/jailer/dm-crypt half. If it is
	// "" and this process is root, an in-process helper is used instead
	// (tests; Firecracker and Jailer name its binaries then).
	Helper      string
	Firecracker string // in-process helper only (default /usr/local/bin/firecracker)
	Jailer      string // in-process helper only (default /usr/local/bin/jailer)
	Kernel      string // uncompressed guest kernel (vmlinux)
	// ContainerKernel boots container guests (needs erofs + overlayfs:
	// guest/build-kernel.sh); "" = Kernel.
	ContainerKernel string
	Rootfs          string // guest image from guest/build-image.sh (mounted read-only by every VM)
	RunDir          string // per-VM dirs: config drive, console log
	MemMiB          int    // guest memory (default 512)
	VCPUs           int    // default 2 (1 vCPU: workerd's threads contend; p50 latency ~30 ms nested)
	MTU             int    // default 1500
	BootArgs        string // kernel cmdline (default below)
	// HugePages backs guest memory with 2 MiB pages (needs free hugetlbfs
	// pages on the host: vm.nr_hugepages). Far fewer stage-2 faults, which
	// dominate workerd start-up under nested virtualization (measured in
	// Lima: InstanceStart → workerd ready 3.7 s without, 0.43 s with).
	// "auto" (the default, "") uses them when enough are free at boot.
	HugePages string // "" | "auto" | "on" | "off"
	// ReadyTimeout bounds boot → workerd healthy in the guest (default 30 s).
	ReadyTimeout time.Duration
	Log          *slog.Logger
}

// DefaultBootArgs: serial console, no PCI probing, panic → reboot (which
// Firecracker turns into a VM exit), twd-guestd as init, read-only root.
const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 quiet ro rootfstype=ext4 init=/sbin/twd-guestd"

// Backend is the Firecracker microVM backend.
type Backend struct {
	cfg    Config
	seq    atomic.Int64
	ownDir bool
	sock   string
	// in-process helper (root only)
	srv     *helper.Server
	srvLn   *net.UnixListener
	srvBase string
	via     string
}

// Available reports whether this host can run microVMs: KVM, the guest
// kernel and image, and either the helper (as a normal user) or root.
func Available(cfg Config) error {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("no /dev/kvm: %w", err)
	}
	for _, p := range []string{cfg.Kernel, cfg.Rootfs} {
		if _, err := os.Stat(p); err != nil {
			return err
		}
	}
	if cfg.Helper != "" {
		r, err := helperapi.Ping(cfg.Helper)
		if err != nil {
			return fmt.Errorf("microVM helper at %s: %w (run `sudo twd host setup --microvm`)", cfg.Helper, err)
		}
		if r.Version != helperapi.Version {
			return fmt.Errorf("microVM helper at %s speaks protocol %d, want %d (re-run `sudo twd host setup --microvm`)", cfg.Helper, r.Version, helperapi.Version)
		}
		return nil
	}
	if os.Geteuid() != 0 {
		return errors.New("the microVM tier needs the twd-vmhelper service (run `sudo twd host setup --microvm`)")
	}
	return nil
}

// New returns a Backend.
func New(cfg Config) (*Backend, error) {
	if cfg.Firecracker == "" {
		cfg.Firecracker = "/usr/local/bin/firecracker"
	}
	if cfg.Jailer == "" {
		cfg.Jailer = "/usr/local/bin/jailer"
	}
	if cfg.MemMiB == 0 {
		cfg.MemMiB = 512
	}
	if cfg.VCPUs == 0 {
		cfg.VCPUs = 2
	}
	if cfg.MTU == 0 {
		cfg.MTU = 1500
	}
	if cfg.BootArgs == "" {
		cfg.BootArgs = DefaultBootArgs
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if err := Available(cfg); err != nil {
		return nil, err
	}
	b := &Backend{cfg: cfg, sock: cfg.Helper, via: "twd-vmhelper " + cfg.Helper}
	if cfg.RunDir == "" {
		d, err := os.MkdirTemp("/tmp", "twd-vm-")
		if err != nil {
			return nil, err
		}
		b.cfg.RunDir, b.ownDir = d, true
	}
	if err := os.MkdirAll(b.cfg.RunDir, 0o700); err != nil {
		return nil, err
	}
	if b.sock == "" {
		if err := b.startInProcessHelper(); err != nil {
			b.Close()
			return nil, err
		}
	}
	return b, nil
}

// startInProcessHelper runs the privileged helper inside this (root)
// process, on a private socket, with its own jail dir and resource names.
func (b *Backend) startInProcessHelper() error {
	inst := fmt.Sprintf("p%d", os.Getpid())
	b.srvBase = filepath.Join("/var/lib", "twd-vm-"+inst)
	srv, err := helper.New(helper.Config{Firecracker: b.cfg.Firecracker, Jailer: b.cfg.Jailer,
		JailBase: filepath.Join(b.srvBase, "jail"), StateDir: b.srvBase, UIDBase: 3910000000 + (os.Getpid()%1000)*1000,
		MaxVMs: 1000, Instance: inst, Log: b.cfg.Log.With("component", "vmhelper")})
	if err != nil {
		return fmt.Errorf("in-process microVM helper: %w", err)
	}
	b.sock = filepath.Join(b.cfg.RunDir, "helper.sock")
	os.Remove(b.sock)
	ln, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: b.sock, Net: "unixpacket"})
	if err != nil {
		return err
	}
	os.Chmod(b.sock, 0o600)
	b.srv, b.srvLn, b.via = srv, ln, "in-process helper (root)"
	go srv.Serve(ln)
	return nil
}

// Close stops the in-process helper (if any) and removes the backend's run
// directory if it created it. Running VMs must be stopped first.
func (b *Backend) Close() error {
	if b.srv != nil {
		b.srvLn.Close()
		b.srv.Close()
		os.RemoveAll(b.srvBase)
	}
	if b.ownDir {
		return os.RemoveAll(b.cfg.RunDir)
	}
	return nil
}

// Name implements sandbox.Backend.
func (b *Backend) Name() string { return "firecracker" }

// Via says how VMs are launched (helper service or in-process).
func (b *Backend) Via() string { return b.via }

var (
	gatewayIP = netip.MustParseAddr(proto.GatewayIP)
	guestIP   = netip.MustParseAddr(proto.GuestIP)
	gwMAC     = net.HardwareAddr{0x06, 0x00, 0xc0, 0xa8, 0x48, 0x01}
)

const guestMAC = "06:00:c0:a8:48:02"

// Machine is one running Firecracker microVM.
type Machine struct {
	b       *Backend
	id      string
	dir     string
	log     *slog.Logger
	conn    *helperapi.Conn
	pid     int
	vn      *vnet
	tapFD   int
	vsock   net.Listener
	ingress http.Handler
	volume  bool

	info    sandbox.Info
	start   time.Time
	done    chan struct{}
	ready   chan struct{}
	readyOK atomic.Bool
	stopped atomic.Bool
	exitErr error
	disks   []string

	mu      sync.Mutex
	ctl     net.Conn
	enc     *json.Encoder
	pending map[int64]chan proto.Msg
	nextID  int64
	events  []proto.Msg
	lastErr string
	cleanup sync.Once

	ctr *ctrState // container guests only
}

// Boot implements sandbox.Backend.
func (b *Backend) Boot(ctx context.Context, s sandbox.Spec) (vmi sandbox.VM, err error) {
	req := &helperapi.StartRequest{Name: s.ID, MemMiB: b.cfg.MemMiB, VCPUs: b.cfg.VCPUs, HugePages: b.cfg.HugePages,
		BootArgs: b.cfg.BootArgs, MTU: b.cfg.MTU, GuestMAC: guestMAC, GuestCID: 3, VsockPort: proto.VsockPort}
	l := launch{name: s.ID, log: s.Log, kernel: b.cfg.Kernel, req: req, handlers: s.Handlers,
		drive: func(p string) error { return writeConfigDrive(p, s, b.cfg.MTU) }}
	if v := s.Volume; v != nil {
		l.extra = func(m *Machine) (*os.File, error) {
			f, format, err := openVolume(v)
			if err != nil {
				return nil, fmt.Errorf("volume %s: %w", v.Name, err)
			}
			req.Volume = &helperapi.Volume{Name: v.Name, Epoch: v.Epoch, Key: v.Key, Format: format}
			m.disks = append(m.disks, v.Path)
			m.volume = true
			m.info.Volume = v.Name
			return f, nil
		}
	}
	m, err := b.launch(ctx, l)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// launch is one VM boot's inputs (Boot: a workerd guest; BootContainer:
// a container guest).
type launch struct {
	name     string
	log      *slog.Logger
	kernel   string
	drive    func(path string) error // writes the config drive
	req      *helperapi.StartRequest
	extra    func(m *Machine) (*os.File, error) // the fifth drive (DO volume or container image)
	handlers map[uint16]http.Handler
	ctr      *ctrState
}

func (b *Backend) launch(ctx context.Context, l launch) (_ *Machine, err error) {
	t0 := time.Now()
	n := b.seq.Add(1)
	id := fmt.Sprintf("%d-%s-%d", os.Getpid(), l.name, n)
	m := &Machine{b: b, id: id, dir: filepath.Join(b.cfg.RunDir, id), tapFD: -1,
		done: make(chan struct{}), ready: make(chan struct{}), pending: map[int64]chan proto.Msg{}, ctr: l.ctr}
	if l.ctr != nil {
		l.ctr.m = m
	}
	m.log = b.cfg.Log.With("vm", id)
	if l.log != nil {
		m.log = l.log.With("vm", id)
	}
	defer func() {
		if err != nil {
			m.teardown()
		}
	}()
	os.RemoveAll(m.dir)
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, err
	}
	// 1. Config drive: the instance dir + guest.json, as a raw tar.
	drive := filepath.Join(m.dir, "config.tar")
	if err := l.drive(drive); err != nil {
		return nil, fmt.Errorf("config drive: %w", err)
	}
	m.disks = []string{b.cfg.Rootfs, drive}

	// 2. The files the helper puts in the jail, as open fds.
	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for _, p := range []string{l.kernel, b.cfg.Rootfs, drive} {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	console, err := os.OpenFile(filepath.Join(m.dir, "console.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	files = append(files, console)
	m.info.Console = console.Name()
	req := l.req
	if l.extra != nil {
		f, err := l.extra(m)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}

	// 3. The helper: netns + TAPs, jail, volume, jailer → Firecracker,
	// API config, InstanceStart.
	m.conn, err = helperapi.Dial(b.sock)
	if err != nil {
		return nil, fmt.Errorf("microVM helper: %w", err)
	}
	resp, fds, err := m.conn.Do(helperapi.Request{Op: "start", Start: req}, files, 90*time.Second)
	req.Volume = nil // drop our reference to the key copy
	if err != nil {
		return nil, fmt.Errorf("microVM start: %w; console: %s", err, m.consoleTail(1500))
	}
	if len(fds) != 2 {
		for _, fd := range fds {
			unix.Close(fd)
		}
		return nil, fmt.Errorf("microVM helper returned %d fds", len(fds))
	}
	m.tapFD = fds[0]
	vf := os.NewFile(uintptr(fds[1]), "vsock")
	m.vsock, err = net.FileListener(vf)
	vf.Close()
	if err != nil {
		return nil, err
	}
	m.pid = resp.PID
	m.start = time.Unix(0, resp.StartedAt)
	m.info.PID = resp.PID
	m.info.HugePages = resp.HugePages
	m.info.VolumeNew = resp.VolumeNew
	m.info.VolumeAttachMS = resp.Timings["volume"]
	if j := resp.Jail; j != nil {
		m.info.Jail = &sandbox.Jail{ID: j.ID, Chroot: j.Chroot, Cgroup: j.Cgroup, CPUMax: j.CPUMax, Seccomp: j.Seccomp,
			Netns: j.Netns, UID: j.UID, GID: j.GID, MemMax: j.MemMax}
	}
	m.vn, err = newVnet(m.tapFD, uint32(b.cfg.MTU), gwMAC, gatewayIP, guestIP, proto.PrefixLen, l.handlers)
	if err != nil {
		return nil, err
	}
	m.ingress = m.ingressProxy()
	go m.acceptCtl()
	go m.watch()

	// 4. Wait for guestd to report workerd healthy (or, in a container
	// guest, the entrypoint started).
	rctx, cancel := context.WithTimeout(ctx, b.cfg.ReadyTimeout)
	defer cancel()
	select {
	case <-m.ready:
	case <-m.done:
		m.mu.Lock()
		le := m.lastErr
		m.mu.Unlock()
		if le != "" {
			return nil, fmt.Errorf("microVM exited during boot: %s", le)
		}
		return nil, fmt.Errorf("microVM exited during boot: %v", m.exitErr)
	case <-rctx.Done():
		m.mu.Lock()
		le := m.lastErr
		m.mu.Unlock()
		return nil, fmt.Errorf("microVM not ready after %v; events %v; last %s; console: %s", b.cfg.ReadyTimeout, m.eventTypes(), le, m.consoleTail(3000))
	}
	m.info.Total = time.Since(t0)
	m.log.Info("microVM ready", "boot_ms", m.info.Boot.Milliseconds(), "ready_ms", m.info.Ready.Milliseconds(),
		"total_ms", m.info.Total.Milliseconds(), "jail", resp.Jail != nil, "volume", m.info.Volume, "volume_ms", m.info.VolumeAttachMS,
		"container", l.ctr != nil)
	return m, nil
}

// openVolume opens the volume image, creating it (sparse) if missing.
// format reports that it is new (or was created but never formatted).
func openVolume(v *sandbox.Volume) (*os.File, bool, error) {
	if len(v.Key) != 32 {
		return nil, false, errors.New("no volume key")
	}
	size := int64(v.SizeMiB) << 20
	if size == 0 {
		size = 1 << 30
	}
	if err := os.MkdirAll(filepath.Dir(v.Path), 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(v.Path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, false, err
	}
	if st.Size() == 0 {
		if err := f.Truncate(size); err != nil {
			f.Close()
			return nil, false, err
		}
	}
	head := make([]byte, 4096)
	n, _ := f.ReadAt(head, 0)
	blank := true
	for _, c := range head[:n] {
		if c != 0 {
			blank = false
			break
		}
	}
	return f, blank, nil
}

// watch turns the helper's exit event (or a lost helper connection) into
// the VM's exit.
func (m *Machine) watch() {
	var why string
	select {
	case ev := <-m.conn.Events():
		why = ev.Error
	case <-m.conn.Closed():
		why = "helper connection closed"
	}
	if !m.stopped.Load() {
		if why == "" {
			why = "killed"
		}
		m.exitErr = fmt.Errorf("firecracker exited: %s; console: %s", why, m.consoleTail(1500))
		m.log.Warn("microVM exited", "why", why)
	}
	m.teardown()
	close(m.done)
}

func (m *Machine) consoleTail(n int) string {
	b, _ := os.ReadFile(filepath.Join(m.dir, "console.log"))
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return netd.Redact(string(bytes.TrimSpace(b)))
}

// ---- control channel ----

func (m *Machine) acceptCtl() {
	for {
		c, err := m.vsock.Accept()
		if err != nil {
			return
		}
		go m.route(c)
	}
}

// route reads a guest connection's first line: an exec stdio stream
// (container guests) says so; anything else is the control channel
// (guestd opens it with "hello").
func (m *Machine) route(c net.Conn) {
	br := bufio.NewReaderSize(c, 64<<10)
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, err := br.ReadSlice('\n')
	c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return
	}
	var first proto.Msg
	json.Unmarshal(line, &first)
	if first.Type == proto.ExecStreamHello {
		m.attachStream(first.Exec, &bufConn{Conn: c, r: br})
		return
	}
	m.mu.Lock()
	if m.ctl != nil {
		m.ctl.Close()
	}
	m.ctl, m.enc = c, json.NewEncoder(c)
	m.mu.Unlock()
	m.readCtl(io.MultiReader(bytes.NewReader(append([]byte(nil), line...)), br))
}

// bufConn is a conn whose first bytes were read into r.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (m *Machine) readCtl(c io.Reader) {
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
		if len(m.events) < 256 && msg.Type != "log" {
			m.events = append(m.events, msg)
		}
		m.mu.Unlock()
		if m.ctr != nil && m.ctr.event(msg) {
			continue
		}
		switch msg.Type {
		case "boot":
			m.info.Boot = time.Since(m.start)
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

func (m *Machine) eventTypes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s []string
	for _, e := range m.events {
		s = append(s, e.Type)
	}
	return s
}

// Op implements sandbox.VM.
func (m *Machine) Op(ctx context.Context, msg proto.Msg) (proto.Msg, error) {
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

func (m *Machine) ingressProxy() http.Handler {
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
func (m *Machine) Ingress() http.Handler { return m.ingress }

// DialGuest implements sandbox.VM.
func (m *Machine) DialGuest(ctx context.Context) (net.Conn, error) {
	return m.vn.dialGuest(ctx, proto.IngressPort)
}

// ---- lifecycle ----

// Done implements sandbox.VM.
func (m *Machine) Done() <-chan struct{} { return m.done }

// Err implements sandbox.VM.
func (m *Machine) Err() error { return m.exitErr }

// Info implements sandbox.VM.
func (m *Machine) Info() sandbox.Info { return m.info }

// Refused implements sandbox.VM.
func (m *Machine) Refused() []string { return m.vn.refusedFlows() }

// Disks implements sandbox.VM.
func (m *Machine) Disks() []string { return m.disks }

// RSSKB implements sandbox.VM: the firecracker process's RSS.
func (m *Machine) RSSKB() int64 {
	if m.pid > 0 {
		return procRSSKB(m.pid)
	}
	return 0
}

// Kill implements sandbox.VM: the helper SIGKILLs the VMM (no clean
// shutdown; the guest's volume is left as a power cut would leave it).
func (m *Machine) Kill() {
	if m.conn != nil {
		m.conn.Do(helperapi.Request{Op: "kill"}, nil, 5*time.Second)
	}
}

// Stop implements sandbox.VM. With a volume, the guest first stops workerd
// and unmounts it (so every write is on the host), then the VMM is killed
// (arm64 Firecracker has no ACPI shutdown) and the helper releases the
// volume, which can then be attached to the next VM. Stop returns after
// that release.
func (m *Machine) Stop() {
	if m.volume && m.readyOK.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st := time.Now()
		_, err := m.Op(ctx, proto.Msg{Op: "shutdown"})
		cancel()
		m.log.Info("guest released its volume", "ms", time.Since(st).Milliseconds(), "err", err)
	}
	m.stopped.Store(true)
	m.Kill()
	select {
	case <-m.done:
	case <-time.After(15 * time.Second):
		m.log.Warn("microVM did not exit after kill")
	}
	m.teardown()
	if os.Getenv("TWD_KEEP_INSTANCES") == "" {
		os.RemoveAll(m.dir)
	}
}

// teardown aborts every flow and drops the helper session (idempotent).
func (m *Machine) teardown() {
	m.cleanup.Do(func() {
		if m.vn != nil {
			m.vn.close()
		}
		if m.tapFD >= 0 {
			unix.Close(m.tapFD)
		}
		if m.vsock != nil {
			m.vsock.Close()
		}
		m.mu.Lock()
		if m.ctl != nil {
			m.ctl.Close()
		}
		m.mu.Unlock()
		if m.conn != nil {
			m.conn.Close()
		}
	})
}

// MemoryDump implements sandbox.VM via a Firecracker full snapshot (taken
// by the helper inside the jail; we get the memory file as an fd).
func (m *Machine) MemoryDump(ctx context.Context, dir string) (string, error) {
	_, fds, err := m.conn.Do(helperapi.Request{Op: "memdump"}, nil, 60*time.Second)
	if err != nil {
		return "", err
	}
	if len(fds) != 1 {
		for _, fd := range fds {
			unix.Close(fd)
		}
		return "", errors.New("memdump: no fd")
	}
	src := os.NewFile(uintptr(fds[0]), "mem")
	defer src.Close()
	mem := filepath.Join(dir, m.id+".mem")
	out, err := os.OpenFile(mem, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return mem, err
}

// ---- config drive ----

func writeConfigDrive(path string, s sandbox.Spec, mtu int) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	gc := proto.GuestConfig{UnixNano: time.Now().UnixNano(), IP: proto.GuestIP, Gateway: proto.GatewayIP, Prefix: proto.PrefixLen,
		MTU: mtu, WorkerdArgs: s.WorkerdArgs, HealthCF: s.HealthCF, Debug: s.Debug}
	if s.Volume != nil {
		gc.Volume = proto.VolumeDev
	}
	gb, _ := json.Marshal(gc)
	if err := tw.WriteHeader(&tar.Header{Name: proto.GuestConfigFile, Mode: 0o600, Size: int64(len(gb)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	tw.Write(gb)
	err := filepath.WalkDir(s.AppDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(s.AppDir, p)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel) + "/", Mode: 0o700, Typeflag: tar.TypeDir})
		}
		if !d.Type().IsRegular() {
			return nil // sockets etc.
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0o600, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if pad := buf.Len() % 4096; pad != 0 {
		buf.Write(make([]byte, 4096-pad))
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}
