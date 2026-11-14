//go:build linux

// Package helper is twd-vmhelper's server: the privileged half of the
// microVM tier. twd-hostd runs as a normal user and asks the helper (over
// a root-owned unix socket, authorized by peer uid) to boot each VM. The
// helper:
//
//   - creates the VM's network namespace, its bridge and Firecracker's TAP
//     (owned by the jail uid), opens hostd's TAP and passes its fd back;
//   - builds the jail: a chroot per VM holding only the kernel, the
//     read-only rootfs, the config drive, the volume's device node and the
//     vsock socket, and runs Firecracker through the jailer with a
//     dedicated uid/gid, cgroups v2 cpu.max/memory.max, and Firecracker's
//     default seccomp filters;
//   - attaches the workload's persistent volume: a LUKS2 image opened with
//     dm-crypt on the host (the key comes from hostd, which gets it from
//     mgmt custody, fenced by the lease epoch), so the guest only ever
//     sees plaintext ext4 on /dev/vdc and never a key; a volume is
//     attached to at most one VM, and never at an older epoch than the
//     newest it has been attached at;
//   - tears all of it down when the VM exits or hostd hangs up.
//
// It never opens a path a client names: clients pass open file
// descriptors (SCM_RIGHTS) for the kernel, rootfs, config drive, console
// log and volume image, and the Firecracker and jailer binaries come from
// the helper's own root-owned config.
package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rosskukulinski/twd/hostd/vm/helperapi"
	"golang.org/x/sys/unix"
)

// Config configures a helper.
type Config struct {
	// AllowedUIDs may connect. Empty means root only.
	AllowedUIDs []int  `json:"allowedUIDs"`
	Firecracker string `json:"firecracker"`
	Jailer      string `json:"jailer"`
	// JailBase holds the per-VM chroots (must allow device nodes: not
	// /run or /tmp, which are nodev on most distros).
	JailBase string `json:"jailBase"`
	// StateDir keeps the volume epoch fence (volumes.json).
	StateDir string `json:"stateDir"`
	// UIDBase: VM slot n runs as uid/gid UIDBase+n.
	UIDBase int `json:"uidBase"`
	MaxVMs  int `json:"maxVMs"`
	// Instance distinguishes helpers sharing a host (resource names are
	// "twd<Instance>-<slot>"; startup cleanup only touches its own).
	Instance string `json:"instance"`
	// CgroupParent (relative to the cgroup2 mount) holds the per-VM
	// cgroups. "" = the service's delegated subtree under systemd, else
	// "twd-vm<Instance>".
	CgroupParent string `json:"cgroupParent"`
	// MemOverheadMiB is added to guest memory for memory.max (default 256).
	MemOverheadMiB int          `json:"memOverheadMiB"`
	Log            *slog.Logger `json:"-"`
}

// DefaultConfig is the system helper's configuration.
func DefaultConfig() Config {
	return Config{Firecracker: "/usr/local/libexec/twd/firecracker", Jailer: "/usr/local/libexec/twd/jailer",
		JailBase: "/var/lib/twd-vm/jail", StateDir: "/var/lib/twd-vm", UIDBase: 3900000000, MaxVMs: 1024, Instance: "s"}
}

// Server is a running helper.
type Server struct {
	cfg    Config
	log    *slog.Logger
	cgroup string

	mu    sync.Mutex
	slots map[int]bool
	vols  *volumes
	conns map[*net.UnixConn]bool
	wg    sync.WaitGroup
}

