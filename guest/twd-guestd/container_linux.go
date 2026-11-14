//go:build linux

// Container mode (ADR-032 D3/D8): the guest boots twd's kernel and this
// init as usual, but instead of workerd it runs one OCI image:
//
//   - the image (erofs, /dev/vdc) is mounted read-only and the instance's
//     scratch disk (ext4 made fresh by the host, /dev/vdd) holds the
//     overlay's upper layer, so the container sees an ordinary writable
//     root and every write is gone with the VM;
//   - every container process (the image's ENTRYPOINT + CMD, and each
//     exec) is started through "twd-guestd ctr-exec": a re-exec of this
//     binary in a new mount namespace that pivots into the overlay root,
//     drops to the image's (or the exec's) user and execs the target. So
//     no container process ever sees guestd, its config or /run/twd;
//   - all of them live in the guest cgroup "ctr" (pids.max, and cpu.max
//     where the host can't cap the VMM);
//   - they run with Docker's default capability set at most (ctrCaps):
//     no CAP_SYS_ADMIN, so container root can't mount cgroup2 and lift
//     its own pids.max / cpu.max, and no CAP_SYS_PTRACE, so it can't
//     reach guestd's namespace through /proc/1/root;
//   - exec stdio travels on its own vsock connection (proto.ExecStreamHello)
//     as frames; exit codes too.
//
// There is nothing secret in a container guest (no keys of any kind): it
// is a lower trust domain than its parent object, and everything it sends
// is untrusted input to the host.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"golang.org/x/sys/unix"
)

const (
	ctrBase   = "/run/twd/ctr"
	ctrRoot   = ctrBase + "/root"
	ctrCgroup = "/sys/fs/cgroup/ctr"
)

// container is the running image's state (g.ctr).
type container struct {
	cfg      *proto.ContainerConfig
	cgroupFD int

	mu       sync.Mutex
	mainPID  int
	exited   bool
	exitCode int
	execs    map[string]*execProc
	log      *ringBuf
}

type execProc struct {
	id   string
	pid  int
	pty  *os.File // master, if a PTY
	done chan int
}

// runContainer sets up the root and starts the entrypoint; it never
// returns (the host destroys the VM when it is done with it).
func (g *guest) runContainer() {
	c := &container{cfg: g.cfg.Container, cgroupFD: -1, execs: map[string]*execProc{}, log: &ringBuf{max: 64 << 10}}
	g.mu.Lock()
	g.ctr = c
	g.mu.Unlock()
	if err := setupContainerRoot(c.cfg); err != nil {
		logf("container root: %v", err)
		g.send(proto.Msg{Type: "ctr-exit", ExitCode: -1, Error: "container root: " + err.Error()})
		select {}
	}
	fd, err := setupCgroup(c.cfg.Pids)
	if err != nil {
		logf("container cgroup: %v (continuing without pids.max)", err)
	}
	c.cgroupFD = fd
	if q := c.cfg.CPUQuotaUS; q > 0 {
		// The host asked for a fractional-vCPU cap it can't apply itself:
		// no cap, no container (it must not run uncapped).
		if err := capCPU(fd, q); err != nil {
			logf("container cpu.max: %v", err)
			g.send(proto.Msg{Type: "ctr-exit", ExitCode: -1, Error: "container cpu.max: " + err.Error()})
			select {}
		}
	}

	// The entrypoint's output: kept in a ring (the host asks for it on
	// failures) and forwarded as log events, never to the serial console
	// (115200 baud would throttle a chatty server).
	pr, pw, err := os.Pipe()
	if err != nil {
		g.send(proto.Msg{Type: "ctr-exit", ExitCode: -1, Error: err.Error()})
		select {}
	}
	null, _ := os.Open("/dev/null")
	spec := ctrExecSpec{Argv: c.cfg.Argv, Env: c.cfg.Env, Cwd: c.cfg.Cwd, User: c.cfg.User, Mkcwd: true}
	ch := make(chan int, 1)
	pid, err := g.spawnCtr(spec, null, pw, pw, false, ch)
	pw.Close()
	null.Close()
	if err != nil {
		logf("entrypoint: %v", err)
		g.send(proto.Msg{Type: "ctr-exit", ExitCode: 127, Error: "entrypoint: " + err.Error()})
		c.mu.Lock()
		c.exited, c.exitCode = true, 127
		c.mu.Unlock()
		select {}
	}
	c.mu.Lock()
	c.mainPID = pid
	c.mu.Unlock()
	go g.forwardLog(c, pr)
	g.send(proto.Msg{Type: "ctr-ready", PID: pid, UptimeMS: time.Since(start).Milliseconds()})
	code := <-ch
	c.mu.Lock()
	c.exited, c.exitCode, c.mainPID = true, code, 0
	c.mu.Unlock()
	time.Sleep(50 * time.Millisecond) // let the last log lines go first
	g.send(proto.Msg{Type: "ctr-exit", ExitCode: code, Text: c.log.String()})
	select {}
}

