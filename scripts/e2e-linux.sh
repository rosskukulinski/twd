#!/bin/sh
# e2e-linux.sh [distro…]: the E2E-LINUX matrix, at most $TWD_E2E_PARALLEL
# (default 2) VMs at a time; each VM is stopped as soon as its row is done.
# Rows run in the order given (put the slow amd64 one first).
# Exit status: non-zero if any row failed. See scripts/e2e-linux-one.sh.
ROOT=$(cd "$(dirname "$0")/.." && pwd)
: "${TWD_E2E_OUT:=$ROOT/e2e/real/results/linux-$(date +%Y%m%d-%H%M%S)}"
export TWD_E2E_OUT
mkdir -p "$TWD_E2E_OUT"
[ $# -gt 0 ] || set -- ubuntu24-amd64 ubuntu24-arm64 debian12-arm64 ubuntu22-arm64
echo "e2e-linux: rows $* (≤ ${TWD_E2E_PARALLEL:-2} at a time) → $TWD_E2E_OUT"
printf '%s\n' "$@" | xargs -P "${TWD_E2E_PARALLEL:-2}" -n 1 sh "$ROOT/scripts/e2e-linux-one.sh"
rc=$?
echo "e2e-linux: results in $TWD_E2E_OUT"
ls "$TWD_E2E_OUT"/*.json 2>/dev/null | while read -r f; do
	jq -r '[.run, .checks.result, (.numbers.install_ms|tostring), (.numbers.up_ready_ms|tostring), (.numbers.deploy_agent_ready_ms|tostring)] | join(" ")' "$f" 2>/dev/null
done
exit $rc
