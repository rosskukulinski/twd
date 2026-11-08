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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/netd"
	"golang.org/x/sys/unix"
)

// Config configures the Firecracker backend.
type Config struct {
	Firecracker string // firecracker binary (default /usr/local/bin/firecracker)
	Kernel      string // uncompressed guest kernel (vmlinux)
	Rootfs      string // guest image from guest/build-image.sh (mounted read-only by every VM)
	RunDir      string // per-VM dirs: API socket, vsock, config drive, console log
	MemMiB      int    // guest memory (default 512)
	VCPUs       int    // default 2 (1 vCPU: workerd's threads contend; p50 latency ~30 ms nested)
	MTU         int    // default 1500
	BootArgs    string // kernel cmdline (default below)
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
	cfg Config
	seq atomic.Int64
}

// Available reports whether this host can run microVMs (KVM, Firecracker,
// root for network namespaces).
func Available(cfg Config) error {
	if cfg.Firecracker == "" {
		cfg.Firecracker = "/usr/local/bin/firecracker"
	}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		return fmt.Errorf("no usable /dev/kvm: %w", err)
	} else {
		f.Close()
	}
	for _, p := range []string{cfg.Firecracker, cfg.Kernel, cfg.Rootfs} {
		if _, err := os.Stat(p); err != nil {
			return err
		}
	}
	if os.Geteuid() != 0 {
		return errors.New("the microVM tier needs root (network namespaces, TAP devices)")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		return err
	}
	return nil
}

// New returns a Backend.
func New(cfg Config) (*Backend, error) {
	if cfg.Firecracker == "" {
		cfg.Firecracker = "/usr/local/bin/firecracker"
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
	if cfg.RunDir == "" {
		d, err := os.MkdirTemp("/tmp", "twd-vm-")
		if err != nil {
			return nil, err
		}
		cfg.RunDir = d
	}
	if err := os.MkdirAll(cfg.RunDir, 0o700); err != nil {
		return nil, err
	}
	return &Backend{cfg: cfg}, nil
}

func (b *Backend) machineConfig() map[string]any {
	mc := map[string]any{"vcpu_count": b.cfg.VCPUs, "mem_size_mib": b.cfg.MemMiB}
	switch b.cfg.HugePages {
	case "on":
		mc["huge_pages"] = "2M"
	case "", "auto":
		if FreeHugePages2M()*2 >= b.cfg.MemMiB {
			mc["huge_pages"] = "2M"
		}
	}
	return mc
}

// Name implements sandbox.Backend.
func (b *Backend) Name() string { return "firecracker" }

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
	ns      string
	dir     string
	log     *slog.Logger
	cmd     *exec.Cmd
	api     *http.Client
	vn      *vnet
	tapFD   int
	vsock   net.Listener
	ingress http.Handler

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
}

// Boot implements sandbox.Backend.
func (b *Backend) Boot(ctx context.Context, s sandbox.Spec) (vmi sandbox.VM, err error) {
	t0 := time.Now()
	n := b.seq.Add(1)
	id := fmt.Sprintf("%s-%d", s.ID, n)
	if len(id) > 20 {
		id = id[len(id)-20:]
	}
	m := &Machine{b: b, id: id, ns: "twd-" + id, dir: filepath.Join(b.cfg.RunDir, id), tapFD: -1,
		done: make(chan struct{}), ready: make(chan struct{}), pending: map[int64]chan proto.Msg{}}
	m.log = b.cfg.Log.With("vm", id)
	if s.Log != nil {
		m.log = s.Log.With("vm", id)
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
	// 1. Network: namespace, bridge, Firecracker's TAP, our TAP, gVisor stack.
	if err := setupNetns(m.ns, b.cfg.MTU); err != nil {
		return nil, err
	}
	fd, err := openTapInNetns(m.ns, "gv0", b.cfg.MTU)
	if err != nil {
		return nil, err
	}
	m.tapFD = fd
	m.vn, err = newVnet(fd, uint32(b.cfg.MTU), gwMAC, gatewayIP, guestIP, proto.PrefixLen, s.Handlers)
	if err != nil {
		return nil, err
	}
	m.ingress = m.ingressProxy()

	// 2. Config drive: the instance dir + guest.json, as a raw tar.
	drive := filepath.Join(m.dir, "config.tar")
	if err := writeConfigDrive(drive, s, b.cfg.MTU); err != nil {
		return nil, fmt.Errorf("config drive: %w", err)
	}
	m.disks = []string{b.cfg.Rootfs, drive}

	// 3. vsock control channel: guestd connects to CID 2 port VsockPort,
	// which Firecracker forwards to <uds>_<port>.
	vs := filepath.Join(m.dir, "v.sock")
	m.vsock, err = net.Listen("unix", fmt.Sprintf("%s_%d", vs, proto.VsockPort))
	if err != nil {
		return nil, err
	}
	go m.acceptCtl()

	// 4. Firecracker, in the namespace, configured over its API socket.
	apiSock := filepath.Join(m.dir, "fc.sock")
	console, err := os.OpenFile(filepath.Join(m.dir, "console.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	m.cmd = exec.Command("ip", "netns", "exec", m.ns, b.cfg.Firecracker, "--api-sock", apiSock, "--id", id, "--level", "Warning")
	m.cmd.Stdout, m.cmd.Stderr = console, console
	m.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := m.cmd.Start(); err != nil {
		console.Close()
		return nil, err
	}
	go func() {
		werr := m.cmd.Wait()
		console.Close()
		if !m.stopped.Load() {
			m.exitErr = fmt.Errorf("firecracker exited: %v; console: %s", werr, m.consoleTail(1500))
			m.log.Warn("microVM exited", "err", werr)
		}
		m.teardown()
		close(m.done)
	}()
	m.api = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", apiSock)
	}}}
	if err := m.waitAPI(ctx, apiSock); err != nil {
		return nil, err
	}
	for _, c := range []struct {
		path string
		body any
	}{
		{"/machine-config", b.machineConfig()},
		{"/boot-source", map[string]any{"kernel_image_path": b.cfg.Kernel, "boot_args": b.cfg.BootArgs}},
		{"/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": b.cfg.Rootfs, "is_root_device": true, "is_read_only": true}},
		{"/drives/config", map[string]any{"drive_id": "config", "path_on_host": drive, "is_root_device": false, "is_read_only": true}},
		{"/network-interfaces/eth0", map[string]any{"iface_id": "eth0", "host_dev_name": "fc0", "guest_mac": guestMAC}},
		{"/vsock", map[string]any{"guest_cid": 3, "uds_path": vs}},
	} {
		if err := m.call(ctx, "PUT", c.path, c.body); err != nil {
			return nil, err
		}
	}
	m.start = time.Now()
	if err := m.call(ctx, "PUT", "/actions", map[string]any{"action_type": "InstanceStart"}); err != nil {
		return nil, err
	}
	m.info.PID = m.fcPID()

	// 5. Wait for guestd to report workerd healthy.
	rctx, cancel := context.WithTimeout(ctx, b.cfg.ReadyTimeout)
	defer cancel()
	select {
	case <-m.ready:
	case <-m.done:
		return nil, fmt.Errorf("microVM exited during boot: %v", m.exitErr)
	case <-rctx.Done():
		m.mu.Lock()
		le := m.lastErr
		m.mu.Unlock()
		return nil, fmt.Errorf("microVM not ready after %v; events %v; last %s; console: %s", b.cfg.ReadyTimeout, m.eventTypes(), le, m.consoleTail(3000))
	}
	m.info.Total = time.Since(t0)
	m.log.Info("microVM ready", "boot_ms", m.info.Boot.Milliseconds(), "ready_ms", m.info.Ready.Milliseconds(), "total_ms", m.info.Total.Milliseconds())
	return m, nil
}

