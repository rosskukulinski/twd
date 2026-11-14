#!/usr/bin/env bash
# Build the twd microVM guest kernel for the macOS (Virtualization.framework)
# backend: arm64 only, Linux build host only (run it in Lima twd-dev), no root.
#
#   guest/build-kernel.sh [OUT_DIR]      (default: bin/vz)
#
# Why a second kernel: Firecracker's CI kernel already boots under vz (it has
# virtio-pci, the virtio console and vsock), but it has no FUSE/virtiofs, which
# Rosetta for Linux (a VZLinuxRosettaDirectoryShare, mounted in the guest) and
# any later virtiofs share need, and no erofs (ADR-032 container images).
#
# Inputs (all pinned, so the output is reproducible):
#   - linux-$KERNEL_VERSION.tar.xz from cdn.kernel.org (sha256 below);
#   - Firecracker's CI guest config for 6.1 aarch64 at FC_REF (sha256 below);
#   - guest/kernel/vz.config, a fragment applied on top (olddefconfig).
# KBUILD_BUILD_{TIMESTAMP,USER,HOST} and the build path are fixed. Output:
#   OUT_DIR/Image (uncompressed arm64 Image, what VZLinuxBootLoader loads),
#   OUT_DIR/kernel.config, OUT_DIR/kernel-manifest.json.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${1:-$ROOT/bin/vz}
KERNEL_VERSION=${KERNEL_VERSION:-6.1.167}
KERNEL_SHA256=${KERNEL_SHA256:-2818053c07976ba4ed5f44deb0f5dc7ae7b0975d7918c313d48d8fe7c4e598cb}
FC_REF=${FC_REF:-v1.17.0}
FC_CONFIG_SHA256=${FC_CONFIG_SHA256:-5c73202d00b5591ad987d9f808208a318e317fd32dbc17de613c5a93fabe2939}
SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-1762560000} # 2025-11-08T00:00:00Z
CACHE=${CACHE:-$HOME/.cache/twd-kernel}
BUILD=/tmp/twd-kernel-build # fixed path: it ends up in debug strings
JOBS=${JOBS:-$(nproc)}

[ "$(uname -s)" = Linux ] || { echo "build-kernel: Linux only (run it in Lima)" >&2; exit 1; }
case "$(uname -m)" in aarch64|arm64) ;; *) echo "build-kernel: arm64 only (the vz backend runs on Apple silicon)" >&2; exit 1 ;; esac

mkdir -p "$CACHE" "$OUT"
TARBALL=$CACHE/linux-$KERNEL_VERSION.tar.xz
FCCONF=$CACHE/fc-$FC_REF-aarch64-6.1.config
fetch() { # url file
  [ -f "$2" ] && return 0
  curl -fsSL -o "$2.part" "$1"
  mv "$2.part" "$2"
}
fetch "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$KERNEL_VERSION.tar.xz" "$TARBALL"
fetch "https://raw.githubusercontent.com/firecracker-microvm/firecracker/$FC_REF/resources/guest_configs/microvm-kernel-ci-aarch64-6.1.config" "$FCCONF"
check() { # file want
  got=$(sha256sum "$1" | cut -d' ' -f1)
  if [ "$2" = REPLACE_ME ]; then echo "build-kernel: UNPINNED $1 sha256 $got" >&2; return 0; fi
  [ "$got" = "$2" ] || { echo "build-kernel: $1: sha256 $got, want $2" >&2; exit 1; }
}
check "$TARBALL" "$KERNEL_SHA256"
check "$FCCONF" "$FC_CONFIG_SHA256"

rm -rf "$BUILD"
mkdir -p "$BUILD"
tar -C "$BUILD" -xf "$TARBALL"
SRC=$BUILD/linux-$KERNEL_VERSION
cp "$FCCONF" "$SRC/.config"
cat "$ROOT/guest/kernel/vz.config" >>"$SRC/.config"
export KBUILD_BUILD_TIMESTAMP="@$SOURCE_DATE_EPOCH" KBUILD_BUILD_USER=twd KBUILD_BUILD_HOST=twd KBUILD_BUILD_VERSION=1
make -C "$SRC" -s olddefconfig
# The fragment must have survived olddefconfig (a dropped dependency would
# silently turn an option off).
while IFS= read -r line; do
  case "$line" in CONFIG_*=*) grep -qx "$line" "$SRC/.config" || { echo "build-kernel: $line did not stick" >&2; exit 1; } ;; esac
done <"$ROOT/guest/kernel/vz.config"
make -C "$SRC" -s -j"$JOBS" Image
cp "$SRC/arch/arm64/boot/Image" "$OUT/Image"
cp "$SRC/.config" "$OUT/kernel.config"
sha() { sha256sum "$1" | cut -d' ' -f1; }
cat >"$OUT/kernel-manifest.json" <<EOF
{
  "image": "Image",
  "image_sha256": "$(sha "$OUT/Image")",
  "kernel_version": "$KERNEL_VERSION",
  "kernel_tarball_sha256": "$(sha "$TARBALL")",
  "base_config": "firecracker $FC_REF resources/guest_configs/microvm-kernel-ci-aarch64-6.1.config",
  "base_config_sha256": "$(sha "$FCCONF")",
  "fragment_sha256": "$(sha "$ROOT/guest/kernel/vz.config")",
  "config_sha256": "$(sha "$OUT/kernel.config")",
  "source_date_epoch": $SOURCE_DATE_EPOCH
}
EOF
rm -rf "$BUILD"
echo "built $OUT/Image ($(du -h "$OUT/Image" | cut -f1)), sha256 $(sha "$OUT/Image")"
