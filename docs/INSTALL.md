# Installing twd

twd runs on **macOS (Apple silicon)** and **Linux (arm64 and amd64, glibc
≥ 2.35)**. Each has a self-contained release tarball and the same
`install.sh`; nothing needs root. [macOS](#macos) · [Linux](#linux) ·
[Linux support matrix](#linux-support-matrix).

## macOS

These are the exact steps for an Apple-silicon Mac with nothing installed:
no Go, no Node.js, no Homebrew packages. They were run on `mbp` (M2 Pro,
macOS 26.6) by `make e2e-mbp`; measured numbers are in STATUS.md (M6).

> **Isolation, honestly.** On macOS twd runs the **process tier**: every app
> runs in a workerd process on your Mac, and each Durable Object gets its own
> tailnet node and identity. V8 isolates and twd's egress guard are real
> boundaries, but this is **dev-grade isolation**: there is no VM between
> tenants and your user account. On macOS, twd-hostd also runs every
> tenant's workerd **and Node app** under a generated **Seatbelt**
> (`sandbox-exec`) profile: it can write only its own instance and data
> dirs, and can't read your home dir (including `~/.twd`), twd's state (node
> keys, mgmt custody, other tenants' storage), its sockets, or any tenant's
> API key file. `twd doctor` checks that Seatbelt enforces on your Mac, and
> while twd runs, that every workload is under it. **microVM isolation
> (Firecracker) needs Linux with KVM** (the `linux-arm64` tarball, or a
> Linux VM); on Linux the process tier has no per-tenant sandbox. Don't run
> code you don't trust on the process tier.

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
API key can read the tailnet's policy, that the process sandbox (Seatbelt)
works and enforces, and that twd is not using the system tailscaled. Every line has a `fix:` if it fails.

### 4. Start twd and deploy an app

```sh
twd up --tailnet <your-tailnet>.ts.net --api-key-file ~/path/to/api.env --yes
twd init support --template agent        # or hello, mcp
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
netmap only), its own workerd/Node processes, 0700 state dirs, throttles and
audit chain. Objects of one tenant can't resolve or reach the other's
(`peer_not_visible`).

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
  didn't start workerd or didn't enforce a deny on this macOS version.
  Report it; tenants are not confined until it is fixed.
- Logs: `twd logs --component mgmt|hostd|supervisor`.

## Linux

The same flow on a Linux host: one tarball per architecture, `install.sh`
into `~/.twd` without root, `twd doctor`, `twd up`. Measured on fresh Lima
VMs by `make e2e-linux` (see the [support matrix](#linux-support-matrix)).

> **Isolation on Linux, honestly.** The **process tier** has **no
> per-tenant sandbox yet**. Every tenant's workerd and Node app runs as your
> user, kept apart only by 0700 directories. That means a tenant's code could
> read other tenants' node keys and state. A Landlock sandbox (unprivileged)
> is being built; until it ships, `twd doctor` reports the kernel's Landlock
> ABI and says the sandbox is pending. The **microVM tier** (Firecracker)
> needs `/dev/kvm`, firecracker, a guest image and, today, root for TAP/netns
> setup; `twd doctor` reports it as available or unavailable, with the
> reasons. Don't run code you don't trust on the process tier.

### What you need

- **glibc ≥ 2.35, arm64 or amd64.** Ubuntu 22.04+, Debian 12+, Fedora 36+,
  RHEL/Alma/Rocky 10 all qualify. The pinned workerd itself imports
  symbols up to `GLIBC_2.27`; 2.35 is the floor twd is tested and
  supported on. **Not supported:** Alpine and other musl distros, Debian 11
  (2.31), RHEL/Alma/Rocky 8–9 (2.28/2.34), Amazon Linux 2 (2.26).
  `install.sh` refuses them with a clear message, and so does `bin/twd doctor`
  (a static binary, which runs anywhere).
- Kernel ≥ 5.13 with Landlock (for the coming sandbox; all the distros
  above), cgroups v2 (the default), systemd (for `twd up --service`).
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
| `landlock` | `landlock_create_ruleset(NULL, 0, LANDLOCK_CREATE_RULESET_VERSION)` returns an ABI ≥ 1 | warn: `ENOSYS` (not built in) or `EOPNOTSUPP` (not in `lsm=`) |
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

Use twd's default persistent nodes with a service (don't pass `--ephemeral`).
An ephemeral node that goes offline during a restart can be removed by
the control plane, and then it can't rejoin.

### Per-distro notes

- **Ubuntu 24.04**: works out of the box. AppArmor restricts unprivileged user
  namespaces by default; twd doesn't use them (doctor reports it).
- **Ubuntu 22.04**: glibc 2.35, exactly the floor. Kernel 5.15 has Landlock ABI 1–2.
- **Debian 12**: glibc 2.36. Debian's genericcloud image has no `lsof`;
  doctor uses `/proc` and `ss`, so nothing extra is needed.
- **Fedora**: glibc 2.4x, SELinux enforcing. twd needs no policy changes:
  everything runs as your user under `~/.twd`.
- **amd64**: the same release built for x86-64 (`@cloudflare/workerd-linux-64`).
  The matrix ran it under qemu emulation on an Apple-silicon Mac, so its
  timings are far slower than a real x86 host would be.
- **Alpine / musl, Debian 11, RHEL 8–9, Amazon Linux 2**: unsupported.
  `install.sh` and `twd doctor` say so (see the matrix).

### Linux support matrix

<!-- filled from make e2e-linux results -->
