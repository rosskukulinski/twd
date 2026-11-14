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
- A tailnet you control, where you are an **admin** (`autogroup:admin`).
- An **OAuth client** (admin console › Settings › OAuth clients; scope
  Auth Keys write with tag `tag:twd`) saved in a 0600 file:
  [Recommended: OAuth client](#recommended-oauth-client). No browser
  login, no Funnel, no HTTPS needed. Alternatives:
  [workload identity federation](#alternative-workload-identity-federation-twd-up---wif)
  (`twd up --wif`: browser login, HTTPS + Funnel, no secret anywhere) and
  the [API-key mode](#last-resort-api-key-mode).
- A policy snippet in your tailnet policy (tagOwners: `tag:twd` owns
  `tag:twd-mgmt` and `tag:twd-host`; [below](#recommended-oauth-client)).

twd **never uses your Mac's Tailscale app or tailscaled**, its login, its
state, or its LocalAPI. Every twd node (mgmt's own node and every object
node) is a separate in-process (tsnet) node of the tailnet you log in to.
If Tailscale.app is running on a different (personal) tailnet, it is left
alone; `twd doctor` checks this. The one place your machine's own Tailscale
*is* used is a **developer machine** running `twd link`: there the CLI is
not a node at all; it talks to mgmt over your existing Tailscale
connection, which is exactly the identity mgmt wants to see.

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
twd doctor
```

It checks that workerd runs, that the built-in bundler can build every
template offline, that `~/.twd` (and the state dir) are writable, that the
process sandbox (Seatbelt on macOS, Landlock + seccomp on Linux) works and
enforces, and that twd is not using the system tailscaled. Every line has
a `fix:` if it fails. Once twd is up, the `mint_mode` row says which mode
is active (`oauth`, `wif (keyless)` or `apikey`; it warns if the OAuth
client file is readable by anyone but you) and `api_key` is `skip` unless
you chose the API-key fallback.

twd mints a Tailscale auth key for every node it runs (mgmt's own node,
this host's node, every object), so it needs **one** credential. Three
choices, in order of preference; `twd up` with none of them prints the
same list and exits (it never starts a browser flow on its own).

Two machines appear below: the **host** (runs `twd up`: mgmt, the host
agent, your apps' nodes) and a **developer machine** (your laptop: `twd
link`, `twd deploy`). They can be the same machine; then skip `twd link`.

### Recommended: OAuth client

An OAuth client scoped to auth keys for `tag:twd`: the same mechanism the
Tailscale Kubernetes operator and GitHub Action use. Its secret never
expires and stays in a 0600 file on the host. **No browser login, no
Funnel, no HTTPS requirement, no public issuer.**

1. **Policy first** (admin console › Access controls). `tag:twd` must own
   mgmt's and the host's tags, so the client can mint their keys:
   ```jsonc
   "tagOwners": {
     "tag:twd":      ["autogroup:admin"],
     "tag:twd-mgmt": ["autogroup:admin", "tag:twd"],
     "tag:twd-host": ["autogroup:admin", "tag:twd"],
   },
   "grants": [
     // hosts call mgmt's API; mgmt pushes kills to the hosts
     {"src": ["tag:twd-host"], "dst": ["tag:twd-mgmt"], "ip": ["tcp:443", "tcp:80"]},
     {"src": ["tag:twd-mgmt"], "dst": ["tag:twd-host"], "ip": ["tcp:7474"]},
     // admins may use twd over the tailnet (twd link)
     {"src": ["autogroup:admin"], "dst": ["tag:twd-mgmt"], "ip": ["tcp:443", "tcp:80"], "app": {"twd.dev/cap/admin": [{}]}},
   ],
   // no nodeAttrs: no Funnel in this mode
   ```
   `tag:twd` must exist before the console lets you tag a client with it.
2. **Generate the client** (admin console › Settings › OAuth clients ›
   Generate OAuth client, <https://login.tailscale.com/admin/settings/oauth>):
   - **Auth Keys › Write**, tag **`tag:twd`** (required);
   - optionally **Devices › Core › Write**, tag **`tag:twd`**: `twd kill`
     then also deletes the device (without it the kill still holds, the
     node is unreachable and tombstoned, but the device stays listed;
     the kill result says `device_delete_unavailable` with this fix);
   - optionally **Policy File › Read**: `twd deploy` preflights your
     app's grants and `spawn()` checks sub-agent tags/caps against the
     policy (without it, deploy says `preflight skipped` and `spawn()`
     answers `policy_unavailable`).
3. **Save it** where only you can read it (twd refuses a looser mode):
   ```sh
   mkdir -p ~/.config/twd
   printf 'TS_OAUTH_CLIENT_ID=%s\nTS_OAUTH_CLIENT_SECRET=%s\n' '<client id>' '<tskey-client-…>' > ~/.config/twd/oauth.env
   chmod 600 ~/.config/twd/oauth.env
   ```
4. **Start twd:**
   ```sh
   twd up --oauth-client-file ~/.config/twd/oauth.env --service --yes
   ```
   Before minting anything, `twd up` exchanges the client for an access
   token (a wrong or revoked secret stops here: `oauth_exchange_failed`),
   reads the tailnet name from the client's devices (or from mgmt's node
   once it joins; `--tailnet` overrides), and, if the client can read the
   policy, checks that `tag:twd` owns `tag:twd-mgmt` and `tag:twd-host`.
   If not, or if the control plane refuses the first key with
   `requested tags … are invalid or not permitted`, it stops with
   **`policy_missing_tag_owners`** and the exact tagOwners snippet;
   nothing is minted. Then the client mints mgmt's node key
   (`tag:twd-mgmt`) and, through mgmt, the host's (`tag:twd-host`); both
   join with no login. twd stores the file's **path** and the client's
   public ID only; `twd-mgmt` re-reads the secret at each token exchange
   (cached until shortly before it expires; a 401 triggers one fresh
   exchange) and never logs it.

mgmt serves its API on its node over plain HTTP on `:80` (WireGuard
encrypts it; WhoIs authenticates every request) and, when the tailnet has
HTTPS certificates, on `:443` too (mgmt fetches the certificate in the
background at start). `twd link` tries https first, then http, and says
which it used.

To rotate or upgrade the client (e.g. add Devices Core), generate a new
one, overwrite the file (0600), run `twd up` again (it updates the
tenant), then revoke the old client in the console. Revoking the client is
also your kill switch for all future minting.

### Alternative: workload identity federation (`twd up --wif`)

No secret exists anywhere, which suits a hosted / multi-tenant mgmt; the
price is a browser login, Funnel and HTTPS on the tailnet, and a public
OIDC issuer served by mgmt.

#### On the host: `twd up --wif`

```sh
twd up --wif              # add --service to install a LaunchAgent (starts at login)
```

What happens, in order (each line is a step you'll see):

1. **Policy snippet.** `twd up` prints what your tailnet policy needs and,
   in a terminal, asks you to confirm it is in place **before** it
   requests a login URL (the login is refused with `requested tags
   [tag:twd-mgmt] are invalid or not permitted` otherwise; that refusal
   is reported as `policy_missing_tag_owners` with the snippet):
   ```jsonc
   "tagOwners": {
     "tag:twd":      ["autogroup:admin"],
     "tag:twd-mgmt": ["autogroup:admin", "tag:twd"],
     "tag:twd-host": ["autogroup:admin", "tag:twd"],
   },
   "grants": [
     {"src": ["tag:twd-host"], "dst": ["tag:twd-mgmt"], "ip": ["tcp:443", "tcp:80"]},
     {"src": ["tag:twd-mgmt"], "dst": ["tag:twd-host"], "ip": ["tcp:7474"]},
     // admins may use twd over the tailnet (twd link); ip: the API ports, app: the twd admin capability
     {"src": ["autogroup:admin"], "dst": ["tag:twd-mgmt"], "ip": ["tcp:443", "tcp:80"], "app": {"twd.dev/cap/admin": [{}]}},
   ],
   "nodeAttrs": [
     // Funnel publishes mgmt's OIDC issuer, so Tailscale can verify its tokens
     {"target": ["tag:twd-mgmt"], "attr": ["funnel"]},
   ],
   ```
   Your apps' tags (`tag:twd-wl-<name>`, object class tags) are added as
   usual: `twd deploy` preflights them and prints the snippet.
2. **Login URL.** mgmt's node `twd-mgmt` joins your tailnet with
   `tag:twd-mgmt`. twd prints a `https://login.tailscale.com/a/…` URL;
   open it and sign in as a tailnet admin. The node's state is kept under
   `~/.twd/mgmt/tsnet`, so later `twd up` runs need no login. The tailnet
   name is read from the login (no `--tailnet`). If the tailnet has no
   HTTPS certificates, or `tag:twd-mgmt` has no Funnel attribute, `twd up`
   stops here and says exactly what to enable; the node stays joined.
3. **Trust credential.** mgmt starts and publishes its OIDC issuer over
   Funnel at `https://twd-mgmt.<tailnet>.ts.net/t/tn_<tailnet>`. twd prints
   the exact admin-console steps: Settings › Trust credentials › Add, with
   the issuer URL, the subject `twd:tenant:tn_<tailnet>:mint`, scope
   `auth_keys`, tag `tag:twd`; and an optional second **lifecycle**
   credential (subject `…:lifecycle`, scopes `devices:core` and
   `policy_file:read`) that enables kill tier 2 (device delete), the
   reaper, spawn checks and deploy preflight. Paste each credential's
   **client ID** at the prompt (they are public identifiers; twd stores
   nothing else). Non-interactive: `twd up --wif-client-id <id>
   [--wif-lifecycle-client-id <id>]` (implies `--wif`); without the IDs, `twd up --wif --yes`
   prints the steps, leaves the stack running and exits with
   `wif_credential_required`.
4. **Ready.** twd waits until the issuer answers from the public internet
   (the first HTTPS certificate for a new node name takes 1–2 minutes),
   creates the tenant in mint mode `wif`, and verifies both credentials
   with a real token exchange (`✓ trust … exchanges`). From now on every
   object node's auth key is minted through the trust credential: single
   use, pre-authorized, tagged, ≤ 5 min. Deleting the credential in the
   console is your kill switch.

Why a console step at all: creating *new* devices needs a control-plane
credential; a connection's identity can't do it. The trust credential is
the keyless form of that credential: no secret exists anywhere, only a
trust relationship with mgmt's issuer, scoped to `auth_keys` for `tag:twd`.
Until Tailscale offers a "connect a platform" consent page, this is one
paste per tailnet.

`twd up … --service` on macOS installs the LaunchAgent
`~/Library/LaunchAgents/dev.twd.up.plist` (per user, no sudo) so the
supervisor starts at login; `twd down --service` removes it. **Sleep:** a
sleeping Mac serves nothing. `twd up` prints a hint if sleep is enabled:
`sudo pmset -a sleep 0 displaysleep 10`, or run `caffeinate -s` in a
terminal, or use a Mac mini that doesn't sleep.

#### On a developer machine: `twd link`

Your laptop is on the same tailnet with the Tailscale app, as yourself (an
`autogroup:admin` member, or a user/group/tag granted `twd.dev/cap/admin`
on `tag:twd-mgmt`). Install twd there too (steps 1–2), then:

```sh
twd link twd-mgmt.<tailnet>.ts.net       # https first, then http on :80; or just `twd link`: finds twd-mgmt through MagicDNS
twd init support --template agent        # or hello, mcp, ai-agent; node-workflow needs Node.js on this machine
cd support && twd dev                    # optional: run it locally against an embedded control plane
twd deploy                               # bundles here (built-in esbuild), uploads to mgmt over the tailnet, waits until the node is ready
twd ls · twd audit <object> · twd kill <object> --cascade
```

`twd link` calls mgmt's `GET /v1/whoami` over your own Tailscale
connection. mgmt identifies the caller by **WhoIs** (your login, or your
node's tags) and admits admins by the `twd.dev/cap/admin` capability; it
stores only the endpoint in `~/.twd/link.json`. No key, no token, nothing
to rotate. If you are not an admin, `twd link` fails with `not_admin` and
prints the grant to add. `twd ls` shows `by <login>` on each deployment
and `twd audit` carries your login on the deploy record. Logs stay on the
host (`twd logs` there). `twd link --unlink` forgets the endpoint.

#### Transport: tsnet only

Normal operation never uses a unix socket or a token. mgmt serves the
`/v1` API (OpenAPI: `api/openapi.yaml`, rendered in
`docs/contracts/mgmt-api.md`) on its own tailnet node `twd-mgmt`
(`tag:twd-mgmt`), authenticated by WhoIs. twd-hostd runs its **own** node
`twd-host-<hostID>` (`tag:twd-host`), joined once with a single-use key
mgmt mints through the tenant's credential (OAuth client, trust credential
or API key), and talks to mgmt
over the tailnet; mgmt pushes kills to that node (port 7474). The `twd` CLI
on the host machine reaches mgmt through the node's loopback proxy
(`~/.twd/mgmt/loopback.json`, 0600), so it is the operator by WhoIs; a
developer machine uses `twd link` (its own Tailscale) or `twd link --tsnet`
(no Tailscale installed: an ephemeral CLI node with an interactive login,
state under `~/.twd/cli`). `twd doctor` has a `transport` row; `ok tsnet:`
is the expected answer. The policy additions `twd up` prints include the
host ⇄ mgmt grants (`tag:twd-host → tag:twd-mgmt tcp:80,443`,
`tag:twd-mgmt → tag:twd-host tcp:7474`).

Add a second machine as a host with `twd host add <host-id>`: mgmt records
it (pending) and writes a single-use join key file; on the new machine run
`twd-hostd join --host-id <id> --token-file <file> --control-url …`, then
start `twd-hostd -host-id <id> -mgmt http://twd-mgmt.<tailnet> -tsnet-control-url …`.
`twd host ls` / `twd host rm <id> [--drain]` manage hosts; `twd tenant set`
(PATCH) and `twd tenant rm`, `twd undeploy <workload>` round out the CRUD.

`twd up --local-dev` keeps the old unix socket + per-host token for
`twd dev` and hermetic tests only; `twd doctor` flags it.

### Last resort: API-key mode

For tests, or when you'd rather hand twd a Tailscale API access token
(it expires within 90 days and can do anything its owner can; prefer an
OAuth client): put it in a file as `TS_API_KEY=tskey-api-…`,
mode `0600`, and name the tailnet explicitly (a key doesn't name its
tailnet, and twd never bootstraps from your Mac's tailscaled):

```sh
twd doctor --tailnet <your-tailnet>.ts.net --api-key-file ~/path/to/api.env
twd up --tailnet <your-tailnet>.ts.net --api-key-file ~/path/to/api.env --yes
twd init support --template agent && cd support && twd deploy
```

The key is read when needed and never copied, logged or printed; `twd
doctor` labels the mode `apikey`. `twd up` prints the `tag:twd` tagOwners
snippet if it's missing (`--policy-apply` writes it, validated, with
If-Match). `twd link` works in this mode too if you give mgmt a tailnet
node: `twd up … --mgmt-hostname twd-mgmt` (the join is interactive unless
`--mgmt-authkey-file` is given). A state dir is bound to one mode (`twd
up` with no mode flag reuses it); switch with `twd down --purge` and a
fresh `--state-dir`.

### 5. Day 2, stop and uninstall

`twd up` starts `twd-mgmt` (SQLite, local key custody) and `twd-hostd`
(workerd supervisor) under a detached supervisor; state and logs are in
`~/.twd` (`--state-dir` to use another directory). `twd deploy` preflights
the grants your app declares (`tailnet.connects`), bundles it, and waits
until the workload node is on your tailnet:
`http://<prefix>wl-support.<tailnet>/`. Day 2: `twd ls`, `twd logs
support`, `twd kill <object> --cascade`, `twd audit <object>`, `twd status`.

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
started with `twd up --insecure-shared-host`. (`twd tenant add` is apikey
mode only for now; keyless second tenants are a follow-up.)

```sh
twd down --purge           # kill every object and delete its devices, then stop
~/Downloads/twd/twd-*-darwin-arm64/install.sh --uninstall --purge   # remove ~/.twd
```

`--uninstall` without `--purge` keeps your state (`twd.json`, logs, data).

### Troubleshooting

- `workerd does not run on this machine`: re-sign it (`codesign -f -s -
  ~/.twd/bin/workerd`) and clear quarantine (`xattr -d com.apple.quarantine
  ~/.twd/bin/workerd`), then `twd doctor`.
- `mint_mode_required` from `twd up`: pick a credential (OAuth client
  recommended); the error lists all three.
- `policy_missing_tag_owners`: `tag:twd` must own `tag:twd-mgmt` and
  `tag:twd-host` (and, in `--wif`, your login must own `tag:twd-mgmt`);
  add the printed tagOwners and re-run. Nothing was minted.
- `credential_file_insecure`: `chmod 600` the OAuth client file.
- `oauth_exchange_failed`: the client secret is wrong or the client was
  revoked; generate a new one (Settings › OAuth clients).
- `device_delete_unavailable` on `twd kill`: the kill held, but the OAuth
  client has no Devices › Core scope; generate one with it, overwrite the
  file, `twd up`, revoke the old client; remove leftover devices in the
  console (Machines).
- `https_required` / `funnel_required` from `twd up --wif`: enable HTTPS
  certificates (admin console › DNS) or add the `nodeAttrs` funnel entry
  for `tag:twd-mgmt`; mgmt's node stays joined, re-run `twd up`.
- `bootstrap_failed … not a tagOwner`: the account you logged in with may
  not own `tag:twd-mgmt`; add the tagOwners snippet, remove the node in the
  console, `twd up` again.
- `issuer_unreachable`: the Funnel issuer never answered publicly. Check
  the funnel attribute, and `twd logs --component mgmt`.
- `wif_exchange_failed`: the credential's issuer or subject doesn't match
  exactly what `twd up` printed (watch for a trailing slash or the wrong
  tenant ID); fix it in the console and `twd up --wif-client-id <id>`.
- `not_admin` from `twd link`: you need `twd.dev/cap/admin` from a grant
  to `tag:twd-mgmt`; the error prints the grant.
- `mgmt_unreachable` from a linked machine: is your Tailscale connected to
  the tailnet that runs twd, and does a grant let you reach
  `tag:twd-mgmt` on tcp:443?
- `tailnet_required`: apikey mode needs `--tailnet`; twd won't bootstrap
  from your Mac's Tailscale login.
- `acl_denied` on deploy: add the printed grant to your tailnet policy, or
  deploy with `--allow-missing-grants`. `preflight skipped` means mgmt has
  no lifecycle credential with `policy_file:read` to read the policy.
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
- Logs: `twd logs --component mgmt|hostd|supervisor` (on the host).

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
- The same as on macOS: a tailnet where you are an admin and an
  [OAuth client](#recommended-oauth-client) file (`twd up
  --oauth-client-file …`; no HTTPS needed); or HTTPS + Funnel for
  [workload identity federation](#alternative-workload-identity-federation-twd-up---wif);
  or an API access token file for the [API-key mode](#last-resort-api-key-mode).

twd **never uses the machine's tailscaled**. If a system Tailscale is
installed and logged in to some other tailnet, it is left alone: every twd
node is its own tsnet node of the tailnet you log in to (or pass with
`--tailnet` in apikey mode).
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
twd doctor
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
twd up --service                                                             # keyless (login URL + trust credential, as on macOS)
twd up --tailnet <your-tailnet>.ts.net --api-key-file ~/path/to/api.env --yes --service   # apikey mode
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
keys. **Sandbox: none on every row in L1's run (made before the Landlock sandbox landed).** **microVM
tier: unavailable on every row** (no `/dev/kvm` in these VMs, no
firecracker, not root). Raw results: `e2e/real/results/l1-linux/`.
With the sandbox on, the Ubuntu 24.04 arm64 row (reboot + ephemeral phases),
Debian 12 and Ubuntu 24.04 amd64 were re-run during integration, and Ubuntu
24.04 arm64 again on the final tree (`sandbox ok`, both workloads
`process/landlock`, 410 after kill in ≤ 1 ms, reboot → answering 7.7 s;
STATUS.md "Integration (Linux)" and "Final verification"). The microVM
tier's own rows (`make e2e-linux-vm`, `make e2e-linux-reboot` on a
nested-virt VM, from the shipped tarball) are there too.

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
DO storage and workerd. A linux-arm64 or linux-amd64 release **built with
the microVM assets** ships everything the tier needs in
`~/.twd/share/twd/vm/`, for its arch: the guest kernel (`vmlinux`, Linux
6.1 LTS with Firecracker's CI config plus erofs/overlayfs; `kernel.json` has
its hashes), the read-only guest image (`rootfs.ext4`, reproducible;
`manifest.json` has its hashes and arch), Firecracker and its jailer
(v1.17), plus `bin/twd-vmhelper`. Every input is pinned in
`guest/inputs.sh`; `make vm-assets GUEST_ARCH=amd64|arm64` builds either
arch on either. A guest of the wrong arch is refused (`twd doctor`:
`microvm_tier warn … is arm64, this host is amd64`).

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

- Linux arm64 or amd64 (x86_64) with `/dev/kvm` (bare metal, or a VM with
  nested virtualization), cgroups v2 and systemd. Tested end to end on
  Ubuntu 24.04 arm64. amd64: the assets are built and the guest boots to
  workerd, but the only x86 test host so far (nested KVM in a QEMU VM on an
  AMD Zen 1 NAS, whose TSC it reports unstable) stalls its guests in an
  interrupt storm, so the amd64 tier is not yet verified end to end.
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
