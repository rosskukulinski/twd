# Good morning: overnight build summary (2026-11-08)

**Result: every item in BUILD-PLAN.md's definition of done is met, and Linux is a first-class host (LINUX-PLAN.md).** twd installs on a clean MacBook (`mbp`) and on fresh Linux VMs and works end to end against real tailnets. Unit and e2e suites pass on macOS and Linux, re-measured on the final tree (branch `final-verify`, fast-forward of master; STATUS.md "Final verification"). `make release` writes `dist/twd-0.1.0-<commit>-{darwin-arm64,linux-arm64,linux-amd64}.tar.gz`.

## Install it on another Mac
Follow `docs/INSTALL.md`. Short version (M10, keyless by default; STATUS.md "M10"):
```sh
scp dist/twd-0.1.0-<commit>-darwin-arm64.tar.gz other-mac:
ssh other-mac 'tar xzf twd-*.tar.gz && ./twd-*/install.sh'        # ~6 s, no sudo, installs into ~/.twd
# host
twd up [--service]      # prints the policy snippet → a login URL (mgmt's node joins as tag:twd-mgmt) → the admin-console
                        # steps for the WIF trust credential (issuer over Funnel) → paste the client ID → ready. No API key.
twd doctor              # workerd, esbuild, sandbox active, mint_mode wif (keyless), system tailscaled NOT used
# your laptop (on the same tailnet, as an admin)
twd link twd-mgmt.<tailnet>.ts.net      # WhoIs identity over your own Tailscale connection; no key or token
twd init hello --template agent && cd hello && twd dev && twd deploy && twd ls && twd audit --all
# alternative (tests, no HTTPS): apikey mode
twd up --tailnet <your-test-tailnet> --api-key-file <file with TS_API_KEY=…> --yes
```
twd never uses the host's Tailscale app or login for its nodes: mgmt's node and every object node are tsnet nodes. The one-time console step that remains is the trust credential: creating new devices needs a control-plane credential, and WIF is its keyless form (ADR-026).

## What was verified (measured, final runs on master)
| Acceptance test | Result |
|---|---|
| **E2E-MAC on mbp** (M2 Pro, macOS 26.6.2, clean). twd from the release tarball on mbp; test client + echo on this Mac over the tailnet | **all checks green in 9 of 9 runs** (5 by M6, 2 by the integration agent, 2 by me; the last one on master had 20/20 checks). Install 5.7 s · `twd up` → ready 1.57 s · deploy → ready ~1.6 s · first object node 1.7 s · spawn 2.0 s · kill 79 ms · request to a killed object gets a typed 410 in 8 ms · zero leftovers. **Correction (red team, RT-01):** those 9 runs' check (d) "sub-agent with narrower caps" only verified that mgmt *recorded* the narrower caps; receivers still saw the child's full class-tag caps, so least privilege was not enforced. Fixed on master (node certificates, `rt-fixes`); (d) now asserts the **denial** (`cap_missing` on `refund` for a same-tag child spawned with `lim=[lookup_order]`) **and** that the unnarrowed parent is admitted. Measured again on the final tree: see "Final verification" below and STATUS.md. |
| E2E-MULTI (tenant A on tailnet 1 + tenant B on tailnet-2, one host, same app names) | 5/5 runs. Cross-tenant reach, by name or IP, in both directions: `peer_not_visible` 8/8 · per-tenant audit · kill isolation · **Seatbelt sandbox** blocks reading other tenants' state, keys and the mgmt socket |
| Unit / integration (`make test`, hermetic) | Green on macOS and Linux (Lima), 10 Go packages + JS |
| E2E-VM: Firecracker in Lima (nested virt) | VM boot → ready 0.60 s · ~120 MiB per VM · request p50 0.69 ms (vs 0.37 ms in-process) · **no key material in guest disks, fs or a RAM snapshot** · guest can't reach the host network |
| SDKs | Cloudflare Agents SDK 0.33 (state, SQL, schedule, WebSocket hibernation, routing, MCP client), `createMcpHandler`, Vercel `mcp-handler`, Vercel AI SDK 7 `ToolLoopAgent` (mock LLM). See docs/COMPAT.md |
| Opt-in Node tier + Vercel Workflow | Signed caller header · egress attributed and SSRF-guarded · workflow survives a kill mid-sleep and mid-hook (resume ~0.6 s) · ~90 MB per app, ~300 MB with Workflow |
| Real-tailnet netd suite | Ingress +0.33 ms p50 · egress p50 0.69 ms · kill → caller fails in 0.12–0.68 ms |

