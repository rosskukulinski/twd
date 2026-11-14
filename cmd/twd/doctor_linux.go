//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Linux host checks for twd doctor (LINUX-PLAN DoD 6). Each reads the
// kernel or the filesystem directly; none needs root, none changes the
// machine. The microvm_tier line sums up whether Firecracker microVMs can
// run here (KVM, the release's guest assets, the twd-vmhelper service from
// `sudo twd host setup --microvm`) and why not.

// doctorPlatform runs before the workerd check: on Linux, the libc.
func doctorPlatform(run func(string, func() (string, string, string))) {
	run("glibc", doctorGlibc)
}

func doctorGlibc() (string, string, string) {
	// twd supports glibc >= minGlibc (what releases are tested on). The
	// loader's own floor (the highest GLIBC_ version workerd imports) is
	// usually lower; both are reported.
	need := minGlibc
	src := "twd's supported minimum"
	if wd, err := findWorkerd(""); err == nil {
		if elfNeed, ok := workerdGlibcNeed(wd); ok {
			src += "; workerd's ELF imports up to GLIBC_" + verString(elfNeed)
			if verLess(need, elfNeed) {
				need = elfNeed
			}
		}
	}
	distro := osRelease()
	on := ""
	if distro != "" {
		on = " on " + distro
	}
	if ml := muslLoader(); ml != "" {
		if _, err := exec.Command("getconf", "GNU_LIBC_VERSION").Output(); err != nil {
			return "fail", "this system uses musl libc (" + ml + ")" + on + "; twd's workerd is built for glibc and needs glibc >= " + verString(need) + " (" + src + "), so it can't run here",
				"use a glibc distro (Ubuntu 22.04+, Debian 12+, Fedora 36+, RHEL 10), or run twd in a glibc VM/container; Alpine's gcompat is not enough for workerd"
		}
	}
	out, err := exec.Command("getconf", "GNU_LIBC_VERSION").Output()
	f := strings.Fields(string(out))
	var have [2]int
	ok := false
	if err == nil && len(f) == 2 && f[0] == "glibc" {
		have, ok = parseVer(f[1])
	}
	if !ok {
		// getconf missing: ldd --version prints "ldd (… GLIBC 2.39-…) 2.39".
		if o, err := exec.Command("ldd", "--version").CombinedOutput(); err == nil {
			ln := firstLine(string(o))
			if i := strings.LastIndex(ln, " "); i > 0 && strings.Contains(strings.ToLower(ln), "libc") {
				have, ok = parseVer(ln[i+1:])
			}
		}
	}
	if !ok {
		return "fail", "can't tell which libc this system has (no `getconf GNU_LIBC_VERSION`, no glibc `ldd`)" + on + "; workerd needs glibc >= " + verString(need),
			"use a glibc distro (Ubuntu 22.04+, Debian 12+, Fedora 36+)"
	}
	if verLess(have, need) {
		return "fail", "glibc " + verString(have) + on + " is too old: twd's workerd needs glibc >= " + verString(need) + " (" + src + ")",
			"upgrade to a distro with glibc >= " + verString(need) + " (Ubuntu 22.04+, Debian 12+, Fedora 36+, RHEL 10); Debian 11, RHEL/Alma/Rocky 8–9 and Amazon Linux 2 are too old"
	}
	return "ok", "glibc " + verString(have) + on + " (workerd needs >= " + verString(need) + ", " + src + ")", ""
}

