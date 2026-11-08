# Linux as a first-class host (opened 2026-11-08)

**Goal (Ross):** "Make sure Linux is also seamlessly supported as a host." Spin up VMs as needed. The safety rules in BUILD-PLAN.md still apply: never touch the personal tailnet, use test tailnets 1 and 2 only, never print keys, clean up your own IDs, and push only to private repos.

## Where Linux stood at the start
- ✅ Hermetic `make test`, `test-node` and `test-sdk` pass in Lima `twd-dev` (Ubuntu 24.04 arm64). `make e2e-vm` (Firecracker) passes there.
- ❌ No **linux-amd64** release; arm64 only.
- ❌ The full flow (install → `twd up` → deploy → checks a–f) had **never run on a clean Linux host from the release tarball**. The linux tarball had only had install + doctor run.
- ❌ **Process tier on Linux has no per-tenant sandbox.** Seatbelt is macOS-only, so any tenant's workerd or Node app could read other tenants' keys and state.
- ❌ **microVM tier needs root,** has no jailer, and **loses DO storage on VM restart**.
- ❌ `twd doctor` doesn't check Linux prerequisites: glibc ≥ 2.35 for workerd, kernel/Landlock ABI, `/dev/kvm` access, user namespaces, cgroups v2.

## Definition of done
1. **Releases:** `linux-arm64` and `linux-amd64` tarballs, both built and tested. `install.sh` installs into `~/.twd` without root, on any supported distro.
2. **E2E-LINUX:** the full mbp-style e2e (checks a–f, plus multi-tenant isolation) passes from the installed release on **fresh** VMs:
   - Ubuntu 24.04 arm64
   - Debian 12 arm64
   - Ubuntu 22.04 arm64
   - Ubuntu 24.04 **amd64** (Lima qemu emulation; process tier only, no KVM)
   - Fedora (latest, arm64) if time allows
   The client and echo run on this Mac over tailnet 1.
3. **Linux process-tier sandbox:** each tenant's workerd and Node app runs under **Landlock** (unprivileged; no user namespaces needed, since Ubuntu 24.04 restricts them), plus seccomp where cheap. It must give the same guarantees as the macOS Seatbelt profile and pass the same cross-tenant attack tests as M7. If Landlock is unavailable, `twd doctor` warns loudly and twd refuses multi-tenant use unless `--insecure-shared-host` is passed.
4. **microVM tier, production-shaped:**
   - **DO storage persists** across VM restart and redeploy, via a per-(tenant, workload) host volume.
   - Firecracker **jailer** is used.
   - **Privilege model:** a one-time `sudo twd host setup --microvm` (kvm group, a TAP/netns helper with the minimum capabilities, and a systemd unit). After that, twd runs as the user. Without setup, twd falls back to the process tier with Landlock, and doctor explains.
   - E2E-VM passes **from the installed release** on a fresh Ubuntu 24.04 arm64 nested-virt VM, on real tailnet 1.
5. **Service management:** `twd up --service` installs a systemd **user** unit so twd survives logout and reboot. Document `loginctl enable-linger`. `twd down --service` removes it.
6. **`twd doctor` on Linux** checks glibc, the kernel/Landlock ABI, `/dev/kvm`, the jailer, cgroups v2, systemd, and that the system tailscaled is not used. It gives clear errors for unsupported hosts: musl/Alpine, glibc < 2.35 (e.g. Amazon Linux 2), and missing Landlock.
7. **docs/INSTALL.md** covers Linux as a peer of macOS: per-distro notes and a support matrix with measured results.

## Work streams (parallel, separate worktrees)
- **L1 (`l1-linux-release`):** amd64 builds, doctor Linux checks, systemd user service, the fresh-VM matrix harness (`make e2e-linux`), INSTALL.md.
- **L2 (`l2-landlock`):** the Landlock (+seccomp) sandbox for the process tier, with tests mirroring M7's Seatbelt attack tests.
- **L3 (`l3-microvm`):** persistent DO storage, jailer, the privilege helper + `twd host setup`, and e2e-vm from the installed release.

The lead integrates (merge order L2 → L3 → L1) and runs the final matrix.

## VMs
- Name them `twdl-<distro>-<arch>`. Size: 4 CPU / 6 GiB / 30 GiB each, unless the microVM tier needs more.
- Mount nothing from the Mac into the clean-matrix VMs; copy the tarball in with `limactl copy`, as a real user would get it.
- Delete matrix VMs at the end, or stop them, and record which in STATUS.md.
- `twd-dev` stays the dev VM.