// New prepares a helper: checks its binaries, sets up the cgroup parent
// and removes anything a previous instance left behind.
func New(cfg Config) (*Server, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("twd-vmhelper must run as root")
	}
	d := DefaultConfig()
	if cfg.Firecracker == "" {
		cfg.Firecracker = d.Firecracker
	}
	if cfg.Jailer == "" {
		cfg.Jailer = d.Jailer
	}
	if cfg.JailBase == "" {
		cfg.JailBase = d.JailBase
	}
	if cfg.StateDir == "" {
		cfg.StateDir = d.StateDir
	}
	if cfg.UIDBase == 0 {
		cfg.UIDBase = d.UIDBase
	}
	if cfg.MaxVMs == 0 {
		cfg.MaxVMs = d.MaxVMs
	}
	if cfg.MemOverheadMiB == 0 {
		cfg.MemOverheadMiB = 256
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	for _, p := range []string{cfg.Firecracker, cfg.Jailer} {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
			return nil, fmt.Errorf("%s is not an executable file", p)
		}
	}
	for _, b := range []string{"ip", "cryptsetup", "losetup", "mkfs.ext4"} {
		if _, err := exec.LookPath(b); err != nil {
			return nil, fmt.Errorf("%s not found (install iproute2, cryptsetup-bin, util-linux, e2fsprogs)", b)
		}
	}
	if err := os.MkdirAll(cfg.JailBase, 0o711); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: cfg.Log, slots: map[int]bool{}, conns: map[*net.UnixConn]bool{}}
	s.vols = newVolumes(filepath.Join(cfg.StateDir, "volumes.json"))
	cg, err := s.setupCgroup()
	if err != nil {
		return nil, fmt.Errorf("cgroup: %w", err)
	}
	s.cgroup = cg
	s.cleanupStale()
	return s, nil
}

func (s *Server) prefix() string { return "twd" + s.cfg.Instance + "-" }

// Serve accepts connections on ln until it is closed.
func (s *Server) Serve(ln *net.UnixListener) error {
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveConn(c)
		}()
	}
}

