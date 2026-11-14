# The microVM guest kernel

Container guests (ADR-032) mount their image as **erofs** under an
**overlayfs** root. The Firecracker CI 6.1 guest kernel the VM tier has used so
far (`~/fc/vmlinux` in Lima) has overlayfs, cgroup pids and vsock, but not
erofs (`# CONFIG_EROFS_FS is not set`), so twd builds its own:

    guest/build-kernel.sh [OUT_DIR]        # Linux; default guest/out

- **Source:** linux-6.1.155 from cdn.kernel.org, sha256-checked against the
  release's entry in kernel.org's signed `sha256sums.asc`.
- **Config:** `microvm-6.1-arm64.base.config` is the Firecracker CI config,
  extracted verbatim from the CI kernel's embedded config (`CONFIG_IKCONFIG`,
  the `IKCFG_ST` blob of `~/fc/vmlinux`), so it is exactly the config the VM
  tier was tested on. `containers.fragment` adds erofs (with xattrs, ACLs,
  security labels and compressed images) and pins overlayfs, cgroup pids and
  devpts; `merge_config.sh` + `olddefconfig` merge them, and the script
  fails if any required option did not end up `=y`.
- **Reproducibility:** `KBUILD_BUILD_TIMESTAMP/USER/HOST` are fixed, so a
  given toolchain gives the same `vmlinux`; `kernel-manifest.json` records the
  source, config, fragment and output hashes and the compiler.
- **Output:** `vmlinux` (arm64: the uncompressed `Image` Firecracker boots;
  x86_64: `vmlinux`), `vmlinux.config`, `kernel-manifest.json`.

The DO tier keeps booting whatever `TWD_VM_KERNEL` names; hostd boots
container guests with `vm.Config.ContainerKernel` (default: the same
kernel). One kernel for both is fine: this one is a superset.

Built in Lima twd-dev (Ubuntu 24.04, gcc 13.3.0, 6 jobs: ~2 minutes):
`vmlinux` sha256 `f9b19380bdb28e9cf7e21ed39f7e17e1b1460401352805ad83f8a1eabfbf87c4`.

An x86_64 base config (`microvm-6.1-x86_64.base.config`, from the
Firecracker CI x86_64 kernel) is still to be added for amd64 hosts.