// doctorLinuxHost runs the kernel, Landlock, KVM, cgroups and systemd
// checks, then the microvm_tier summary.
func doctorLinuxHost(run func(string, func() (string, string, string)), s *state) {
	var uts unix.Utsname
	unix.Uname(&uts)
	rel := unix.ByteSliceToString(uts.Release[:])
	kv, _ := parseVer(rel)
	run("kernel", func() (string, string, string) {
		d := "Linux " + rel + " " + unix.ByteSliceToString(uts.Machine[:])
		if verLess(kv, [2]int{5, 13}) {
			return "warn", d + ": older than 5.13, so no Landlock (the process-tier sandbox needs it)", "use a distro kernel >= 5.13 (Ubuntu 22.04+, Debian 12+)"
		}
		return "ok", d, ""
	})

	abi, llErr := landlockABI()
	run("landlock", func() (string, string, string) {
		const missing = "the process tier's sandbox (Landlock + seccomp) needs it: without it process-tier tenants are not confined from each other, and twd hosts one tenant only (see the sandbox check)"
		switch {
		case llErr == nil && abi >= 4:
			return "ok", fmt.Sprintf("Landlock ABI %d (landlock_create_ruleset VERSION): filesystem and TCP rules for the process-tier sandbox (enforcement: the sandbox check)", abi), ""
		case llErr == nil && abi >= 1:
			return "ok", fmt.Sprintf("Landlock ABI %d (landlock_create_ruleset VERSION): filesystem rules only; TCP needs ABI >= 4 (kernel 6.7+), so workloads' TCP is restricted by seccomp's socket filter alone", abi), ""
		case errors.Is(llErr, unix.ENOSYS):
			return "warn", "Landlock is not in this kernel (landlock_create_ruleset: ENOSYS); " + missing, "use a kernel >= 5.13 built with CONFIG_SECURITY_LANDLOCK=y"
		case errors.Is(llErr, unix.EOPNOTSUPP):
			return "warn", "Landlock is built in but disabled at boot (landlock_create_ruleset: EOPNOTSUPP); " + missing,
				"add landlock to the kernel's lsm= list (e.g. lsm=landlock,lockdown,yama,integrity,apparmor) and reboot"
		default:
			return "warn", fmt.Sprintf("Landlock ABI unknown (%v); %s", llErr, missing), ""
		}
	})

	run("user_namespaces", func() (string, string, string) {
		var notes []string
		st := "ok"
		if b, err := os.ReadFile("/proc/sys/user/max_user_namespaces"); err == nil && strings.TrimSpace(string(b)) == "0" {
			st = "warn"
			notes = append(notes, "user namespaces are disabled (user.max_user_namespaces=0)")
		}
		if b, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); err == nil && strings.TrimSpace(string(b)) == "1" {
			notes = append(notes, "AppArmor restricts unprivileged user namespaces (Ubuntu 24.04 default)")
		}
		if b, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil && strings.TrimSpace(string(b)) == "0" {
			notes = append(notes, "unprivileged user namespaces are off (kernel.unprivileged_userns_clone=0)")
		}
		if len(notes) == 0 {
			notes = append(notes, "unprivileged user namespaces available")
		}
		return st, strings.Join(notes, "; ") + "; twd doesn't need them (Landlock is unprivileged)", ""
	})

	kvmOK, kvmDetail, kvmFix := kvmAccess()
	run("kvm", func() (string, string, string) {
		if kvmOK {
			return "ok", kvmDetail, ""
		}
		return "skip", kvmDetail, kvmFix
	})

	// Firecracker + jailer: `twd host setup --microvm` installs root-owned
	// copies in /usr/local/libexec/twd (what the helper runs); the linux
	// release ships them in share/twd/vm.
	share := filepath.Join(filepath.Dir(exeDir()), "share", "twd", "vm")
	fc := findFirst("firecracker", "/usr/local/libexec/twd/firecracker", filepath.Join(share, "firecracker"), "/usr/local/bin/firecracker")
	jl := findFirst("jailer", "/usr/local/libexec/twd/jailer", filepath.Join(share, "jailer"), "/usr/local/bin/jailer")
	run("firecracker", func() (string, string, string) {
		if fc == "" {
			return "skip", "firecracker not found (PATH, ~/.twd/bin, /usr/local/libexec/twd, the release's share/twd/vm, /usr/local/bin); only needed for the microVM tier", "install a linux release built with the microVM assets (make vm-assets), then sudo twd host setup --microvm"
		}
		d := fc + " " + cmdVersion1(fc)
		if jl == "" {
			return "warn", d + "; jailer not found (the microVM tier runs every VM under it)", "install jailer from the same Firecracker release (the release's share/twd/vm has it)"
		}
		return "ok", d + "; jailer " + jl, ""
	})

	cg2, cgDetail := cgroupsV2()
	run("cgroups_v2", func() (string, string, string) {
		if !cg2 {
			return "warn", cgDetail, "boot with systemd.unified_cgroup_hierarchy=1 (cgroups v2 is the default on current distros)"
		}
		return "ok", cgDetail, ""
	})

	sdOK, sdDetail, sdFix := systemdUser()
	run("systemd", func() (string, string, string) {
		if !sdOK {
			return "warn", sdDetail, sdFix
		}
		return "ok", sdDetail, sdFix
	})

	// microvm_tier: the L3 summary. KVM, the release's guest kernel + image,
	// and the twd-vmhelper service answering this uid (the helper opens
	// /dev/kvm and runs the jailer, so the user needs neither kvm access nor
	// root). While twd runs, that twd-hostd offers the tier and every
	// microVM workload is jailed. A warn, never a failure: microvm
	// deployments are refused (tier_unavailable), never run in the process tier.
	run("microvm_tier", func() (string, string, string) {
		st, d, fix := doctorMicroVM(s)
		if !cg2 {
			return "warn", "no cgroups v2 (the jailer needs it for per-VM cpu and memory limits); " + d, fix
		}
		return st, d, fix
	})
}