func (g *guest) forwardLog(c *container, r *os.File) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		ln, err := br.ReadString('\n')
		if ln != "" {
			c.log.Write([]byte(ln))
			if len(g.out) < cap(g.out)/2 { // logs never crowd out ctr-exit/exec-exit
				g.send(proto.Msg{Type: "log", Text: strings.TrimRight(ln, "\n")})
			}
		}
		if err != nil {
			return
		}
	}
}

func waitDev(p string) error {
	var err error
	for i := 0; i < 200; i++ {
		if _, err = os.Stat(p); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}

// setupContainerRoot mounts image + scratch as an overlay at ctrRoot and
// gives it its own /proc, /sys, a minimal /dev and /etc/hosts.
func setupContainerRoot(cc *proto.ContainerConfig) error {
	lower, scratch := ctrBase+"/lower", ctrBase+"/scratch"
	for _, d := range []string{lower, scratch, ctrRoot} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if err := waitDev(cc.Image); err != nil {
		return fmt.Errorf("image device: %w", err)
	}
	if err := unix.Mount(cc.Image, lower, "erofs", unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("mount image (erofs) %s: %w", cc.Image, err)
	}
	if cc.Scratch != "" {
		if err := waitDev(cc.Scratch); err != nil {
			return fmt.Errorf("scratch device: %w", err)
		}
		if cc.ScratchFormat {
			if err := formatScratch(cc.Scratch); err != nil {
				return err
			}
		}
		if err := unix.Mount(cc.Scratch, scratch, "ext4", unix.MS_NOATIME, "errors=remount-ro"); err != nil {
			return fmt.Errorf("mount scratch %s: %w", cc.Scratch, err)
		}
	} else if err := unix.Mount("tmpfs", scratch, "tmpfs", 0, "mode=0700,size=25%"); err != nil {
		return err
	}
	for _, d := range []string{scratch + "/upper", scratch + "/work"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	opts := "lowerdir=" + lower + ",upperdir=" + scratch + "/upper,workdir=" + scratch + "/work"
	if err := unix.Mount("overlay", ctrRoot, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	r := func(p string) string { return filepath.Join(ctrRoot, p) }
	for _, d := range []string{"proc", "sys", "dev", "tmp", "etc"} {
		if fi, err := os.Lstat(r(d)); err == nil && !fi.IsDir() {
			continue // e.g. a symlink in the image; leave it
		}
		os.MkdirAll(r(d), 0o755)
	}
	os.Chmod(r("tmp"), 0o1777)
	if err := unix.Mount("proc", r("proc"), "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("container /proc: %w", err)
	}
	if err := unix.Mount("sysfs", r("sys"), "sysfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC|unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("container /sys: %w", err)
	}
	// /dev: a tmpfs with the usual nodes only (no block devices: the
	// image, scratch and guest disks are not the container's business).
	if err := unix.Mount("tmpfs", r("dev"), "tmpfs", unix.MS_NOSUID|unix.MS_NOEXEC, "mode=0755,size=64k"); err != nil {
		return fmt.Errorf("container /dev: %w", err)
	}
	for _, n := range []struct {
		name         string
		major, minor uint32
	}{{"null", 1, 3}, {"zero", 1, 5}, {"full", 1, 7}, {"random", 1, 8}, {"urandom", 1, 9}, {"tty", 5, 0}} {
		p := r("dev/" + n.name)
		if err := unix.Mknod(p, unix.S_IFCHR|0o666, int(unix.Mkdev(n.major, n.minor))); err != nil {
			return fmt.Errorf("mknod %s: %w", p, err)
		}
		os.Chmod(p, 0o666)
	}
	for l, t := range map[string]string{"fd": "/proc/self/fd", "stdin": "/proc/self/fd/0", "stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2", "ptmx": "pts/ptmx"} {
		os.Symlink(t, r("dev/"+l))
	}
	// One devpts instance for the guest (exec PTYs), shared with the container.
	os.MkdirAll("/dev/pts", 0o755)
	if err := unix.Mount("devpts", "/dev/pts", "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620,gid=5"); err != nil && !errors.Is(err, unix.EBUSY) {
		logf("devpts: %v", err)
	}
	os.MkdirAll(r("dev/pts"), 0o755)
	unix.Mount("/dev/pts", r("dev/pts"), "", unix.MS_BIND, "")
	os.MkdirAll(r("dev/shm"), 0o1777)
	unix.Mount("tmpfs", r("dev/shm"), "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777")
	os.MkdirAll(r("dev/mqueue"), 0o755)
	host := cc.Hostname
	if host == "" {
		host = "sandbox"
	}
	unix.Sethostname([]byte(host))
	writeIfAbsent(r("etc/hosts"), "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n127.0.1.1\t"+host+"\n")
	os.WriteFile(r("etc/hostname"), []byte(host+"\n"), 0o644)
	// DNS is the host's sandbox egress proxy (ADR-032 D4), which answers
	// only names the container may reach. Without it, an empty resolv.conf
	// makes lookups fail fast instead of timing out. Like Docker, this
	// replaces the image's own resolv.conf.
	os.Remove(r("etc/resolv.conf"))
	rc := ""
	if cc.DNS != "" {
		rc = "nameserver " + cc.DNS + "\noptions timeout:2 attempts:2\n"
	}
	os.WriteFile(r("etc/resolv.conf"), []byte(rc), 0o644)
	if cc.CACert != "" {
		if err := installCA(ctrRoot, cc.CACert); err != nil {
			return fmt.Errorf("intercept CA: %w", err)
		}
	}
	return nil
}

// installCA puts the HTTPS-intercept CA certificate at proto.CACertPath
// and appends it to every trust bundle the image has (resolved inside the
// container root, so an image symlink can't point outside it).
func installCA(root, pem string) error {
	p := filepath.Join(root, proto.CACertPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	os.Remove(p)
	if err := os.WriteFile(p, []byte(pem), 0o644); err != nil {
		return err
	}
	for _, b := range proto.CATrustBundles {
		bp, err := resolveIn(root, b)
		if err != nil {
			continue
		}
		fi, err := os.Stat(bp)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		f, err := os.OpenFile(bp, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			logf("trust bundle %s: %v", b, err)
			continue
		}
		f.WriteString("\n# twd: this container's HTTPS-intercept CA (ADR-032)\n" + pem)
		f.Close()
	}
	return nil
}

// resolveIn resolves p (an absolute path inside the container) under
// root, following symlinks as the container would: absolute targets are
// relative to root and ".." never leaves it.
func resolveIn(root, p string) (string, error) {
	cur := "/"
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(p), "/"), "/")
	for hops := 0; len(parts) > 0; {
		part := parts[0]
		parts = parts[1:]
		if part == "" || part == "." {
			continue
		}
		next := filepath.Join(cur, part) // Clean: ".." stops at "/"
		fi, err := os.Lstat(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if hops++; hops > 40 {
			return "", errors.New("too many symlinks")
		}
		t, err := os.Readlink(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if !strings.HasPrefix(t, "/") {
			t = filepath.Join(cur, t)
		}
		parts = append(strings.Split(strings.TrimPrefix(filepath.Clean(t), "/"), "/"), parts...)
		cur = "/"
	}
	return filepath.Join(root, cur), nil
}

// formatScratch makes the overlay upper's ext4 on a blank scratch device
// (no journal: the disk dies with the VM). Like formatVolume, it refuses a
// device that isn't blank.
func formatScratch(dev string) error {
	f, err := os.Open(dev)
	if err != nil {
		return err
	}
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	f.Close()
	for _, b := range head[:n] {
		if b != 0 {
			return fmt.Errorf("refusing to format scratch %s: not blank", dev)
		}
	}
	st := time.Now()
	// foreground: PID 1's reaper runs by now and would take mke2fs's exit
	// status (cmd.Wait: "waitid: no child processes").
	out, err := foreground(exec.Command("/usr/sbin/mke2fs", "-q", "-F", "-t", "ext4", "-O", "^has_journal", "-L", "twd-scratch", "-m", "0",
		"-E", "lazy_itable_init=1,nodiscard", dev))
	if err != nil {
		return fmt.Errorf("mke2fs scratch: %v: %s", err, strings.TrimSpace(string(out)))
	}
	logf("formatted scratch %s in %v", dev, time.Since(st).Round(time.Millisecond))
	return nil
}

func writeIfAbsent(p, s string) {
	if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
		return
	}
	os.Remove(p)
	os.WriteFile(p, []byte(s), 0o644)
}

// setupCgroup makes the container's cgroup (pids.max) and returns its fd.
func setupCgroup(pids int) (int, error) {
	if pids <= 0 {
		pids = 4096
	}
	os.MkdirAll("/sys/fs/cgroup", 0o755)
	if err := unix.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil && !errors.Is(err, unix.EBUSY) {
		return -1, fmt.Errorf("mount cgroup2: %w", err)
	}
	if err := os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+pids"), 0o644); err != nil {
		return -1, fmt.Errorf("enable pids: %w", err)
	}
	if err := os.MkdirAll(ctrCgroup, 0o755); err != nil {
		return -1, err
	}
	if err := os.WriteFile(ctrCgroup+"/pids.max", []byte(strconv.Itoa(pids)), 0o644); err != nil {
		return -1, fmt.Errorf("pids.max: %w", err)
	}
	return unix.Open(ctrCgroup, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
}

// capCPU sets cpu.max of the container cgroup (fd from setupCgroup) to
// quota µs per 100 ms.
func capCPU(fd, quota int) error {
	if fd < 0 {
		return errors.New("no container cgroup")
	}
	if quota < 1000 {
		quota = 1000 // the kernel's floor is 1 ms
	}
	if err := os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+cpu"), 0o644); err != nil {
		return fmt.Errorf("enable cpu controller: %w", err)
	}
	return os.WriteFile(ctrCgroup+"/cpu.max", []byte(strconv.Itoa(quota)+" 100000"), 0o644)
}

// ctrExecSpec is what "twd-guestd ctr-exec" gets on fd 3.
type ctrExecSpec struct {
	Argv  []string `json:"argv"`
	Env   []string `json:"env"`
	Cwd   string   `json:"cwd"`
	User  string   `json:"user"`
	Mkcwd bool     `json:"mkcwd,omitempty"` // the entrypoint: create a missing WORKDIR (as Docker does)
}

// spawnCtr starts one container process (via ctr-exec) with the given
// stdio; done gets its exit status. With pty, stdin is the PTY slave and
// becomes the controlling terminal.
func (g *guest) spawnCtr(spec ctrExecSpec, stdin, stdout, stderr *os.File, pty bool, done chan int) (int, error) {
	if len(spec.Argv) == 0 {
		return 0, errors.New("empty argv")
	}
	g.mu.Lock()
	c := g.ctr
	g.mu.Unlock()
	specR, specW, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		specR.Close()
		specW.Close()
		return 0, err
	}
	defer errR.Close()
	cmd := &exec.Cmd{Path: "/proc/self/exe", Args: []string{"twd-guestd", "ctr-exec"},
		Stdin: stdin, Stdout: stdout, Stderr: stderr, ExtraFiles: []*os.File{specR, errW},
		Env: []string{}}
	attr := &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNS}
	if c != nil && c.cgroupFD >= 0 {
		attr.UseCgroupFD, attr.CgroupFD = true, c.cgroupFD
	}
	if pty {
		attr.Setsid, attr.Setctty, attr.Ctty = true, true, 0
	} else {
		attr.Setpgid = true
	}
	cmd.SysProcAttr = attr
	g.mu.Lock()
	err = cmd.Start()
	if err == nil {
		g.waiters[cmd.Process.Pid] = done
	}
	g.mu.Unlock()
	specR.Close()
	errW.Close()
	if err != nil {
		specW.Close()
		return 0, err
	}
	json.NewEncoder(specW).Encode(spec)
	specW.Close()
	// ctr-exec reports a setup/exec failure on fd 4, which is close-on-exec:
	// EOF with nothing written means the target is running.
	msg, _ := io.ReadAll(io.LimitReader(errR, 4096))
	if len(msg) > 0 {
		return 0, errors.New(strings.TrimSpace(string(msg)))
	}
	return cmd.Process.Pid, nil
}

