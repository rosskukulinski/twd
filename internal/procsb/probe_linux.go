//go:build linux

package procsb

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// probe performs one operation under the already-applied sandbox.
func probe(op string, args []string) error {
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	switch op {
	case ProbeRead:
		f, err := os.Open(arg(0))
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Read(make([]byte, 1))
		if err != nil && err.Error() == "EOF" {
			err = nil
		}
		return err
	case ProbeList:
		_, err := os.ReadDir(arg(0))
		return err
	case ProbeWrite:
		return os.WriteFile(arg(0), []byte("probe"), 0o600)
	case ProbeExec:
		return exec.Command(arg(0), "--version").Run()
	case ProbeConnectUnix:
		c, err := net.DialTimeout("unix", arg(0), 3*time.Second)
		if err != nil {
			return err
		}
		defer c.Close()
		// A server that refuses sandboxed peers accepts, then closes:
		// a request must get an answer to count as connected.
		c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte("GET /v1/status HTTP/1.0\r\n\r\n")); err != nil {
			return err
		}
		b := make([]byte, 1)
		if _, err := c.Read(b); err != nil {
			return fmt.Errorf("connected but refused: %w", syscall.ECONNRESET)
		}
		return nil
	case ProbeConnectTCP:
		c, err := (&net.Dialer{Timeout: 3 * time.Second}).Dial("tcp", arg(0))
		if err != nil {
			return err
		}
		return c.Close()
	case ProbeBindTCP:
		l, err := net.Listen("tcp", arg(0))
		if err != nil {
			return err
		}
		return l.Close()
	case ProbeUDP:
		c, err := net.Dial("udp", arg(0))
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = c.Write([]byte("probe"))
		return err
	case ProbePtrace:
		pid, _ := strconv.Atoi(arg(0))
		if err := unix.PtraceAttach(pid); err != nil {
			return err
		}
		unix.PtraceDetach(pid)
		return nil
	case ProbeVMRead:
		pid, _ := strconv.Atoi(arg(0))
		var b [8]byte
		local := []unix.Iovec{{Base: &b[0], Len: 8}}
		remote := []unix.RemoteIovec{{Base: uintptr(unsafe.Pointer(&b[0])), Len: 8}}
		_, err := unix.ProcessVMReadv(pid, local, remote, 0)
		return err
	case ProbeTgkill:
		pid, _ := strconv.Atoi(arg(0))
		return unix.Tgkill(pid, pid, 0)
	case ProbeSocketpair:
		ty := map[string]int{"stream": unix.SOCK_STREAM, "seqpacket": unix.SOCK_SEQPACKET, "dgram": unix.SOCK_DGRAM}[arg(0)]
		fds, err := unix.Socketpair(unix.AF_UNIX, ty, 0)
		if err != nil {
			return err
		}
		unix.Close(fds[0])
		unix.Close(fds[1])
		return nil
	case ProbeSignal:
		if arg(0) == "self" {
			return syscall.Kill(os.Getpid(), 0)
		}
		pid, _ := strconv.Atoi(arg(0))
		if pid == -1 {
			// Raw syscall: never signal everything if the filter failed.
			_, _, e := unix.RawSyscall(unix.SYS_KILL, ^uintptr(0), 0, 0)
			if e != 0 {
				return e
			}
			return nil
		}
		return syscall.Kill(pid, 0)
	}
	return errors.New("unknown probe op " + op)
}

func errnoName(e syscall.Errno) string {
	if n := unix.ErrnoName(e); n != "" {
		return n
	}
	return e.Error()
}
