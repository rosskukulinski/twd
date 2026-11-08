//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/hostd/vm/helper"
	"github.com/rosskukulinski/twd/hostd/vm/helperapi"
)

// What `twd host setup --microvm` installs (and --undo removes). Nothing
// else on the system is changed.
const (
	setupLibexec = "/usr/local/libexec/twd"                   // root-owned copies: twd-vmhelper, firecracker, jailer
	setupEtc     = "/etc/twd"                                 // vmhelper.json (allowed uids), setup.json (what to undo)
	setupUnit    = "/etc/systemd/system/twd-vmhelper.service" // the helper service
	setupSysctl  = "/etc/sysctl.d/60-twd-microvm.conf"        // vm.nr_hugepages (guest RAM)
	setupState   = "/var/lib/twd-vm"                          // the helper's jails and volume epoch fence
	unitName     = "twd-vmhelper.service"
)

// setupRecord is /etc/twd/setup.json: what setup changed, for --undo.
type setupRecord struct {
	Users        []string `json:"users"`
	AddedToKVM   []string `json:"addedToKVM"`
	HugePages    int      `json:"hugePages"`
	PrevHugePage int      `json:"prevHugePages"`
	Version      string   `json:"version"`
	At           string   `json:"at"`
}

// The unit: root, but with a bounded capability set; Delegate= gives it a
// cgroup subtree for the per-VM cgroups (cpu.max, memory.max). No
// ProtectSystem=/PrivateTmp=: they put the service in its own mount
// namespace, and the jails' bind mounts of the files hostd passes (fds
// from the host's namespace) must be made in the host's.
const unitText = `[Unit]
Description=twd microVM helper (netns/TAP, Firecracker jailer, encrypted DO volumes) for twd-hostd
After=network.target systemd-udevd.service

[Service]
Type=simple
ExecStart=/usr/local/libexec/twd/twd-vmhelper -config /etc/twd/vmhelper.json -socket /run/twd/vmhelper.sock
Restart=on-failure
RestartSec=1
Delegate=yes
KillMode=mixed
TimeoutStopSec=20
RuntimeDirectory=twd
RuntimeDirectoryMode=0755
StateDirectory=twd-vm
StateDirectoryMode=0711
NoNewPrivileges=yes
CapabilityBoundingSet=CAP_NET_ADMIN CAP_SYS_ADMIN CAP_SYS_CHROOT CAP_SETUID CAP_SETGID CAP_CHOWN CAP_FOWNER CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_MKNOD CAP_KILL CAP_SYS_RESOURCE CAP_IPC_LOCK

[Install]
WantedBy=multi-user.target
`

func needRoot(what string) error {
	if os.Geteuid() != 0 {
		return fail("needs_root", what+" changes the system and must run as root",
			"it installs a systemd service and root-owned binaries; nothing else of twd needs root").
			withFix("sudo " + filepath.Join(exeDir(), "twd") + " host setup " + strings.TrimPrefix(what, "twd host setup "))
	}
	return nil
}