// ---- exec ----

// startExec runs one exec: it opens the stdio stream to the host, starts
// the process and pumps frames until it exits.
func (g *guest) startExec(id, token string, es *proto.ExecSpec) (int, error) {
	g.mu.Lock()
	c := g.ctr
	g.mu.Unlock()
	if c == nil {
		return 0, errors.New("not a container guest")
	}
	c.mu.Lock()
	gone := c.exited
	_, dup := c.execs[id]
	c.mu.Unlock()
	if gone {
		return 0, errors.New("the container has exited")
	}
	if dup || id == "" {
		return 0, errors.New("bad exec id")
	}
	stream, err := dialHost()
	if err != nil {
		return 0, fmt.Errorf("exec stream: %w", err)
	}
	hello, _ := json.Marshal(proto.Msg{Type: proto.ExecStreamHello, Exec: id, Token: token})
	if _, err := stream.Write(append(hello, '\n')); err != nil {
		stream.Close()
		return 0, err
	}
	// Cloudflare's exec inherits only PATH from the container's env.
	var base []string
	if p := envGet(c.cfg.Env, "PATH"); p != "" {
		base = []string{"PATH=" + p}
	}
	env := mergeEnv(base, es.Env)
	cwd := es.Cwd
	if cwd == "" {
		cwd = c.cfg.Cwd
	}
	user := es.User
	if user == "" {
		user = c.cfg.User
	}
	spec := ctrExecSpec{Argv: es.Argv, Env: env, Cwd: cwd, User: user}

	var toClose []*os.File
	closeAll := func() {
		for _, f := range toClose {
			f.Close()
		}
	}
	null, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		stream.Close()
		return 0, err
	}
	toClose = append(toClose, null)
	ep := &execProc{id: id, done: make(chan int, 1)}
	var stdin, stdout, stderr *os.File
	var inW, outR, errR *os.File
	if es.PTY {
		m, s, err := openPTY(es.Cols, es.Rows)
		if err != nil {
			closeAll()
			stream.Close()
			return 0, err
		}
		toClose = append(toClose, s)
		ep.pty = m
		stdin, stdout, stderr = s, s, s
		if es.Stdin {
			inW = m
		}
		outR = m
	} else {
		stdin, stdout, stderr = null, null, null
		if es.Stdin {
			r, w, err := os.Pipe()
			if err != nil {
				closeAll()
				stream.Close()
				return 0, err
			}
			toClose = append(toClose, r)
			stdin, inW = r, w
		}
		if es.Stdout != "ignore" {
			r, w, err := os.Pipe()
			if err != nil {
				closeAll()
				stream.Close()
				return 0, err
			}
			toClose = append(toClose, w)
			stdout, outR = w, r
		}
		switch es.Stderr {
		case "ignore":
		case "combined":
			stderr = stdout
		default:
			r, w, err := os.Pipe()
			if err != nil {
				closeAll()
				stream.Close()
				return 0, err
			}
			toClose = append(toClose, w)
			stderr, errR = w, r
		}
	}
	pid, err := g.spawnCtr(spec, stdin, stdout, stderr, es.PTY, ep.done)
	closeAll() // the child has its copies
	if err != nil {
		for _, f := range []*os.File{inW, outR, errR, ep.pty} {
			if f != nil {
				f.Close()
			}
		}
		stream.Close()
		return 0, err
	}
	ep.pid = pid
	c.mu.Lock()
	c.execs[id] = ep
	c.mu.Unlock()
	go g.pumpExec(c, ep, stream, inW, outR, errR)
	return pid, nil
}

