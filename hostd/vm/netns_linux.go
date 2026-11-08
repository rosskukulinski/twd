//go:build linux

package vm

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Each VM gets its own network namespace holding only:
//
//	fc0 (TAP, opened by Firecracker) ─┐
//	                                  br0 (bridge, no IP)
//	gv0 (TAP, opened by hostd)  ──────┘   └─ hostd's gVisor stack is the gateway
//
// The namespace has no address, no route and no listener of the host
// kernel's, and IPv6 is disabled in it: even a frame the guest aims at the
// kernel finds nothing. The guest's only peer is hostd's userspace stack.
// (tailvisor-bridged uses the same TAP-on-a-bridge shape; the namespace is
// twd's addition, so the bridge can never leak to the host network.)

func ipCmd(args ...string) error {
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func setupNetns(ns string, mtu int) error {
	if err := ipCmd("netns", "add", ns); err != nil {
		return err
	}
	steps := [][]string{
		{"netns", "exec", ns, "sysctl", "-qw", "net.ipv6.conf.all.disable_ipv6=1", "net.ipv6.conf.default.disable_ipv6=1", "net.ipv4.ip_forward=0"},
		{"-n", ns, "link", "add", "br0", "type", "bridge", "stp_state", "0", "forward_delay", "0"},
		{"-n", ns, "tuntap", "add", "dev", "fc0", "mode", "tap"},
		{"-n", ns, "link", "set", "fc0", "mtu", fmt.Sprint(mtu), "master", "br0", "up"},
		{"-n", ns, "link", "set", "br0", "mtu", fmt.Sprint(mtu), "up"},
	}
	for _, s := range steps {
		if err := ipCmd(s...); err != nil {
			teardownNetns(ns)
			return err
		}
	}
	return nil
}

// openTapInNetns creates TAP name inside ns (non-persistent: it disappears
// with the fd), attaches it to br0 and returns the fd.
func openTapInNetns(ns, name string, mtu int) (int, error) {
	type res struct {
		fd  int
		err error
	}
	ch := make(chan res, 1)
	go func() {
		// This thread switches namespaces; it is never handed back to the
		// scheduler (no UnlockOSThread), so the Go runtime retires it.
		runtime.LockOSThread()
		fd, err := func() (int, error) {
			nsf, err := os.Open("/run/netns/" + ns)
			if err != nil {
				return -1, err
			}
			defer nsf.Close()
			if err := unix.Unshare(unix.CLONE_FS); err != nil {
				return -1, err
			}
			if err := unix.Setns(int(nsf.Fd()), unix.CLONE_NEWNET); err != nil {
				return -1, fmt.Errorf("setns: %w", err)
			}
			fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
			if err != nil {
				return -1, err
			}
			var ifr [unix.IFNAMSIZ + 24]byte
			copy(ifr[:unix.IFNAMSIZ-1], name)
			binary.LittleEndian.PutUint16(ifr[unix.IFNAMSIZ:], uint16(unix.IFF_TAP|unix.IFF_NO_PI))
			if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&ifr[0]))); errno != 0 {
				unix.Close(fd)
				return -1, fmt.Errorf("TUNSETIFF %s: %w", name, errno)
			}
			return fd, nil
		}()
		ch <- res{fd, err}
	}()
	r := <-ch
	if r.err != nil {
		return -1, r.err
	}
	if err := ipCmd("-n", ns, "link", "set", name, "mtu", fmt.Sprint(mtu), "master", "br0", "up"); err != nil {
		unix.Close(r.fd)
		return -1, err
	}
	return r.fd, nil
}

func teardownNetns(ns string) {
	exec.Command("ip", "netns", "del", ns).Run()
}

// procRSSKB is pid's resident memory in kB: VmRSS plus hugetlb pages
// (guest memory backed by huge pages is not counted in VmRSS).
func procRSSKB(pid int) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	var total int64
	for _, ln := range strings.Split(string(b), "\n") {
		for _, k := range []string{"VmRSS:", "HugetlbPages:"} {
			if strings.HasPrefix(ln, k) {
				var n int64
				fmt.Sscan(strings.TrimSpace(strings.TrimPrefix(ln, k)), &n)
				total += n
			}
		}
	}
	return total
}

// FreeHugePages2M returns the host's free 2 MiB hugetlbfs pages.
func FreeHugePages2M() int {
	b, err := os.ReadFile("/sys/kernel/mm/hugepages/hugepages-2048kB/free_hugepages")
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscan(strings.TrimSpace(string(b)), &n)
	return n
}
