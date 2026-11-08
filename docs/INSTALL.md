# Installing twd

twd runs on **macOS (Apple silicon)** and **Linux (arm64 and amd64, glibc
≥ 2.35)**. Each has a self-contained release tarball and the same
`install.sh`; nothing needs root. [macOS](#macos) · [Linux](#linux) ·
[Linux support matrix](#linux-support-matrix).

## macOS

These are the exact steps for an Apple-silicon Mac with nothing installed:
no Go, no Node.js, no Homebrew packages. They were run on `mbp` (M2 Pro,
macOS 26.6) by `make e2e-mbp`; measured numbers are in STATUS.md (M6).

> **Isolation, honestly.** twd's default is the **process tier**: every app
> runs in a workerd (or Node) process on your machine, and each Durable
> Object gets its own tailnet node and identity. V8 isolates and twd's
> egress guard are real boundaries, but this is **dev-grade isolation**:
> there is no VM between tenants and your user account. On top of that,
> twd-hostd runs every tenant's workerd **and Node app** in a per-workload
> **process sandbox**, on by default (`twd-hostd -sandbox auto`):
>
> - **macOS: Seatbelt** (`sandbox-exec`, generated profile).
> - **Linux: Landlock + seccomp** (no root and no user namespaces needed;
>   kernel 5.13+ with Landlock enabled, e.g. Ubuntu 22.04+/24.04, Debian 12,
>   Fedora). A small launcher in twd-hostd applies the rules, then execs
>   workerd or node in place.
>
> Either way a workload can write only its own instance and data dirs, read
> only the OS, its runtime binary and its install prefix, and can't read
> your home dir (including `~/.twd`), twd's state (node keys, mgmt custody,
> other tenants' storage) or any tenant's API key file, and can't use
> mgmt's or hostd's sockets. **Linux adds more:** with Landlock ABI ≥ 4
> (kernel 6.7+; Ubuntu 24.04's 6.8 has ABI 4) a workload may open TCP only
> to its own app port and egress proxy, so a malicious Node app can't
> bypass netd's proxy; a seccomp filter refuses ptrace and
> `process_vm_readv/writev` (so no reading a sibling's memory), kexec,
> mount, bpf, perf_event_open, keyctl, new namespaces, io_uring, and
> UDP/ICMP/raw/MPTCP sockets; and other processes' `/proc` entries (their
> environment, cmdline) are unreadable. Workloads get **no unix sockets**
> (seccomp; workerd's listener is inherited and its services are
> token-checked loopback TCP), so the host's tailscaled or docker socket is
> out of reach, and they may signal only themselves. twd's own sockets
> refuse sandboxed peers too. As defence in depth, if *your user* can reach
> the system tailscaled's socket or a container engine (or is in the
> docker/podman/lxd groups), `twd doctor` warns (`host_sockets`) and twd
> serves one tenant only unless `twd up --insecure-shared-host`: run twd as
> a dedicated user to share a host.
>
> `twd doctor` checks that the sandbox enforces on your machine (Linux: the
> Landlock ABI too), and while twd runs, that every workload is under it.
> **Without a sandbox** (Linux without Landlock) `twd doctor` warns loudly,
> and twd refuses to run a second tenant's process-tier workloads unless you
> pass `twd up --insecure-shared-host`. **microVM isolation (Firecracker)
> needs Linux with KVM.** Don't run code you don't trust on the process tier.

### What you need

- macOS 14 or later on Apple silicon (arm64). For Linux, see [Linux](#linux).
- A tailnet you control, and a **Tailscale API access token** for it in a
  file, as `TS_API_KEY=tskey-api-…`, mode `0600`. twd mints its nodes' auth
  keys with it (homelab "apikey" mint mode). It is read when needed and never
  copied, logged or printed.
- Your tailnet policy must let `tag:twd` own the tags your apps use. `twd up`
  prints the snippet if it's missing (`--policy-apply` writes it, validated,
  with If-Match).

twd **never uses your Mac's Tailscale app or tailscaled**, its login, its
state, or its LocalAPI. Every twd node is a separate in-process (tsnet) node
of the tailnet you name with `--tailnet`. If Tailscale.app is running on a
different (personal) tailnet, it is left alone; `twd doctor` checks this.

### 1. Get the release

The tarball is self-contained (~87 MB compressed, ~300 MB installed): the Go binaries `twd`, `twd-mgmt`,
`twd-hostd`, `twd-netd` (static, `CGO_ENABLED=0`; esbuild is built into
`twd`, so deploys need no Node.js), the pinned `workerd` (1.20261108.1, from
npm `@cloudflare/workerd-darwin-arm64`), the app templates and the
`node_modules` they need (vendored, so `twd init` and `twd deploy` work
offline).

```sh
mkdir -p ~/Downloads/twd && cd ~/Downloads/twd
# copy twd-<version>-darwin-arm64.tar.gz and SHA256SUMS here (scp, AirDrop, …)
shasum -a 256 -c SHA256SUMS --ignore-missing
tar -xzf twd-*-darwin-arm64.tar.gz
```

The binaries are **unsigned except for an ad-hoc signature** (`codesign -s -`,
required to run arm64 code). Files copied with `scp`, `curl` or `tar` are not
quarantined, so Gatekeeper doesn't get involved. If you downloaded the
tarball with a browser, `install.sh` removes the `com.apple.quarantine`
attribute from its own copy.

### 2. Install (no sudo)

```sh
cd twd-*-darwin-arm64
./install.sh                  # installs into ~/.twd; safe to re-run (upgrade/repair)
export PATH="$HOME/.twd/bin:$PATH"
echo 'export PATH="$HOME/.twd/bin:$PATH"' >> ~/.zprofile   # the installer prints this; it doesn't edit your profile
```

It verifies `SHA256SUMS`, checks the platform, copies `bin/` and `share/`
into `~/.twd`, runs `workerd --version` and `twd version`, and touches
nothing outside `~/.twd`.

### 3. Check the machine

```sh
twd doctor --tailnet <your-tailnet>.ts.net --api-key-file ~/path/to/api.env
```

It checks that workerd runs, that the built-in bundler can build every
template offline, that `~/.twd` (and the state dir) are writable, that the
API key can read the tailnet's policy, that the process sandbox (Seatbelt
on macOS, Landlock + seccomp on Linux) works and enforces, and that twd is not using the system tailscaled. Every line has a `fix:` if it fails.

### 4. Start twd and deploy an app

```sh
twd up --tailnet <your-tailnet>.ts.net --api-key-file ~/path/to/api.env --yes
twd init support --template agent        # or hello, mcp, ai-agent; node-workflow needs Node.js on this machine
cd support && twd deploy
twd status
```

`twd up` starts `twd-mgmt` (SQLite, local key custody) and `twd-hostd`
(workerd supervisor) under a detached supervisor; state and logs are in
`~/.twd` (`--state-dir` to use another directory). `twd deploy` preflights
the grants your app declares (`tailnet.connects`), bundles it, and waits
until the workload node is on your tailnet:
`http://<prefix>wl-support.<tailnet>/`.

Day 2: `twd ls`, `twd logs support`, `twd kill <object> --cascade`,
`twd audit <object>`.

#### A second tenant (another tailnet) on the same Mac

```sh
twd tenant add <other-tailnet>.ts.net --api-key-file ~/path/to/other-api.env
twd deploy --tenant <other-tailnet>.ts.net      # also: ls, kill, audit, logs --tenant
twd tenant ls
```

Each tenant gets its own netd registry (its tailnet's control plane and
netmap only), its own workerd/Node processes, each in its own sandbox, 0700
state dirs, throttles and audit chain. Objects of one tenant can't resolve
or reach the other's (`peer_not_visible`). On a host without a process
sandbox, `twd tenant add` refuses (`sandbox_unavailable`) unless twd was
started with `twd up --insecure-shared-host`.

### 5. Stop and uninstall

```sh
twd down --purge           # kill every object and delete its devices, then stop
~/Downloads/twd/twd-*-darwin-arm64/install.sh --uninstall --purge   # remove ~/.twd
```

`--uninstall` without `--purge` keeps your state (`twd.json`, logs, data).

### Troubleshooting

- `workerd does not run on this machine`: re-sign it (`codesign -f -s -
  ~/.twd/bin/workerd`) and clear quarantine (`xattr -d com.apple.quarantine
  ~/.twd/bin/workerd`), then `twd doctor`.
- `tailnet_required`: twd won't bootstrap from your Mac's Tailscale login;
  pass `--tailnet` and `--api-key-file`.
- `acl_denied` on deploy: add the printed grant to your tailnet policy, or
  deploy with `--allow-missing-grants`.
- `twd doctor` says `sandbox` failed: Seatbelt (`/usr/bin/sandbox-exec`)
  or Landlock didn't start workerd or didn't enforce a deny on this OS
  version. Report it; tenants are not confined until it is fixed.
- `twd doctor` warns `NO PROCESS SANDBOX` on Linux: the kernel has no
  Landlock (older than 5.13, or `landlock` missing from the `lsm=` boot
  parameter; check `cat /sys/kernel/security/lsm`). Add it to the boot
  parameters, use a newer kernel, or deploy with tier `microvm`.
- A Node app on Linux gets `EACCES` from `net.connect` or its own
  dispatcher: that's the sandbox; send egress through `HTTP(S)_PROXY` /
  `@twd/node`'s `fetch`.
- Logs: `twd logs --component mgmt|hostd|supervisor`.

## Linux

The same flow on a Linux host: one tarball per architecture, `install.sh`
into `~/.twd` without root, `twd doctor`, `twd up`. Measured on fresh Lima
VMs by `make e2e-linux` (see the [support matrix](#linux-support-matrix)).

> **Isolation on Linux.** The **process tier** runs every tenant's workerd
> and Node app under **Landlock + seccomp** (unprivileged; no user
> namespaces needed), with the guarantees listed at the top of this file.
> `twd doctor` reports the kernel's Landlock ABI and that the sandbox
> enforces; without Landlock it warns loudly and twd refuses to host a
> second tenant on the process tier. The **microVM tier** (Firecracker,
> jailer, encrypted per-workload DO volume) needs `/dev/kvm` and the
> one-time `sudo twd host setup --microvm` ([below](#linux-the-microvm-tier-firecracker-kvm));
> `twd doctor` reports it as available or unavailable, with the reasons.

### What you need

- **glibc ≥ 2.35, arm64 or amd64.** Ubuntu 22.04+, Debian 12+, Fedora 36+,
  RHEL/Alma/Rocky 10 all qualify. The pinned workerd itself imports
  symbols up to `GLIBC_2.27`; 2.35 is the floor twd is tested and
  supported on. **Not supported:** Alpine and other musl distros, Debian 11
  (2.31), RHEL/Alma/Rocky 8–9 (2.28/2.34), Amazon Linux 2 (2.26).
  `install.sh` refuses them with a clear message, and so does `bin/twd doctor`
  (a static binary, which runs anywhere).
- Kernel ≥ 5.13 with Landlock (the process sandbox; ABI ≥ 4, i.e. kernel
  6.7+, also restricts TCP; all the distros above have Landlock), cgroups v2 (the default), systemd (for `twd up --service`).
- No Node.js, Go or npm. `curl`/`scp` and `tar` to get the tarball.
- The same Tailscale API access token file as on macOS (`TS_API_KEY=…`,
  mode `0600`).

twd **never uses the machine's tailscaled**. If a system Tailscale is
installed and logged in to some other tailnet, it is left alone: every twd
node is its own tsnet node of the tailnet you pass with `--tailnet`.
While twd runs, `twd doctor` checks that no twd process holds tailscaled's
socket or state. It reads `/proc/<pid>/fd` and matches unix-socket peers with
`ss -x`, and never connects to tailscaled.

### Install

```sh
mkdir -p ~/twd-release && cd ~/twd-release
# copy twd-<version>-linux-arm64.tar.gz (or -linux-amd64) and SHA256SUMS here
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf twd-*-linux-*.tar.gz
cd twd-*-linux-*/ && ./install.sh          # no sudo; refuses to run as root
export PATH="$HOME/.twd/bin:$PATH"
echo 'export PATH="$HOME/.twd/bin:$PATH"' >> ~/.profile
twd doctor --tailnet <your-tailnet>.ts.net --api-key-file ~/path/to/api.env
```

On Linux, `install.sh` checks the architecture (`uname -m`: `aarch64` →
arm64, `x86_64` → amd64) and glibc before copying anything. On bash it
suggests `~/.profile`, because creating a `~/.bash_profile` on Debian or
Ubuntu would make bash skip `~/.profile`.

`twd doctor` on Linux adds these checks:

| check | ok when | otherwise |
|---|---|---|
| `glibc` | glibc ≥ 2.35 (and ≥ workerd's ELF `GLIBC_` floor) | **fail**: musl, or glibc too old (the message names the distro and the fix) |
| `kernel` | ≥ 5.13 | warn |
| `landlock` | `landlock_create_ruleset(NULL, 0, LANDLOCK_CREATE_RULESET_VERSION)` returns an ABI ≥ 1 (the `sandbox` check then proves it enforces; ABI < 4 means TCP isn't restricted) | warn: `ENOSYS` (not built in) or `EOPNOTSUPP` (not in `lsm=`) |
| `user_namespaces` | informational (twd doesn't need them; Ubuntu 24.04's AppArmor restriction is reported) | |
| `kvm` | `/dev/kvm` opens read-write and answers `KVM_GET_API_VERSION` | skip, with the `usermod -aG kvm` fix if it's a permission problem |
| `firecracker` | firecracker (+ jailer) on PATH, in `~/.twd/bin` or `/usr/local/bin` | skip / warn (no jailer) |
| `cgroups_v2` | unified hierarchy (and what's delegated to your user manager) | warn |
| `systemd` | your systemd user manager answers; reports linger and `twd.service` | warn: no systemd / no user session |
| `microvm_tier` | KVM + firecracker + cgroups v2 + guest image + privileges | skip: "microVM tier unavailable: …", with every reason |
| `system_tailscaled_not_used` | no twd process holds tailscaled's socket or state | **fail** |

### Run twd as a systemd user service

```sh
twd up --tailnet <your-tailnet>.ts.net --api-key-file ~/path/to/api.env --yes --service
systemctl --user status twd.service
```

`--service` writes `~/.config/systemd/user/twd.service`. That unit runs the
same supervisor as plain `twd up` (`twd __supervise`: twd-mgmt, then
twd-hostd) with `Restart=on-failure`, and twd enables and starts it. Logs go
to the same `<state>/logs/`. `systemctl --user restart twd` brings twd back
in a few seconds, and deployed workloads rejoin with the same tailnet nodes
and IPs. A plain `twd down` stops it through systemd but leaves the unit
enabled, so it starts again at your next login or boot.
`twd down --service` stops, disables and removes it.

A user service stops when your last session ends, unless lingering is on.
`twd up --service` and `twd doctor` tell you when it's off. twd never runs
sudo for you, so to keep it running after logout and start it at boot, run:

```sh
sudo loginctl enable-linger "$USER"
```

Across a restart or a reboot, **persistent** nodes (the default) rejoin from
their saved state with the same node IDs and IPs. **Ephemeral** nodes
(`twd up --ephemeral`) are logged out when twd stops, and control rejects
their saved state when they come back; twd notices (the backend sits in
`NeedsLogin`), deletes the dead device, mints a fresh key and rejoins
**under the same hostname**, so the MagicDNS name is stable but the node ID
and IP change. Measured on Ubuntu 24.04 arm64: a spawned sub-agent was
ready again 4.1 s after `systemctl --user restart twd` and answering a
client on another machine after 9.5 s.

**Reboot** (measured, Ubuntu 24.04 arm64, lingering on): from the start of
boot, ssh up at 6.6 s, `twd.service` active and twd healthy at 6.6 s, every
object rejoined from saved state (same node IDs, nothing re-minted), and
the workload answered a client on another machine at 8.6–17.1 s (two runs).
The spread is on the **client's** side, not twd's: a persistent node comes
back with the same node key but fresh WireGuard state, so a client that
still holds a recent session to it keeps sending on that dead session and
WireGuard only re-handshakes after ~15 s of silence; a client whose session
had already gone idle, or a client that has never talked to the node,
reaches it within 2–3 s of `twd up` (measured in STATUS.md, "Diagnosis: the
12–17 s re-find delay"). The node is reachable over DERP the moment it is
up.

### Per-distro notes

- **Ubuntu 24.04**: works out of the box (glibc 2.39, kernel 6.8, Landlock ABI 4).
  AppArmor restricts unprivileged user namespaces by default; twd doesn't use
  them, and doctor reports it.
- **Ubuntu 22.04**: glibc 2.35, exactly the floor. Kernel 5.15 has Landlock ABI 1,
  the oldest Landlock.
- **Debian 12**: glibc 2.36, kernel 6.1 (Landlock ABI 2). No extra packages needed;
  doctor's process checks use `/proc` and `ss`, not `lsof`.
- **Fedora 45 (Cloud)**: glibc 2.44, kernel 7.2 (Landlock ABI 10). No extra
  packages and no policy changes; everything runs as your user under `~/.twd`.
- **amd64**: the same release built for x86-64 (`@cloudflare/workerd-linux-64`).
  The matrix ran it under qemu x86-64 emulation on an Apple-silicon Mac, so its
  timings are slower than a real x86 host's.
- **Debian 11** (glibc 2.31) is refused by `install.sh` and fails doctor's
  `glibc` check. The pinned workerd does start there (`workerd --version`),
  because its ELF needs only `GLIBC_2.27`, but twd is not tested below 2.35.
  Debian 11's kernel 5.10 also has no Landlock. Lima note: its
  genericcloud image doesn't boot under `vmType: vz`; use qemu.
- **Alpine / musl**: refused. workerd can't even be exec'd: there is no glibc
  loader. The same goes for RHEL/Alma/Rocky 8–9 and Amazon Linux 2 (glibc too
  old; not run in the matrix).

### Linux support matrix

`make e2e-linux`, 2026-11-08, release `0.1.0-bb3a3296`. Every VM was **fresh**:
a Lima official template, 4 CPU / 6 GiB / 30 GiB, `--plain --mount-none`, and
nothing mounted from the Mac. The tarball and the API key file (0600) were
copied in with `limactl copy`. twd ran in the VM under `twd up --service`; the
tag:ttest client and echo ran on the Mac (M5 Pro) over tailnet 1. Every row
passed checks (a)–(f), `twd doctor` before and while running,
`systemctl --user restart twd` (healthy again, the same workload IP answering
the Mac), `twd down --purge --service`, and cleanup: zero leftover devices or
keys. **Sandbox: none on every row in L1's run (made before the Landlock sandbox landed; the integration rows below re-ran with Landlock on).** **microVM
tier: unavailable on every row** (no `/dev/kvm` in these VMs, no
firecracker, not root). Raw results: `e2e/real/results/l1-linux/`.

| | Ubuntu 24.04 arm64 | Debian 12 arm64 | Ubuntu 22.04 arm64 | Fedora 45 arm64 | Ubuntu 24.04 **amd64** (qemu, emulated) |
|---|---|---|---|---|---|
| result | **PASS** | **PASS** | **PASS** | **PASS** | **PASS** |
| glibc · kernel · Landlock ABI | 2.39 · 6.8 · 4 | 2.36 · 6.1 · 2 | 2.35 · 5.15 · 1 | 2.44 · 7.2 · 10 | 2.39 · 6.8 · 4 |
| `install.sh` | 0.41 s | 0.81 s | 0.84 s | 0.47 s | 9.1 s |
| `twd up --service` → ready | 0.70 s | 0.65 s | 0.64 s | 0.68 s | 3.4 s |
| `twd deploy` → ready (agent / mcp) | 2.78 / 1.57 s | 1.57 / 1.45 s | 1.50 / 1.46 s | 1.59 / 1.56 s | 4.70 / 5.55 s |
| first request → object node ready | 1.64 s | 1.67 s | 1.62 s | 1.59 s | 3.95 s |
| `twd kill --cascade` (CLI / push) | 66 / 56 ms | 72 / 63 ms | 65 / 59 ms | 69 / 56 ms | 319 / 133 ms |
| request to a killed object → 410 | 1.7 ms | 1.3 ms | 0.9 ms | 0.9 ms | 10.9 ms |
| Mac → VM path | **direct** after 0.19 s | **direct** after 0.18 s | **direct** after 0.18 s | **direct** after 0.20 s | **direct** after 0.47 s |
| MCP `tools/list` direct · DERP-only (median) | 1.9 · 10.7 ms | 2.5 · 11.2 ms | 3.5 · 11.2 ms | 2.0 · 16.6 ms | 33.1 · 41.8 ms |
| disco ping direct · DERP (median) | 0.41 · 7.8 ms | 0.57 · 9.2 ms | 0.67 · 8.5 ms | 0.42 · 14.1 ms | 1.47 · 13.4 ms |
| `systemctl --user restart twd` → healthy / workload answers the Mac | 3.6 / 6.6 s | 3.6 / 6.7 s | 3.6 / 6.7 s | 0.7 / 3.8 s | 5.6 / 13.1 s |
| `twd down --purge --service` | 0.62 s | 0.56 s | 0.60 s | 0.63 s | 4.0 s |
| reboot (enable-linger, `limactl stop`/`start`) → service active / healthy / workload answers | 6.6 / 6.6 / 8.6–17.1 s | — | — | — | — |
| `--ephemeral` sub-agent across `systemctl --user restart` → re-minted, ready / answers | 4.1 / 9.5 s | — | — | — | — |

The direct path is the Mac ↔ Lima VM NAT (a private address); DERP-only
numbers come from a second client with `TS_DEBUG_ALWAYS_USE_DERP=1`
(region `sea`). The `systemctl --user restart` command itself returned in
about 3.1 s on Ubuntu and Debian (the supervisor drains hostd, then mgmt,
before systemd starts the new one), 0.17 s on Fedora 45, and 4.6 s on amd64.
In every row, the first request after the restart that succeeded had waited
about 3 s for the workload node to rejoin.

Unsupported hosts (bonus rows; `TestUnsupportedHost`):

| | Debian 11 arm64 (glibc 2.31, kernel 5.10) | Alpine 3.24 arm64 (musl) |
|---|---|---|
| `install.sh` | refused: `glibc 2.31 is too old: twd's workerd needs glibc >= 2.35 (Ubuntu 22.04+, Debian 12+, Fedora 36+, RHEL 10)` | refused: `this Linux uses musl libc (Alpine?); twd's workerd needs glibc >= 2.35 …` |
| `bin/twd doctor` | `✗ glibc` (names the distro and the fix); `! kernel` (< 5.13); `! landlock` (ENOSYS) | `✗ glibc`: `this system uses musl libc (/lib/ld-musl-aarch64.so.1) …` |
| would workerd start anyway? | yes (`workerd --version`; untested beyond that) | no (no glibc loader) |

## Linux: the microVM tier (Firecracker, KVM)

On Linux with KVM, twd can run each workload in its own **Firecracker
microVM** instead of a host process. Every tailnet identity (node keys,
auth keys) stays in twd-hostd on the host; the guest gets only plaintext
DO storage and workerd. A linux-arm64 release **built with the microVM
assets** ships everything the tier needs in `~/.twd/share/twd/vm/`: the
guest kernel (`vmlinux`), the read-only guest image (`rootfs.ext4`,
reproducible; `manifest.json` has its hashes), Firecracker and its jailer
(v1.17), plus `bin/twd-vmhelper`.

> **Check your tarball.** The guest image can only be built on Linux.
> `make release` includes it when it runs on Linux or when Lima `twd-dev`
> is running on the Mac (`scripts/vm-assets-lima.sh` builds it there and
> caches it); otherwise it prints a warning and the tarball has **no
> microVM tier**: `share/twd/release.json` says `"microvm": false`,
> `share/twd/vm/` is absent, and `twd doctor` reports `firecracker: skip …
> install a linux release built with the microVM assets`. The process tier
> (Landlock + seccomp) is unaffected. The red team's second pass found a
> Mac-built tarball without the guest; the `make release` change above is
> the fix, and the e2e-linux-vm row in STATUS.md says which tarball it ran.

### What you need

- Linux arm64 with `/dev/kvm` (bare metal, or a VM with nested
  virtualization), cgroups v2 and systemd. Tested on Ubuntu 24.04 arm64.
- `cryptsetup`, `losetup`, `mkfs.ext4`, `ip` (Ubuntu/Debian cloud images
  have them; else `apt-get install cryptsetup-bin util-linux e2fsprogs iproute2`).

### One-time setup (the only sudo)

```sh
./install.sh                                   # as you, into ~/.twd (no sudo)
sudo ~/.twd/bin/twd host setup --microvm       # sudo's PATH lacks ~/.twd/bin: use the full path
twd doctor                                     # "microvm_tier: ok"
```

`twd host setup --microvm` (for `$SUDO_USER`, or `--user <name>`):

- installs **root-owned copies** of `twd-vmhelper`, `firecracker` and `jailer`
  in `/usr/local/libexec/twd` (root never runs code from your writable `~/.twd`);
- writes `/etc/twd/vmhelper.json` (which uids may use the helper) and
  `/etc/twd/setup.json` (what to undo);
- installs and starts **`twd-vmhelper.service`** (systemd, enabled at boot):
  root, with a bounded capability set (`CAP_NET_ADMIN CAP_SYS_ADMIN
  CAP_SYS_CHROOT CAP_SETUID CAP_SETGID CAP_CHOWN CAP_FOWNER CAP_DAC_OVERRIDE
  CAP_DAC_READ_SEARCH CAP_MKNOD CAP_KILL CAP_SYS_RESOURCE CAP_IPC_LOCK`),
  `NoNewPrivileges`, and `Delegate=yes` for the per-VM cgroups. It listens on
  `/run/twd/vmhelper.sock` and authorizes every connection by its peer uid;
- adds you to the `kvm` group (twd itself doesn't depend on it);
- reserves 2 MiB **huge pages** for guest RAM (`/etc/sysctl.d/60-twd-microvm.conf`;
  default enough for 4 VMs, at most ¼ of RAM; `--hugepages N`). Under nested
  virtualization they cut workerd start-up from ~3.7 s to ~0.5 s.

After that **twd runs as you**: `twd up …` as usual. `twd up` prints
`tiers: process [Landlock+seccomp, landlock (ABI 4)], microvm [Firecracker,
jailed, via twd-vmhelper; "tier": "microvm" in twd.jsonc]`. Without the
setup, `"tier": "microvm"` falls back to the process tier with a warning,
and `twd doctor` says why (`microvm_tier: warn … fix: sudo
~/.twd/bin/twd host setup --microvm`).

### Deploying in a microVM

Add `"tier": "microvm"` to the app's `twd.jsonc`, then `twd deploy`.
`twd status --json` shows the VM (pid, boot times, jail, volume). Each VM:

- runs through the **jailer**: its own chroot (`/var/lib/twd-vm/jail/…`),
  its own uid/gid, its own network namespace, cgroup v2 `cpu.max` (vCPUs)
  and `memory.max`, and Firecracker's default seccomp filters;
- has a **persistent, encrypted DO volume** per (tenant, workload):
  `~/.twd/hostd/tenants/<t>/workloads/<w>/do.img`, a LUKS2 image opened with
  dm-crypt **on the host**. Its key is created in twd-mgmt's custody and
  released only to the host that holds the workload's lease, at that epoch.
  The guest sees plaintext ext4 on `/dev/vdc`; the key never enters it.
  DO SQLite and KV survive a VM crash (SIGKILL), a redeploy, and a host
  reboot or power cut (the helper starts at boot; run `twd up` again, and
  objects rejoin with the same nodes);
- is attached to its volume **alone**: a redeploy stops the old VM (which
  unmounts and releases the volume) before the new one starts, and the
  helper refuses a second attach (`volume_busy`) or an older epoch
  (`volume_fenced`).

Check a running VM: `twd host vmcheck <workload> [--keyscan]` (guest memory,
DO volume fsync latency, and a scan of the guest's disks, files and full RAM
for any node key, `tskey-` or the volume key).

### Removing it

```sh
twd down
sudo ~/.twd/bin/twd host setup --undo
```

This stops the service (and every microVM), and removes the unit,
`/usr/local/libexec/twd`, `/etc/twd`, `/var/lib/twd-vm`, the sysctl file
(huge pages back to their old value) and the `kvm` membership it added. Your
DO volumes stay in `~/.twd` until `install.sh --uninstall --purge`.