// landlockABI asks the kernel for its Landlock ABI version:
// landlock_create_ruleset(NULL, 0, LANDLOCK_CREATE_RULESET_VERSION).
func landlockABI() (int, error) {
	const createRulesetVersion = 1 << 0
	r, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, createRulesetVersion)
	if e != 0 {
		return 0, e
	}
	return int(r), nil
}

// kvmAccess opens /dev/kvm read-write (what Firecracker does) and asks for
// the KVM API version.
func kvmAccess() (bool, string, string) {
	fi, err := os.Stat("/dev/kvm")
	if err != nil {
		return false, "/dev/kvm not present (no hardware virtualization exposed; in a VM, enable nested virtualization); only needed for the microVM tier",
			"bare metal, or a VM with nested virtualization (Lima: vmType vz + nestedVirtualization on M3+ Macs)"
	}
	fd, err := unix.Open("/dev/kvm", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		grp := ""
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			if g, err := user.LookupGroupId(strconv.Itoa(int(st.Gid))); err == nil {
				grp = g.Name
			}
		}
		fix := "add yourself to the device's group and log in again: sudo usermod -aG " + grp + " $USER"
		if grp == "" {
			fix = "give your user read-write access to /dev/kvm (usually the kvm group)"
		}
		return false, "/dev/kvm exists but this user can't open it read-write (" + err.Error() + ", group " + grp + ")", fix
	}
	defer unix.Close(fd)
	const kvmGetAPIVersion = 0xAE00
	v, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), kvmGetAPIVersion, 0)
	if e != 0 {
		return false, "/dev/kvm opens but KVM_GET_API_VERSION failed: " + e.Error(), ""
	}
	return true, fmt.Sprintf("/dev/kvm read-write, KVM API %d", v), ""
}

func cgroupsV2() (bool, string) {
	b, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		var st unix.Statfs_t
		if unix.Statfs("/sys/fs/cgroup", &st) == nil && st.Type == unix.TMPFS_MAGIC {
			return false, "cgroups v1 (or hybrid): /sys/fs/cgroup is not the unified hierarchy"
		}
		return false, "no cgroups v2 at /sys/fs/cgroup"
	}
	d := "unified hierarchy, controllers: " + strings.TrimSpace(string(b))
	uid := os.Getuid()
	deleg := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/cgroup.controllers", uid, uid)
	if b, err := os.ReadFile(deleg); err == nil {
		d += "; delegated to your systemd user manager: " + strings.TrimSpace(string(b))
	}
	return true, d
}

