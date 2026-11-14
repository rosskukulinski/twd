#!/usr/bin/env bash
# Prepare the E2E-CONTAINER apps (ADR-032 acceptance) in OUT:
#   OUT/v0min   sandbox-sdk examples/minimal from the v0 branch (Sandbox SDK 0.12.14)
#   OUT/v1min   sandbox-sdk examples/minimal at @cloudflare/sandbox@1.0.1
#   OUT/raw10   e2e/container/apps/raw10 (raw ctx.container + Files, SDK 1.0.1)
# Every upstream example file is copied byte for byte and checked against
# its git blob (OUT/unmodified.txt). twd adds only what `twd deploy` would:
# a twd.jsonc pinning each class's image to the ref the images were built
# as (guest/images/build-sandbox-images.sh), plus node_modules.
set -euo pipefail
OUT=${1:?usage: e2e-container-prep.sh OUT_DIR}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
REPO=${SANDBOX_SDK_REPO:-https://github.com/cloudflare/sandbox-sdk}
V0_EXAMPLES_COMMIT=b9231019e76896571cd35bd6a7982fdc73d6d6e7 # branch v0 (2026-10-22)
V101_COMMIT=8a535b3ce8d29ffd321a625da02594ba449c1b51        # tag @cloudflare/sandbox@1.0.1

rm -rf "$OUT"
mkdir -p "$OUT"
SRC=$(mktemp -d /tmp/twd-sbx-src.XXXXXX)
trap 'rm -rf "$SRC"' EXIT
git clone -q --filter=blob:none "$REPO" "$SRC"
git -C "$SRC" fetch -q origin "$V0_EXAMPLES_COMMIT"

copy_example() { # commit dest
  local commit=$1 dest=$2
  mkdir -p "$dest"
  git -C "$SRC" archive "$commit" examples/minimal | tar -x -C "$dest" --strip-components=2
  git -C "$SRC" ls-tree -r "$commit" examples/minimal | while read -r _ _ blob path; do
    rel=${path#examples/minimal/}
    got=$(git hash-object "$dest/$rel")
    [ "$got" = "$blob" ] || { echo "$dest/$rel differs from upstream $commit:$path" >&2; exit 1; }
    echo "$commit  $path  blob $blob  == $(basename "$dest")/$rel" >>"$OUT/unmodified.txt"
  done
}
copy_example "$V0_EXAMPLES_COMMIT" "$OUT/v0min"
copy_example "$V101_COMMIT" "$OUT/v1min"
cp -R "$ROOT/e2e/container/apps/raw10" "$OUT/raw10"

# Pins (what twd deploy writes after building ./Dockerfile). The 0.x app
# runs its Sandbox class in facet mode (the default); the 1.0 example in
# plain-DO mode, so both modes run the SDKs.
cat >"$OUT/v0min/twd.jsonc" <<'EOF'
{ "containers": { "Sandbox": { "image": "twd.local/e2e/sandbox-minimal-v0:1" } } }
EOF
cat >"$OUT/v1min/twd.jsonc" <<'EOF'
{ "containers": { "Sandbox": { "images": { "sandbox": "twd.local/e2e/sandbox-minimal-v1:1" } } },
  "tailnet": { "objects": { "Sandbox": { "egress": "workload" } } } }
EOF

# Dependencies, without lifecycle scripts, installed next to (not into)
# each example and linked as node_modules, so no example file changes.
# The v0 example lists the SDK under devDependencies (beside wrangler and
# typescript, which a twd build doesn't need): its pinned 0.12.14.
deps() { # app spec...
  local app=$1
  shift
  mkdir -p "$OUT/.deps/$app"
  (cd "$OUT/.deps/$app" && echo '{"private":true}' >package.json && npm install --no-audit --no-fund --ignore-scripts "$@" >/dev/null)
  ln -s "$OUT/.deps/$app/node_modules" "$OUT/$app/node_modules"
}
deps v0min @cloudflare/sandbox@0.12.14
# shellcheck disable=SC2046
deps v1min $(node -p "Object.entries(require('$OUT/v1min/package.json').dependencies).map(([k,v])=>k+'@'+v).join(' ')")
(cd "$OUT/raw10" && npm install --no-audit --no-fund --ignore-scripts >/dev/null)
echo "prepared $OUT"
cat "$OUT/unmodified.txt"
