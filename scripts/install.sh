#!/bin/sh
# twd installer: run from the extracted release directory.
#
#   ./install.sh                 install (or upgrade, or repair) into ~/.twd
#   ./install.sh --prefix DIR    install into DIR instead
#   ./install.sh --uninstall     remove bin/ and share/ (keeps twd state: twd.json, logs, …)
#   ./install.sh --uninstall --purge   also remove the state in the prefix (run `twd down` first)
#
# No sudo, nothing outside the prefix is touched (your shell profile
# included: it prints the PATH line to add). Re-running is safe.
set -eu
PREFIX=${TWD_HOME:-$HOME/.twd}
MODE=install
PURGE=0
FORCE=0
while [ $# -gt 0 ]; do
	case $1 in
	--prefix) PREFIX=$2; shift ;;
	--prefix=*) PREFIX=${1#*=} ;;
	--uninstall) MODE=uninstall ;;
	--purge) PURGE=1 ;;
	--force) FORCE=1 ;;
	-h | --help) sed -n '2,10p' "$0"; exit 0 ;;
	*) echo "install.sh: unknown argument $1" >&2; exit 2 ;;
	esac
	shift
done

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" != 0 ] || die "don't run as root (twd installs per user, into ~/.twd, and needs no sudo)"

running() {
	pf=$PREFIX/supervisor.pid
	[ -f "$pf" ] && kill -0 "$(cat "$pf")" 2>/dev/null
}

if [ "$MODE" = uninstall ]; then
	if running && [ "$FORCE" != 1 ]; then
		die "twd is running from $PREFIX (twd down first, or pass --force)"
	fi
	rm -rf "$PREFIX/bin" "$PREFIX/share" "$PREFIX/VERSION" "$PREFIX/.install-"*
	if [ "$PURGE" = 1 ]; then
		rm -rf "$PREFIX/twd.json" "$PREFIX/logs" "$PREFIX/run" "$PREFIX/mgmt" "$PREFIX/hostd" "$PREFIX/supervisor.pid"
	fi
	rmdir "$PREFIX" 2>/dev/null || true
	if [ -d "$PREFIX" ]; then
		say "✓ uninstalled twd from $PREFIX (state kept: $(ls -A "$PREFIX" | tr '\n' ' '))"
	else
		say "✓ uninstalled twd ($PREFIX removed)"
	fi
	say "  remove the PATH line for $PREFIX/bin from your shell profile if you added one"
	exit 0
fi

SRC=$(cd "$(dirname "$0")" && pwd)
[ -f "$SRC/VERSION" ] && [ -x "$SRC/bin/twd" ] || die "run me from the extracted twd release directory"
VERSION=$(cat "$SRC/VERSION")
t0=$(date +%s)

# Platform check.
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
[ "$arch" = aarch64 ] && arch=arm64
want=$(sed -n 's/.*"os":"\([^"]*\)","arch":"\([^"]*\)".*/\1-\2/p' "$SRC/share/twd/release.json")
[ "$os-$arch" = "$want" ] || die "this release is for $want, this machine is $os-$arch"

# Integrity.
if command -v sha256sum >/dev/null 2>&1; then
	(cd "$SRC" && sha256sum -c --quiet SHA256SUMS) || die "SHA256SUMS check failed: the release is corrupt"
else
	(cd "$SRC" && shasum -a 256 -c --quiet SHA256SUMS) || die "SHA256SUMS check failed: the release is corrupt"
fi

# Copy into a staging dir in the prefix, then swap (re-runs and upgrades).
mkdir -p "$PREFIX"
chmod 700 "$PREFIX"
STAGE=$PREFIX/.install-$$
rm -rf "$STAGE"
mkdir -p "$STAGE"
trap 'rm -rf "$STAGE"' EXIT
cp -R "$SRC/bin" "$SRC/share" "$SRC/VERSION" "$STAGE/"
if [ "$os" = darwin ]; then
	# Release binaries are ad-hoc signed; files fetched by a browser carry
	# com.apple.quarantine (scp/curl don't). Clear it on our copy only.
	xattr -dr com.apple.quarantine "$STAGE" 2>/dev/null || true
fi
# Smoke-test before swapping: the binaries must run on this machine.
"$STAGE/bin/workerd" --version >/dev/null 2>&1 || die "workerd does not run on this machine ($STAGE/bin/workerd --version)"
"$STAGE/bin/twd" version >/dev/null 2>&1 || die "twd does not run on this machine"
for d in bin share; do
	rm -rf "$PREFIX/$d.old"
	[ -e "$PREFIX/$d" ] && mv "$PREFIX/$d" "$PREFIX/$d.old"
	mv "$STAGE/$d" "$PREFIX/$d"
	rm -rf "$PREFIX/$d.old"
done
mv "$STAGE/VERSION" "$PREFIX/VERSION"

say "✓ installed twd $VERSION into $PREFIX ($(($(date +%s) - t0)) s)"
say "  $("$PREFIX/bin/twd" version)"
say "  $("$PREFIX/bin/workerd" --version) · process tier (dev-grade isolation on macOS; microVMs need Linux/KVM)"
case ":$PATH:" in
*":$PREFIX/bin:"*) ;;
*)
	prof="~/.profile"
	case ${SHELL:-} in */zsh) prof="~/.zprofile" ;; */bash) prof="~/.bash_profile" ;; esac
	say ""
	say "Add twd to your PATH (this installer doesn't edit your shell profile):"
	say "  echo 'export PATH=\"$PREFIX/bin:\$PATH\"' >> $prof && export PATH=\"$PREFIX/bin:\$PATH\""
	;;
esac
say ""
say "Next: twd doctor --tailnet <tailnet>.ts.net --api-key-file <file>   then   twd up …   (see INSTALL.md)"