func (g *guest) pumpExec(c *container, ep *execProc, stream *os.File, inW, outR, errR *os.File) {
	var wmu sync.Mutex
	send := func(kind byte, p []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return proto.WriteFrame(stream, kind, p)
	}
	var wg sync.WaitGroup
	pump := func(r *os.File, kind byte) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if send(kind, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	for _, p := range []struct {
		f    *os.File
		kind byte
	}{{outR, proto.FrameStdout}, {errR, proto.FrameStderr}} {
		if p.f != nil {
			wg.Add(1)
			go pump(p.f, p.kind)
		}
	}
	// stdin: frames from the host until EOF (half-close) or the stream ends.
	go func() {
		br := bufio.NewReaderSize(stream, 64<<10)
		for {
			kind, p, err := proto.ReadFrame(br)
			if err != nil {
				break
			}
			if inW == nil {
				continue
			}
			switch kind {
			case proto.FrameStdin:
				if _, err := inW.Write(p); err != nil {
					if ep.pty == nil {
						inW.Close()
					}
					inW = nil
				}
			case proto.FrameStdinEOF:
				if ep.pty == nil {
					inW.Close()
				} else {
					inW.Write([]byte{4}) // ^D on a terminal
				}
				inW = nil
			}
		}
		if inW != nil && ep.pty == nil {
			inW.Close()
		}
	}()
	code := <-ep.done
	// Whatever the process wrote is in the pipes now; a daemon it left
	// behind may hold them open, so drain with a deadline, not to EOF.
	for _, f := range []*os.File{outR, errR} {
		if f != nil {
			f.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		}
	}
	wg.Wait()
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(int32(code)))
	send(proto.FrameExit, b[:])
	for _, f := range []*os.File{outR, errR} {
		if f != nil {
			f.Close()
		}
	}
	if ep.pty != nil {
		ep.pty.Close()
	}
	stream.Close()
	c.mu.Lock()
	delete(c.execs, ep.id)
	c.mu.Unlock()
	g.send(proto.Msg{Type: "exec-exit", Exec: ep.id, ExitCode: code})
}

