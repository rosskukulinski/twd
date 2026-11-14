//go:build darwin

package vm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm/vzapi"
	"github.com/rosskukulinski/twd/netd"
)

// Config configures the Virtualization.framework (vz) backend: the macOS
// microVM tier (ADR-032 D7). Each VM is one twd-vz process (hostd/vm/vzapi)
// booting the same guest image as Firecracker; hostd itself links no
// Objective-C and needs no entitlement.
type Config struct {
	// VMM is the twd-vz binary, signed with the
	// com.apple.security.virtualization entitlement (default: next to this
	// executable).
	VMM      string
	Kernel   string // arm64 Linux Image (guest/build-kernel-vz.sh; Firecracker's CI kernel also boots)
	Rootfs   string // guest image from guest/build-image.sh (read-only in every VM)
	RunDir   string // per-VM dirs: config drive, console and VMM logs
	MemMiB   int    // guest memory (default 512)
	VCPUs    int    // default 2
	MTU      int    // default 1500
	BootArgs string // kernel cmdline (default below)
	// Rosetta shares Rosetta for Linux into every guest and registers it
	// for x86_64 binaries (needs Rosetta installed, and a kernel with
	// virtiofs: guest/build-kernel-vz.sh).
	Rosetta bool
	// ReadyTimeout bounds boot → workerd healthy in the guest (default 30 s).
	ReadyTimeout time.Duration
	Log          *slog.Logger
}

// DefaultBootArgs: the virtio console, panic → reboot, twd-guestd as init,
// read-only root on the first disk. vz restarts a rebooting guest (unlike
// Firecracker, which exits): a guest that boots a second time is treated
// as crashed (agent.onBoot), so a panic still ends the VM.
const DefaultBootArgs = "console=hvc0 root=/dev/vda reboot=k panic=1 quiet ro rootfstype=ext4 init=/sbin/twd-guestd"

// Backend is the vz microVM backend.
type Backend struct {
	cfg     Config
	seq     atomic.Int64
	ownDir  bool
	rosetta string // the host's Rosetta state from the VMM's check
}

// DefaultVMM is twd-vz next to the running executable.
func DefaultVMM() string {
	exe, err := os.Executable()
	if err != nil {
		return "twd-vz"
	}
	return filepath.Join(filepath.Dir(exe), "twd-vz")
}

// Available reports whether this host can run microVMs: the guest kernel
// and image, and a twd-vz that can configure a VM (Apple silicon or Intel
// with Virtualization.framework, and the entitlement on the binary).
func Available(cfg Config) error {
	_, err := check(cfg)
	return err
}

func check(cfg Config) (rosetta string, err error) {
	if cfg.VMM == "" {
		cfg.VMM = DefaultVMM()
	}
	for _, p := range []string{cfg.VMM, cfg.Kernel, cfg.Rootfs} {
		if _, err := os.Stat(p); err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, cfg.VMM, "-check", cfg.Kernel).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("%s: %v: %s (is it signed with the virtualization entitlement? `make build` does that)", cfg.VMM, err, s)
	}
	_, rosetta, _ = strings.Cut(s, "rosetta=")
	return rosetta, nil
}