// systemdUser: is systemd PID 1, is this user's manager reachable (what
// `twd up --service` needs), and is lingering on (survives logout/reboot)?
func systemdUser() (bool, string, string) {
	if !isDir("/run/systemd/system") {
		return false, "systemd is not the init system here, so `twd up --service` is unavailable (use `twd up`, or your init's own service)", ""
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false, "systemd runs but systemctl is not on PATH", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, err := exec.CommandContext(ctx, "systemctl", "--user", "is-system-running").Output()
	st := strings.TrimSpace(string(state))
	if st == "" {
		return false, "systemd is PID 1, but your user manager isn't reachable (systemctl --user: " + errString(err) + "); `twd up --service` needs it",
			"log in through a real session (ssh/console, not `su`), or check XDG_RUNTIME_DIR=/run/user/" + strconv.Itoa(os.Getuid())
	}
	u, _ := user.Current()
	name := ""
	if u != nil {
		name = u.Username
	}
	linger := "unknown"
	if name != "" {
		if isFile("/var/lib/systemd/linger/" + name) {
			linger = "yes"
		} else {
			linger = "no"
		}
	}
	d := "systemd user manager " + st + "; `twd up --service` installs ~/.config/systemd/user/twd.service; linger " + linger
	if svc := filepath.Join(userUnitDir(), serviceUnit); isFile(svc) {
		a, _ := exec.CommandContext(ctx, "systemctl", "--user", "is-active", serviceUnit).Output()
		d += "; " + serviceUnit + " installed (" + strings.TrimSpace(string(a)) + ")"
	}
	if linger != "yes" {
		return true, d + " (twd stops when you log out and doesn't start at boot)", "sudo loginctl enable-linger " + name + "   (run it yourself; twd never uses sudo)"
	}
	return true, d, ""
}

func errString(err error) string {
	if err == nil {
		return "no output"
	}
	return err.Error()
}

func isDir(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

// findFirst finds a helper binary on PATH, next to twd, or at fallback.
func findFirst(name string, fallbacks ...string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if p := filepath.Join(exeDir(), name); isFile(p) {
		return p
	}
	for _, p := range fallbacks {
		if isFile(p) {
			return p
		}
	}
	return ""
}

func cmdVersion1(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	return firstLine(string(out))
}

// tailscaledHolders reports open files of pids that belong to the system
// tailscaled: its LocalAPI socket (a unix-socket connection whose peer is
// bound to tailscaled.sock, found with `ss -x` by peer inode) or its state
// dir. It never connects to tailscaled.
func tailscaledHolders(pids []int) (bad []string, method string) {
	inodes := map[string]int{}
	for _, p := range pids {
		fds, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", p))
		for _, fd := range fds {
			l, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", p, fd.Name()))
			if err != nil {
				continue
			}
			if ino, ok := strings.CutPrefix(l, "socket:["); ok {
				inodes[strings.TrimSuffix(ino, "]")] = p
				continue
			}
			if strings.Contains(l, "/var/lib/tailscale") || strings.Contains(l, "tailscaled.state") || strings.Contains(l, "/run/tailscale") {
				bad = append(bad, fmt.Sprintf("pid %d has %s open", p, l))
			}
		}
	}
	method = "/proc/<pid>/fd"
	// Our own sockets' bound paths (/proc/net/unix: inode is column 7, path 8).
	if b, err := os.ReadFile("/proc/net/unix"); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) >= 8 && strings.Contains(f[7], "tailscale") {
				if p, ok := inodes[f[6]]; ok {
					bad = append(bad, fmt.Sprintf("pid %d has a socket on %s", p, f[7]))
				}
			}
		}
	}
	// Connections: the server end carries the path, the peer inode is ours.
	if ss, err := exec.LookPath("ss"); err == nil {
		out, err := exec.Command(ss, "-xaH").Output()
		if err == nil {
			method += " + ss -x (peer inodes)"
			for _, ln := range strings.Split(string(out), "\n") {
				f := strings.Fields(ln)
				// Netid State Recv-Q Send-Q Local Port Peer Port
				if len(f) >= 8 && strings.Contains(f[4], "tailscale") {
					if p, ok := inodes[f[7]]; ok {
						bad = append(bad, fmt.Sprintf("pid %d is connected to %s", p, f[4]))
					}
				}
			}
		}
	}
	return bad, method
}

// systemTailscaled says whether a system tailscaled is present (left alone).
func systemTailscaled() bool {
	return isSocket("/run/tailscale/tailscaled.sock") || isSocket("/var/run/tailscale/tailscaled.sock") || lookPathOK("tailscaled")
}

func isSocket(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}
