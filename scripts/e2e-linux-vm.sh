#!/bin/sh
# e2e-linux-vm.sh <lima vm>: L3 E2E from the installed release on a fresh
# nested-virt Lima VM (see the Makefile target).
#   1. build the linux-arm64 release with the microVM assets in Lima twd-dev
#      (from this worktree's files), and copy it here (dist/);
#   2. create <vm> from e2e/linuxvm/twdl-kvm-u24.yaml if it doesn't exist, start it;
#   3. run the Go test (e2e/linuxvm), which drives the VM over Lima's ssh;
#   4. on exit, always: twd down --purge, sudo twd host setup --undo, uninstall,
#      remove ~/.twd-e2e (and the API key file in it) on the VM; stop the VM.
set -eu
VM=$1
ROOT=$(cd "$(dirname "$0")/.." && pwd)
DEV=${TWD_DEV_VM:-twd-dev}
cd "$ROOT"
VERSION=$(cat VERSION)-$(git rev-parse --short=8 HEAD)$(git diff --quiet HEAD -- . ':!STATUS.md' || echo -dirty)
REL=/tmp/twd-l3-release
echo "e2e-linux-vm: release $VERSION, built in $DEV:$REL"

# 1. release (linux-arm64 + microVM assets) in twd-dev
limactl shell "$DEV" -- mkdir -p "$REL"
git ls-files -co --exclude-standard | tar cf - -T - | limactl shell "$DEV" -- tar xf - -C "$REL"
limactl shell "$DEV" -- bash -lc "cd $REL && make deps >/tmp/twd-l3-release.log 2>&1 && make vm-assets TWD_VM_KERNEL=\$HOME/fc/vmlinux >>/tmp/twd-l3-release.log 2>&1 && \
	TWD_VM_ASSETS_arm64=$REL/dist/vm-assets-arm64 make release RELEASE_TARGETS=linux-arm64 RELEASE_VERSION=$VERSION >>/tmp/twd-l3-release.log 2>&1 || { tail -40 /tmp/twd-l3-release.log; exit 1; }"
TGZ=twd-$VERSION-linux-arm64.tar.gz
mkdir -p dist
limactl copy "$DEV:$REL/dist/$TGZ" "dist/$TGZ"
ls -lh "dist/$TGZ"

# 2. the fresh VM
if ! limactl list -q 2>/dev/null | grep -qx "$VM"; then
	echo "e2e-linux-vm: creating $VM (fresh, nested virt, nothing mounted)"
	limactl create --name="$VM" --tty=false e2e/linuxvm/twdl-kvm-u24.yaml
fi
limactl start "$VM" >/dev/null 2>&1 || limactl start "$VM"

cleanup() {
	limactl shell "$VM" -- bash -c '
		[ -d ~/.twd-e2e ] || [ -e /etc/twd ] || exit 0
		echo "e2e-linux-vm trap: cleaning up on $(hostname)"
		if [ -x ~/.twd/bin/twd ] && [ -f ~/.twd-e2e/state/supervisor.pid ] && kill -0 "$(cat ~/.twd-e2e/state/supervisor.pid)" 2>/dev/null; then
			~/.twd/bin/twd --state-dir ~/.twd-e2e/state down --purge >/dev/null 2>&1 || true
		fi
		pkill -TERM -f "$HOME/.twd/bin/" 2>/dev/null; sleep 0.5; pkill -KILL -f "$HOME/.twd/bin/" 2>/dev/null
		[ -x ~/.twd/bin/twd ] && sudo ~/.twd/bin/twd host setup --undo >/dev/null 2>&1
		u=$(ls -d ~/.twd-e2e/twd-*/install.sh 2>/dev/null | head -1)
		[ -n "$u" ] && "$u" --uninstall --purge --force >/dev/null
		rm -rf ~/.twd-e2e
		true' || echo "e2e-linux-vm trap: cleanup on $VM failed: check ~/.twd-e2e there by hand" >&2
	if [ "${TWD_E2E_KEEP_VM_RUNNING:-}" = "" ]; then
		limactl stop "$VM" >/dev/null 2>&1 && echo "e2e-linux-vm: stopped $VM (not deleted)"
	fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# 3. the test
TWD_E2E_API_KEY_FILE="$TS_CREDS/tailnet1_api.env" TWD_E2E_TAILNET="$TAILNET1" TWD_E2E_LIMA_VM="$VM" \
	TWD_E2E_TARBALL="$ROOT/dist/$TGZ" \
	go test -tags e2elinuxvm -count=1 -v -timeout 40m ./e2e/linuxvm/...
