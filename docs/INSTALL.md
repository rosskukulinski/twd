# Installing twd on a fresh Mac

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

## What you need

- macOS 14 or later on Apple silicon (arm64). `linux-arm64` works the same way.
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

## 1. Get the release

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

## 2. Install (no sudo)

```sh
cd twd-*-darwin-arm64
./install.sh                  # installs into ~/.twd; safe to re-run (upgrade/repair)
export PATH="$HOME/.twd/bin:$PATH"
echo 'export PATH="$HOME/.twd/bin:$PATH"' >> ~/.zprofile   # the installer prints this; it doesn't edit your profile
```

It verifies `SHA256SUMS`, checks the platform, copies `bin/` and `share/`
into `~/.twd`, runs `workerd --version` and `twd version`, and touches
nothing outside `~/.twd`.

## 3. Check the machine

```sh
twd doctor --tailnet <your-tailnet>.ts.net --api-key-file ~/path/to/api.env
```

It checks that workerd runs, that the built-in bundler can build every
template offline, that `~/.twd` (and the state dir) are writable, that the
API key can read the tailnet's policy, that the process sandbox (Seatbelt)
works and enforces, and that twd is not using the system tailscaled. Every line has a `fix:` if it fails.

## 4. Start twd and deploy an app

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

### A second tenant (another tailnet) on the same Mac

```sh
twd tenant add <other-tailnet>.ts.net --api-key-file ~/path/to/other-api.env
twd deploy --tenant <other-tailnet>.ts.net      # also: ls, kill, audit, logs --tenant
twd tenant ls
```

Each tenant gets its own netd registry (its tailnet's control plane and
netmap only), its own workerd/Node processes, 0700 state dirs, throttles and
audit chain. Objects of one tenant can't resolve or reach the other's
(`peer_not_visible`).

## 5. Stop and uninstall

```sh
twd down --purge           # kill every object and delete its devices, then stop
~/Downloads/twd/twd-*-darwin-arm64/install.sh --uninstall --purge   # remove ~/.twd
```

`--uninstall` without `--purge` keeps your state (`twd.json`, logs, data).

## Troubleshooting

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

## Linux: the microVM tier (Firecracker, KVM)

On Linux with KVM, twd can run each workload in its own **Firecracker
microVM** instead of a host process. Every tailnet identity (node keys,
auth keys) stays in twd-hostd on the host; the guest gets only plaintext
DO storage and workerd. The linux release ships everything the tier needs
in `~/.twd/share/twd/vm/`: the guest kernel (`vmlinux`), the read-only guest
image (`rootfs.ext4`, reproducible; `manifest.json` has its hashes),
Firecracker and its jailer (v1.17), plus `bin/twd-vmhelper`.

### What you need

- Linux arm64 with `/dev/kvm` (bare metal, or a VM with nested
  virtualization), cgroups v2 and systemd. Tested on Ubuntu 24.04 arm64.
- `cryptsetup`, `losetup`, `mkfs.ext4`, `ip` (Ubuntu/Debian cloud images
  have them; else `apt-get install cryptsetup-bin util-linux e2fsprogs iproute2`).

### One-time setup (the only sudo)

```sh
./install.sh                                   # as you, into ~/.twd (no sudo)
sudo ~/.twd/bin/twd host setup --microvm       # sudo's PATH lacks ~/.twd/bin: use the full path
twd doctor                                     # "microvm: ok"
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
`tiers: process, microvm`. Without the setup, `"tier": "microvm"` falls
back to the process tier with a warning, and `twd doctor` says why
(`microvm: warn … fix: sudo ~/.twd/bin/twd host setup --microvm`).

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
  DO SQLite and KV survive a VM crash, a redeploy and a reboot;
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
