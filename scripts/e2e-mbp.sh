#!/bin/sh
# e2e-mbp.sh <ssh host> <go test args…>: runs the e2e-mbp Go test, and on
# exit (pass, fail, timeout or ^C) makes sure nothing of the run is left on
# the remote Mac: twd down --purge if still running, stop twd processes,
# uninstall, and remove ~/.twd-e2e (with the API key file). The Go test does
# the same in t.Cleanup; this is the backstop for a killed test process.
host=$1
shift
cleanup() {
	ssh -o BatchMode=yes -o ConnectTimeout=15 "$host" '
		[ -d ~/.twd-e2e ] || exit 0
		echo "e2e-mbp trap: cleaning ~/.twd-e2e on $(hostname -s)"
		if [ -x ~/.twd/bin/twd ] && [ -f ~/.twd-e2e/state/supervisor.pid ] && kill -0 "$(cat ~/.twd-e2e/state/supervisor.pid)" 2>/dev/null; then
			~/.twd/bin/twd --state-dir ~/.twd-e2e/state down --purge >/dev/null 2>&1 || true
		fi
		pkill -TERM -f "$HOME/.twd/bin/" 2>/dev/null; sleep 0.5; pkill -KILL -f "$HOME/.twd/bin/" 2>/dev/null
		u=$(ls -d ~/.twd-e2e/twd-*/install.sh 2>/dev/null | head -1)
		[ -n "$u" ] && "$u" --uninstall --purge --force >/dev/null
		rm -rf ~/.twd-e2e
		true' || echo "e2e-mbp trap: ssh $host failed: check ~/.twd-e2e on it by hand" >&2
}
trap cleanup EXIT
trap 'exit 130' INT TERM
go test "$@"
