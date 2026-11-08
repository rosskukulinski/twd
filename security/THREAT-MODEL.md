# twd Threat Model (red-team)

Date: 2026-11-08 · Branch `rt-redteam` · Author: red team (adversarial review of the
overnight build, M0–M9 as recorded in `STATUS.md`). Companion: `RED-TEAM.md` (findings),
`attacks/` (PoCs + regression tests).

This is the attacker's view. It is built from the design (`~/dev/tailnet-agents-design`
RFC §4.3–4.11, §8, `review/security.md`) and verified against the **implementation** in
`~/dev/twd` (netd, mgmt, hostd, the JS runtime, the generated platform wrapper). Where the
paper threat model and the code disagree, the code wins.

## 1. Assets (what an attacker wants)

| # | Asset | Where it lives |
|---|-------|----------------|
| A1 | Per-object tailnet **node keys** / machine keys | `~/.twd` state dirs, one `tsnet.Server` per object (netd) |
| A2 | Tenant **API keys** (`tskey-*`, OAuth client secrets) | tenant `apiKeyFile` paths; mgmt reads at use time |
| A3 | mgmt **custody key** + custody secrets (AES-256-GCM DEKs, WIF issuer P-256 key) | `custody.key`, `issuer` key (0600) |
| A4 | **Caller identity** (`request.cf.tailscale.caller`, Node `x-twd-caller`) | set by netd/hostd from WhoIs |
| A5 | **Object egress identity** (which node a `fetch()` leaves from) | platform Egress + MAC + netd dialer |
| A6 | **Capabilities / delegation** (policy capMap, spawn `lim`) | tailnet policy + mgmt spawn checks |
| A7 | **Control surfaces**: mgmt admin socket, hostd `hostapi.sock`, netd `ctl.sock`/`egress.sock`, push API | unix sockets, 0600 in 0700 dirs |
| A8 | **Audit log** (hash-chained, ed25519 host sigs) | mgmt store; `twd audit` |
| A9 | **Budgets / leases** (epoch fencing, maxLive/maxDepth, registration throttle) | mgmt store |

## 2. Trust boundaries

```
 tailnet client ──(WhoIs)── netd ingress ── hostd routing ── platform wrapper ──┐
                                                                                 ├─ tenant isolate/VM (HOSTILE)
 tenant code ── globalOutbound(Egress+MAC) ── netd egress(ADR-019) ── tailnet ──┘
                      │                                   │
                hostapi.sock                         mgmt admin sock ── Tailscale control API
```

- **B1 Tenant code ↔ platform.** The sharpest boundary. Tenant code (workerd isolate,
  Node process, or microVM guest) is fully hostile (a prompt-injected agent). It must not
  reach A1–A3/A7 or forge A4–A6.
- **B2 Tenant ↔ tenant** on one host. One hostd process, one uid, per-tenant `tsnet`
  registries. Isolation is the netmap (egress), object state dirs (0700), and — on the
  process/Node tiers — the OS sandbox (Seatbelt on macOS only).
- **B3 Object ↔ object** within a tenant. Per-object nodes + the egress MAC bind
  attribution; spawn `lim` is meant to narrow a child.
- **B4 Host ↔ mgmt.** hostd authenticates by lease epoch + (tsnet) WhoIs `tag:twd-host`;
  the local admin socket trusts same-uid peercred.
- **B5 Platform ↔ Tailscale control.** mint broker (API-key / WIF). Keys are single-use,
  ≤300 s, ephemeral for children.

## 3. Attacker capabilities per tier

| Tier | Native code? | Isolation mechanism | Residual attacker power |
|------|-------------|---------------------|-------------------------|
| **workerd process (macOS)** | only via V8 escape | V8 isolate + **Seatbelt** profile | after an escape, fs/socket blocked by Seatbelt; strong |
| **workerd process (Linux)** | only via V8 escape | V8 isolate **only** (`ResolveSandbox→none`) | after an escape: full same-uid fs + all control sockets (A1–A3,A7) — **cross-tenant** |
| **Node process (macOS)** | **yes, natively** | Seatbelt profile (EPERM on others' dirs) | raw sockets bypass egress proxy (A5) to loopback/LAN/internet; fs blocked by Seatbelt |
| **Node process (Linux)** | **yes, natively** | **none** | full same-uid fs + mgmt admin socket → **cross-tenant takeover, no exploit needed** |
| **microVM (Linux, Firecracker)** | yes, inside guest | VM + per-VM netns + gVisor gateway; keys never enter guest | confined to one workload's objects; can forge attribution *among its own VM's objects* (MAC key is in-guest); no jailer/seccomp beyond Firecracker's |

Key asymmetry the docs under-state: **the only real isolation boundary on Linux is the
microVM tier**, and it is not the default, is not offered for the Node runtime, and loses
DO storage on restart. The process/Node tiers on Linux are effectively single-tenant.

## 4. Attacker model and goals (mapped to scope)

1. **Tenant→tenant** (B2): read A1/A2/A3, forge A5 to another object, reach another
   tenant's objects. → netd netmap isolation holds; filesystem/socket isolation is
   OS-sandbox-dependent (fails open on Linux process/Node).
2. **Tenant→platform** (B1): reach A7, forge A4/props/`ctx.props`, abuse spawn to
   escalate A6 or exhaust A9, SSRF via egress. → props/caller are unforgeable in-isolate;
   egress ADR-019 guard is robust for workerd; spawn `lim` is **not enforced** (RT-01);
   Node-tier egress is advisory (RT-08).
3. **Identity/crypto** (A4/A6/A3): OBO assertions, caller HMAC, WIF, custody. → OBO
   assertions are **not implemented** (RFC §4.6); caller HMAC is sound (60 s replay
   window, local-only); custody/issuer keys are 0600 and mode-checked.
4. **Kill/revocation** (A9): race the reaper/throttle, survive a kill. → in-flight flows
   RST fast; **netmap retraction is bimodal to 3–5 s** and new direct dials to a killed IP
   black-hole (RT-07).
5. **Audit** (A8): forge/omit records, tamper the chain, fake trace IDs. → chain integrity
   (tamper-evidence) holds; **content authenticity of app-submitted records does not**
   (RT-02).
6. **Host/ops**: key leaks, file perms, release/`install.sh`, `--policy-apply` widening.
   → redaction + secret hook are thorough; `--policy-apply` can merge attacker-declared
   grants from a bundle's `twd.jsonc` (RT-06).
7. **Sandboxes**: Seatbelt (macOS), Landlock/seccomp/microVM (Linux). → Seatbelt profile
   is real and tested; Linux process/Node tiers have **no confinement** (RT-03); microVM
   tier confines but shares a per-VM MAC key across the VM's objects (RT-04).

## 5. What is genuinely strong (do not regress)

- netd ingress strips every identity header; `request.cf` caller comes only from WhoIs.
- ADR-019 egress guard: netmap-only name/IP resolution, refuses self/sibling/host/
  loopback/link-local/multicast/metadata, checked twice (handler + dialer). 22 hermetic
  SSRF cases pass; verified in code (`netd/`), credible.
- Per-object egress attribution via unforgeable `ctx.props` + per-process egress MAC on
  the workerd tiers — tenant code cannot read the MAC key in-isolate.
- Lease epoch fencing (single conditional writes) and the registration throttle (AIMD).
- Audit **hash chain** tamper-evidence (truncation/gap/forged-sig all caught).
- Secret hygiene: redacting slog handler, pre-commit scan, keys never in guest image.

See `RED-TEAM.md` for the ranked findings and exploits against the rest.