## Linux (measured on the final tree, 2026-11-08; details in STATUS.md "Final verification")
Linux is a first-class host: the same tarball flow (`install.sh` into `~/.twd`, no root), a **Landlock + seccomp** process sandbox, `twd up --service` (systemd user unit), and on KVM hosts the **Firecracker microVM tier** with persistent encrypted DO storage. Fresh Lima VMs, nothing mounted, tarball via `limactl copy`; test client + echo on this Mac over tailnet 1.

| Row | Result |
|---|---|
| **Ubuntu 24.04 arm64** (kernel 6.8, Landlock ABI 4), `make e2e-linux` with reboot + ephemeral phases | **PASS** (run 2; run 1 failed on a harness timing flaw, fixed, see STATUS). doctor 19 rows none failed, `sandbox ok`; install 0.56 s · `twd up --service` 0.66 s · deploy → ready 3.8 s · first object node 1.6 s · kill 70 ms · **410 in ≤ 1 ms** · `systemctl --user restart` → answering 6.7 s · **reboot** → service active 6.7 s, answering this Mac **7.7 s**, 2 objects rejoined with their old node IDs · **ephemeral** sub-agent re-minted after a restart in 4.1 s, answering at 9.5 s · `twd down --purge --service` → redeploy on the same state dir works · 0 leftovers |
| **microVM tier** (`twdl-kvm-u24`, nested virt), `make e2e-linux-vm` **from the shipped Mac-built tarball** | **PASS** 23/23 in 42 s: `sudo twd host setup --microvm` 1.0 s (the only sudo) · agent + mcp in jailed Firecracker VMs (boot 0.97 s) · checks (a)–(f) · DO storage survives a VMM SIGKILL (1.15 s) and a redeploy · no key material in either guest (28 needles) · `setup --undo` |
| **Host reboot with microVMs**, `make e2e-linux-reboot` (clean stop + power cut) | **PASS**: helper back 3.7–4.2 s after kernel boot, `twd up` 0.6 s, LUKS2 volume re-attached in ~25 ms, same node ID, every write present; a pre-existing client re-finds the node in **20 s (clean) vs 6 s (power cut)**, a client-side WireGuard stale-session effect, explained in STATUS/INSTALL |
| Lima `twd-dev` | `make test` (`-race`) 164 s · `test-node` 35 s · `test-sdk` 91 s · `e2e-vm` 19 s (13/13): all **PASS** |
| Packaging | The Mac-built linux-arm64 tarball **lacked the microVM guest** (red team, pass 2). `make release` now builds it in `twd-dev` and includes it (`share/twd/vm`, `release.json` `"microvm": true`; 162 MB); linux-amd64 has no guest. |

Linux rows from before this tree (Debian 12, Ubuntu 22.04, Fedora 45, Ubuntu 24.04 amd64) are in `docs/INSTALL.md`'s support matrix and STATUS.md (L1 / Integration); they were not re-run on the final tree.

## Bugs found and fixed overnight (the interesting ones)
- **Cross-tenant key exposure:** without a sandbox, any tenant's workerd could read every tenant's node keys and drive mgmt's admin socket. Fixed with a per-tenant **Seatbelt** profile on macOS, now applied to Node apps too and checked by `twd doctor`.
- **Identity collision:** two apps with the same DO class and object name shared a node, so one app's traffic left as the other's. Identity is now keyed by (tenant, workload, class, object), with an in-place store migration.
- **Kill:** callers hung until timeout. Fixed with RST before close, device delete before logout, push-based kill and a typed `object_killed` tombstone.
- A tsnet `Up`/`Close` data race in netd teardown.
- Facet alarms broke Agents SDK `schedule()`; they're now delegated to the parent DO.
- **Importing `undici` silently disables env proxies,** so Node egress bypassed the guard. `@twd/node` now installs a global dispatcher. Concrete proof that the process tier is not a security boundary.
- WIF token audience must be `api.tailscale.com/<clientID>`, not the bare client ID. Corrected in the design RFC.
- Clean-clone build breakages: JS build order, the npm lockfile across npm versions, release vendoring.

