#!/bin/sh
# release.sh <version> [targets…]: self-contained twd release tarballs.
#
#   dist/twd-<version>-<os>-<arch>.tar.gz   (default targets: darwin-arm64 linux-arm64)
#   dist/SHA256SUMS
#
# Each tarball holds: bin/{twd,twd-mgmt,twd-hostd,twd-netd} (CGO_ENABLED=0,
# esbuild built into twd), bin/workerd (the pinned npm
# @cloudflare/workerd-<os>-<arch>, integrity-checked against
# package-lock.json), share/twd/create-twd (templates + build.mjs),
# share/twd/node_modules (the templates' runtime deps, vendored: deploy works
# offline, no npm/node), install.sh, VERSION, SHA256SUMS, INSTALL.md.
# darwin binaries are ad-hoc code-signed (codesign -s -; they are unsigned
# otherwise and arm64 macOS refuses to run unsigned code).
set -eu
VERSION=$1
shift
TARGETS=${*:-darwin-arm64 linux-arm64}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
DIST=$ROOT/dist
CACHE=$DIST/cache
cd "$ROOT"
export GOTOOLCHAIN=local
WORKERD_VERSION=$(node -p 'require("./package-lock.json").packages["node_modules/workerd"].version')
mkdir -p "$DIST" "$CACHE"

# The JS packages must be built (dist/ is gitignored).
for p in tailnet mcp; do
	[ -f "js/packages/$p/dist/index.js" ] || { echo "release: js/packages/$p/dist missing (make js-build)"; exit 1; }
done

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }

# vendored node_modules (same for every target: pure JS)
VNM=$CACHE/vendor
rm -rf "$VNM"
# Roots: what the templates need to init + deploy offline (scripts/template-roots.mjs).
ROOTS=$(node scripts/template-roots.mjs js/packages/create-twd/templates | tr '\n' ' ')
echo "release: vendoring the templates' deps: $ROOTS"
node scripts/vendor-node-modules.mjs "$ROOT" "$VNM/node_modules" $ROOTS >"$CACHE/vendor.json"
echo "release: vendored node_modules $(cat "$CACHE/vendor.json")"

for t in $TARGETS; do
	os=${t%-*}
	arch=${t#*-}
	name=twd-$VERSION-$os-$arch
	st=$DIST/stage/$name
	rm -rf "$st"
	mkdir -p "$st/bin" "$st/share/twd"
	echo "release: $name"

	# Go binaries
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
		-o "$st/bin/" ./cmd/twd ./cmd/twd-mgmt ./cmd/twd-hostd ./cmd/twd-netd

	# workerd from the npm platform package, checked against the lockfile.
	pkg=@cloudflare/workerd-$os-$arch
	tgz=$CACHE/workerd-$os-$arch-$WORKERD_VERSION.tgz
	want=$(node -p "require('./package-lock.json').packages['node_modules/$pkg'].integrity")
	if [ ! -f "$tgz" ]; then
		(cd "$CACHE" && npm pack --silent "$pkg@$WORKERD_VERSION" >/dev/null && mv "cloudflare-workerd-$os-$arch-$WORKERD_VERSION.tgz" "$tgz")
	fi
	got="sha512-$(openssl dgst -sha512 -binary "$tgz" | openssl base64 -A)"
	[ "$got" = "$want" ] || { echo "release: $pkg integrity mismatch ($got != $want)"; exit 1; }
	tar -xzf "$tgz" -C "$CACHE" package/bin/workerd
	mv "$CACHE/package/bin/workerd" "$st/bin/workerd"
	rm -rf "$CACHE/package"
	chmod 755 "$st/bin/"*

	# templates + bundler sources, and the vendored modules
	mkdir -p "$st/share/twd/create-twd"
	cp -R js/packages/create-twd/package.json js/packages/create-twd/bin js/packages/create-twd/lib js/packages/create-twd/templates "$st/share/twd/create-twd/"
	find "$st/share/twd/create-twd/templates" -name node_modules -prune -exec rm -rf {} + 2>/dev/null || true
	find "$st/share/twd/create-twd/templates" -name dist -type d -prune -exec rm -rf {} + 2>/dev/null || true
	cp -R "$VNM/node_modules" "$st/share/twd/node_modules"

	cp scripts/install.sh "$st/install.sh"
	chmod 755 "$st/install.sh"
	cp docs/INSTALL.md "$st/INSTALL.md"
	printf '%s\n' "$VERSION" >"$st/VERSION"
	cat >"$st/share/twd/release.json" <<EOF
{"version":"$VERSION","os":"$os","arch":"$arch","workerd":"$WORKERD_VERSION","esbuild":"built into twd","sandbox":"$( [ "$os" = darwin ] && echo seatbelt || echo none )","commit":"$(git rev-parse HEAD)","built":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
EOF

	if [ "$os" = darwin ]; then
		if [ "$(uname -s)" != Darwin ]; then
			echo "release: darwin binaries must be ad-hoc signed on macOS (codesign)"; exit 1
		fi
		for b in "$st/bin/"*; do
			codesign --force --sign - "$b"
			codesign --verify "$b"
		done
		# workerd must run here (the native target).
		if [ "$(uname -m)" = "$arch" ]; then
			"$st/bin/workerd" --version
			"$st/bin/twd" version
		fi
	fi
	xattr -cr "$st" 2>/dev/null || true

	(cd "$st" && find . -type f ! -name SHA256SUMS | LC_ALL=C sort | sed 's|^\./||' | while IFS= read -r f; do sha256 "$f"; done >SHA256SUMS)
	COPYFILE_DISABLE=1 tar -czf "$DIST/$name.tar.gz" -C "$DIST/stage" "$name"
	ls -lh "$DIST/$name.tar.gz" | awk '{print "release: " $NF " " $5}'
done
(cd "$DIST" && sha256 twd-"$VERSION"-*.tar.gz >SHA256SUMS && cat SHA256SUMS)
