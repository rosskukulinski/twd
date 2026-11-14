# guest/inputs.sh: the pinned external inputs of the microVM guest assets
# (sourced by guest/build-image.sh, guest/build-kernel.sh and
# guest/fetch-firecracker.sh). Every download is checked against the sha256
# here before use; a mismatch deletes the file and fails the build. Bumping a
# version means bumping its hash in the same commit.
#
# Arch names: twd uses Go's (amd64, arm64); kernel.org, Firecracker and the
# Firecracker CI bucket use x86_64/aarch64; npm names amd64 "64".

# Ubuntu 24.04 base rootfs (cdimage.ubuntu.com, SHA256SUMS of 24.04.5): the
# glibc that workerd links. glibc 2.39 >= workerd's floor.
UBUNTU_BASE_VERSION=24.04.5
UBUNTU_BASE_URL=https://cdimage.ubuntu.com/ubuntu-base/releases/$UBUNTU_BASE_VERSION/release
UBUNTU_BASE_SHA256_amd64=e77b6f10c2590cef872b33ee9f635a0e3fd1f57fb074c0e52b5c7f56147a0c86
UBUNTU_BASE_SHA256_arm64=a91d5a93010193712d346d761372b7c9db6dfcf093893161c64ca107f05914f2

# The CA store (ubuntu-base has none; workerd refuses to start without one,
# though https egress is originated by host netd). Arch-independent; the noble
# release pocket's version (the pool file stays as long as noble is served).
CA_DEB=ca-certificates_20240203_all.deb
CA_DEB_URL=http://archive.ubuntu.com/ubuntu/pool/main/c/ca-certificates/$CA_DEB
CA_DEB_SHA256=641de77d8f142cfd62a1a6f964ba67b20754d3337c480efb529d086075a06c9a

# Guest kernel: kernel.org 6.1 LTS with Firecracker's CI config for that
# kernel line (firecracker-ci/v1.15/<arch>/vmlinux-6.1.155.config, the build
# ~/fc/vmlinux came from) plus guest/kernel/twd.config (erofs + overlayfs for
# container mode).
KERNEL_VERSION=6.1.155
KERNEL_URL=https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$KERNEL_VERSION.tar.xz
KERNEL_SHA256=c29387aeee085fbcbd91236224b9df805063bac43615e75cea2c6b29604a5c73
KERNEL_CONFIG_URL_BASE=https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15
KERNEL_CONFIG_SHA256_amd64=024b2aae62fe7131f9a9ab80f27619c882c0a37265dd105adae01d2867bef7c3
KERNEL_CONFIG_SHA256_arm64=3bf796f6dd0d2449d596d688ea5be1a0996e3ed3f404f0cc1116d3a8fa1bc3a2

# Firecracker + jailer release (the tarball's digest is GitHub's; the binary
# digests are the release's own SHA256SUMS).
FIRECRACKER_VERSION=1.17.0
FIRECRACKER_URL=https://github.com/firecracker-microvm/firecracker/releases/download/v$FIRECRACKER_VERSION
FIRECRACKER_TGZ_SHA256_amd64=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558
FIRECRACKER_TGZ_SHA256_arm64=e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256
FIRECRACKER_SHA256_amd64=99ad0f5cd0514a88aad0e9ae8cfdb3cc3b4ab9d190e1194602406c786b5de7a5
FIRECRACKER_SHA256_arm64=fe726e0b43c04363ac07e358be4dee982c3947c65ed3ae10c770fef5e1cd756c
JAILER_SHA256_amd64=65ef226e96f0ceda55ba643f445801ef2cc0ea667ef67cad8ac4f406c9c8434f
JAILER_SHA256_arm64=4d8d2dd4dfc1d47932b2bd261479181dd0c54f0a2f32714421a7e7f9f07ddec8

# GUEST_CACHE: downloads (content-checked, so safe to share between builds).
GUEST_CACHE=${GUEST_CACHE:-${ROOT:?}/dist/cache/guest}

# guest_arch [ARCH]: normalize ARCH (default: this machine) to Go's name and
# set the per-arch names: KARCH (kernel make ARCH), UARCH (uname -m), LIBDIR,
# NPMARCH, and FCARCH (Firecracker's).
guest_arch() {
	local a=${1:-$(uname -m)}
	case $a in
	arm64 | aarch64) GARCH=arm64 KARCH=arm64 UARCH=aarch64 LIBDIR=aarch64-linux-gnu NPMARCH=arm64 ;;
	amd64 | x86_64) GARCH=amd64 KARCH=x86_64 UARCH=x86_64 LIBDIR=x86_64-linux-gnu NPMARCH=64 ;;
	*) echo "unsupported guest arch $a (want amd64 or arm64)" >&2; return 1 ;;
	esac
	FCARCH=$UARCH
}

sha256_of() { sha256sum "$1" | cut -d' ' -f1; }

# fetch URL SHA256 [NAME]: the file in GUEST_CACHE (downloaded once), checked.
# Prints its path.
fetch() {
	local url=$1 want=$2 name=${3:-${1##*/}}
	local f=$GUEST_CACHE/$name
	mkdir -p "$GUEST_CACHE"
	if [ ! -f "$f" ]; then
		echo "fetch: $url" >&2
		curl -fsSL --retry 3 -o "$f.part" "$url" || { rm -f "$f.part"; echo "fetch: download failed: $url" >&2; return 1; }
		mv "$f.part" "$f"
	fi
	local got
	got=$(sha256_of "$f")
	if [ "$got" != "$want" ]; then
		rm -f "$f"
		echo "fetch: $name: sha256 $got, want $want (deleted; check the pin in guest/inputs.sh)" >&2
		return 1
	fi
	printf '%s\n' "$f"
}