## Known gaps (honest)
- **Red team (security/RED-TEAM.md) found two High and one Critical gap** behind the headline claims above: spawn `lim` was not enforced at the receiver (RT-01), tenant code could forge audit content (RT-02), and mgmt's admin socket trusted any same-uid process (RT-03, Critical on Linux process/Node hosts before the Landlock sandbox). All three are fixed on master, plus RT-04/05/06/08 (STATUS.md "RT fixes"); the red team's second, live pass on the Linux release (`rt2-live`) found RT-01..08 holding and one Low. The Node tier's egress proxy is an **enforcement boundary only under Landlock ABI ≥ 4** (Linux 6.7+) or a microVM; on macOS (Seatbelt confines files and unix sockets, not TCP) and on Landlock ABI < 4 it is cooperative: `twd status` says `egress unenforced`, `twd doctor` warns, and multi-tenant hosts refuse Node-tier apps by default.
- **Linux process tier: Landlock + seccomp sandbox** (unprivileged, no user namespaces; mirrors the Seatbelt guarantees and adds TCP confinement on ABI ≥ 4). Without Landlock, `twd doctor` warns loudly and twd serves one tenant unless `--insecure-shared-host`. Measured on the matrix below.
- **microVM tier (Linux + KVM):** DO storage persists across VM crash, redeploy and host reboot (per-workload LUKS2 volume); every VM runs under the jailer; the only sudo is the one-time `twd host setup --microvm`. Still missing: snapshot restore; Node apps inside VMs; an ephemeral microVM object across a host reboot was argued from the code path, not run. **Packaging:** the guest image can only be built on Linux; a `make release` without Lima `twd-dev` running ships a linux-arm64 tarball with **no microVM tier** (`release.json` `"microvm": false`, the red team's live pass hit this). `make release` now builds the guest in `twd-dev` and includes it (`scripts/vm-assets-lima.sh`, cached).
- All tenants' netd registries share one hostd process.
- (M10 closed this) ~~Interactive `twd up` is deliberately refused; you must pass `--tailnet`.~~ `twd up` is keyless now: tsnet login for mgmt's node, WIF minting, `twd link` for developers; `--api-key-file` is the alternative. Still true: twd never bootstraps from the host's tailscaled. `twd dev` has no embedded policy evaluator yet.
- Binaries are ad-hoc signed, not notarized. Workflow's Postgres World is untested. With multiple Postgres-backed mgmt replicas, spawn budgets can race.
- Unsupported in-isolate: `env.AI`, KV, Workflows (Cloudflare's), email, browser, `env.LOADER`, `subAgent()` (see docs/COMPAT.md).

## Policy changes made (test tailnets only; originals backed up in `~/.config/tailnet-agents-design/`)
- **tailnet 1:** test tags and grants (`tag:twd*`, `tag:ttest`, the `example.com/cap/{ttest,poi,mcp}` caps) and a Funnel nodeAttr for `tag:twd-mgmt`.
- **tailnet-2:** the tenant-B mirror of the same.
- **Your personal tailnet (`kukulinski.com`) was never touched.**

## Please do
1. **Revoke the credentials pasted into chat:** both API keys and the tailnet-1 auth key.
2. Delete the throwaway tailnets when you're done, or keep them for the next session.
3. Read STATUS.md for per-milestone detail and DECISIONS/RFC updates in the design repo.

## Suggested next steps
microVM snapshot restore · Node tier inside microVMs · an ephemeral microVM object across a host reboot (run it) · Ubuntu 22.04 / Fedora rows re-run with the sandbox on · embedded policy evaluator for `twd dev` · interactive `twd up` on a dedicated tailnet · notarized builds · the multi-host DO layer (v1, upstream workerd cluster hooks).