// New returns a Backend.
func New(cfg Config) (*Backend, error) {
	if cfg.VMM == "" {
		cfg.VMM = DefaultVMM()
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
	rosetta, err := check(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Rosetta && rosetta != "installed" {
		return nil, fmt.Errorf("rosetta requested but Rosetta for Linux is %q on this host (softwareupdate --install-rosetta)", rosetta)
	}
	b := &Backend{cfg: cfg, rosetta: rosetta}
	if cfg.RunDir == "" {
		d, err := os.MkdirTemp("", "twd-vz-")
		if err != nil {
			return nil, err
		}
		b.cfg.RunDir, b.ownDir = d, true
	}
	if err := os.MkdirAll(b.cfg.RunDir, 0o700); err != nil {
		return nil, err
	}
	return b, nil
}

// Close removes the backend's run directory if it created it. Running VMs
// must be stopped first.
func (b *Backend) Close() error {
	if b.ownDir {
		return os.RemoveAll(b.cfg.RunDir)
	}
	return nil
}

// Name implements sandbox.Backend.
func (b *Backend) Name() string { return "vz" }

// Via says how VMs are launched.
func (b *Backend) Via() string {
	return "twd-vz " + b.cfg.VMM + " (Virtualization.framework; rosetta " + b.rosetta + ")"
}

// Machine is one running vz microVM.
type Machine struct {
	agent
	b    *Backend
	id   string
	dir  string
	cmd  *exec.Cmd
	vol  *vzVolume
	vsln *fdListener

	ctlConn    net.Conn
	ctlMu      sync.Mutex
	ctlEnc     *json.Encoder
	ctlSeq     int64
	ctlPending map[int64]chan vzapi.Ctl
	started    chan vzapi.Ctl
	vmmWhy     atomic.Value // string: the VMM's "stopped" reason

	stopped atomic.Bool
	exitErr error
	disks   []string
	cleanup sync.Once
}

// Boot implements sandbox.Backend.
func (b *Backend) Boot(ctx context.Context, s sandbox.Spec) (vmi sandbox.VM, err error) {
	t0 := time.Now()
	n := b.seq.Add(1)
	id := fmt.Sprintf("%d-%s-%d", os.Getpid(), s.ID, n)
	m := &Machine{agent: newAgent(), b: b, id: id, dir: filepath.Join(b.cfg.RunDir, id),
		ctlPending: map[int64]chan vzapi.Ctl{}, started: make(chan vzapi.Ctl, 1)}
	m.log = b.cfg.Log.With("vm", id)
	if s.Log != nil {
		m.log = s.Log.With("vm", id)
	}
	m.onBoot = func(n int) {
		if n > 1 { // the guest rebooted (panic, or guestd gave up): a crash
			m.vmmWhy.Store("the guest rebooted")
			m.Kill()
		}
	}
	defer func() {
		if err != nil {
			if m.cmd != nil && m.cmd.Process != nil {
				m.stopped.Store(true)
				m.cmd.Process.Kill()
				<-m.done
			}
			m.teardown()
		}
	}()
	os.RemoveAll(m.dir)
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, err
	}

	// 1. The DO volume: attached (decrypted) on the host first, so the
	// guest is told whether to format it.
	if v := s.Volume; v != nil {
		m.vol, err = attachVolume(v)
		if err != nil {
			return nil, fmt.Errorf("volume %s: %w", v.Name, err)
		}
		m.info.Volume = v.Name
		m.info.VolumeNew = m.vol.format
		m.info.VolumeAttachMS = m.vol.attachMS
	}
	// 2. Config drive: the instance dir + guest.json, as a raw tar.
	drive := filepath.Join(m.dir, "config.tar")
	if err := writeConfigDrive(drive, s, b.cfg.MTU, func(gc *proto.GuestConfig) {
		gc.VolumeFormat = m.vol != nil && m.vol.format
		if b.cfg.Rosetta {
			gc.Rosetta = vzapi.RosettaTag
		}
	}); err != nil {
		return nil, fmt.Errorf("config drive: %w", err)
	}
	m.disks = []string{b.cfg.Rootfs, drive}
	if m.vol != nil {
		m.disks = append(m.disks, m.vol.img)
	}
	m.info.Console = filepath.Join(m.dir, "console.log")

	// 3. The VMM's descriptors: NIC (datagram frames), control, vsock.
	nicHost, nicVMM, err := dgramPair()
	if err != nil {
		return nil, err
	}
	defer nicVMM.Close()
	ctlHost, ctlVMM, err := streamPair("ctl")
	if err != nil {
		nicHost.Close()
		return nil, err
	}
	defer ctlVMM.Close()
	vsHost, vsVMM, err := streamPair("vsock")
	if err != nil {
		nicHost.Close()
		ctlHost.Close()
		return nil, err
	}
	defer vsVMM.Close()
	if m.ctlConn, err = net.FileConn(ctlHost); err != nil {
		nicHost.Close()
		ctlHost.Close()
		vsHost.Close()
		return nil, err
	}
	ctlHost.Close()
	m.ctlEnc = json.NewEncoder(m.ctlConn)
	vc, err := net.FileConn(vsHost)
	vsHost.Close()
	if err != nil {
		nicHost.Close()
		return nil, err
	}
	m.vsln = &fdListener{c: vc.(*net.UnixConn)}
	m.vn, err = newDgramVnet(nicHost, uint32(b.cfg.MTU), gwMAC, gatewayIP, guestIP, proto.PrefixLen, s.Handlers)
	if err != nil {
		return nil, err
	}
	m.ingress = m.ingressProxy()

	// 4. The VMM.
	vc2 := vzapi.Config{Kernel: b.cfg.Kernel, Cmdline: b.cfg.BootArgs, CPUs: b.cfg.VCPUs, MemMiB: b.cfg.MemMiB,
		Disks:   []vzapi.Disk{{Path: b.cfg.Rootfs, ReadOnly: true}, {Path: drive, ReadOnly: true}},
		Volume:  m.vol != nil,
		Console: m.info.Console, MAC: guestMAC, MTU: b.cfg.MTU, VsockPort: proto.VsockPort, Rosetta: b.cfg.Rosetta}
	cb, _ := json.Marshal(vc2)
	logf, err := os.OpenFile(filepath.Join(m.dir, "vmm.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	m.cmd = exec.Command(b.cfg.VMM)
	m.cmd.Stdin = bytes.NewReader(cb)
	m.cmd.Stdout, m.cmd.Stderr = logf, logf
	m.cmd.ExtraFiles = []*os.File{nicVMM, ctlVMM, vsVMM} // fds 3, 4, 5 (vzapi)
	if m.vol != nil {
		m.cmd.ExtraFiles = append(m.cmd.ExtraFiles, m.vol.f) // fd 6
	}
	m.start = time.Now()
	if err := m.cmd.Start(); err != nil {
		m.cmd = nil
		return nil, fmt.Errorf("start %s: %w", b.cfg.VMM, err)
	}
	if m.vol != nil {
		// The VMM holds the decrypted device now; hostd doesn't need it.
		m.vol.f.Close()
		m.vol.f = nil
	}
	m.info.PID = m.cmd.Process.Pid
	go m.readVMM()
	go m.acceptCtl(m.vsln)
	go m.watch()

	// 5. Wait for the VM to run, then for guestd to report workerd healthy.
	rctx, cancel := context.WithTimeout(ctx, b.cfg.ReadyTimeout)
	defer cancel()
	select {
	case st := <-m.started:
		m.info.ServicePID = st.ServicePID
		if b.cfg.Rosetta {
			m.log.Info("rosetta share", "host", st.Rosetta)
		}
	case <-m.done:
		return nil, fmt.Errorf("vz VMM exited during start: %v", m.exitErr)
	case <-rctx.Done():
		return nil, fmt.Errorf("vz VMM did not start the VM in %v: %s", b.cfg.ReadyTimeout, m.logTail("vmm.log", 1500))
	}
	select {
	case <-m.ready:
	case <-m.done:
		return nil, fmt.Errorf("microVM exited during boot: %v", m.exitErr)
	case <-rctx.Done():
		return nil, fmt.Errorf("microVM not ready after %v; events %v; last %s; console: %s", b.cfg.ReadyTimeout, m.eventTypes(), m.lastError(), m.logTail("console.log", 3000))
	}
	m.info.Total = time.Since(t0)
	m.log.Info("microVM ready", "backend", "vz", "boot_ms", m.info.Boot.Milliseconds(), "ready_ms", m.info.Ready.Milliseconds(),
		"total_ms", m.info.Total.Milliseconds(), "volume", m.info.Volume, "volume_ms", m.info.VolumeAttachMS, "volume_new", m.info.VolumeNew)
	return m, nil
}

func streamPair(name string) (host, vmm *os.File, err error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	return os.NewFile(uintptr(fds[0]), name+"-host"), os.NewFile(uintptr(fds[1]), name+"-vmm"), nil
}

// readVMM reads the VMM's control events.
func (m *Machine) readVMM() {
	sc := bufio.NewScanner(m.ctlConn)
	for sc.Scan() {
		var c vzapi.Ctl
		if json.Unmarshal(sc.Bytes(), &c) != nil {
			continue
		}
		switch c.Event {
		case "started":
			select {
			case m.started <- c:
			default:
			}
		case "stopped":
			m.vmmWhy.Store(c.Error)
		case "resp":
			m.ctlMu.Lock()
			ch := m.ctlPending[c.ID]
			delete(m.ctlPending, c.ID)
			m.ctlMu.Unlock()
			if ch != nil {
				ch <- c
			}
		}
	}
}

// vmmOp sends a control op to the VMM.
func (m *Machine) vmmOp(ctx context.Context, op vzapi.Ctl) error {
	ch := make(chan vzapi.Ctl, 1)
	m.ctlMu.Lock()
	m.ctlSeq++
	op.ID = m.ctlSeq
	m.ctlPending[op.ID] = ch
	err := m.ctlEnc.Encode(op)
	m.ctlMu.Unlock()
	if err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r.Error != "" {
			return errors.New(r.Error)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-m.done:
		return errors.New("microVM exited")
	}
}

// watch turns the VMM's exit into the VM's exit.
func (m *Machine) watch() {
	werr := m.cmd.Wait()
	if !m.stopped.Load() {
		why, _ := m.vmmWhy.Load().(string)
		if why == "" {
			why = fmt.Sprint(werr)
		}
		m.exitErr = fmt.Errorf("vz VMM exited: %s; vmm: %s; console: %s", why, m.logTail("vmm.log", 500), m.logTail("console.log", 1500))
		m.log.Warn("microVM exited", "why", why)
	}
	m.teardown()
	close(m.done)
}

func (m *Machine) logTail(name string, n int) string {
	b, _ := os.ReadFile(filepath.Join(m.dir, name))
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return netd.Redact(string(bytes.TrimSpace(b)))
}

// Ingress, DialGuest, Op, Info, Refused, Done: agent.

// Err implements sandbox.VM.
func (m *Machine) Err() error { return m.exitErr }

// Disks implements sandbox.VM: the rootfs, the config drive and (if any)
// the encrypted volume image, as the host stores them.
func (m *Machine) Disks() []string { return m.disks }

// RSSKB implements sandbox.VM: the VMM's resident set plus the
// Virtualization.framework service process that holds guest memory.
func (m *Machine) RSSKB() int64 {
	pids := []string{}
	for _, p := range []int{m.info.PID, m.info.ServicePID} {
		if p > 0 {
			pids = append(pids, strconv.Itoa(p))
		}
	}
	if len(pids) == 0 {
		return 0
	}
	out, _ := exec.Command("/bin/ps", "-o", "rss=", "-p", strings.Join(pids, ",")).Output()
	var total int64
	for _, f := range strings.Fields(string(out)) {
		n, _ := strconv.ParseInt(f, 10, 64)
		total += n
	}
	return total
}

// Kill implements sandbox.VM: SIGKILL the VMM (no clean shutdown; the
// framework tears the VM service down with it).
func (m *Machine) Kill() {
	if m.cmd != nil && m.cmd.Process != nil {
		m.cmd.Process.Kill()
	}
}

// Stop implements sandbox.VM. With a volume, the guest first stops workerd
// and unmounts it, then the VMM is killed and the volume detached; Stop
// returns after that release.
func (m *Machine) Stop() {
	if m.vol != nil && m.readyOK.Load() {
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

// teardown aborts every flow, drops the VMM's control connection (it exits
// on that) and, once the VMM is gone, detaches the volume (idempotent).
func (m *Machine) teardown() {
	m.cleanup.Do(func() {
		if m.vn != nil {
			m.vn.close()
		}
		if m.vsln != nil {
			m.vsln.Close()
		}
		m.mu.Lock()
		if m.ctl != nil {
			m.ctl.Close()
		}
		m.mu.Unlock()
		if m.ctlConn != nil {
			m.ctlConn.Close()
		}
		if m.vol != nil {
			if err := m.vol.release(); err != nil {
				m.log.Warn("volume release", "err", err)
			}
		}
	})
}

// MemoryDump implements sandbox.VM via Virtualization.framework's saved
// machine state (pause, save, resume). On macOS 27.2 the framework
// validates this configuration for save but the VM service then fails it
// ("permission denied"), so the no-keys memory scan is not available on
// vz; callers get the error (and must in any case check that a dump is
// usable: vmcheck's positive control does).
func (m *Machine) MemoryDump(ctx context.Context, dir string) (string, error) {
	p := filepath.Join(dir, m.id+".vzstate")
	if err := m.vmmOp(ctx, vzapi.Ctl{Op: "save", Path: p}); err != nil {
		return "", fmt.Errorf("vz save state: %w", err)
	}
	return p, nil
}

// fdListener accepts the guest's vsock connections as descriptors from
// the VMM (vzapi FDVsock).
type fdListener struct {
	c    *net.UnixConn
	once sync.Once
}

func (l *fdListener) Accept() (net.Conn, error) {
	for {
		buf := make([]byte, 1)
		oob := make([]byte, syscall.CmsgSpace(4*4))
		_, oobn, _, _, err := l.c.ReadMsgUnix(buf, oob)
		if err != nil {
			return nil, err
		}
		msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
		if err != nil || len(msgs) == 0 {
			continue
		}
		fds, err := syscall.ParseUnixRights(&msgs[0])
		if err != nil || len(fds) == 0 {
			continue
		}
		for _, fd := range fds[1:] {
			syscall.Close(fd)
		}
		syscall.CloseOnExec(fds[0])
		f := os.NewFile(uintptr(fds[0]), "vsock")
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			continue
		}
		return c, nil
	}
}

func (l *fdListener) Close() error {
	var err error
	l.once.Do(func() { err = l.c.Close() })
	return err
}

func (l *fdListener) Addr() net.Addr { return l.c.LocalAddr() }