// Close stops every VM (hanging up every session) and waits.
func (s *Server) Close() {
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func peerUID(c *net.UnixConn) (int, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred *unix.Ucred
	var cerr error
	rc.Control(func(fd uintptr) { cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	if cerr != nil {
		return -1, cerr
	}
	return int(cred.Uid), nil
}

func (s *Server) allowed(uid int) bool {
	if uid == 0 {
		return true
	}
	return slices.Contains(s.cfg.AllowedUIDs, uid)
}

func errResp(op string, err error) helperapi.Response {
	var he *helperapi.Err
	if errors.As(err, &he) {
		return helperapi.Response{Re: op, Code: he.Code, Error: he.Msg}
	}
	return helperapi.Response{Re: op, Code: helperapi.CodeInternal, Error: err.Error()}
}

func herr(code, format string, a ...any) error {
	return &helperapi.Err{Code: code, Msg: fmt.Sprintf(format, a...)}
}

func closeAll(fds []int) {
	for _, fd := range fds {
		unix.Close(fd)
	}
}

func (s *Server) serveConn(c *net.UnixConn) {
	s.mu.Lock()
	s.conns[c] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		c.Close()
	}()
	uid, err := peerUID(c)
	if err != nil {
		return
	}
	var vm *session
	defer func() {
		if vm != nil {
			vm.stop()
			<-vm.done
		}
	}()
	for {
		req, fds, err := helperapi.ReadRequest(c)
		if err != nil {
			return
		}
		if !s.allowed(uid) {
			closeAll(fds)
			s.log.Warn("unauthorized helper client", "uid", uid, "op", req.Op)
			helperapi.WriteResponse(c, helperapi.Response{Re: req.Op, Code: helperapi.CodeUnauthorized,
				Error: fmt.Sprintf("uid %d may not use the microVM helper (run `sudo twd host setup --microvm` as that user)", uid)})
			return
		}
		switch req.Op {
		case "ping":
			closeAll(fds)
			helperapi.WriteResponse(c, helperapi.Response{Re: "ping", OK: true, Version: helperapi.Version, UID: uid,
				Jailer: s.cfg.Jailer, Crypt: "dm-crypt (LUKS2, aes-xts-plain64)", Features: []string{helperapi.FeatureContainer}})
		case "start":
			if vm != nil || req.Start == nil {
				closeAll(fds)
				helperapi.WriteResponse(c, helperapi.Response{Re: "start", Code: helperapi.CodeBadRequest, Error: "one start per connection"})
				return
			}
			sess, resp, out, err := s.start(c, uid, req.Start, fds)
			if err != nil {
				s.log.Warn("VM start failed", "uid", uid, "name", req.Start.Name, "err", err)
				helperapi.WriteResponse(c, errResp("start", err))
				return
			}
			vm = sess
			resp.Re, resp.OK = "start", true
			werr := helperapi.WriteResponse(c, resp, out...)
			closeAll(out)
			if werr != nil {
				return
			}
		case "kill":
			closeAll(fds)
			if vm != nil {
				vm.stop()
			}
			helperapi.WriteResponse(c, helperapi.Response{Re: "kill", OK: true})
		case "memdump":
			closeAll(fds)
			if vm == nil {
				helperapi.WriteResponse(c, helperapi.Response{Re: "memdump", Code: helperapi.CodeBadRequest, Error: "no VM"})
				continue
			}
			fd, err := vm.memdump()
			if err != nil {
				helperapi.WriteResponse(c, errResp("memdump", err))
				continue
			}
			helperapi.WriteResponse(c, helperapi.Response{Re: "memdump", OK: true}, fd)
			unix.Close(fd)
		case "cpu":
			closeAll(fds)
			if vm == nil {
				helperapi.WriteResponse(c, helperapi.Response{Re: "cpu", Code: helperapi.CodeBadRequest, Error: "no VM"})
				continue
			}
			cpuMax, err := vm.lowerCPU(req.CPUQuotaUS)
			if err != nil {
				helperapi.WriteResponse(c, errResp("cpu", err))
				continue
			}
			helperapi.WriteResponse(c, helperapi.Response{Re: "cpu", OK: true, Jail: &helperapi.Jail{CPUMax: cpuMax}})
		default:
			closeAll(fds)
			helperapi.WriteResponse(c, helperapi.Response{Re: req.Op, Code: helperapi.CodeBadRequest, Error: "unknown op " + req.Op})
		}
	}
}

func (s *Server) allocSlot() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < s.cfg.MaxVMs; i++ {
		if !s.slots[i] {
			s.slots[i] = true
			return i, nil
		}
	}
	return -1, herr(helperapi.CodeCapacity, "all %d VM slots are in use", s.cfg.MaxVMs)
}

func (s *Server) freeSlot(n int) {
	s.mu.Lock()
	delete(s.slots, n)
	s.mu.Unlock()
}

// ---- cgroups ----

// setupCgroup returns the parent cgroup for VMs (relative to the cgroup2
// mount). Under systemd with Delegate=, the helper moves itself into a
// leaf ("helper") of its own service cgroup so that the per-VM cgroups can
// live in a sibling ("vms") with cpu and memory enabled.
func (s *Server) setupCgroup() (string, error) {
	if s.cfg.CgroupParent != "" {
		return s.cfg.CgroupParent, nil
	}
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	self := ""
	for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(ln, "0::") {
			self = strings.TrimPrefix(ln, "0::")
		}
	}
	if os.Getenv("INVOCATION_ID") != "" && strings.HasSuffix(self, ".service") {
		base := filepath.Join("/sys/fs/cgroup", self)
		if err := os.MkdirAll(filepath.Join(base, "helper"), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(base, "helper", "cgroup.procs"), []byte(fmt.Sprint(os.Getpid())), 0o644); err != nil {
			return "", fmt.Errorf("move into %s/helper: %w (is Delegate=yes set?)", base, err)
		}
		if err := os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), []byte("+cpu +memory"), 0o644); err != nil {
			return "", fmt.Errorf("enable cpu, memory in %s: %w", base, err)
		}
		// pids for container VMMs (ADR-032 D5), where delegated.
		os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), []byte("+pids"), 0o644)
		if err := os.MkdirAll(filepath.Join(base, "vms"), 0o755); err != nil {
			return "", err
		}
		return strings.TrimPrefix(self, "/") + "/vms", nil
	}
	return "twd-vm" + s.cfg.Instance, nil
}

