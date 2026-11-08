# twd overnight build plan (2026-11-07 → 11-08)

**Goal (Ross):** wake up to a working implementation that installs on another MacBook (`mbp`) and works, with unit and end-to-end tests passing.
**Design source of truth:** `~/dev/tailnet-agents-design` (private repo `rosskukulinski/tailnet-agents-design`): RFC.md, DX.md, DECISIONS.md (ADR-001…022), prototype/per-object-identity (working netd + workerd chain to port).

## Definition of done (in priority order; stop-the-line on safety rules)
1. **E2E-MAC (the headline):** on `mbp` (M2 Pro, macOS 26.6, clean, no Go/Node), run the release tarball's `install.sh`. Then:
   1. `twd up --tailnet tail7906d5.ts.net --api-key-file …` (non-interactive test mode)
   2. `twd init hello --template agent`
   3. `twd deploy`
   4. a tsnet test client on tailnet 1 calls the agent. All of these pass:
      - (a) the response contains the verified caller (`getCaller`);
      - (b) the agent's `fetch()` to a private echo node leaves from **the object's own node** (the echo's WhoIs proves it);
      - (c) an MCP tool call (`createMcpHandler` + `withTailnet`) is allowed or denied by the app cap, with a typed `cap_missing` error;
      - (d) `spawn()` creates a sub-agent with its own node and narrower caps;
      - (e) `twd kill --cascade` stops it in under 1 s, and later calls fail fast (RST, not a timeout);
      - (f) `twd audit` shows the human → agent → sub-agent chain.
2. **E2E-MULTI:** the same host also serves tenant B on **tailnet-2**, and isolation holds: a tenant-A object can't see or reach tenant B.
3. **Unit tests** (`make test`, hermetic: Go testcontrol + local DERP + stock workerd) are green on macOS and in Lima.
4. **E2E-VM (Linux):** in Lima `twd-dev`, the same app runs in a **Firecracker** microVM with a host-side tsnet NIC (tailvisor netd library), and keys never enter the guest.
5. **SDKs (ADR-022):** the Cloudflare Agents SDK (`Agent`, `createMcpHandler`) and the Vercel AI SDK (`ToolLoopAgent`) run in-isolate with conformance tests. The **opt-in Node process tier** plus **Vercel Workflow** come after 1–4.

## Milestones (commit and push each; update STATUS.md after each)
- **M0** Repo skeleton (RFC §15 layout), Makefile (`test`, `e2e-local`, `e2e-vm`, `e2e-real`, `release`), Go module, JS workspace, the secrets hook, STATUS.md.
- **M1** `netd` library, ported and hardened from the prototype:
  - a multi-node registry;
  - ingress: WhoIs → cfBlob, with identity headers stripped;
  - an egress guard (ADR-019: netmap-only resolution, tailnet-IP allow list, refuse self/host/loopback) that returns `peer_not_visible` / `forbidden_destination`;
  - `OmitAuth`;
  - kill = RST all flows, then `Close()`, then a device-delete hook;
  - per-object state dirs;
  - parallel Close.
- **M2** `twd-mgmt`:
  - Postgres leases with epoch fencing (an SQLite backend for homelab/`twd up`);
  - object registry;
  - a mint broker with two backends: (1) API-key mode (tests/homelab), (2) the WIF issuer via Funnel, with per-tenant trust and `auth_keys` scope;
  - a registration throttle of 200/min per tenant;
  - spawn checks (caps ⊆ parent from policy);
  - a reaper;
  - an audit log, hash-chained.
  - Custody: an encrypted local store in homelab mode; the setec client behind an interface.
- **M3** `twd-hostd`, process tier:
  - workerd supervisor;
  - generated platform wrapper (entry → DO namespace, facet-based per-object egress props as proven in the prototype; plain-DO mode for alarm-using classes);
  - async node creation on first `create()`/`spawn()`;
  - the `@twd/tailnet` JS package (`getCaller`, `ctx.tailnet.fetch/spawn/forward`).
- **M4** `twd` CLI: `up`, `init`/templates, `dev`, `deploy`, `status`, `kill`, `audit`, `host add/ls/doctor`, `tenant set`. Errors use the DX.md catalog format.
- **M5** SDK conformance: Cloudflare `agents` (`Agent`, state, `this.sql`, schedule, `createMcpHandler`) and Vercel `ai` `ToolLoopAgent` with a mock LLM provider (no real LLM keys needed).
- **M6** Release packaging: `make release` produces a darwin-arm64 + linux-arm64 tarball with workerd pinned and an `install.sh` that needs no sudo and installs under `~/.twd`. Install on `mbp` via scp (the private repo means mbp has no gh auth). Run E2E-MAC there.
- **M7** E2E-MULTI on tailnet-2.
- **M8** Firecracker tier in Lima (E2E-VM).
- **M9** Opt-in Node process tier plus Vercel Workflow.

## Environment and credentials (validated 2026-11-07 22:30 PT)
| Thing | Where | Verified |
|---|---|---|
| tailnet 1 = operator + tenant A, `tail7906d5.ts.net`, HTTPS on, Funnel on for `tag:twd-mgmt` | API key in `~/.config/tailnet-agents-design/tailnet1_api.env` (`TS_API_KEY`) | ✅ mint auth keys; ✅ create/delete federated (WIF) trust credentials via API; ✅ policy edit (validate + If-Match); ✅ Funnel public fetch (first one ~55 s) |
| tailnet-2 = tenant B | `~/.config/tailnet-agents-design/tailnet2_api.env` | ✅ mint auth keys; ✅ policy edit. HTTPS **off** (not needed) |
| Policy backups (originals) | `~/.config/tailnet-agents-design/tailnet{1,2}_acl.backup.hujson` | ✅ |
| Lima VM `twd-dev` (Ubuntu 24.04 arm64, 8 CPU / 16 GiB, nested virt), mounts `~/dev/twd` + design repo | `limactl shell twd-dev` | ✅ /dev/kvm; ✅ Firecracker v1.17.0 boots a guest (~1.9 s full cycle, nested); kernel + rootfs in `~/fc`; ✅ Go 1.27.3; ✅ Node 24 |
| This Mac (M5 Pro, macOS 27.2) | — | Go 1.27.3, Node 25, OrbStack docker (✅ postgres:17-alpine, ✅ headscale image; ❌ minio image unavailable, so use Postgres plus local-disk backup) |
| `mbp` (M2 Pro, macOS 26.6, 32 GB, 96 GB free) | `ssh mbp` | ✅ key auth; ✅ passwordless sudo (avoid using it); Homebrew at /opt/homebrew; Tailscale.app is on Ross's **personal** tailnet. **No Firecracker possible** (M2, no nested virt), so it runs the process tier |
| GitHub | `gh` as rosskukulinski | ✅ private repos `rosskukulinski/twd` and `rosskukulinski/tailnet-agents-design` |
| Sleep | `caffeinate -dimsu` running for 14 h on this Mac | ✅ |

## Safety rules (non-negotiable)
1. **Never touch Ross's personal tailnet (`kukulinski.com` / `stonecat-wage.ts.net`).** No policy reads or writes and no `tailscale` CLI changes there. On `mbp`, twd must never use the system tailscaled's identity. Always pass the test tailnet explicitly.
2. Only tailnet 1 (`tail7906d5.ts.net`) and tailnet-2 may be modified. Validate before every policy write and send `If-Match`. Keep the original backups.
3. Test nodes are ephemeral and named `ttest-*` / `twd-*`. Clean up after each e2e run: delete devices via the API, revoke keys.
4. Never print, log or commit any `tskey-*`. Keep the pre-commit hook installed. Redact logs.
5. Push only to the two private repos above. Make no public posts, issues or PRs.
6. On `mbp`, install only under `~/.twd` and `~/dev`. No sudo, no system changes, no Homebrew installs unless unavoidable (and record them in STATUS.md).
7. Measured beats claimed. STATUS.md must say what passed, what failed and what was skipped.
EOF
cd ~/dev/twd && git add -A && git commit -qm "BUILD-PLAN: overnight goals, milestones, validated environment, safety rules" && git push -q && echo pushed