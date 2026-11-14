# The microVM guest kernels

Container guests (ADR-032) mount their image as **erofs** under an
**overlayfs** root. The Firecracker CI 6.1 guest kernels have overlayfs,
cgroup pids and vsock but not erofs, so twd builds its own:

    TARGETARCH=amd64|arm64 guest/build-kernel.sh [OUT_DIR]   # make guest-kernel

- **Inputs** (pinned in `guest/inputs.sh`): linux 6.1.155 from kernel.org and
  Firecracker's CI guest config for it, per arch, plus `twd.config` (erofs,
  overlayfs, and the options container mode relies on). Every fragment line
  must survive `olddefconfig`.
- **Output:** `vmlinux` (x86_64: the ELF vmlinux; arm64: the uncompressed
  `Image`), `kernel.config`, `kernel.json` (input and output hashes,
  `erofs`/`overlayfs`). One kernel serves the DO tier and container guests;
  `twd-hostd -vm-container-kernel` can name another, and hostd offers
  containers only when the container kernel has erofs.

The macOS (Virtualization.framework) guest kernel is separate for now:
`guest/build-kernel-vz.sh` (linux 6.1.167, Firecracker v1.17.0's aarch64 CI
config plus `vz.config`: virtiofs/FUSE for Rosetta, erofs, overlayfs).
