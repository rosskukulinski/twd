//go:build linux

package procsb

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	llsys "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// The launcher runs from package init, before main or testing.Main, in any
// binary that imports procsb (twd-hostd, twd, hostd's test binaries).
func init() {
	if len(os.Args) > 1 && os.Args[1] == ExecArg {
		os.Exit(launcherMain(os.Args[2:]))
	}
}

// systemRO are read-and-execute everywhere (if present): the OS, its
// libraries, CA certificates, time zones. They hold nothing of twd's.
var systemRO = []string{"/usr", "/lib", "/lib64", "/lib32", "/bin", "/sbin", "/etc", "/opt", "/nix/store", "/run/current-system"}

// procRO and sysRO are the global /proc and /sys files runtimes read (CPU
// count, memory, cgroup limits). Not /proc itself: other processes' cmdline,
// environ (the Node tier's caller key) and /proc/net/unix (every socket's
// path) stay unreadable.
var procRO = []string{"/proc/cpuinfo", "/proc/meminfo", "/proc/stat", "/proc/loadavg", "/proc/uptime",
	"/proc/version", "/proc/version_signature", "/proc/filesystems", "/proc/sys/kernel", "/proc/sys/vm", "/proc/sys/fs",
	"/proc/sys/net/core/somaxconn"}
var sysRO = []string{"/sys/devices/system/cpu", "/sys/fs/cgroup"}

// selfProc are the process's own /proc entries (by pid: exec keeps it).
// Not task/ or net/ (net/unix lists every unix socket's path).
var selfProc = []string{"maps", "smaps", "smaps_rollup", "stat", "statm", "status", "cgroup", "limits", "mountinfo",
	"mounts", "auxv", "cmdline", "comm", "environ", "io", "fd", "fdinfo", "oom_score_adj"}

// Rules builds the Landlock rules for p, for the process with pid.
func (p *Profile) Rules(pid int) []landlock.Rule {
	ro := func(paths []string) []landlock.Rule {
		var rs []landlock.Rule
		for _, x := range paths {
			st, err := os.Stat(x)
			if err != nil {
				continue
			}
			if st.IsDir() {
				rs = append(rs, landlock.RODirs(x))
			} else {
				rs = append(rs, landlock.ROFiles(x))
			}
		}
		return rs
	}
	var rs []landlock.Rule
	rs = append(rs, ro(systemRO)...)
	rs = append(rs, ro(p.Exec)...)
	rs = append(rs, ro(p.ReadOnly)...)
	rs = append(rs, ro(procRO)...)
	rs = append(rs, ro(sysRO)...)
	var self []string
	for _, e := range selfProc {
		self = append(self, "/proc/"+strconv.Itoa(pid)+"/"+e)
	}
	rs = append(rs, ro(self)...)
	rs = append(rs,
		landlock.RWFiles("/dev/null", "/dev/zero", "/dev/full").IgnoreIfMissing(),
		landlock.ROFiles("/dev/urandom", "/dev/random").IgnoreIfMissing(),
	)
	if len(p.Own) > 0 {
		rs = append(rs, landlock.RWDirs(p.Own...).WithRefer().WithResolveUnix())
	}
	for _, port := range p.ConnectTCP {
		rs = append(rs, landlock.ConnectTCP(port))
	}
	for _, port := range p.BindTCP {
		rs = append(rs, landlock.BindTCP(port))
	}
	return rs
}

// Apply restricts the calling process (all threads) to p: Landlock (best
// effort up to the newest ABI go-landlock knows), then no_new_privs and
// the seccomp filter. It fails if the kernel has no Landlock at all.
func (p *Profile) Apply() error {
	abi, err := llsys.LandlockGetABIVersion()
	if err != nil || abi < 1 {
		return fmt.Errorf("landlock unavailable: %v", err)
	}
	for _, o := range p.Own {
		if err := os.MkdirAll(o, 0o700); err != nil {
			return err
		}
	}
	if err := landlock.V11.BestEffort().Restrict(p.Rules(os.Getpid())...); err != nil {
		return fmt.Errorf("landlock: %w", err)
	}
	return installSeccomp()
}

func launcherMain(args []string) int {
	runtime.LockOSThread()
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "twd sandbox: "+format+"\n", a...)
		return 126
	}
	if len(args) < 3 || args[0] != "-f" {
		return fail("usage: %s -f <profile.json> (-- <bin> [args…] | --probe <op> [args…])", ExecArg)
	}
	p, err := Load(args[1])
	if err != nil {
		return fail("%v", err)
	}
	switch args[2] {
	case "--":
		if len(args) < 4 {
			return fail("no command")
		}
		bin, err := exec.LookPath(args[3])
		if err != nil {
			return fail("%v", err)
		}
		if bin, err = filepath.Abs(bin); err != nil {
			return fail("%v", err)
		}
		if err := p.Apply(); err != nil {
			return fail("%v", err)
		}
		err = syscall.Exec(bin, args[3:], os.Environ())
		return fail("exec %s: %v", bin, err)
	case "--probe":
		if len(args) < 4 {
			return fail("no probe op")
		}
		if err := p.Apply(); err != nil {
			return fail("%v", err)
		}
		err := probe(args[3], args[4:])
		if err == nil {
			fmt.Println("ok")
			return 0
		}
		var en syscall.Errno
		if errors.As(err, &en) {
			fmt.Println(errnoName(en))
		} else {
			fmt.Println(err)
		}
		return 3
	}
	return fail("unknown mode %q", args[2])
}
