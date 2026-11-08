#!/bin/sh
# vm-assets-lima.sh <outdir>: the linux-arm64 microVM guest assets for the
# release (vmlinux, rootfs.ext4 + manifest.json, firecracker, jailer), built
# where they can be built:
#   - on Linux (e.g. inside Lima twd-dev): `make vm-assets VM_ASSETS=<outdir>`;
#   - on macOS: a copy of this tree goes to Lima $TWD_DEV_VM (default twd-dev,
#     which must be running and have ~/fc/vmlinux + /usr/local/bin/{firecracker,
#     jailer}), `make deps && make vm-assets` run there in /tmp/twd-vm-assets,
#     and the five files come back with `limactl copy`.
# Cached: <outdir>/inputs.sha256 hashes every input (guest/, go.mod, go.sum,
# the workerd version in package-lock.json, the in-repo Go packages
# twd-guestd depends on, and the kernel + Firecracker binaries in the VM), so
# an unchanged guest is not rebuilt (the image is reproducible anyway).
# Exit 0: assets are in <outdir>. 3: can't build here (no limactl, or the dev
# VM is not running); anything else: the build failed.
set -u
OUT=${1:?outdir}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
DEV=${TWD_DEV_VM:-twd-dev}
cd "$ROOT" || exit 1
mkdir -p "$OUT"
case $OUT in /*) ;; *) OUT=$ROOT/$OUT ;; esac
say() { echo "vm-assets: $*" >&2; }

os=$(uname -s)
if [ "$os" = Linux ]; then
	remote() { sh -c "$1"; }
elif command -v limactl >/dev/null 2>&1 && [ "$(limactl list -f '{{.Status}}' "$DEV" 2>/dev/null)" = Running ]; then
	remote() { limactl shell "$DEV" -- bash -lc "$1"; }
else
	say "no Linux builder: run on Linux or start Lima $DEV (limactl start $DEV); the linux release will have no microVM guest"
	exit 3
fi
remote 'test -f ~/fc/vmlinux && test -x /usr/local/bin/firecracker && test -x /usr/local/bin/jailer' ||
	{ say "the builder lacks ~/fc/vmlinux or /usr/local/bin/{firecracker,jailer}"; exit 3; }

# Input hash: in-repo inputs + the builder's kernel and Firecracker binaries.
deps=$(go list -deps -f '{{if not .Standard}}{{.Dir}}{{end}}' ./guest/twd-guestd 2>/dev/null | grep "^$ROOT/" | sed "s|^$ROOT/||")
inputs=$( (git ls-files -co --exclude-standard guest go.mod go.sum; for d in $deps; do git ls-files -co --exclude-standard "$d"; done) | sort -u |
	while read -r f; do [ -f "$f" ] && shasum -a 256 "$f"; done
	node -p "require('$ROOT/package-lock.json').packages['node_modules/@cloudflare/workerd-linux-arm64'].version"
	remote 'sha256sum ~/fc/vmlinux /usr/local/bin/firecracker /usr/local/bin/jailer | cut -c1-64')
want=$(printf '%s\n' "$inputs" | shasum -a 256 | cut -c1-64)
have=$(cat "$OUT/inputs.sha256" 2>/dev/null || true)
complete=1
for f in vmlinux rootfs.ext4 manifest.json firecracker jailer; do [ -s "$OUT/$f" ] || complete=0; done
if [ "$complete" = 1 ] && [ "$have" = "$want" ]; then
	say "cached in $OUT (inputs $want)"
	exit 0
fi

t0=$(date +%s)
if [ "$os" = Linux ]; then
	make vm-assets TWD_VM_KERNEL="$HOME/fc/vmlinux" VM_ASSETS="$OUT" >"$OUT/build.log" 2>&1 || { tail -30 "$OUT/build.log" >&2; exit 1; }
else
	# A fixed scratch dir in the VM (a fresh copy of the tree each build).
	say "building in Lima $DEV:/tmp/twd-vm-assets (first time: make deps + the guest image; later builds are cached by input hash)"
	remote 'rm -rf /tmp/twd-vm-assets && mkdir -p /tmp/twd-vm-assets' || exit 1
	git ls-files -co --exclude-standard | tar cf - -T - | limactl shell "$DEV" -- tar xf - -C /tmp/twd-vm-assets || exit 1
	remote 'cd /tmp/twd-vm-assets && make deps >build.log 2>&1 && make vm-assets TWD_VM_KERNEL=$HOME/fc/vmlinux VM_ASSETS=/tmp/twd-vm-assets/out >>build.log 2>&1 || { tail -30 build.log; exit 1; }' || exit 1
	for f in vmlinux rootfs.ext4 manifest.json firecracker jailer; do
		limactl copy "$DEV:/tmp/twd-vm-assets/out/$f" "$OUT/$f" || exit 1
	done
	limactl copy "$DEV:/tmp/twd-vm-assets/build.log" "$OUT/build.log" 2>/dev/null
fi
printf '%s\n' "$want" >"$OUT/inputs.sha256"
say "built in $(( $(date +%s) - t0 )) s -> $OUT"
