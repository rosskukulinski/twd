//go:build linux

package helper

import (
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
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/hostd/vm/helperapi"
	"golang.org/x/sys/unix"
)

// session is one VM (one client connection).
type session struct {
	s      *Server
	c      *net.UnixConn
	uid    int
	slot   int
	id     string // jailer id = netns name = dm name
	jailID int    // uid = gid
	chroot string
	cgroup string
	mounts []string
	vol    *attached
	cmd    *exec.Cmd
	api    *http.Client
	done   chan struct{}

	stopOnce sync.Once
	cleaned  sync.Once
	stopped  bool
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// start builds the jail and boots the VM. fds: kernel, rootfs, config
// drive, console, [volume]. It returns the fds for the client: hostd's
// TAP and the vsock listener.
func (s *Server) start(c *net.UnixConn, uid int, req *helperapi.StartRequest, fds []int) (_ *session, resp helperapi.Response, out []int, err error) {
	files := make([]*os.File, len(fds))
	for i, fd := range fds {
		files[i] = os.NewFile(uintptr(fd), fmt.Sprintf("client-fd-%d", i))
	}
	defer func() {
		for _, f := range files {
			if f != nil {
				f.Close()
			}
		}
	}()
	want := 4
	if req.Volume != nil && req.Image {
		return nil, resp, nil, herr(helperapi.CodeBadRequest, "a VM has a DO volume or a container image, not both")
	}
	if req.Volume != nil || req.Image {
		want = 5
	}
	if len(files) != want {
		return nil, resp, nil, herr(helperapi.CodeBadRequest, "start needs %d file descriptors, got %d", want, len(files))
	}
	if req.ScratchMiB < 0 || req.ScratchMiB > 1<<20 || req.ScratchMiB > 0 && !req.Image ||
		req.CPUQuotaUS < 0 || req.CPUQuotaUS > req.VCPUs*100000 || req.CPUQuotaUS > 0 && req.CPUQuotaUS < 1000 ||
		req.Pids < 0 || req.Pids > 1<<20 {
		return nil, resp, nil, herr(helperapi.CodeBadRequest, "bad container config")
	}
	if req.MemMiB < 128 || req.MemMiB > 32768 || req.VCPUs < 1 || req.VCPUs > 32 || req.MTU < 576 || req.MTU > 9000 {
		return nil, resp, nil, herr(helperapi.CodeBadRequest, "bad machine config")
	}
	if req.GuestCID < 3 || req.VsockPort <= 0 || req.VsockPort > 65535 {
		return nil, resp, nil, herr(helperapi.CodeBadRequest, "bad vsock config")
	}
	if strings.ContainsAny(req.BootArgs, "\n\x00") || len(req.BootArgs) > 1024 {
		return nil, resp, nil, herr(helperapi.CodeBadRequest, "bad boot args")
	}
	if _, err := net.ParseMAC(req.GuestMAC); err != nil {
		return nil, resp, nil, herr(helperapi.CodeBadRequest, "bad guest MAC")
	}
	t0 := time.Now()
	tm := map[string]float64{}
	slot, err := s.allocSlot()
	if err != nil {
		return nil, resp, nil, err
	}
	sess := &session{s: s, c: c, uid: uid, slot: slot, id: fmt.Sprintf("%s%d", s.prefix(), slot),
		jailID: s.cfg.UIDBase + slot, done: make(chan struct{})}
	sess.chroot = filepath.Join(s.cfg.JailBase, "firecracker", sess.id, "root")
	sess.cgroup = s.cgroup + "/" + sess.id
	defer func() {
		if err != nil {
			sess.cleanup()
		}
	}()
	log := s.log.With("vm", sess.id, "uid", uid, "name", req.Name)

	// 1. Network namespace: br0 + fc0 (the jail's) + gv0 (hostd's).
	if err := setupNetns(sess.id, req.MTU, sess.jailID, sess.jailID); err != nil {
		return nil, resp, nil, err
	}
	tapFD, err := openTapInNetns(sess.id, "gv0", req.MTU)
	if err != nil {
		return nil, resp, nil, err
	}
	out = append(out, tapFD)
	defer func() {
		if err != nil {
			closeAll(out)
			out = nil
		}
	}()
	tm["netns"] = ms(time.Since(t0))

	// 2. The chroot (the jailer adds dev/kvm, dev/net/tun, the binary).
	os.RemoveAll(filepath.Dir(sess.chroot))
	if err := os.MkdirAll(sess.chroot, 0o700); err != nil {
		return nil, resp, nil, err
	}
	if err := os.Chown(sess.chroot, sess.jailID, sess.jailID); err != nil {
		return nil, resp, nil, err
	}
	for i, p := range []struct {
		name string
		ro   bool
		copy bool
	}{{"vmlinux", true, false}, {"rootfs.ext4", true, false}, {"config.tar", true, true}} {
		if err := sess.place(files[i], p.name, p.copy); err != nil {
			return nil, resp, nil, fmt.Errorf("jail %s: %w", p.name, err)
		}
	}
	// vsock: the guest connects to the host (CID 2) on VsockPort, which
	// Firecracker turns into a connect to <uds>_<port>. The listener is
	// created here (owned by the jail uid) and handed to hostd.
	vsPath := filepath.Join(sess.chroot, fmt.Sprintf("v.sock_%d", req.VsockPort))
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: vsPath, Net: "unix"})
	if err != nil {
		return nil, resp, nil, err
	}
	ln.SetUnlinkOnClose(false)
	lf, err := ln.File()
	ln.Close()
	if err != nil {
		return nil, resp, nil, err
	}
	vsFD, err := unix.FcntlInt(lf.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	lf.Close()
	if err != nil {
		return nil, resp, nil, err
	}
	out = append(out, vsFD)
	os.Chown(vsPath, sess.jailID, sess.jailID)
	os.Chmod(vsPath, 0o600)
	// Container guests: the image (read-only, bind-mounted when the jail
	// uid may read it) and a scratch disk made here, owned by the jail uid
	// and removed with the jail: nothing of one instance's writes survives
	// it, and the host-side file is never one a client named.
	if req.Image {
		if err := sess.place(files[4], "image.erofs", false); err != nil {
			return nil, resp, nil, fmt.Errorf("jail image: %w", err)
		}
	}
	if req.ScratchMiB > 0 {
		st := time.Now()
		if err := makeScratch(filepath.Join(sess.chroot, "scratch.ext4"), req.ScratchMiB, sess.jailID); err != nil {
			return nil, resp, nil, fmt.Errorf("scratch disk: %w", err)
		}
		tm["scratch"] = ms(time.Since(st))
	}
	tm["jail"] = ms(time.Since(t0))

	// 3. The volume: LUKS2 → dm-crypt on the host; the jail gets the
	// plaintext block device node, nothing else.
	if req.Volume != nil {
		vt := time.Now()
		a, err := s.vols.attach(files[4], req.Volume, uid, sess.id)
		if err != nil {
			return nil, resp, nil, err
		}
		sess.vol = a
		resp.VolumeNew = a.formatted
		var st unix.Stat_t
		if err := unix.Stat(a.mapper, &st); err != nil {
			return nil, resp, nil, err
		}
		node := filepath.Join(sess.chroot, "vol")
		if err := unix.Mknod(node, unix.S_IFBLK|0o600, int(st.Rdev)); err != nil {
			return nil, resp, nil, fmt.Errorf("mknod volume: %w", err)
		}
		os.Chown(node, sess.jailID, sess.jailID)
		tm["volume"] = ms(time.Since(vt))
		log.Info("volume attached", "volume", req.Volume.Name, "epoch", req.Volume.Epoch, "formatted", a.formatted, "ms", tm["volume"])
	}

	// 4. The jailer: chroot, uid/gid, netns, cgroups; then Firecracker
	// (default seccomp filters) with its API socket inside the jail.
	hp := false
	switch req.HugePages {
	case "on":
		hp = true
	case "", "auto":
		hp = freeHugePages2M()*2 >= req.MemMiB
	}
	memMax := int64(s.cfg.MemOverheadMiB) << 20
	if !hp {
		memMax += int64(req.MemMiB) << 20
	}
	quota := req.VCPUs * 100000
	if req.CPUQuotaUS > 0 {
		quota = req.CPUQuotaUS // a fractional vCPU (ADR-032 D5: lite = 1/16)
	}
	cpuMax := fmt.Sprintf("%d 100000", quota)
	args := []string{"--id", sess.id, "--exec-file", s.cfg.Firecracker, "--uid", fmt.Sprint(sess.jailID), "--gid", fmt.Sprint(sess.jailID),
		"--chroot-base-dir", s.cfg.JailBase, "--netns", "/var/run/netns/" + sess.id,
		"--cgroup-version", "2", "--parent-cgroup", s.cgroup,
		"--cgroup", "cpu.max=" + cpuMax, "--cgroup", fmt.Sprintf("memory.max=%d", memMax)}
	if req.Pids > 0 && s.pidsController() {
		args = append(args, "--cgroup", fmt.Sprintf("pids.max=%d", req.Pids))
	}
	args = append(args, "--", "--api-sock", "/api.sock", "--level", "Warning")
	sess.cmd = exec.Command(s.cfg.Jailer, args...)
	sess.cmd.Stdout, sess.cmd.Stderr = files[3], files[3]
	sess.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sess.cmd.Start(); err != nil {
		return nil, resp, nil, err
	}
	exited := make(chan struct{})
	go func() {
		sess.cmd.Wait()
		close(exited)
	}()
	apiSock := filepath.Join(sess.chroot, "api.sock")
	sess.api = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", apiSock)
	}}}
	if err := waitSock(apiSock, exited, 10*time.Second); err != nil {
		sess.kill()
		<-exited
		return nil, resp, nil, err
	}
	tm["jailer"] = ms(time.Since(t0))
	mc := map[string]any{"vcpu_count": req.VCPUs, "mem_size_mib": req.MemMiB}
	if hp {
		mc["huge_pages"] = "2M"
	}
	calls := []struct {
		path string
		body any
	}{
		{"/machine-config", mc},
		{"/boot-source", map[string]any{"kernel_image_path": "/vmlinux", "boot_args": req.BootArgs}},
		{"/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": "/rootfs.ext4", "is_root_device": true, "is_read_only": true}},
		{"/drives/config", map[string]any{"drive_id": "config", "path_on_host": "/config.tar", "is_root_device": false, "is_read_only": true}},
	}
	if req.Image {
		calls = append(calls, struct {
			path string
			body any
		}{"/drives/image", map[string]any{"drive_id": "image", "path_on_host": "/image.erofs", "is_root_device": false, "is_read_only": true}})
	}
	if req.ScratchMiB > 0 {
		calls = append(calls, struct {
			path string
			body any
		}{"/drives/scratch", map[string]any{"drive_id": "scratch", "path_on_host": "/scratch.ext4", "is_root_device": false, "is_read_only": false}})
	}
	if sess.vol != nil {
		// Writeback: the guest's flushes (fsync) reach the host as fsync
		// of the dm-crypt device; "Unsafe" (the default) would drop them.
		calls = append(calls, struct {
			path string
			body any
		}{"/drives/do", map[string]any{"drive_id": "do", "path_on_host": "/vol", "is_root_device": false, "is_read_only": false, "cache_type": "Writeback"}})
	}
	calls = append(calls,
		struct {
			path string
			body any
		}{"/network-interfaces/eth0", map[string]any{"iface_id": "eth0", "host_dev_name": "fc0", "guest_mac": req.GuestMAC}},
		struct {
			path string
			body any
		}{"/vsock", map[string]any{"guest_cid": req.GuestCID, "uds_path": "/v.sock"}})
	for _, cl := range calls {
		if err := sess.call("PUT", cl.path, cl.body); err != nil {
			sess.kill()
			<-exited
			return nil, resp, nil, err
		}
	}
	started := time.Now()
	if err := sess.call("PUT", "/actions", map[string]any{"action_type": "InstanceStart"}); err != nil {
		sess.kill()
		<-exited
		return nil, resp, nil, err
	}
	tm["instance_start"] = ms(time.Since(t0))
	go sess.wait(exited)
	resp.PID = sess.cmd.Process.Pid
	resp.StartedAt = started.UnixNano()
	resp.HugePages = hp
	resp.Timings = tm
	resp.Jail = &helperapi.Jail{ID: sess.id, UID: sess.jailID, GID: sess.jailID, Chroot: sess.chroot, Cgroup: sess.cgroup,
		CPUMax: cpuMax, MemMax: memMax, Seccomp: "firecracker default (advanced)", Netns: sess.id}
	log.Info("VM started", "pid", resp.PID, "hugepages", hp, "ms", ms(time.Since(t0)))
	return sess, resp, out, nil
}

