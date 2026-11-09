#!/bin/sh
# Runs the mgmt Store tests against a throwaway Postgres 17 container (OrbStack docker).
set -u
ctx="${1:-orbstack}"
port="${2:-55433}"
name=twd-test-pg
docker --context "$ctx" rm -f "$name" >/dev/null 2>&1 || true
docker --context "$ctx" run -d --rm --name "$name" -e POSTGRES_PASSWORD=twdtest \
	-p "127.0.0.1:$port:5432" postgres:17-alpine >/dev/null || exit 1
trap 'docker --context "$ctx" rm -f "$name" >/dev/null 2>&1' EXIT
i=0
until docker --context "$ctx" exec "$name" pg_isready -U postgres -h 127.0.0.1 >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -gt 30 ] && { echo "postgres did not start"; exit 1; }
	sleep 1
done
export TWD_TEST_POSTGRES_DSN="postgres://postgres:twdtest@127.0.0.1:$port/postgres?sslmode=disable"
# Store tests on both backends, then the API tests with the Postgres store.
# -p 1 / no t.Parallel: the tests share one database.
GOTOOLCHAIN=local go test -race -count=1 -run 'Lease|Audit|Store|Migrat' -v ./mgmt/ >/tmp/twd-test-pg.log 2>&1
st=$?
TWD_TEST_API_POSTGRES=1 GOTOOLCHAIN=local go test -race -count=1 -run 'API|Kill|WIF|Tailnet|V1' -v ./mgmt/ >>/tmp/twd-test-pg.log 2>&1
st2=$?
[ "$st" -eq 0 ] && st=$st2
grep -E '^(---|    ---|ok|FAIL|panic)' /tmp/twd-test-pg.log
exit $st