func dialHost() (*os.File, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: proto.VsockPort}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "vsock-exec"), nil
}

func openPTY(cols, rows int) (master, slave *os.File, err error) {
	mfd, err := unix.Open("/dev/pts/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("ptmx: %w", err)
	}
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		unix.Close(mfd)
		return nil, nil, err
	}
	n, err := unix.IoctlGetInt(mfd, unix.TIOCGPTN)
	if err != nil {
		unix.Close(mfd)
		return nil, nil, err
	}
	sfd, err := unix.Open(fmt.Sprintf("/dev/pts/%d", n), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(mfd)
		return nil, nil, err
	}
	if cols > 0 && rows > 0 {
		unix.IoctlSetWinsize(mfd, unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(cols), Row: uint16(rows)})
	}
	return os.NewFile(uintptr(mfd), "ptmx"), os.NewFile(uintptr(sfd), "pts"), nil
}

// mergeEnv overlays KEY=VALUE pairs (later wins, order kept).
func mergeEnv(base, over []string) []string {
	idx := map[string]int{}
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range append(append([]string{}, base...), over...) {
		k, _, _ := strings.Cut(kv, "=")
		if i, ok := idx[k]; ok {
			out[i] = kv
			continue
		}
		idx[k] = len(out)
		out = append(out, kv)
	}
	return out
}

