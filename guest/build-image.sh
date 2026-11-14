#!/usr/bin/env bash
# Build the twd microVM guest rootfs (Linux only; no root needed).
#
#   TARGETARCH=amd64|arm64 guest/build-image.sh [OUT_DIR]   (default: this
#   machine's arch, guest/out/<arch>)
#
# Inputs (all pinned in guest/inputs.sh or the lockfile, so the output is
# byte-for-byte reproducible, and either arch can be built on either arch):
#   - workerd: the npm package @cloudflare/workerd-linux-<arch> at the exact
#     version in package-lock.json (node_modules/ after `npm ci` when it is
#     there, else `npm pack`, sha512-checked against the lockfile);
#   - twd-guestd: built from this repo (CGO off, -trimpath, no build ID);
#   - glibc: the shared libraries workerd needs, copied from the Ubuntu
#     24.04 base rootfs tarball for the arch (sha256-pinned). glibc 2.39 >=
#     workerd's floor. BASE_ROOTFS=<dir> uses an extracted rootfs instead
#     (and its CA bundle);
#   - the CA bundle: built from Ubuntu's ca-certificates .deb (sha256-pinned).
#
# The image is an ext4 without a journal, every mtime is SOURCE_DATE_EPOCH,
# owners are 0:0, and the UUID and hash seed are fixed. It is mounted
# read-only by every VM (runtime state is tmpfs in the guest). Output:
#   OUT_DIR/rootfs.ext4, OUT_DIR/manifest.json (input + output hashes).
#
# There is no tailscale, no tsnet and no key material in the image; the e2e
# suite scans it (and guest memory) to check that.
set -euo pipefail
umask 022 # modes in the image don't depend on the builder's umask

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BASE_ROOTFS=${BASE_ROOTFS:-}
SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-1762560000} # 2025-11-08T00:00:00Z
FS_UUID=7b1d6a8e-2c3f-4e5a-9b0c-7d7700000008
GO=${GO:-go}
. "$ROOT/guest/inputs.sh"
guest_arch "${TARGETARCH:-}"
OUT=${1:-$ROOT/guest/out/$GARCH}
[ "$(uname -s)" = Linux ] || { echo "build-image: Linux only (run it in Lima)" >&2; exit 1; }
for t in readelf mkfs.ext4 debugfs sha256sum node openssl; do
  command -v $t >/dev/null || { echo "build-image: needs $t" >&2; exit 1; }
done

WORK=$(mktemp -d /tmp/twd-guest-work.XXXXXX)
STAGE=$WORK/stage
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$OUT" "$STAGE"

# elf_machine FILE: the ELF e_machine as Go's arch name.
elf_machine() {
  case $(readelf -h "$1" | sed -n 's/^ *Machine: *//p') in
    "Advanced Micro Devices X86-64") echo amd64 ;;
    AArch64) echo arm64 ;;
    *) echo unknown ;;
  esac
}

# workerd for the target arch: node_modules when npm installed it (the
# native arch), else the lockfile's tarball via npm pack, integrity-checked.
PKG=@cloudflare/workerd-linux-$NPMARCH
WORKERD_VERSION=$(node -p "require('$ROOT/package-lock.json').packages['node_modules/$PKG'].version")
WORKERD=$ROOT/node_modules/$PKG/bin/workerd
if [ ! -x "$WORKERD" ]; then
  want=$(node -p "require('$ROOT/package-lock.json').packages['node_modules/$PKG'].integrity")
  tgz=$GUEST_CACHE/workerd-linux-$NPMARCH-$WORKERD_VERSION.tgz
  mkdir -p "$GUEST_CACHE"
  if [ ! -f "$tgz" ]; then
    (cd "$WORK" && npm pack --silent "$PKG@$WORKERD_VERSION" >/dev/null) || { echo "build-image: npm pack $PKG@$WORKERD_VERSION failed" >&2; exit 1; }
    mv "$WORK/cloudflare-workerd-linux-$NPMARCH-$WORKERD_VERSION.tgz" "$tgz"
  fi
  got="sha512-$(openssl dgst -sha512 -binary "$tgz" | openssl base64 -A)"
  [ "$got" = "$want" ] || { rm -f "$tgz"; echo "build-image: $PKG integrity mismatch ($got != $want)" >&2; exit 1; }
  mkdir -p "$WORK/workerd"
  tar -xzf "$tgz" -C "$WORK/workerd" package/bin/workerd
  WORKERD=$WORK/workerd/package/bin/workerd
fi
[ "$(elf_machine "$WORKERD")" = "$GARCH" ] || { echo "build-image: $WORKERD is not $GARCH" >&2; exit 1; }

# The base rootfs: only usr/lib/<triplet> of the pinned ubuntu-base tarball.
if [ -n "$BASE_ROOTFS" ]; then
  GLIBC_FROM=$BASE_ROOTFS
