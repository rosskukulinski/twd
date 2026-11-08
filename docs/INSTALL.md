# Installing twd on a fresh Mac

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
API key can read the tailnet's policy, that the process sandbox (Seatbelt
on macOS, Landlock + seccomp on Linux) works and enforces, and that twd is not using the system tailscaled. Every line has a `fix:` if it fails.

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
netmap only), its own workerd/Node processes, each in its own sandbox, 0700
state dirs, throttles and audit chain. Objects of one tenant can't resolve
or reach the other's (`peer_not_visible`). On a host without a process
sandbox, `twd tenant add` refuses (`sandbox_unavailable`) unless twd was
started with `twd up --insecure-shared-host`.

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