// place puts the file behind f into the jail as name: a bind mount when
// the jail uid can read it (world-readable; kernel and rootfs), else a
// private copy owned by the jail uid (the config drive, 0600 in hostd).
func (sess *session) place(f *os.File, name string, forceCopy bool) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	dst := filepath.Join(sess.chroot, name)
	if forceCopy || st.Mode().Perm()&0o004 == 0 {
		return copyFile(f, dst, sess.jailID, sess.jailID)
	}
	if err := os.WriteFile(dst, nil, 0o400); err != nil {
		return err
	}
	src := fmt.Sprintf("/proc/self/fd/%d", f.Fd())
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	sess.mounts = append(sess.mounts, dst)
	if err := unix.Mount("", dst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
		return fmt.Errorf("remount ro: %w", err)
	}
	return nil
}

func waitSock(p string, exited <-chan struct{}, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if c, err := net.Dial("unix", p); err == nil {
			c.Close()
			return nil
		}
		select {
		case <-exited:
			return errors.New("jailer/firecracker exited during start (see the console log)")
		default:
		}
		if time.Now().After(deadline) {
			return errors.New("firecracker API socket did not appear")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (sess *session) call(method, path string, body any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, "http://fc"+path, bytes.NewReader(b))
	req.Header.Set("content-type", "application/json")
	resp, err := sess.api.Do(req)
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

func (sess *session) kill() {
	if sess.cmd != nil && sess.cmd.Process != nil {
		syscall.Kill(-sess.cmd.Process.Pid, syscall.SIGKILL)
		sess.cmd.Process.Kill()
	}
	// Anything else in the VM's cgroup.
	os.WriteFile(filepath.Join("/sys/fs/cgroup", sess.cgroup, "cgroup.kill"), []byte("1"), 0o644)
}

// stop kills the VMM (the guest is told to release its volume by hostd
// first, over vsock; by the time it gets here the guest is disposable).
func (sess *session) stop() {
	sess.stopOnce.Do(func() {
		sess.stopped = true
		sess.kill()
	})
}

// wait reaps the VMM, releases everything, then tells the client.
func (sess *session) wait(exited <-chan struct{}) {
	<-exited
	st := sess.cmd.ProcessState
	sess.cleanup()
	ev := helperapi.Response{Event: "exit", OK: true}
	if !sess.stopped {
		ev.Error = fmt.Sprintf("firecracker exited: %v", st)
	}
	helperapi.WriteResponse(sess.c, ev)
	close(sess.done)
	sess.c.Close()
}

// cleanup releases the volume, mounts, netns, jail and cgroup (idempotent).
func (sess *session) cleanup() {
	sess.cleaned.Do(func() {
		if sess.vol != nil {
			if err := sess.s.vols.release(sess.vol); err != nil {
				sess.s.log.Warn("volume release", "vm", sess.id, "err", err)
			}
		}
		for i := len(sess.mounts) - 1; i >= 0; i-- {
			unix.Unmount(sess.mounts[i], unix.MNT_DETACH)
		}
		teardownNetns(sess.id)
		os.RemoveAll(filepath.Dir(sess.chroot))
		cg := filepath.Join("/sys/fs/cgroup", sess.cgroup)
		for i := 0; i < 100; i++ {
			if err := os.Remove(cg); err == nil || errors.Is(err, os.ErrNotExist) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		sess.s.freeSlot(sess.slot)
		if sess.done != nil && sess.cmd == nil {
			close(sess.done) // never started: nothing will wait()
		}
	})
}

// memdump pauses the VM, writes a full snapshot inside the jail, resumes
// it, and returns an fd of the memory file (unlinked).
func (sess *session) memdump() (int, error) {
	if err := sess.call("PATCH", "/vm", map[string]any{"state": "Paused"}); err != nil {
		return -1, err
	}
	err := sess.call("PUT", "/snapshot/create", map[string]any{"snapshot_type": "Full", "snapshot_path": "/snap", "mem_file_path": "/mem"})
	if rerr := sess.call("PATCH", "/vm", map[string]any{"state": "Resumed"}); err == nil {
		err = rerr
	}
	defer os.Remove(filepath.Join(sess.chroot, "snap"))
	defer os.Remove(filepath.Join(sess.chroot, "mem"))
	if err != nil {
		return -1, err
	}
	return unix.Open(filepath.Join(sess.chroot, "mem"), unix.O_RDONLY|unix.O_CLOEXEC, 0)
}
