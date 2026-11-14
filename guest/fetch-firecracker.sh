#!/usr/bin/env bash
# Fetch the pinned Firecracker + jailer release binaries for an arch.
#
#   TARGETARCH=amd64|arm64 guest/fetch-firecracker.sh OUT_DIR
#
# The release tarball and both binaries in it are checked against the sha256
# pins in guest/inputs.sh. Output: OUT_DIR/firecracker, OUT_DIR/jailer.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
. "$ROOT/guest/inputs.sh"
guest_arch "${TARGETARCH:-}"
OUT=${1:?usage: fetch-firecracker.sh OUT_DIR}
v=$FIRECRACKER_VERSION
eval "tgz_sha=\$FIRECRACKER_TGZ_SHA256_$GARCH fc_sha=\$FIRECRACKER_SHA256_$GARCH jl_sha=\$JAILER_SHA256_$GARCH"
tgz=$(fetch "$FIRECRACKER_URL/firecracker-v$v-$FCARCH.tgz" "$tgz_sha")

WORK=$(mktemp -d /tmp/twd-fc-$GARCH.XXXXXX)
trap 'rm -rf "$WORK"' EXIT
d=release-v$v-$FCARCH
tar -xzf "$tgz" -C "$WORK" "$d/firecracker-v$v-$FCARCH" "$d/jailer-v$v-$FCARCH"
for b in firecracker:$fc_sha jailer:$jl_sha; do
  name=${b%%:*} want=${b#*:}
  f=$WORK/$d/$name-v$v-$FCARCH
  got=$(sha256_of "$f")
  [ "$got" = "$want" ] || { echo "fetch-firecracker: $name sha256 $got, want $want" >&2; exit 1; }
done
mkdir -p "$OUT"
install -m 0755 "$WORK/$d/firecracker-v$v-$FCARCH" "$OUT/firecracker"
install -m 0755 "$WORK/$d/jailer-v$v-$FCARCH" "$OUT/jailer"
echo "fetch-firecracker: Firecracker v$v + jailer ($GARCH) in $OUT"