else
  eval "base_sha=\$UBUNTU_BASE_SHA256_$GARCH"
  base_tgz=$(fetch "$UBUNTU_BASE_URL/ubuntu-base-$UBUNTU_BASE_VERSION-base-$GARCH.tar.gz" "$base_sha")
  BASE_ROOTFS=$WORK/base
  mkdir -p "$BASE_ROOTFS"
  tar -xzf "$base_tgz" -C "$BASE_ROOTFS" --no-same-owner --no-same-permissions "usr/lib/$LIBDIR"
  GLIBC_FROM="ubuntu-base-$UBUNTU_BASE_VERSION-base-$GARCH.tar.gz sha256:$base_sha"
fi
[ -d "$BASE_ROOTFS/usr/lib/$LIBDIR" ] || { echo "BASE_ROOTFS=$BASE_ROOTFS has no usr/lib/$LIBDIR" >&2; exit 1; }

# twd-guestd: static, reproducible.
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=$GARCH GOFLAGS=-mod=mod \
  $GO build -trimpath -buildvcs=false -ldflags "-s -w -buildid=" -o "$STAGE/sbin/twd-guestd" ./guest/twd-guestd)
[ "$(elf_machine "$STAGE/sbin/twd-guestd")" = "$GARCH" ] || { echo "build-image: twd-guestd is not $GARCH" >&2; exit 1; }

mkdir -p "$STAGE"/{usr/bin,usr/lib/$LIBDIR,dev,proc,sys,run,tmp,etc}
ln -s usr/lib "$STAGE/lib"
ln -s usr/bin "$STAGE/bin"
install -m 0755 "$WORKERD" "$STAGE/usr/bin/workerd"

# The ELF interpreter workerd asks for: /lib/ld-linux-aarch64.so.1 on arm64,
# /lib64/ld-linux-x86-64.so.2 on amd64 (the x86-64 psABI path).
INTERP=$(readelf -l "$WORKERD" | sed -n 's/.*Requesting program interpreter: \(.*\)]/\1/p')
LDSO=${INTERP##*/}
case $GARCH:$INTERP in
  arm64:/lib/ld-linux-aarch64.so.1)
    ln -s "$LIBDIR/$LDSO" "$STAGE/usr/lib/$LDSO" ;;
  amd64:/lib64/ld-linux-x86-64.so.2)
    mkdir -p "$STAGE/usr/lib64"
    ln -s usr/lib64 "$STAGE/lib64"
    ln -s "../lib/$LIBDIR/$LDSO" "$STAGE/usr/lib64/$LDSO" ;;
  *) echo "build-image: unexpected ELF interpreter '$INTERP' for $GARCH" >&2; exit 1 ;;
esac

# The shared libraries workerd links (glibc only), from the base rootfs.
for lib in $(readelf -d "$WORKERD" | sed -n 's/.*Shared library: \[\(.*\)\]/\1/p') $LDSO; do
  src=$BASE_ROOTFS/usr/lib/$LIBDIR/$lib
  [ -e "$src" ] || { echo "missing $lib in $BASE_ROOTFS" >&2; exit 1; }
  cp -L "$src" "$STAGE/usr/lib/$LIBDIR/$lib"
  chmod 0755 "$STAGE/usr/lib/$LIBDIR/$lib"
  [ "$(elf_machine "$STAGE/usr/lib/$LIBDIR/$lib")" = "$GARCH" ] || { echo "build-image: $lib is not $GARCH" >&2; exit 1; }
done

printf 'root:x:0:0:root:/tmp:/sbin/twd-guestd\n' >"$STAGE/etc/passwd"
printf 'root:x:0:\n' >"$STAGE/etc/group"
printf 'twd-guest\n' >"$STAGE/etc/hostname"
printf 'hosts: files\n' >"$STAGE/etc/nsswitch.conf"
printf '127.0.0.1 localhost\n' >"$STAGE/etc/hosts"
# workerd refuses to start without a system CA store (it is unused: https
# egress is originated by host netd). With BASE_ROOTFS, its bundle; else the
# Mozilla certificates of the pinned ca-certificates .deb, concatenated in
# C-locale name order (what update-ca-certificates ships, modulo order).
mkdir -p "$STAGE/etc/ssl/certs"
CA=$STAGE/etc/ssl/certs/ca-certificates.crt
if [ -f "$BASE_ROOTFS/etc/ssl/certs/ca-certificates.crt" ]; then
  cp -L "$BASE_ROOTFS/etc/ssl/certs/ca-certificates.crt" "$CA"
  CA_FROM=$GLIBC_FROM
