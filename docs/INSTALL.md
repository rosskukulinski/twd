# Installing twd on a fresh Mac

These are the exact steps for an Apple-silicon Mac with nothing installed:
no Go, no Node.js, no Homebrew packages. They were run on `mbp` (M2 Pro,
macOS 26.6) by `make e2e-mbp`; measured numbers are in STATUS.md (M6).

> **Isolation, honestly.** On macOS twd runs the **process tier**: every app
> runs in a workerd process on your Mac, and each Durable Object gets its own
> tailnet node and identity. V8 isolates and twd's egress guard are real
> boundaries, but this is **dev-grade isolation**: there is no VM between
> tenants and your user account. **microVM isolation (Firecracker) needs
> Linux with KVM** (the `linux-arm64` tarball, or a Linux VM). Don't run code
> you don't trust on the process tier.

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

The tarball is self-contained (~70 MB): the Go binaries `twd`, `twd-mgmt`,
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
API key can read the tailnet's policy, and that twd is not using the system
tailscaled. Every line has a `fix:` if it fails.

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
- Logs: `twd logs --component mgmt|hostd|supervisor`.
