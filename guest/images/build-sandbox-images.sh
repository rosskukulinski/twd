#!/usr/bin/env bash
# Build twd's arm64 stand-ins for Cloudflare's amd64-only sandbox images
# (ADR-032 D3) and the images the container e2e runs, then `docker save`
# them into OUT_DIR with an images.json index (hostd/image.DirSource).
#
#   guest/images/build-sandbox-images.sh OUT_DIR
#
# Runs on the developer machine (macOS with OrbStack/Docker Desktop, or
# Linux with docker buildx): this is the "build on the developer's machine"
# side of ADR-032 D3. twd hosts never run a container engine.
#
# Inputs (pinned; see guest/images/README.md):
#   - github.com/cloudflare/sandbox-sdk (Apache-2.0) at the release tags
#     below, cloned fresh into a temp dir and treated as data: only its
#     Dockerfiles run, inside BuildKit;
#   - docker.io/library/debian:trixie (the stand-in for Cloudflare's
#     non-public cloudflare/debian-trixie base) and alpine:3.23.
#
# Outputs (twd.local/... names are what DefaultAliases point at):
#   twd.local/cloudflare-sandbox:0.12.14-arm64   packages/sandbox/Dockerfile, target "default"
#   twd.local/cloudflare-sandbox:1.0.0-arm64     images/sandbox-tools/Dockerfile, target "image" (sandbox-shim)
#   twd.local/cloudflare-sandbox:1.0.1-arm64     same, at 1.0.1
#   twd.local/debian-trixie-python:1             debian:trixie + python3 + the 1.0.1 sandbox-shim
#   twd.local/e2e/sandbox-minimal-v0:1           sandbox-sdk examples/minimal (v0 branch) Dockerfile, unmodified,
#                                                with FROM cloudflare/sandbox:0.12.14 aliased to the arm64 rebuild
#   twd.local/e2e/sandbox-minimal-v1:1           sandbox-sdk examples/minimal at @cloudflare/sandbox@1.0.1, unmodified,
#                                                with its shim donor aliased likewise
#
# PLATFORM=linux/amd64 (x86-64 hosts; Macs with Rosetta for Linux, twd-hostd
# -vm-rosetta) builds nothing of Cloudflare's: it pulls the STOCK
# docker.io/cloudflare/sandbox:0.12.14, 1.0.0 and 1.0.1 as published and
# saves them under those refs; the three twd.local images above are built
# for amd64 with their FROM lines resolving to the stock images (no alias).
# IMAGE-IDS records the image IDs that were pulled.
set -euo pipefail

