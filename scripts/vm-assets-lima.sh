#!/bin/sh
# vm-assets-lima.sh <outdir> [arch]: the linux-<arch> (arm64 default, or
# amd64) microVM guest assets for the release (vmlinux + kernel.json,
# rootfs.ext4 + manifest.json, firecracker, jailer), built where they can be
# built:
#   - on Linux (e.g. inside Lima twd-dev): `make vm-assets GUEST_ARCH=<arch>
#     VM_ASSETS=<outdir>`;
#   - on macOS: a copy of this tree goes to Lima $TWD_DEV_VM (default
#     twd-dev, which must be running), `make deps && make vm-assets` run there
#     in /tmp/twd-vm-assets-<arch>, and the files come back with `limactl
#     copy`. Downloads and the kernel build are kept in the VM's
#     ~/.cache/twd-guest (both are checked against their pinned inputs).
# Either arch builds on either: the other arch's kernel needs its cross
# compiler (gcc-x86-64-linux-gnu on arm64, gcc-aarch64-linux-gnu on amd64).
# Cached: <outdir>/inputs.sha256 hashes every input (guest/, which pins the
# kernel, base rootfs, CA and Firecracker downloads; go.mod, go.sum; the
# workerd version in package-lock.json; the in-repo Go packages twd-guestd
# depends on; and the builder's compiler), so an unchanged guest is not
# rebuilt (the image is reproducible anyway).
# Exit 0: assets are in <outdir>. 3: can't build here (no limactl, the dev
# VM is not running, or no compiler); anything else: the build failed.
set -u
OUT=${1:?outdir}
ARCH=${2:-arm64}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
DEV=${TWD_DEV_VM:-twd-dev}
cd "$ROOT" || exit 1
mkdir -p "$OUT"
case $OUT in /*) ;; *) OUT=$ROOT/$OUT ;; esac
say() { echo "vm-assets ($ARCH): $*" >&2; }
case $ARCH in
	arm64) uarch=aarch64 npmarch=arm64 ;;
	amd64) uarch=x86_64 npmarch=64 ;;
	*) say "unsupported arch (want arm64 or amd64)"; exit 1 ;;
esac
FILES="vmlinux kernel.json rootfs.ext4 manifest.json firecracker jailer"

os=$(uname -s)
if [ "$os" = Linux ]; then
	remote() { sh -c "$1"; }
elif command -v limactl >/dev/null 2>&1 && [ "$(limactl list -f '{{.Status}}' "$DEV" 2>/dev/null)" = Running ]; then
	remote() { limactl shell "$DEV" -- bash -lc "$1"; }
else
	say "no Linux builder: run on Linux or start Lima $DEV (limactl start $DEV); the linux-$ARCH release will have no microVM guest"
	exit 3
fi
cc=$(remote "if [ \"\$(uname -m)\" = $uarch ]; then echo gcc; else echo $uarch-linux-gnu-gcc; fi")
ccv=$(remote "$cc --version 2>/dev/null | head -1")
[ -n "$ccv" ] || { say "the builder has no $cc (the guest kernel's compiler; apt install gcc-$(echo $uarch | tr _ -)-linux-gnu)"; exit 3; }

# Input hash: in-repo inputs + the builder's compiler.
deps=$(go list -deps -f '{{if not .Standard}}{{.Dir}}{{end}}' ./guest/twd-guestd 2>/dev/null | grep "^$ROOT/" | sed "s|^$ROOT/||")
inputs=$( (git ls-files -co --exclude-standard guest go.mod go.sum; for d in $deps; do git ls-files -co --exclude-standard "$d"; done) | sort -u |
	while read -r f; do [ -f "$f" ] && shasum -a 256 "$f"; done
	echo "$ARCH"
	node -p "require('$ROOT/package-lock.json').packages['node_modules/@cloudflare/workerd-linux-$npmarch'].version"
	echo "$ccv")
want=$(printf '%s\n' "$inputs" | shasum -a 256 | cut -c1-64)
have=$(cat "$OUT/inputs.sha256" 2>/dev/null || true)
complete=1
for f in $FILES; do [ -s "$OUT/$f" ] || complete=0; done
if [ "$complete" = 1 ] && [ "$have" = "$want" ]; then
	say "cached in $OUT (inputs $want)"
	exit 0
fi

t0=$(date +%s)
if [ "$os" = Linux ]; then
	make vm-assets GUEST_ARCH="$ARCH" VM_ASSETS="$OUT" >"$OUT/build.log" 2>&1 || { tail -30 "$OUT/build.log" >&2; exit 1; }
else
	# A fixed scratch dir in the VM (a fresh copy of the tree each build).
	W=/tmp/twd-vm-assets-$ARCH
	say "building in Lima $DEV:$W (first time: make deps, the kernel and the guest image; later builds are cached by input hash)"
	remote "rm -rf $W && mkdir -p $W" || exit 1
	git ls-files -co --exclude-standard | tar cf - -T - | limactl shell "$DEV" -- tar xf - -C "$W" || exit 1
	remote "cd $W && export GUEST_CACHE=\$HOME/.cache/twd-guest && make deps >build.log 2>&1 && make vm-assets GUEST_ARCH=$ARCH KERNEL_OUT=\$HOME/.cache/twd-guest/kernel-$ARCH VM_ASSETS=$W/out >>build.log 2>&1 || { tail -30 build.log; exit 1; }" || exit 1
	for f in $FILES; do
		limactl copy "$DEV:$W/out/$f" "$OUT/$f" || exit 1
	done
	limactl copy "$DEV:$W/build.log" "$OUT/build.log" 2>/dev/null
fi
printf '%s\n' "$want" >"$OUT/inputs.sha256"
say "built in $(( $(date +%s) - t0 )) s -> $OUT"