// handleContainer answers the container ops.
func (g *guest) handleContainer(m proto.Msg, r *proto.Msg) {
	g.mu.Lock()
	c := g.ctr
	g.mu.Unlock()
	if c == nil {
		r.Error = "not a container guest"
		return
	}
	switch m.Op {
	case "exec":
		if m.ExecSpec == nil {
			r.Error = "exec: no spec"
			return
		}
		pid, err := g.startExec(m.Exec, m.Token, m.ExecSpec)
		if err != nil {
			r.Error = err.Error()
			return
		}
		r.PID, r.Exec = pid, m.Exec
	case "signal":
		if m.Signal <= 0 || m.Signal > 64 {
			r.Error = "bad signal"
			return
		}
		c.mu.Lock()
		pid := c.mainPID
		if m.Exec != "" {
			pid = 0
			if ep := c.execs[m.Exec]; ep != nil {
				pid = ep.pid
			}
		}
		c.mu.Unlock()
		if pid <= 0 {
			r.Error = "no such process"
			return
		}
		if err := unix.Kill(pid, unix.Signal(m.Signal)); err != nil {
			r.Error = err.Error()
		}
	case "resize":
		c.mu.Lock()
		ep := c.execs[m.Exec]
		c.mu.Unlock()
		if ep == nil || ep.pty == nil {
			r.Error = "no such PTY exec"
			return
		}
		if err := unix.IoctlSetWinsize(int(ep.pty.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(m.Cols), Row: uint16(m.Rows)}); err != nil {
			r.Error = err.Error()
		}
	case "status":
		c.mu.Lock()
		r.PID, r.Running, r.Exited, r.ExitCode = c.mainPID, c.mainPID > 0, c.exited, c.exitCode
		c.mu.Unlock()
	}
}

