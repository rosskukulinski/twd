#!/usr/bin/env bash
# Build the twd microVM guest kernel with container support (Linux only; no
# root needed).
#
#   guest/build-kernel.sh [OUT_DIR]      (default: guest/out)
#
# Why: the Firecracker CI 6.1 guest kernel (the one ~/fc/vmlinux is) has
# overlayfs but not erofs, which twd's container guests mount their image
# from (ADR-032 D3). This rebuilds the same kernel version from kernel.org
# with the Firecracker CI config plus guest/kernel/containers.fragment.
#
# Inputs (pinned):
#   - linux-$KVER.tar.xz from cdn.kernel.org, checked against KSHA256 (the
#     value in kernel.org's signed sha256sums.asc for that release);
#   - guest/kernel/microvm-6.1-$ARCH.base.config: the Firecracker CI guest
#     config (resources/guest_configs/microvm-kernel-ci-<arch>-6.1.config),
#     extracted verbatim from the CI kernel's embedded /proc/config.gz
#     (CONFIG_IKCONFIG) so it matches the kernel the VM tier was tested on;
#   - guest/kernel/containers.fragment, merged with scripts/kconfig/merge_config.sh.
#
# KBUILD_BUILD_TIMESTAMP/USER/HOST are fixed, so the build is reproducible
# for a given toolchain (the toolchain is recorded in kernel-manifest.json).
# Output: OUT_DIR/vmlinux (arm64: the uncompressed Image Firecracker boots;
# x86_64: vmlinux) and OUT_DIR/kernel-manifest.json.
#
# Build deps (Ubuntu): build-essential flex bison bc libelf-dev libssl-dev.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${1:-$ROOT/guest/out}
KVER=${KVER:-6.1.155}
KSHA256=${KSHA256:-c29387aeee085fbcbd91236224b9df805063bac43615e75cea2c6b29604a5c73}
CACHE=${KERNEL_CACHE:-$HOME/.cache/twd-kernel}
JOBS=${JOBS:-$(nproc)}

[ "$(uname -s)" = Linux ] || { echo "build-kernel: Linux only (run it in Lima)" >&2; exit 1; }
case "$(uname -m)" in
  aarch64|arm64) ARCH=arm64; TARGET=Image; IMAGE=arch/arm64/boot/Image ;;
  x86_64) ARCH=x86_64; TARGET=vmlinux; IMAGE=vmlinux ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;;
esac
BASE=$ROOT/guest/kernel/microvm-6.1-$ARCH.base.config
[ -f "$BASE" ] || { echo "missing $BASE (the Firecracker CI config for $ARCH)" >&2; exit 1; }

mkdir -p "$CACHE" "$OUT"
TARBALL=$CACHE/linux-$KVER.tar.xz
if [ ! -f "$TARBALL" ]; then
  curl -fsSL -o "$TARBALL.tmp" "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$KVER.tar.xz"
  mv "$TARBALL.tmp" "$TARBALL"
fi
echo "$KSHA256  $TARBALL" | sha256sum -c --quiet - || { echo "checksum mismatch for $TARBALL" >&2; exit 1; }

SRC=$CACHE/linux-$KVER
if [ ! -f "$SRC/Makefile" ]; then
  rm -rf "$SRC"
  tar -C "$CACHE" -xf "$TARBALL"
fi
export KBUILD_BUILD_TIMESTAMP="2025-11-08 00:00:00 UTC" KBUILD_BUILD_USER=twd KBUILD_BUILD_HOST=twd-guest-kernel
cd "$SRC"
cp "$BASE" .config
scripts/kconfig/merge_config.sh -m .config "$ROOT/guest/kernel/containers.fragment" >/dev/null
make -s olddefconfig
for opt in CONFIG_EROFS_FS CONFIG_OVERLAY_FS CONFIG_CGROUP_PIDS CONFIG_VIRTIO_VSOCKETS; do
  grep -q "^$opt=y" .config || { echo "build-kernel: $opt did not end up =y" >&2; exit 1; }
done
make -s -j"$JOBS" "$TARGET"
cp -f "$IMAGE" "$OUT/vmlinux"
cp -f .config "$OUT/vmlinux.config"

sha() { sha256sum "$1" | cut -d' ' -f1; }
cat >"$OUT/kernel-manifest.json" <<EOF
{
  "kernel": "vmlinux",
  "version": "$KVER",
  "arch": "$ARCH",
  "source_sha256": "$KSHA256",
  "base_config_sha256": "$(sha "$BASE")",
  "fragment_sha256": "$(sha "$ROOT/guest/kernel/containers.fragment")",
  "config_sha256": "$(sha .config)",
  "vmlinux_sha256": "$(sha "$OUT/vmlinux")",
  "toolchain": "$(${CC:-gcc} --version | head -1)"
}
EOF
echo "built $OUT/vmlinux ($(du -h "$OUT/vmlinux" | cut -f1)), sha256 $(sha "$OUT/vmlinux")"
