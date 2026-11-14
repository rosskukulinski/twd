//go:build linux

package main

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// seccompProbe reports what each guarded syscall does in this process.
func seccompProbe() string {
	var out []string
	try := func(name string, f func() error) {
		err := f()
		switch {
		case err == nil:
			out = append(out, name+"=ok")
		case err == unix.EPERM:
			out = append(out, name+"=EPERM")
		default:
			out = append(out, name+"="+err.Error())
		}
	}
	sock := func(dom, typ int) func() error {
		return func() error {
			fd, err := unix.Socket(dom, typ, 0)
			if err == nil {
				unix.Close(fd)
			}
			return err
		}
	}
	try("vsock", sock(unix.AF_VSOCK, unix.SOCK_STREAM))
	try("vsock-dgram", sock(unix.AF_VSOCK, unix.SOCK_DGRAM))
	try("inet", sock(unix.AF_INET, unix.SOCK_STREAM))
	try("unix", sock(unix.AF_UNIX, unix.SOCK_STREAM))
	try("socketpair-unix", func() error {
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err == nil {
			unix.Close(fds[0])
			unix.Close(fds[1])
		}
		return err
	})
	try("socketpair-vsock", func() error {
		_, err := unix.Socketpair(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
		return err
	})
	try("io_uring_setup", func() error {
		var params [120]byte
		r, _, e := unix.Syscall(unix.SYS_IO_URING_SETUP, 1, uintptr(unsafe.Pointer(&params[0])), 0)
		if e != 0 {
			return e
		}
		unix.Close(int(r))
		return nil
	})
	return strings.Join(out, " ")
}

func init() {
	switch os.Getenv("TWD_SECCOMP_TEST") {
	case "install":
		// As ctr-exec does (but unprivileged: no_new_privs stands in for
		// CAP_SYS_ADMIN), then exec a fresh copy that probes.
		runtime.LockOSThread()
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			os.Stdout.WriteString("nnp: " + err.Error())
			os.Exit(1)
		}
		if err := installCtrSeccomp(); err != nil {
			os.Stdout.WriteString(err.Error())
			os.Exit(1)
		}
		os.Setenv("TWD_SECCOMP_TEST", "probe")
		err := syscall.Exec("/proc/self/exe", os.Args[:1], os.Environ())
		os.Stdout.WriteString("exec: " + err.Error())
		os.Exit(1)
	case "probe":
		os.Stdout.WriteString(seccompProbe())
		os.Exit(0)
	}
}

// TestCtrSeccomp: a process under the container filter (after an exec,
// as the container's target is) can't make an AF_VSOCK socket or an
// io_uring, and everything else about sockets works.
func TestCtrSeccomp(t *testing.T) {
	cmd := exec.Command("/proc/self/exe", "-test.run=^$")
	cmd.Env = append(os.Environ(), "TWD_SECCOMP_TEST=install")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("filtered child: %v: %s", err, out)
	}
	got := string(out)
	t.Logf("under the filter: %s", got)
	for _, want := range []string{"vsock=EPERM", "vsock-dgram=EPERM", "socketpair-vsock=EPERM", "io_uring_setup=EPERM",
		"inet=ok", "unix=ok", "socketpair-unix=ok"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s", want)
		}
	}
	// Without the filter, the same probe gets past seccomp (whatever the
	// kernel then says), so the EPERMs above are the filter's.
	if un := seccompProbe(); strings.Contains(un, "vsock=EPERM") {
		t.Logf("unfiltered probe: %s (vsock EPERM without the filter?)", un)
	}
}

// TestCtrSeccompFilterArches: the filter assembles for every guest arch.
func TestCtrSeccompFilterArches(t *testing.T) {
	for _, a := range []string{"amd64", "arm64"} {
		f, err := ctrSeccompFilter(a)
		if err != nil || len(f) < 10 {
			t.Fatalf("%s: %d instructions, %v", a, len(f), err)
		}
	}
	if _, err := ctrSeccompFilter("riscv64"); err == nil {
		t.Fatal("an unknown arch got a filter")
	}
}