OUT=${1:?usage: build-sandbox-images.sh OUT_DIR}
PLATFORM=${PLATFORM:-linux/arm64}
ARCH=${PLATFORM#linux/}
DOCKER=${DOCKER:-docker}
REPO=${SANDBOX_SDK_REPO:-https://github.com/cloudflare/sandbox-sdk}
V0_TAG=@cloudflare/sandbox@0.12.14
V0_COMMIT=b96de6ea1ca248e11a56a78f1efabc2717d6c7b9
V100_TAG=@cloudflare/sandbox@1.0.0
V100_COMMIT=f9e972a14123bef3b93e9bb337bab6e83419fe7e
V101_TAG=@cloudflare/sandbox@1.0.1
V101_COMMIT=8a535b3ce8d29ffd321a625da02594ba449c1b51
# The v0 branch's examples/minimal (the 0.x acceptance app, ADR-032 D1).
V0_EXAMPLES_COMMIT=b9231019e76896571cd35bd6a7982fdc73d6d6e7
HERE=$(cd "$(dirname "$0")" && pwd)

mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
WORK=$(mktemp -d /tmp/twd-sbx-build.XXXXXX)
trap 'rm -rf "$WORK"' EXIT

git clone -q --filter=blob:none "$REPO" "$WORK/src"
export_tree() { # tag commit dir
  local got
  got=$(git -C "$WORK/src" rev-parse "$1^{commit}")
  [ "$got" = "$2" ] || { echo "tag $1 is $got, want $2" >&2; exit 1; }
  mkdir -p "$3"
  git -C "$WORK/src" archive "$2" | tar -x -C "$3"
}
export_tree "$V0_TAG" "$V0_COMMIT" "$WORK/v0"
# The one source change: packages/sandbox-container/build.ts hard-codes
# `bun build --compile --target=bun-linux-x64(-musl)` for the control
# server binary (/container-server/sandbox), so an arm64 image build would
# still ship an x86-64 binary. Target the image's architecture instead.
if [ "$ARCH" != amd64 ]; then
  perl -pi -e "s/--target=bun-linux-x64/--target=bun-linux-$ARCH/g" "$WORK/v0/packages/sandbox-container/build.ts"
  grep -q "bun-linux-$ARCH" "$WORK/v0/packages/sandbox-container/build.ts" || { echo "build.ts patch failed" >&2; exit 1; }
fi
export_tree "$V100_TAG" "$V100_COMMIT" "$WORK/v100"
export_tree "$V101_TAG" "$V101_COMMIT" "$WORK/v101"
git -C "$WORK/src" fetch -q origin "$V0_EXAMPLES_COMMIT"
mkdir -p "$WORK/v0ex" && git -C "$WORK/src" archive "$V0_EXAMPLES_COMMIT" examples/minimal | tar -x -C "$WORK/v0ex"

build() { # tag context [args...]
  local tag=$1 ctx=$2
  shift 2
  echo "== $tag"
  $DOCKER buildx build --platform "$PLATFORM" --provenance=false -t "$tag" --load "$@" "$ctx"
}

IMAGES=() # "ref file" pairs for images.json
if [ "$ARCH" = amd64 ]; then
  # linux/amd64 (x86-64 hosts, and Apple silicon with Rosetta for Linux):
  # Cloudflare's own images, pulled as published. No rebuild, no alias:
  # the apps' FROM lines resolve to the stock images.
  for v in 0.12.14 1.0.0 1.0.1; do
    echo "== pull docker.io/cloudflare/sandbox:$v"
    $DOCKER pull -q --platform "$PLATFORM" "docker.io/cloudflare/sandbox:$v"
    IMAGES+=("docker.io/cloudflare/sandbox:$v cloudflare-sandbox-$v-$ARCH.tar")
  done
  build twd.local/debian-trixie-python:1 "$HERE" -f "$HERE/Dockerfile.debian-trixie-python"
  build twd.local/e2e/sandbox-minimal-v0:1 "$WORK/v0ex/examples/minimal"
  build twd.local/e2e/sandbox-minimal-v1:1 "$WORK/v101/examples/minimal"
else
  # 0.12.14: the Bun control server image, as Cloudflare's CI builds it
  # (docker-bake.hcl "default" target; BUN/NODE versions from that tag).
  build twd.local/cloudflare-sandbox:0.12.14-$ARCH "$WORK/v0" -f "$WORK/v0/packages/sandbox/Dockerfile" --target default \
    --build-arg SANDBOX_VERSION=0.12.14 --build-arg BUN_VERSION="$(cat "$WORK/v0/.bun-version")" --build-arg NODE_VERSION=22.23.4
  # 1.0.x: the sandbox-shim donor image (static Rust binary; the Dockerfile's
  # "verify" stage asserts x86-64, so it builds the "image" target directly).
  build twd.local/cloudflare-sandbox:1.0.0-$ARCH "$WORK/v100" -f "$WORK/v100/images/sandbox-tools/Dockerfile" --target image
  build twd.local/cloudflare-sandbox:1.0.1-$ARCH "$WORK/v101" -f "$WORK/v101/images/sandbox-tools/Dockerfile" --target image
  # The raw ctx.container flow's image.
  build twd.local/debian-trixie-python:1 "$HERE" -f "$HERE/Dockerfile.debian-trixie-python" \
    --build-context "docker.io/cloudflare/sandbox:1.0.1=docker-image://twd.local/cloudflare-sandbox:1.0.1-$ARCH"
  # The acceptance apps' own Dockerfiles, unmodified: the alias map applied
  # as BuildKit named contexts (FROM <ref> resolves to the rebuild).
  build twd.local/e2e/sandbox-minimal-v0:1 "$WORK/v0ex/examples/minimal" \
    --build-context "docker.io/cloudflare/sandbox:0.12.14=docker-image://twd.local/cloudflare-sandbox:0.12.14-$ARCH"
  build twd.local/e2e/sandbox-minimal-v1:1 "$WORK/v101/examples/minimal" \
    --build-context "docker.io/cloudflare/sandbox:1.0.0=docker-image://twd.local/cloudflare-sandbox:1.0.0-$ARCH"
  for v in 0.12.14 1.0.0 1.0.1; do
    IMAGES+=("twd.local/cloudflare-sandbox:$v-$ARCH cloudflare-sandbox-$v-$ARCH.tar")
  done
fi
IMAGES+=("twd.local/debian-trixie-python:1 debian-trixie-python.tar"
  "twd.local/e2e/sandbox-minimal-v0:1 sandbox-minimal-v0.tar"
  "twd.local/e2e/sandbox-minimal-v1:1 sandbox-minimal-v1.tar")

save() { # ref file
  echo "== save $1"
  $DOCKER save --platform "$PLATFORM" -o "$OUT/$2" "$1" 2>/dev/null || $DOCKER save -o "$OUT/$2" "$1"
}
{
  echo "{"
  n=0
  for e in "${IMAGES[@]}"; do
    ref=${e% *} file=${e#* }
    save "$ref" "$file" >&2
    n=$((n + 1))
    sep=,
    [ "$n" = "${#IMAGES[@]}" ] && sep=
    printf '  "%s": "%s"%s\n' "$ref" "$file" "$sep"
  done
  echo "}"
} >"$OUT/images.json.tmp"
mv "$OUT/images.json.tmp" "$OUT/images.json"
cat "$OUT/images.json"
for e in "${IMAGES[@]}"; do
  ref=${e% *}
  printf '%s  %s\n' "$($DOCKER image inspect --format '{{.Id}}' "$ref")" "$ref"
done | tee "$OUT/IMAGE-IDS"
for f in "$OUT"/*.tar; do
  printf '%s  %s\n' "$(shasum -a 256 "$f" | cut -d' ' -f1)" "$(basename "$f")"
done | tee "$OUT/SHA256SUMS"