func (m *Machine) waitAPI(ctx context.Context, sock string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			return nil
		}
		select {
		case <-m.done:
			return fmt.Errorf("firecracker exited: %s", m.consoleTail(1500))
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if time.Now().After(deadline) {
			return errors.New("firecracker API socket did not appear")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (m *Machine) call(ctx context.Context, method, path string, body any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, method, "http://fc"+path, bytes.NewReader(b))
	req.Header.Set("content-type", "application/json")
	resp, err := m.api.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("firecracker %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	return nil
}

// fcPID finds the firecracker process (a child of `ip netns exec`, which
// execs it, so it is usually the same PID).
func (m *Machine) fcPID() int {
	if m.cmd == nil || m.cmd.Process == nil {
		return 0
	}
	return m.cmd.Process.Pid
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
		m.mu.Lock()
		if m.ctl != nil {
			m.ctl.Close()
		}
		m.ctl, m.enc = c, json.NewEncoder(c)
		m.mu.Unlock()
		go m.readCtl(c)
	}
}

func (m *Machine) readCtl(c net.Conn) {
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
	if p := m.fcPID(); p > 0 {
		return procRSSKB(p)
	}
	return 0
}

// Kill implements sandbox.VM: SIGKILL the VMM (no clean shutdown).
func (m *Machine) Kill() {
	if m.cmd != nil && m.cmd.Process != nil {
		syscall.Kill(-m.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// Stop implements sandbox.VM. Firecracker on arm64 has no ACPI shutdown, and
// the guest's state is disposable (tmpfs), so stopping is a kill.
func (m *Machine) Stop() {
	m.stopped.Store(true)
	m.Kill()
	select {
	case <-m.done:
	case <-time.After(5 * time.Second):
	}
	m.teardown()
	if os.Getenv("TWD_KEEP_INSTANCES") == "" {
		os.RemoveAll(m.dir)
	}
}

// teardown aborts every flow and releases the network (idempotent).
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
		if m.cmd != nil && m.cmd.Process != nil && m.cmd.ProcessState == nil {
			syscall.Kill(-m.cmd.Process.Pid, syscall.SIGKILL)
		}
		teardownNetns(m.ns)
	})
}

// MemoryDump implements sandbox.VM via a Firecracker full snapshot.
func (m *Machine) MemoryDump(ctx context.Context, dir string) (string, error) {
	mem := filepath.Join(dir, m.id+".mem")
	snap := filepath.Join(dir, m.id+".snap")
	if err := m.call(ctx, "PATCH", "/vm", map[string]any{"state": "Paused"}); err != nil {
		return "", err
	}
	err := m.call(ctx, "PUT", "/snapshot/create", map[string]any{"snapshot_type": "Full", "snapshot_path": snap, "mem_file_path": mem})
	if rerr := m.call(ctx, "PATCH", "/vm", map[string]any{"state": "Resumed"}); err == nil {
		err = rerr
	}
	os.Remove(snap)
	return mem, err
}

// ---- config drive ----

func writeConfigDrive(path string, s sandbox.Spec, mtu int) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	gc := proto.GuestConfig{UnixNano: time.Now().UnixNano(), IP: proto.GuestIP, Gateway: proto.GatewayIP, Prefix: proto.PrefixLen,
		MTU: mtu, WorkerdArgs: s.WorkerdArgs, HealthCF: s.HealthCF, Debug: s.Debug}
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
