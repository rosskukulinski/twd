//go:build linux

package procsb

import (
	"bufio"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	llsys "github.com/landlock-lsm/go-landlock/landlock/syscall"
	"golang.org/x/sys/unix"
)

// ABI is the kernel's Landlock ABI version (0 and an error if Landlock is
// unavailable: kernel without it, or not in the boot "lsm=" list).
func ABI() (int, error) {
	v, err := llsys.LandlockGetABIVersion()
	if err != nil {
		return 0, err
	}
	return v, nil
}

// Supported reports whether this OS can run the sandbox at all.
func Supported() bool { v, _ := ABI(); return v >= 1 }

// seccompState reads NoNewPrivs and the number of seccomp filters of pid.
func seccompState(pid string) (nnp bool, filters int, err error) {
	f, err := os.Open("/proc/" + pid + "/status")
	if err != nil {
		return false, 0, err
	}
	defer f.Close()
	mode, sawCount := 0, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "NoNewPrivs":
			nnp = v == "1"
		case "Seccomp":
			mode, _ = strconv.Atoi(v)
		case "Seccomp_filters":
			filters, _ = strconv.Atoi(v)
			sawCount = true
		}
	}
	if !sawCount && mode == 2 { // kernels < 5.9 don't count filters
		filters = 1
	}
	return nnp, filters, sc.Err()
}

var selfFilters = sync.OnceValue(func() int {
	_, n, _ := seccompState("self")
	return n
})

// Sandboxed reports whether pid runs under a procsb sandbox: it has
// no_new_privs and more seccomp filters than this process (the launcher
// adds one, and filters can't be removed). Unknown pids count as
// sandboxed (fail closed).
func Sandboxed(pid int) bool {
	nnp, n, err := seccompState(strconv.Itoa(pid))
	if err != nil {
		return true
	}
	return nnp && n > selfFilters()
}

// Descends reports whether pid is root or one of its descendants.
func Descends(pid, root int) bool {
	for i := 0; i < 128 && pid > 1; i++ {
		if pid == root {
			return true
		}
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return false
		}
		// pid (comm) state ppid …; comm may contain spaces and parens.
		s := string(b)
		r := strings.LastIndexByte(s, ')')
		if r < 0 {
			return false
		}
		f := strings.Fields(s[r+1:])
		if len(f) < 2 {
			return false
		}
		pid, _ = strconv.Atoi(f[1])
	}
	return false
}

// PeerPID is the pid of a unix-socket connection's peer (at connect time).
func PeerPID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a unix socket")
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var serr error
	if err := rc.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	return int(cred.Pid), nil
}

// Guard wraps a unix-socket listener: connections from sandboxed processes
// are closed unless allow(pid) accepts them (allow nil: refuse every
// sandboxed peer). Unsandboxed peers pass (the uid check, if any, is the
// wrapped listener's).
//
// This is what keeps a sandboxed workload away from twd's sockets on
// kernels whose Landlock can't restrict unix-socket connects (ABI < 9).
func Guard(ln net.Listener, allow func(pid int) bool) net.Listener {
	return &guard{Listener: ln, allow: allow}
}

type guard struct {
	net.Listener
	allow func(int) bool
}

func (g *guard) Accept() (net.Conn, error) {
	for {
		c, err := g.Listener.Accept()
		if err != nil {
			return nil, err
		}
		pid, err := PeerPID(c)
		if err != nil {
			// Not a unix socket (or no credentials): not ours to judge.
			if _, isUnix := c.(*net.UnixConn); !isUnix {
				return c, nil
			}
			c.Close()
			continue
		}
		if !Sandboxed(pid) || (g.allow != nil && g.allow(pid)) {
			return c, nil
		}
		c.Close()
	}
}
