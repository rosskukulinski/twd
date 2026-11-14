#!/bin/sh
# vz-assets-lima.sh <outdir>: the macOS (Virtualization.framework) microVM
# guest: vmlinux (an arm64 Image from guest/build-kernel-vz.sh: Firecracker's CI
# config + virtiofs/erofs), rootfs.ext4 + manifest.json (guest/build-image.sh,
# the same image the linux-arm64 release ships) and kernel-manifest.json.
# Both builds need Linux, so on macOS a copy of this tree goes to Lima
# $TWD_DEV_VM (default twd-dev, running, with ~/fc/rootfs for glibc), they run
# there in /tmp/twd-vz-assets, and the files come back with `limactl copy`.
# Cached: <outdir>/inputs.sha256 hashes every input (guest/, go.mod, go.sum,
# the workerd version, the in-repo packages twd-guestd depends on), so an
# unchanged guest is not rebuilt (~5 min for the kernel the first time).
# Exit 0: assets are in <outdir>. 3: can't build here (no limactl, or the dev
# VM is not running); anything else: the build failed.
set -u
OUT=${1:?outdir}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
DEV=${TWD_DEV_VM:-twd-dev}
cd "$ROOT" || exit 1
mkdir -p "$OUT"
case $OUT in /*) ;; *) OUT=$ROOT/$OUT ;; esac
say() { echo "vz-assets: $*" >&2; }
FILES="vmlinux rootfs.ext4 manifest.json kernel-manifest.json"

os=$(uname -s)
if [ "$os" = Linux ]; then
	remote() { sh -c "$1"; }
elif command -v limactl >/dev/null 2>&1 && [ "$(limactl list -f '{{.Status}}' "$DEV" 2>/dev/null)" = Running ]; then
	remote() { limactl shell "$DEV" -- bash -lc "$1"; }
else
	say "no Linux builder: run on Linux or start Lima $DEV (limactl start $DEV)"
	exit 3
fi
remote '[ "$(uname -m)" = aarch64 ] && test -d ~/fc/rootfs/usr/lib/aarch64-linux-gnu' ||
	{ say "the builder must be arm64 Linux with ~/fc/rootfs (the glibc source for the guest image)"; exit 3; }

deps=$(go list -deps -f '{{if not .Standard}}{{.Dir}}{{end}}' ./guest/twd-guestd 2>/dev/null | grep "^$ROOT/" | sed "s|^$ROOT/||")
inputs=$( (git ls-files -co --exclude-standard guest go.mod go.sum; for d in $deps; do git ls-files -co --exclude-standard "$d"; done) | sort -u |
	while read -r f; do [ -f "$f" ] && shasum -a 256 "$f"; done
	node -p "require('$ROOT/package-lock.json').packages['node_modules/@cloudflare/workerd-linux-arm64'].version")
want=$(printf '%s\n' "$inputs" | shasum -a 256 | cut -c1-64)
have=$(cat "$OUT/inputs.sha256" 2>/dev/null || true)
complete=1
for f in $FILES; do [ -s "$OUT/$f" ] || complete=0; done
if [ "$complete" = 1 ] && [ "$have" = "$want" ]; then
	say "cached in $OUT (inputs $want)"
	exit 0
fi

t0=$(date +%s)
build='npm ci --no-audit --no-fund --loglevel=error >build.log 2>&1 && guest/build-image.sh out >>build.log 2>&1 && guest/build-kernel-vz.sh out >>build.log 2>&1 && cp out/Image out/vmlinux || { tail -30 build.log; exit 1; }'
if [ "$os" = Linux ]; then
	B=$(mktemp -d)
	git ls-files -co --exclude-standard | tar cf - -T - | tar xf - -C "$B" || exit 1
	(cd "$B" && sh -c "$build") || exit 1
	for f in $FILES; do cp "$B/out/$f" "$OUT/$f" || exit 1; done
	rm -rf "$B"
else
	say "building in Lima $DEV:/tmp/twd-vz-assets (first time: npm ci, the guest image and the kernel)"
	remote 'rm -rf /tmp/twd-vz-assets && mkdir -p /tmp/twd-vz-assets' || exit 1
	git ls-files -co --exclude-standard | tar cf - -T - | limactl shell "$DEV" -- tar xf - -C /tmp/twd-vz-assets || exit 1
	remote "cd /tmp/twd-vz-assets && $build" || exit 1
	for f in $FILES; do
		limactl copy "$DEV:/tmp/twd-vz-assets/out/$f" "$OUT/$f" || exit 1
	done
	limactl copy "$DEV:/tmp/twd-vz-assets/build.log" "$OUT/build.log" 2>/dev/null
fi
printf '%s\n' "$want" >"$OUT/inputs.sha256"
say "built in $(( $(date +%s) - t0 )) s -> $OUT"
