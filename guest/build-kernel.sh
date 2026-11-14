#!/usr/bin/env bash
# Build the twd microVM guest kernel (Linux only; no root needed).
#
#   TARGETARCH=amd64|arm64 guest/build-kernel.sh [OUT_DIR]   (default: this
#   machine's arch, guest/out/kernel-<arch>)
#
# kernel.org linux-$KERNEL_VERSION with Firecracker's CI guest config for that
# kernel line, plus guest/kernel/twd.config (erofs + overlayfs for container
# mode; the same fragment for both arches). All three are pinned in
# guest/inputs.sh. Cross builds need the cross compiler
# (gcc-x86-64-linux-gnu / gcc-aarch64-linux-gnu) and both need flex, bison,
# bc, libelf-dev and libssl-dev.
#
# Reproducible for a given compiler: KBUILD_BUILD_{TIMESTAMP,USER,HOST,
# VERSION} are fixed, there are no modules (so no signing key) and the build
# salt is fixed; the compiler version is recorded in the manifest.
#
# Output: OUT_DIR/vmlinux (x86_64: the ELF vmlinux; arm64: the uncompressed
# Image, which is what Firecracker boots on each), OUT_DIR/kernel.config (the
# final .config) and OUT_DIR/kernel.json (input + output hashes). An output
# whose recorded inputs match is reused, not rebuilt.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
. "$ROOT/guest/inputs.sh"
guest_arch "${TARGETARCH:-}"
OUT=${1:-$ROOT/guest/out/kernel-$GARCH}
SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-1762560000} # 2025-11-08T00:00:00Z
FRAGMENT=$ROOT/guest/kernel/twd.config
[ "$(uname -s)" = Linux ] || { echo "build-kernel: Linux only (run it in Lima)" >&2; exit 1; }

# Always the triplet-prefixed toolchain, native builds too (Debian/Ubuntu's
# gcc package installs <triplet>-gcc): the compiler's name is part of
# CONFIG_CC_VERSION_TEXT, which the kernel embeds, so a native and a cross
# build with the same gcc version give the same vmlinux.
CROSS=$UARCH-linux-gnu-
if ! command -v "${CROSS}gcc" >/dev/null; then
  if [ "$(uname -m)" = "$UARCH" ]; then echo "build-kernel: needs ${CROSS}gcc (apt install gcc)" >&2
  else echo "build-kernel: needs ${CROSS}gcc (apt install gcc-${UARCH//_/-}-linux-gnu)" >&2; fi
  exit 1
fi
for t in flex bison bc make perl; do
  command -v $t >/dev/null || { echo "build-kernel: needs $t" >&2; exit 1; }
done
CC_VERSION=$("${CROSS}gcc" --version | head -1)

eval "config_sha=\$KERNEL_CONFIG_SHA256_$GARCH"
inputs=$(printf '%s\n' "$KERNEL_VERSION" "$KERNEL_SHA256" "$config_sha" "$(sha256_of "$FRAGMENT")" \
  "$(sha256_of "$0")" "$CC_VERSION" "$SOURCE_DATE_EPOCH" | sha256sum | cut -c1-64)
if [ -s "$OUT/vmlinux" ] && [ -f "$OUT/kernel.json" ] &&
  grep -q "\"inputs_sha256\": \"$inputs\"" "$OUT/kernel.json" &&
  grep -q "\"vmlinux_sha256\": \"$(sha256_of "$OUT/vmlinux")\"" "$OUT/kernel.json"; then
  echo "build-kernel: $OUT/vmlinux is current ($GARCH, inputs $inputs)"
  exit 0
fi

src_tar=$(fetch "$KERNEL_URL" "$KERNEL_SHA256")
config=$(fetch "$KERNEL_CONFIG_URL_BASE/$FCARCH/vmlinux-$KERNEL_VERSION.config" "$config_sha" "fc-ci-vmlinux-$KERNEL_VERSION-$FCARCH.config")

WORK=$(mktemp -d /tmp/twd-kernel-$GARCH.XXXXXX)
trap 'rm -rf "$WORK"' EXIT
echo "build-kernel: linux-$KERNEL_VERSION for $GARCH ($CC_VERSION, on $(uname -m)) in $WORK"
tar -xJf "$src_tar" -C "$WORK"
K=$WORK/linux-$KERNEL_VERSION
export KBUILD_BUILD_TIMESTAMP="$(date -u -d "@$SOURCE_DATE_EPOCH" '+%a %b %e %H:%M:%S UTC %Y')"
export KBUILD_BUILD_USER=twd KBUILD_BUILD_HOST=twd KBUILD_BUILD_VERSION=1
mk() { make -C "$K" -s ARCH="$KARCH" CROSS_COMPILE="$CROSS" "$@"; }

cp "$config" "$K/.config"
(cd "$K" && ARCH=$KARCH CROSS_COMPILE=$CROSS scripts/kconfig/merge_config.sh -m -O "$K" .config "$FRAGMENT" >/dev/null)
mk olddefconfig
# Every line of the fragment must have survived (a dependency kconfig
# silently drops would otherwise ship a kernel without, e.g., erofs).
missing=0
while IFS= read -r l; do
  case $l in
    CONFIG_*=*) grep -qxF "$l" "$K/.config" || { echo "build-kernel: $l did not stick" >&2; missing=1; } ;;
    "# CONFIG_"*" is not set") grep -qxF "$l" "$K/.config" || { echo "build-kernel: '$l' did not stick" >&2; missing=1; } ;;
  esac
done <"$FRAGMENT"
[ $missing = 0 ] || exit 1

case $GARCH in
  amd64) mk -j"$(nproc)" vmlinux; img=$K/vmlinux ;;
  arm64) mk -j"$(nproc)" Image; img=$K/arch/arm64/boot/Image ;;
esac

mkdir -p "$OUT"
install -m 0644 "$img" "$OUT/vmlinux"
install -m 0644 "$K/.config" "$OUT/kernel.config"
opt() { grep -E "^(# )?$1[= ]" "$K/.config" | head -1; }
cat >"$OUT/kernel.json" <<EOF
{
  "arch": "$GARCH",
  "kernel_version": "$KERNEL_VERSION",
  "release": "$(cat "$K/include/config/kernel.release")",
  "source_sha256": "$KERNEL_SHA256",
  "base_config": "firecracker-ci/v1.15/$FCARCH/vmlinux-$KERNEL_VERSION.config",
  "base_config_sha256": "$config_sha",
  "fragment_sha256": "$(sha256_of "$FRAGMENT")",
  "config_sha256": "$(sha256_of "$OUT/kernel.config")",
  "compiler": "$CC_VERSION",
  "source_date_epoch": $SOURCE_DATE_EPOCH,
  "erofs": "$(opt CONFIG_EROFS_FS)",
  "overlayfs": "$(opt CONFIG_OVERLAY_FS)",
  "inputs_sha256": "$inputs",
  "vmlinux_sha256": "$(sha256_of "$OUT/vmlinux")"
}
EOF
echo "build-kernel: built $OUT/vmlinux ($GARCH, $(du -h "$OUT/vmlinux" | cut -f1)), sha256 $(sha256_of "$OUT/vmlinux")"