// ---- the ctr-exec stage (a child; never PID 1) ----

// ctrExec runs in the container process before the target: new mount
// namespace (from clone), pivot into the overlay root, credentials, cwd,
// exec. Errors go to fd 4 (close-on-exec), then exit 127.
func ctrExec() {
	// Capabilities are per thread: drop and exec from this one.
	runtime.LockOSThread()
	errf := os.NewFile(4, "err")
	fail := func(format string, a ...any) {
		fmt.Fprintf(errf, format, a...)
		os.Exit(127)
	}
	unix.CloseOnExec(4)
	var spec ctrExecSpec
	if err := json.NewDecoder(os.NewFile(3, "spec")).Decode(&spec); err != nil {
		fail("spec: %v", err)
	}
	unix.Close(3)
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		fail("make mounts private: %v", err)
	}
	if err := unix.Chdir(ctrRoot); err != nil {
		fail("chdir root: %v", err)
	}
	// pivot_root(".", ".") stacks the old root under the new one; the
	// detach unmount then removes every guest path from this namespace.
	if err := unix.PivotRoot(".", "."); err != nil {
		fail("pivot_root: %v", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		fail("detach old root: %v", err)
	}
	unix.Chdir("/")
	uid, gid, groups, home, err := lookupUser(spec.User)
	if err != nil {
		fail("user %q: %v", spec.User, err)
	}
	env := spec.Env
	if envGet(env, "PATH") == "" {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if envGet(env, "HOME") == "" {
		env = append(env, "HOME="+home)
	}
	if envGet(env, "HOSTNAME") == "" {
		if h, err := os.Hostname(); err == nil {
			env = append(env, "HOSTNAME="+h)
		}
	}
	cwd := spec.Cwd
	if cwd == "" {
		cwd = "/"
	}
	if spec.Mkcwd {
		if _, err := os.Stat(cwd); errors.Is(err, os.ErrNotExist) {
			os.MkdirAll(cwd, 0o755)
			os.Chown(cwd, uid, gid)
		}
	}
	path, err := lookPath(spec.Argv[0], envGet(env, "PATH"))
	if err != nil {
		fail("exec %s: %v", spec.Argv[0], err)
	}
	// No vsock (guestd's channel to the host) and no io_uring for any
	// container process: installed while this is still root with every
	// capability (no no_new_privs needed), inherited across the exec.
	if err := installCtrSeccomp(); err != nil {
		fail("%v", err)
	}
	if err := dropBoundingCaps(); err != nil {
		fail("capabilities: %v", err)
	}
	// syscall's (not x/sys's) set*id apply to every thread of the process.
	if err := syscall.Setgroups(groups); err != nil {
		fail("setgroups: %v", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		fail("setgid: %v", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		fail("setuid: %v", err)
	}
	if err := setCaps(uid); err != nil {
		fail("capabilities: %v", err)
	}
	if err := unix.Chdir(cwd); err != nil {
		fail("chdir %s: %v", cwd, err)
	}
	err = unix.Exec(path, spec.Argv, env)
	fail("exec %s: %v", path, err)
}

// ctrCaps is the most a container process holds: Docker's default set
// (what images and the Sandbox SDK's servers are written against).
var ctrCaps = []int{unix.CAP_CHOWN, unix.CAP_DAC_OVERRIDE, unix.CAP_FSETID, unix.CAP_FOWNER, unix.CAP_MKNOD,
	unix.CAP_NET_RAW, unix.CAP_SETGID, unix.CAP_SETUID, unix.CAP_SETFCAP, unix.CAP_SETPCAP,
	unix.CAP_NET_BIND_SERVICE, unix.CAP_SYS_CHROOT, unix.CAP_KILL, unix.CAP_AUDIT_WRITE}

func ctrCapMask() (m uint64) {
	for _, c := range ctrCaps {
		m |= 1 << uint(c)
	}
	return m
}

// dropBoundingCaps removes every capability outside ctrCaps from this
// thread's bounding set, so no exec (a setuid-root binary included) in
// the container can regain it.
func dropBoundingCaps() error {
	keep := ctrCapMask()
	for c := 0; c <= unix.CAP_LAST_CAP; c++ {
		if keep&(1<<uint(c)) != 0 {
			continue
		}
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil && !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("drop capability %d from the bounding set: %w", c, err)
		}
	}
	return nil
}

// setCaps sets this thread's capabilities for the exec: ctrCaps for
// root, none for any other user (set*id already cleared those).
func setCaps(uid int) error {
	var m uint64
	if uid == 0 {
		m = ctrCapMask()
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	for i := range data {
		w := uint32(m >> (32 * uint(i)))
		data[i] = unix.CapUserData{Effective: w, Permitted: w, Inheritable: w}
	}
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return err
	}
	// No ambient capabilities survive into the target either.
	return unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0)
}

func envGet(env []string, k string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], k+"="); ok {
			return v
		}
	}
	return ""
}