else
  deb=$(fetch "$CA_DEB_URL" "$CA_DEB_SHA256")
  mkdir -p "$WORK/ca"
  if command -v dpkg-deb >/dev/null; then
    dpkg-deb --fsys-tarfile "$deb" | tar -xf - -C "$WORK/ca" ./usr/share/ca-certificates/mozilla
  else
    (cd "$WORK/ca" && ar x "$deb" data.tar.zst) && zstd -dc "$WORK/ca/data.tar.zst" | tar -xf - -C "$WORK/ca" ./usr/share/ca-certificates/mozilla
  fi
  n=0
  : >"$CA"
  while IFS= read -r c; do cat "$c" >>"$CA"; n=$((n + 1)); done < <(find "$WORK/ca/usr/share/ca-certificates/mozilla" -name '*.crt' | LC_ALL=C sort)
  [ "$n" -gt 50 ] || { echo "build-image: only $n CA certificates in $CA_DEB" >&2; exit 1; }
  CA_FROM="$CA_DEB sha256:$CA_DEB_SHA256 ($n certificates)"
fi
chmod 0644 "$CA"
: >"$STAGE/etc/resolv.conf" # no DNS: names are resolved by host netd, from the netmap

find "$STAGE" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +
# ASCII names only (see the directory-hash flag below).
if (cd "$STAGE" && find . | LC_ALL=C grep -q '[^ -~]'); then
  echo "build-image: non-ASCII file names in the image" >&2; exit 1
fi

SIZE_KB=$(( $(du -sk "$STAGE" | cut -f1) * 11 / 10 + 8192 ))
IMG=$OUT/rootfs.ext4
rm -f "$IMG"
E2FSPROGS_FAKE_TIME=$SOURCE_DATE_EPOCH mkfs.ext4 -q -F -t ext4 -O ^has_journal,^metadata_csum_seed \
  -U "$FS_UUID" -E "hash_seed=$FS_UUID,root_owner=0:0,lazy_itable_init=0,nodiscard" \
  -L twd-guest -m 0 -b 4096 -I 256 -d "$STAGE" "$IMG" "${SIZE_KB}k"

# mke2fs -d copies each file's ctime, which touch can't set, and its owner
# (root_owner covers only /): pin them (and the root's) with debugfs so the
# image depends only on its inputs, not on the builder's uid.
TS=$(date -u -d "@$SOURCE_DATE_EPOCH" +%Y%m%d%H%M%S)
N=$(( $(find "$STAGE" | wc -l) + 16 ))
# By inode number: path lookups in debugfs follow symlinks (/lib, /bin).
# atime too: mke2fs reading the staged files bumps it (relatime).
{ for i in $(seq 2 "$N"); do echo "sif <$i> ctime $TS"; echo "sif <$i> atime $TS"; echo "sif <$i> ctime_extra 0"; echo "sif <$i> atime_extra 0"; echo "sif <$i> uid 0"; echo "sif <$i> gid 0"; done
  echo "sif /lost+found mtime $TS"; echo "sif /lost+found atime $TS"; echo "sif /lost+found crtime $TS"
  # mke2fs records the builder's char signedness (signed on x86_64, unsigned
  # on arm64) as the directory-hash flavor. The two hash differently only
  # for name bytes >= 0x80, which the image doesn't have (checked above), so
  # pin "unsigned" (EXT2_FLAGS_UNSIGNED_HASH) and an amd64 and an arm64
  # builder make the same image.
  echo "ssv flags 2"
} >"$WORK/debugfs.cmds"
E2FSPROGS_FAKE_TIME=$SOURCE_DATE_EPOCH debugfs -w -f "$WORK/debugfs.cmds" "$IMG" >/dev/null 2>&1
# debugfs writes only the primary superblock; rewriting the label makes
# tune2fs copy it (with the pinned flags) to the backups too.
E2FSPROGS_FAKE_TIME=$SOURCE_DATE_EPOCH tune2fs -L twd-guest "$IMG" >/dev/null

sha() { sha256_of "$1"; }
cat >"$OUT/manifest.json" <<EOF
{
  "rootfs": "rootfs.ext4",
  "arch": "$GARCH",
  "rootfs_sha256": "$(sha "$IMG")",
  "size_bytes": $(stat -c %s "$IMG"),
  "source_date_epoch": $SOURCE_DATE_EPOCH,
  "workerd_version": "$WORKERD_VERSION",
  "workerd_sha256": "$(sha "$WORKERD")",
  "twd_guestd_sha256": "$(sha "$STAGE/sbin/twd-guestd")",
  "glibc_from": "$GLIBC_FROM",
  "libc_sha256": "$(sha "$STAGE/usr/lib/$LIBDIR/libc.so.6")",
  "ca_from": "$CA_FROM",
  "ca_sha256": "$(sha "$CA")"
}
EOF
echo "built $IMG ($GARCH, $(du -h "$IMG" | cut -f1)), sha256 $(sha "$IMG")"
