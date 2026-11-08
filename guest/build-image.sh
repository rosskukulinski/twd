#!/usr/bin/env bash
# Build the twd microVM guest rootfs (Linux only; no root needed).
#
#   guest/build-image.sh [OUT_DIR]      (default: guest/out)
#
# Inputs (all pinned, so the output is byte-for-byte reproducible):
#   - workerd: the npm package @cloudflare/workerd-linux-<arch> at the exact
#     version in package-lock.json (node_modules/, after `npm ci`);
#   - twd-guestd: built from this repo (CGO off, -trimpath, no build ID);
#   - glibc: the shared libraries workerd needs, copied from the base Ubuntu
#     24.04 rootfs (BASE_ROOTFS, a directory: the Firecracker CI rootfs,
#     extracted; default ~/fc/rootfs). glibc 2.39 >= workerd's 2.35 floor.
#
# The image is an ext4 without a journal, every mtime is SOURCE_DATE_EPOCH,
# owners are 0:0, and the UUID and hash seed are fixed. It is mounted
# read-only by every VM (runtime state is tmpfs in the guest). Output:
#   OUT_DIR/rootfs.ext4, OUT_DIR/manifest.json (input + output hashes).
#
# There is no tailscale, no tsnet and no key material in the image; the e2e
# suite scans it (and guest memory) to check that.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${1:-$ROOT/guest/out}
BASE_ROOTFS=${BASE_ROOTFS:-$HOME/fc/rootfs}
SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-1762560000} # 2025-11-08T00:00:00Z
FS_UUID=7b1d6a8e-2c3f-4e5a-9b0c-7d7700000008
GO=${GO:-go}

case "$(uname -m)" in
  aarch64|arm64) ARCH=arm64; LIBDIR=aarch64-linux-gnu; LDSO=ld-linux-aarch64.so.1 ;;
  x86_64) ARCH=64; LIBDIR=x86_64-linux-gnu; LDSO=ld-linux-x86-64.so.2 ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;;
esac
[ "$(uname -s)" = Linux ] || { echo "build-image: Linux only (run it in Lima)" >&2; exit 1; }

WORKERD=$ROOT/node_modules/@cloudflare/workerd-linux-$ARCH/bin/workerd
[ -x "$WORKERD" ] || { echo "missing $WORKERD (run npm ci)" >&2; exit 1; }
[ -d "$BASE_ROOTFS/usr/lib/$LIBDIR" ] || { echo "BASE_ROOTFS=$BASE_ROOTFS has no usr/lib/$LIBDIR" >&2; exit 1; }
WORKERD_VERSION=$(node -p "require('$ROOT/package-lock.json').packages['node_modules/@cloudflare/workerd-linux-$ARCH'].version")

STAGE=$(mktemp -d /tmp/twd-guest-stage.XXXXXX)
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$OUT"

# twd-guestd: static, reproducible.
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) GOFLAGS=-mod=mod \
  $GO build -trimpath -buildvcs=false -ldflags "-s -w -buildid=" -o "$STAGE/sbin/twd-guestd" ./guest/twd-guestd)

mkdir -p "$STAGE"/{usr/bin,usr/lib/$LIBDIR,dev,proc,sys,run,tmp,etc}
ln -s usr/lib "$STAGE/lib"
ln -s usr/bin "$STAGE/bin"
install -m 0755 "$WORKERD" "$STAGE/usr/bin/workerd"

# The shared libraries workerd links (glibc only), from the base rootfs.
for lib in $(readelf -d "$WORKERD" | sed -n 's/.*Shared library: \[\(.*\)\]/\1/p') $LDSO; do
  src=$BASE_ROOTFS/usr/lib/$LIBDIR/$lib
  [ -e "$src" ] || { echo "missing $lib in $BASE_ROOTFS" >&2; exit 1; }
  cp -L "$src" "$STAGE/usr/lib/$LIBDIR/$lib"
  chmod 0755 "$STAGE/usr/lib/$LIBDIR/$lib"
done
# The ELF interpreter path workerd asks for (/lib/ld-linux-*.so.1).
ln -s "$LIBDIR/$LDSO" "$STAGE/usr/lib/$LDSO"

printf 'root:x:0:0:root:/tmp:/sbin/twd-guestd\n' >"$STAGE/etc/passwd"
printf 'root:x:0:\n' >"$STAGE/etc/group"
printf 'twd-guest\n' >"$STAGE/etc/hostname"
printf 'hosts: files\n' >"$STAGE/etc/nsswitch.conf"
printf '127.0.0.1 localhost\n' >"$STAGE/etc/hosts"
: >"$STAGE/etc/resolv.conf" # no DNS: names are resolved by host netd, from the netmap

find "$STAGE" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +

SIZE_KB=$(( $(du -sk "$STAGE" | cut -f1) * 11 / 10 + 8192 ))
IMG=$OUT/rootfs.ext4
rm -f "$IMG"
E2FSPROGS_FAKE_TIME=$SOURCE_DATE_EPOCH mkfs.ext4 -q -F -t ext4 -O ^has_journal,^metadata_csum_seed \
  -U "$FS_UUID" -E "hash_seed=$FS_UUID,root_owner=0:0,lazy_itable_init=0,nodiscard" \
  -L twd-guest -m 0 -b 4096 -I 256 -d "$STAGE" "$IMG" "${SIZE_KB}k"

# mke2fs -d copies each file's ctime, which touch can't set: pin them (and
# the root's) with debugfs so the image depends only on its inputs.
TS=$(date -u -d "@$SOURCE_DATE_EPOCH" +%Y%m%d%H%M%S)
N=$(( $(find "$STAGE" | wc -l) + 16 ))
# By inode number: path lookups in debugfs follow symlinks (/lib, /bin).
# atime too: mke2fs reading the staged files bumps it (relatime).
{ for i in $(seq 2 "$N"); do echo "sif <$i> ctime $TS"; echo "sif <$i> atime $TS"; echo "sif <$i> ctime_extra 0"; echo "sif <$i> atime_extra 0"; done
  echo "sif /lost+found mtime $TS"; echo "sif /lost+found atime $TS"; echo "sif /lost+found crtime $TS"
} >"$STAGE.debugfs"
E2FSPROGS_FAKE_TIME=$SOURCE_DATE_EPOCH debugfs -w -f "$STAGE.debugfs" "$IMG" >/dev/null 2>&1
rm -f "$STAGE.debugfs"

sha() { sha256sum "$1" | cut -d' ' -f1; }
cat >"$OUT/manifest.json" <<EOF
{
  "rootfs": "rootfs.ext4",
  "rootfs_sha256": "$(sha "$IMG")",
  "size_bytes": $(stat -c %s "$IMG"),
  "source_date_epoch": $SOURCE_DATE_EPOCH,
  "workerd_version": "$WORKERD_VERSION",
  "workerd_sha256": "$(sha "$WORKERD")",
  "twd_guestd_sha256": "$(sha "$STAGE/sbin/twd-guestd")",
  "glibc_from": "$BASE_ROOTFS",
  "libc_sha256": "$(sha "$STAGE/usr/lib/$LIBDIR/libc.so.6")"
}
EOF
echo "built $IMG ($(du -h "$IMG" | cut -f1)), sha256 $(sha "$IMG")"