func lookPath(name, path string) (string, error) {
	if strings.Contains(name, "/") {
		if err := executable(name); err != nil {
			return "", err
		}
		return name, nil
	}
	for _, d := range filepath.SplitList(path) {
		if d == "" {
			d = "."
		}
		p := filepath.Join(d, name)
		if executable(p) == nil {
			return p, nil
		}
	}
	return "", errors.New("executable file not found in $PATH")
}

func executable(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return errors.New("permission denied")
	}
	return nil
}

// lookupUser resolves "", "name", "uid", "uid:gid", "name:group" against
// the container's /etc/passwd and /etc/group (as Docker does).
func lookupUser(spec string) (uid, gid int, groups []int, home string, err error) {
	return lookupUserIn(spec, readColon("/etc/passwd"), readColon("/etc/group"))
}

func lookupUserIn(spec string, passwd, group [][]string) (uid, gid int, groups []int, home string, err error) {
	home = "/"
	if spec == "" {
		spec = "0"
	}
	u, g, hasG := strings.Cut(spec, ":")
	uname := ""
	uid = -1
	if n, err := strconv.Atoi(u); err == nil {
		uid = n
		gid = n
		for _, p := range passwd {
			if len(p) >= 7 && p[2] == u {
				uname, home = p[0], p[5]
				gid, _ = strconv.Atoi(p[3])
				break
			}
		}
		if uid == 0 && home == "/" {
			home = "/root"
		}
	} else {
		for _, p := range passwd {
			if len(p) >= 7 && p[0] == u {
				uid, _ = strconv.Atoi(p[2])
				gid, _ = strconv.Atoi(p[3])
				uname, home = p[0], p[5]
				break
			}
		}
		if uid < 0 {
			return 0, 0, nil, "", fmt.Errorf("no user %q in /etc/passwd", u)
		}
	}
	if hasG {
		if n, err := strconv.Atoi(g); err == nil {
			gid = n
		} else {
			found := false
			for _, gr := range group {
				if len(gr) >= 3 && gr[0] == g {
					gid, _ = strconv.Atoi(gr[2])
					found = true
					break
				}
			}
			if !found {
				return 0, 0, nil, "", fmt.Errorf("no group %q in /etc/group", g)
			}
		}
	}
	groups = []int{gid}
	if uname != "" && !hasG {
		for _, gr := range group {
			if len(gr) >= 4 {
				for _, m := range strings.Split(gr[3], ",") {
					if m == uname {
						if n, err := strconv.Atoi(gr[2]); err == nil && n != gid {
							groups = append(groups, n)
						}
					}
				}
			}
		}
	}
	return uid, gid, groups, home, nil
}

func readColon(p string) [][]string {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out [][]string
	for _, ln := range strings.Split(string(b), "\n") {
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		out = append(out, strings.Split(ln, ":"))
	}
	return out
}

// ringBuf keeps the last max bytes written.
type ringBuf struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (r *ringBuf) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.b = append(r.b, p...)
	if len(r.b) > r.max {
		r.b = append([]byte(nil), r.b[len(r.b)-r.max:]...)
	}
	return len(p), nil
}

func (r *ringBuf) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := string(r.b)
	if len(s) > 8<<10 {
		s = s[len(s)-8<<10:]
	}
	return s
}