// ---- stale state from a previous run ----

func (s *Server) cleanupStale() {
	p := s.prefix()
	// Mounts under our jails, deepest first.
	if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		var mps []string
		for _, ln := range strings.Split(string(b), "\n") {
			f := strings.Fields(ln)
			if len(f) > 4 && strings.HasPrefix(f[4], filepath.Join(s.cfg.JailBase, "firecracker", p)) {
				mps = append(mps, f[4])
			}
		}
		slices.SortFunc(mps, func(a, b string) int { return len(b) - len(a) })
		for _, m := range mps {
			unix.Unmount(m, unix.MNT_DETACH)
		}
	}
	// VMMs left in our cgroups.
	cgdir := filepath.Join("/sys/fs/cgroup", s.cgroup)
	if ents, err := os.ReadDir(cgdir); err == nil {
		for _, e := range ents {
			if e.IsDir() && strings.HasPrefix(e.Name(), p) {
				os.WriteFile(filepath.Join(cgdir, e.Name(), "cgroup.kill"), []byte("1"), 0o644)
				for i := 0; i < 50; i++ {
					if os.Remove(filepath.Join(cgdir, e.Name())) == nil {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
		}
	}
	// Volumes, then their loop devices.
	if ents, err := os.ReadDir("/sys/block"); err == nil {
		for _, e := range ents {
			nb, err := os.ReadFile(filepath.Join("/sys/block", e.Name(), "dm", "name"))
			if err != nil || !strings.HasPrefix(strings.TrimSpace(string(nb)), p) {
				continue
			}
			slaves, _ := os.ReadDir(filepath.Join("/sys/block", e.Name(), "slaves"))
			exec.Command("cryptsetup", "close", strings.TrimSpace(string(nb))).Run()
			for _, sl := range slaves {
				if strings.HasPrefix(sl.Name(), "loop") {
					exec.Command("losetup", "-d", "/dev/"+sl.Name()).Run()
				}
			}
		}
	}
	if ents, err := os.ReadDir("/run/netns"); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), p) {
				teardownNetns(e.Name())
			}
		}
	}
	if ents, err := os.ReadDir(filepath.Join(s.cfg.JailBase, "firecracker")); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), p) {
				os.RemoveAll(filepath.Join(s.cfg.JailBase, "firecracker", e.Name()))
			}
		}
	}
}

// ---- container guests ----

// makeScratch creates a sparse ext4 of sizeMiB at p (no journal: it is
// thrown away with the VM), owned by the jail uid.
func makeScratch(p string, sizeMiB, uid int) error {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = f.Truncate(int64(sizeMiB) << 20)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if _, err := runStdin(nil, "mkfs.ext4", "-q", "-F", "-O", "^has_journal", "-E", "lazy_itable_init=1,nodiscard", "-m", "0", "-L", "twd-scratch", p); err != nil {
		return err
	}
	return os.Chown(p, uid, uid)
}

// pidsController reports whether the VM cgroups can take pids.max.
func (s *Server) pidsController() bool {
	b, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	return err == nil && slices.Contains(strings.Fields(string(b)), "pids")
}

// ---- small utilities ----

func freeHugePages2M() int {
	b, err := os.ReadFile("/sys/kernel/mm/hugepages/hugepages-2048kB/free_hugepages")
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscan(strings.TrimSpace(string(b)), &n)
	return n
}

func runStdin(stdin []byte, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		pr, pw, err := os.Pipe()
		if err != nil {
			return "", err
		}
		cmd.Stdin = pr
		go func() {
			pw.Write(stdin)
			pw.Close()
		}()
		defer pr.Close()
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// copyFile copies the whole regular file src into dst (new, mode 0600,
// owned by uid/gid).
func copyFile(src *os.File, dst string, uid, gid int) error {
	st, err := src.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.NewSectionReader(src, 0, st.Size()))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Chown(dst, uid, gid)
}
