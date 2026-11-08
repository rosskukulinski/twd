#!/bin/sh
# Fail if any tracked or staged file contains something shaped like a Tailscale key.
cd "$(git rev-parse --show-toplevel)" || exit 1
if git grep -qE 'tskey-(auth|client|api)-[A-Za-z0-9]{8,}-[A-Za-z0-9]{20,}' -- . 2>/dev/null; then
  echo "check-secrets: a tracked file contains a Tailscale key" >&2
  exit 1
fi
echo "check-secrets: ok"