func hostSetupMicroVM(userName string, huge int, fcSrc, jlSrc string) (*setupResult, error) {
	if err := needRoot("twd host setup --microvm"); err != nil {
		return nil, err
	}
	if userName == "" {
		userName = os.Getenv("SUDO_USER")
	}
	if userName == "" || userName == "root" {
		return nil, fail("bad_request", "which user will run twd?", "run it with sudo as that user, or pass --user <name>")
	}
	u, err := user.Lookup(userName)
	if err != nil {
		return nil, fail("bad_request", "no user "+userName)
	}
	uid, _ := strconv.Atoi(u.Uid)
	res := &setupResult{Action: "setup", User: userName, UID: uid, Socket: helperapi.DefaultSocket, Unit: unitName}
	step := func(format string, a ...any) { res.Steps = append(res.Steps, "✓ "+fmt.Sprintf(format, a...)) }

	// Preconditions: KVM, cgroups v2, systemd, the tools the helper runs.
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return nil, fail("no_kvm", "/dev/kvm does not exist: this machine can't run microVMs",
			"enable virtualization (bare metal, or a VM with nested virtualization) and load kvm").withFix("the process tier works without it")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return nil, fail("no_cgroup2", "cgroups v2 (unified hierarchy) is required")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return nil, fail("no_systemd", "systemd is required to run the helper service")
	}
	var missing []string
	for _, b := range []string{"ip", "cryptsetup", "losetup", "mkfs.ext4", "systemctl", "usermod", "gpasswd"} {
		if _, err := exec.LookPath(b); err != nil {
			missing = append(missing, b)
		}
	}
	if len(missing) > 0 {
		return nil, fail("missing_tools", "not found: "+strings.Join(missing, ", ")).
			withFix("Debian/Ubuntu: apt-get install iproute2 cryptsetup-bin util-linux e2fsprogs", "Fedora: dnf install iproute cryptsetup util-linux e2fsprogs")
	}
	share := filepath.Join(filepath.Dir(exeDir()), "share", "twd", "vm")
	if fcSrc == "" {
		fcSrc = filepath.Join(share, "firecracker")
	}
	if jlSrc == "" {
		jlSrc = filepath.Join(share, "jailer")
	}
	hlSrc, err := findBin("twd-vmhelper")
	if err != nil {
		return nil, err
	}
	for _, p := range []string{fcSrc, jlSrc} {
		if !isFile(p) {
			return nil, fail("not_installed", p+" not found (the linux release ships Firecracker and the jailer in share/twd/vm)").
				withFix("pass --firecracker and --jailer")
		}
	}

	// 1. Root-owned copies of the binaries the helper runs as root (the
	// user's ~/.twd is writable by the user: never run root code from it).
	if err := os.MkdirAll(setupLibexec, 0o755); err != nil {
		return nil, err
	}
	os.Chmod(setupLibexec, 0o755)
	for name, src := range map[string]string{"twd-vmhelper": hlSrc, "firecracker": fcSrc, "jailer": jlSrc} {
		if err := installRootFile(src, filepath.Join(setupLibexec, name), 0o755); err != nil {
			return nil, err
		}
	}
	fv, _ := exec.Command(filepath.Join(setupLibexec, "firecracker"), "--version").Output()
	step("installed %s/{twd-vmhelper,firecracker,jailer} (root:root 0755; %s)", setupLibexec, strings.TrimSpace(strings.SplitN(string(fv), "\n", 2)[0]))

	// 2. The helper's config: who may use it.
	if err := os.MkdirAll(setupEtc, 0o755); err != nil {
		return nil, err
	}
	cfg := helper.DefaultConfig()
	if b, err := os.ReadFile(filepath.Join(setupEtc, "vmhelper.json")); err == nil {
		json.Unmarshal(b, &cfg)
	}
	if !slices.Contains(cfg.AllowedUIDs, uid) {
		cfg.AllowedUIDs = append(cfg.AllowedUIDs, uid)
	}
	cfg.Firecracker, cfg.Jailer = filepath.Join(setupLibexec, "firecracker"), filepath.Join(setupLibexec, "jailer")
	cfg.JailBase, cfg.StateDir = filepath.Join(setupState, "jail"), setupState
	if err := writeRootJSON(filepath.Join(setupEtc, "vmhelper.json"), cfg, 0o644); err != nil {
		return nil, err
	}
	step("wrote %s/vmhelper.json: uids %v may use the helper", setupEtc, cfg.AllowedUIDs)

	rec := setupRecord{}
	if b, err := os.ReadFile(filepath.Join(setupEtc, "setup.json")); err == nil {
		json.Unmarshal(b, &rec)
	} else {
		rec.PrevHugePage = readInt("/proc/sys/vm/nr_hugepages")
	}
	if !slices.Contains(rec.Users, userName) {
		rec.Users = append(rec.Users, userName)
	}

	// 3. kvm group (the helper opens /dev/kvm for the jail; membership
	// lets the user's own tools, and doctor, see KVM too).
	if inGroup(u, "kvm") {
		step("%s is already in the kvm group", userName)
	} else if _, err := runOut("usermod", "-aG", "kvm", userName); err != nil {
		return nil, fail("usermod_failed", err.Error())
	} else {
		rec.AddedToKVM = append(rec.AddedToKVM, userName)
		step("added %s to the kvm group (takes effect at the next login; twd doesn't depend on it)", userName)
	}

	// 4. Huge pages for guest RAM: decisive for boot time under nested
	// virtualization (STATUS M8: 3.7 s → 0.43 s to workerd ready).
	if huge < 0 {
		memMiB := readMemTotalKB() / 1024
		huge = min(4*512/2, int(memMiB/4/2)) // 4 VMs of 512 MiB, ≤ ¼ of RAM
	}
	if huge > 0 {
		cur := readInt("/proc/sys/vm/nr_hugepages")
		if err := os.WriteFile(setupSysctl, []byte(fmt.Sprintf("# twd microVM tier: 2 MiB pages for guest RAM (sudo twd host setup --undo removes this)\nvm.nr_hugepages = %d\n", max(huge, cur))), 0o644); err != nil {
			return nil, err
		}
		if cur < huge {
			os.WriteFile("/proc/sys/vm/nr_hugepages", []byte(strconv.Itoa(huge)), 0o644)
		}
		rec.HugePages = huge
		res.HugePages = readInt("/proc/sys/vm/nr_hugepages")
		step("reserved %d huge pages (2 MiB; %d now) in %s", huge, res.HugePages, setupSysctl)
	}
	exec.Command("modprobe", "dm_crypt").Run()

	// 5. The service.
	if err := os.WriteFile(setupUnit, []byte(unitText), 0o644); err != nil {
		return nil, err
	}
	rec.Version, rec.At = version, time.Now().UTC().Format(time.RFC3339)
	if err := writeRootJSON(filepath.Join(setupEtc, "setup.json"), rec, 0o644); err != nil {
		return nil, err
	}
	for _, a := range [][]string{{"daemon-reload"}, {"enable", unitName}, {"restart", unitName}} {
		if out, err := runOut("systemctl", a...); err != nil {
			return nil, fail("systemd_failed", "systemctl "+strings.Join(a, " ")+": "+err.Error(), out).withFix("journalctl -u " + unitName)
		}
	}
	var perr error
	for i := 0; i < 50; i++ {
		var r helperapi.Response
		if r, perr = helperapi.Ping(helperapi.DefaultSocket); perr == nil && r.Version == helperapi.Version {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if perr != nil {
		out, _ := runOut("journalctl", "-u", unitName, "-n", "20", "--no-pager")
		return nil, fail("helper_failed", "the helper service did not come up: "+perr.Error(), out).withFix("journalctl -u " + unitName)
	}
	step("installed and started %s (enabled at boot; socket %s, authorized by uid)", unitName, helperapi.DefaultSocket)
	return res, nil
}

func hostUndo() (*setupResult, error) {
	if err := needRoot("twd host setup --undo"); err != nil {
		return nil, err
	}
	res := &setupResult{Action: "undo", Unit: unitName}
	step := func(format string, a ...any) { res.Steps = append(res.Steps, "✓ "+fmt.Sprintf(format, a...)) }
	var rec setupRecord
	if b, err := os.ReadFile(filepath.Join(setupEtc, "setup.json")); err == nil {
		json.Unmarshal(b, &rec)
	}
	if isFile(setupUnit) {
		// Stopping the helper stops every microVM and releases its volume.
		runOut("systemctl", "disable", "--now", unitName)
		os.Remove(setupUnit)
		runOut("systemctl", "daemon-reload")
		step("stopped, disabled and removed %s (every microVM stopped; DO volumes stay in each user's ~/.twd)", unitName)
	}
	for _, p := range []string{setupLibexec, setupState} {
		if _, err := os.Stat(p); err == nil {
			os.RemoveAll(p)
			step("removed %s", p)
		}
	}
	if isFile(setupSysctl) {
		os.Remove(setupSysctl)
		if rec.HugePages > 0 {
			os.WriteFile("/proc/sys/vm/nr_hugepages", []byte(strconv.Itoa(rec.PrevHugePage)), 0o644)
		}
		step("removed %s (vm.nr_hugepages back to %d)", setupSysctl, readInt("/proc/sys/vm/nr_hugepages"))
	}
	for _, u := range rec.AddedToKVM {
		if _, err := runOut("gpasswd", "-d", u, "kvm"); err == nil {
			step("removed %s from the kvm group (setup had added it)", u)
		}
	}
	if _, err := os.Stat(setupEtc); err == nil {
		os.RemoveAll(setupEtc)
		step("removed %s", setupEtc)
	}
	if len(res.Steps) == 0 {
		res.Steps = append(res.Steps, "✓ nothing to undo (twd host setup was not run here)")
	}
	return res, nil
}

func installRootFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chown(tmp, 0, 0); err != nil {
		return err
	}
	os.Chmod(tmp, mode)
	return os.Rename(tmp, dst)
}

func writeRootJSON(p string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), mode); err != nil {
		return err
	}
	os.Chown(tmp, 0, 0)
	os.Chmod(tmp, mode)
	return os.Rename(tmp, p)
}

func runOut(name string, args ...string) (string, error) {
	var b bytes.Buffer
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = &b, &b
	err := c.Run()
	if err != nil {
		return b.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(b.String()))
	}
	return b.String(), nil
}

func inGroup(u *user.User, group string) bool {
	g, err := user.LookupGroup(group)
	if err != nil {
		return false
	}
	ids, err := u.GroupIds()
	if err != nil {
		return false
	}
	return slices.Contains(ids, g.Gid)
}

func readInt(p string) int {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func readMemTotalKB() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if f := strings.Fields(ln); len(f) > 1 && f[0] == "MemTotal:" {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			return n
		}
	}
	return 0
}
