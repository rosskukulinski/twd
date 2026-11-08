#!/bin/sh
# e2e-linux-one.sh <distro>: one row of the E2E-LINUX matrix.
#
#   1. a FRESH Lima VM twdl-<distro> from Lima's official template (4 CPU,
#      6 GiB, 30 GiB; --plain --mount-none: nothing from this Mac is mounted),
#      unless TWD_E2E_REUSE_VM=1 and it exists (then it is only started);
#   2. the e2elinux Go test (e2e/linux) against it, with the release tarball
#      for its arch (TWD_E2E_TARBALL_<arch>);
#   3. on exit (pass, fail, timeout or ^C): inside the VM, twd down --purge
#      --service, stop twd processes, uninstall, rm ~/.twd-e2e (with the API
#      key file); then `limactl stop` the VM (kept, not deleted).
#
# Distros: ubuntu24-arm64 debian12-arm64 ubuntu22-arm64 ubuntu24-amd64
# fedora-arm64, and the unsupported-host checks debian11-arm64 alpine-arm64
# (install.sh + doctor must refuse with a clear error; see e2e-linux-unsupported).
# Logs: $OUT/<distro>.log (OUT defaults to e2e/real/results/linux-<date>).
set -u
distro=$1
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${TWD_E2E_OUT:-$ROOT/e2e/real/results/linux}
mkdir -p "$OUT"
vm=twdl-$distro
arch=arm64 vmtype=vz slow=1 extra=
# The reference row also proves reboot survival (enable-linger + limactl
# stop/start) and ephemeral nodes across a service restart (re-mint).
reboot=0 ephemeral=0
case $distro in
ubuntu24-arm64) tmpl=ubuntu-24.04 reboot=1 ephemeral=1 ;;
ubuntu22-arm64) tmpl=ubuntu-22.04 ;;
debian12-arm64) tmpl=debian-12 ;;
fedora-arm64) tmpl=fedora ;;
debian11-arm64) tmpl=debian-11 vmtype=qemu ;; # its genericcloud kernel does not boot under vz
alpine-arm64) tmpl=alpine ;;
ubuntu24-amd64) tmpl=ubuntu-24.04 arch=amd64 vmtype=qemu slow=${TWD_E2E_SLOW_AMD64:-6} extra="--arch x86_64" ;;
*) echo "e2e-linux: unknown distro $distro" >&2; exit 2 ;;
esac
log=$OUT/$distro.log
cd "$ROOT" || exit 2
vssh() { ssh -F "$HOME/.lima/$vm/ssh.config" -o ConnectTimeout=20 "lima-$vm" "$@"; }
say() { printf '%s e2e-linux[%s]: %s\n' "$(date +%H:%M:%S)" "$distro" "$*" | tee -a "$log" >&2; }

cleanup() {
	rc=$?
	if [ "$(limactl list -f '{{.Status}}' "$vm" 2>/dev/null)" = Running ]; then
		vssh '
			[ -d ~/.twd-e2e ] || [ -e ~/.config/systemd/user/twd.service ] || exit 0
			echo "trap: cleaning ~/.twd-e2e"
			if [ -x ~/.twd/bin/twd ] && [ -d ~/.twd-e2e/state ]; then
				~/.twd/bin/twd --state-dir ~/.twd-e2e/state down --purge --service >/dev/null 2>&1 || true
			fi
			systemctl --user disable --now twd.service >/dev/null 2>&1; rm -f ~/.config/systemd/user/twd.service; systemctl --user daemon-reload 2>/dev/null
			pkill -TERM -f "$HOME/.twd/bin/" 2>/dev/null; sleep 0.5; pkill -KILL -f "$HOME/.twd/bin/" 2>/dev/null
			u=$(ls -d ~/.twd-e2e/twd-*/install.sh 2>/dev/null | head -1)
			[ -n "$u" ] && "$u" --uninstall --purge --force >/dev/null
			rm -rf ~/.twd-e2e
			true' >>"$log" 2>&1 || say "trap: cleaning the VM failed: check ~/.twd-e2e in $vm by hand"
		if [ "${TWD_E2E_KEEP_RUNNING:-0}" != 1 ]; then
			limactl stop "$vm" >>"$log" 2>&1 && say "stopped $vm (kept)"
		fi
	fi
	say "exit $rc"
	exit $rc
}
trap cleanup EXIT
trap 'exit 130' INT TERM

eval "tarball=\${TWD_E2E_TARBALL_$arch:-}"
[ -n "$tarball" ] && [ -f "$tarball" ] || { say "no tarball for $arch (TWD_E2E_TARBALL_$arch)"; exit 2; }

t0=$(date +%s)
if limactl list -q 2>/dev/null | grep -qx "$vm"; then
	if [ "${TWD_E2E_REUSE_VM:-0}" = 1 ]; then
		say "reusing $vm (TWD_E2E_REUSE_VM=1)"
	else
		say "deleting the old $vm (fresh VM per run)"
		limactl stop -f "$vm" >>"$log" 2>&1
		limactl delete -f "$vm" >>"$log" 2>&1 || { say "limactl delete $vm failed"; exit 1; }
	fi
fi
if ! limactl list -q 2>/dev/null | grep -qx "$vm"; then
	say "creating $vm from template:$tmpl ($vmtype, $arch, 4 CPU / 6 GiB / 30 GiB, no mounts)"
	# shellcheck disable=SC2086
	limactl create --tty=false --name "$vm" --plain --mount-none --vm-type "$vmtype" $extra \
		--cpus 4 --memory 6 --disk 30 "template:$tmpl" >>"$log" 2>&1 || { say "limactl create failed"; exit 1; }
fi
limactl start --tty=false "$vm" >>"$log" 2>&1 || { say "limactl start failed"; exit 1; }
vm_ready_s=$(( $(date +%s) - t0 ))
say "$vm ready in ${vm_ready_s}s"

case $distro in
debian11-arm64 | alpine-arm64)
	TWD_E2E_LIMA_VM=$vm TWD_E2E_TARBALL=$tarball TWD_E2E_RESULTS_DIR=$OUT TWD_E2E_VM_READY_S=$vm_ready_s \
		go test -tags e2elinux -count=1 -v -timeout 10m -run '^TestUnsupportedHost$' ./e2e/linux/... >>"$log" 2>&1
	;;
*)
	TWD_E2E_LIMA_VM=$vm TWD_E2E_TARBALL=$tarball TWD_E2E_SLOW=$slow TWD_E2E_RESULTS_DIR=$OUT TWD_E2E_VM_READY_S=$vm_ready_s \
		TWD_E2E_REBOOT=${TWD_E2E_REBOOT:-$reboot} TWD_E2E_EPHEMERAL=${TWD_E2E_EPHEMERAL:-$ephemeral} \
		go test -tags e2elinux -count=1 -v -timeout "$((45 * slow))m" -run '^TestE2ELinux$' ./e2e/linux/... >>"$log" 2>&1
	;;
esac
rc=$?
grep -E '^(--- |PASS|FAIL|ok|    linux_test.go:[0-9]+: (results|.*Fatal))' "$log" | tail -5 >&2
exit $rc
