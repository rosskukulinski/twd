#!/bin/sh
# RT-03 live attack (Pass 2): start the REAL twd-mgmt binary on its unix
# socket with a SQLite store (no tailnet), then attack the socket the way a
# tenant Node app or an escaped workerd isolate would — a bare same-uid
# AF_UNIX connect with no credential, a guessed token, and the pre-fix
# "Twd-Host-ID" header — and finally with the real token from the 0600 file.
#
# PASS = every unprivileged call is refused (401/403) and only the token
# holder reaches the API. Run inside the rt2 VM:
#   twd host setup not needed; just an installed ~/.twd/bin.
set -eu
BIN=${TWD_BIN:-$HOME/.twd/bin}
STATE=$(mktemp -d /tmp/rt03-XXXXXX)
SOCK="$STATE/mgmt.sock"
TOKEN_FILE="$STATE/mgmt.token"
cleanup() { kill "$MGMT_PID" 2>/dev/null || true; rm -rf "$STATE"; }
trap cleanup EXIT

"$BIN/twd-mgmt" -state-dir "$STATE" -socket "$SOCK" -host-token-file "$TOKEN_FILE" \
	-rotate-host-token -db "$STATE/mgmt.db" >"$STATE/mgmt.log" 2>&1 &
MGMT_PID=$!
# Wait for the socket.
for i in $(seq 1 50); do [ -S "$SOCK" ] && break; sleep 0.1; done
[ -S "$SOCK" ] || { echo "FAIL: mgmt did not create its socket"; cat "$STATE/mgmt.log"; exit 1; }

echo "== socket perms (expect 0600 in a 0700 dir) =="
ls -ld "$STATE" "$SOCK"
echo "== host token file perms (expect 0600) =="
ls -l "$TOKEN_FILE"

say() { printf '\n--- %s ---\n' "$1"; }
# curl supports --unix-socket; the host part of the URL is ignored.
req() { curl -s -o /tmp/rt03.body -w '%{http_code}' --unix-socket "$SOCK" "$@" "http://mgmt$1" 2>/dev/null || echo 000; }

fail=0
check() { # desc expected_code
	code=$(cat /tmp/rt03.code)
	if [ "$code" != "$2" ]; then echo "  FAIL($1): got $code want $2; body: $(cat /tmp/rt03.body)"; fail=1
	else echo "  ok($1): $code"; fi
}

say "1. bare same-uid connect, no credential -> must be 401 host_token_required"
curl -s -o /tmp/rt03.body -w '%{http_code}' --unix-socket "$SOCK" http://mgmt/v0/tenants >/tmp/rt03.code 2>/dev/null; check tenants-nocred 401
grep -q tn_ /tmp/rt03.body && { echo "  FAIL: tenant data leaked without a token"; fail=1; }

say "2. guessed/forged token -> must be 403 host_token_invalid"
curl -s -o /tmp/rt03.body -w '%{http_code}' -H "Twd-Host-Token: $(printf 'a%.0s' $(seq 1 64))" --unix-socket "$SOCK" http://mgmt/v0/tenants >/tmp/rt03.code 2>/dev/null; check tenants-badtoken 403

say "3. pre-fix 'I am a host' header alone -> must still be 401"
curl -s -o /tmp/rt03.body -w '%{http_code}' -H "Twd-Host-ID: h1" --unix-socket "$SOCK" http://mgmt/v0/tenants >/tmp/rt03.code 2>/dev/null; check tenants-hostid 401

say "4. even /v0/healthz is refused without the token"
curl -s -o /tmp/rt03.body -w '%{http_code}' --unix-socket "$SOCK" http://mgmt/v0/healthz >/tmp/rt03.code 2>/dev/null; check healthz-nocred 401

say "5. the real token (hostd / CLI read the same 0600 file) works"
TOK=$(cat "$TOKEN_FILE")
curl -s -o /tmp/rt03.body -w '%{http_code}' -H "Twd-Host-Token: $TOK" --unix-socket "$SOCK" http://mgmt/v0/tenants >/tmp/rt03.code 2>/dev/null; check tenants-token 200

echo
if [ "$fail" = 0 ]; then echo "RT-03 LIVE: HOLDS — the mgmt socket admits nobody without the per-host token."; else echo "RT-03 LIVE: FINDING — see failures above."; fi
exit $fail
