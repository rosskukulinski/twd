# twd Red-Team Findings

Date: 2026-11-08 · Branch `rt-redteam`. Companion: `THREAT-MODEL.md`, `attacks/`.

Method: adversarial code review of M0–M9 (`STATUS.md`) against the design threat model,
plus hermetic PoCs. Where a finding is grounded in code I cite file:line; where I executed
it I say so. Live tailnet / microVM exploitation was **not** run: this red-team host is
macOS, and the brief forbids touching the other engineers' worktrees, Lima VMs, and the
in-use test tailnets beyond my own `ttest-rt-*` prefix. The code-level evidence is
reproducible and one High (RT-01) ships an executable failing test.

Ranking: Critical = cross-tenant / full platform compromise; High = cross-trust-boundary
within a tenant or defeat of a headline security control; Medium/Low as noted.

| ID | Sev | Claim attacked | STATUS accurate? |
|----|-----|----------------|------------------|
| RT-01 | **High** | DoD(d) spawn "narrower caps"; RFC §4.6 least-privilege delegation | Honest gap, but DoD/e2e headline overstates |
| RT-02 | **High** | "audit … chain verified" as a forensic record | Honest about chain, silent on content authenticity |
| RT-03 | **Critical** (Linux process/Node) | E2E-MULTI "isolation holds"; DoD item 2 | Honest gaps, but headline is macOS-only |
| RT-08 | **High** (Node tier) | "egress leaves from the object's node" / ADR-019 | Honest (M9 gap), but not in the isolation headline |
| RT-04 | Medium | per-object attribution within a workload | Honest (M8 gap) |
| RT-05 | Medium | egress MAC integrity | Honest (M3 gap) |
| RT-06 | Low/Med | `--policy-apply` cannot widen tenant policy | Partially — widening is possible by design |
| RT-07 | Low/Med | "kill in under 1 s"; killed object stops | Honest, well-measured |

---

## RT-01 (High) — Spawn `lim` is never bound to the child's identity

**Claim:** DoD item 1(d): `spawn()` creates a sub-agent "with its own node and **narrower
caps**"; RFC §4.6 delegation (`caps ⊆ parent`, least privilege). STATUS M4(d): child caps
"narrower than the parent's … checked by mgmt".

**Reality:** mgmt validates the requested `lim` (`SpawnOptions.caps`) against
`effective(parent)` in `Policy.CheckSpawnCaps` (`mgmt/policy.go:91`) — but then creates the
child object tagged with **`class.Tags`**, not a subset derived from `lim`:

```
// mgmt/service.go  (CreateObject)
o := Object{... Tags: class.Tags, Caps: r.Caps, ...}   // Caps(lim) stored, Tags = class tags
...
if len(o.Caps) > 0 { detail["lim"] = o.Caps }          // lim only recorded in audit
```

The child's node joins tagged `class.Tags`. A receiver authorizes on the WhoIs capMap,
built straight from the tailnet policy grants for those tags
(`netd/ingress.go:CallerFromWhoIs` → `w.CapMap`). **`lim` never enters the node identity or
the receiver's capMap.** `effective(child.Tags)` can — and is designed to — exceed `lim`.
And because tag ownership (`TagOwnedBy`) only checks *ownership*, not cap-subset, the child's
class tag can be granted MORE than the parent's tags: the child escalates past both `lim`
and the parent.

**Exploit:** operator defines one sub-agent class `tag:sub` granted `{mcp:[lookup_order,
refund]}`, intending to hand each spawn a narrow `lim`. A prompt-injected parent calls
`spawn(Sub, {name, caps:{mcp:[lookup_order]}})` — passes `CheckSpawnCaps` (lim ⊆ parent) —
but the deployed child presents `tag:sub`'s full `{lookup_order, refund}` to any MCP server
it calls. `lim` is cosmetic.

**Evidence (executed):** `attacks/rt01_spawn_lim_test.go`. With the finding-ID skip
removed it FAILS against current code:

```
--- FAIL: TestRT01_SpawnLimBindsToIdentity
    RT-01: child spawned with lim=[lookup_order] still carries `refund` via its class tag
```

**Impact:** the core least-privilege control for a compromised agent (`lim`) provides no
boundary; delegation is coarse (tag granularity) and can escalate. Defeats the DoD(d)
security intent.

**Fix:** intersect the node's effective caps with `lim` at the receiver — mint the child
with a node attribute / assertion carrying `CapMap ∩ lim` and have netd/the platform
enforce it (RFC §4.6 node cert), or derive per-spawn narrowed tags. Until then, stop
advertising `lim` as a security boundary.

**STATUS accuracy:** M3 known gap already says "Spawn caps are not enforced on the
receiving side … not yet written into the node cert (CapMap ∩ lim)" — **accurate and
honest**. But DoD(d) and the M4/M6 e2e "narrower caps" lines read as if enforced; they
should be qualified.

---

## RT-02 (High) — Tenant code forges audit content (fake trace IDs, fake "allow")

**Claim:** `twd audit` shows the human→agent→sub-agent chain and "chain verified"; audit is
the forensic record of what a (possibly compromised) agent did.

**Reality:** the host API `audit` op (`hostd/http.go`, case `"audit"`) forces `ObjectID` to
the attested caller (good) but, for kinds `allow|deny|egress`, copies attacker-controlled
fields verbatim into the **same hash chain** as platform security records:

```
ar.Grant, _  = rec["grant"].(string)
ar.Code, _   = rec["code"].(string)
ar.ActingFor,_= rec["actingFor"].(string)
if tid,_ := rec["traceID"].(string); tid != "" { ar.TraceID = tid }   // override real trace
```

Reachable from tenant code via `ctx.tailnet.audit([...])` (`js/.../runtime.ts:115`).

**Exploit:** a prompt-injected agent emits
`{kind:"allow", grant:"example.com/cap/mcp: refund", actingFor:"ceo@victim",
traceID:"<a human's trace>", detail:"wired $1M"}`. The record chains validly
(`chain verified` stays true), is **indistinguishable** from a platform authorization
decision in `twd audit`, is cross-linked into a victim principal's trace via the overridden
`traceID`, and frames a human via `actingFor`. It can also bury real `deny`/`egress`
evidence under noise.

**Evidence:** `attacks/rt02_audit_forgery_test.go` (skip-marked RT-02) reproduces the
handler's field-copy and encodes the invariant: app records must be provenance-namespaced
and must not override the request trace.

**Impact:** audit cannot be trusted as the forensic account of a hostile agent — exactly
its purpose. Tamper-evidence of the chain is intact; **authenticity of content is not.**

**Fix:** namespace app-submitted records (`app.*` kind or a `source:"app"` field the UI
never conflates with platform records); ignore caller-supplied `traceID`/`actingFor` (bind
to the request's real trace and verified principal); constrain `grant`/`code` to app-origin
values. 

**STATUS accuracy:** STATUS claims chain verification only (true) and lists app records as
a feature; it does not claim content authenticity. The gap is unstated — worth adding.

---

## RT-03 (Critical, Linux process/Node tiers) — Tenant → full platform takeover via mgmt admin socket

**Claim:** E2E-MULTI "isolation holds: a tenant-A object can't see or reach tenant B"
(DoD item 2). STATUS M7: "The Seatbelt profile closes all of these on macOS."

**Reality:** the mgmt admin unix socket authorizes **only** by same-uid peercred
(`mgmt/server.go:ListenUnix` + `uidListener.Accept`), and all twd components plus every
tenant workload run as one uid. On Linux the process and Node tiers apply **no** filesystem
sandbox (`hostd/seatbelt.go` is macOS-only; `ResolveSandbox→none` elsewhere; no
Landlock/seccomp is wired in). So a tenant process that can issue a raw AF_UNIX connect
reaches mgmt as the `local` admin principal: kill any tenant's objects, `GET /v0/tenants`
(enumerate every tenant + their `apiKeyFile` paths), then read each tenant's node keys from
`~/.twd` state dirs and decrypt custody (`custody.key` is 0600 but same-uid readable).

- **Node tier on Linux:** tenant code is ordinary Node — **no exploit needed at all**
  (STATUS M9: "a malicious Node app can bypass the proxy (raw sockets)").
- **workerd process tier on Linux:** needs a V8 isolate escape first, then identical.

**Evidence:** `attacks/rt03_node_mgmt_socket_takeover.md` (PoC) + `rt03_mgmt_socket_test.go`
(skip-marked RT-03: dials the real `ListenUnix` listener same-uid and asserts it must be
refused without a credential — it currently is admitted). Not run against a live Linux
hostd (none here; VMs off-limits).

**Impact:** complete cross-tenant compromise from unprivileged tenant code on multi-tenant
Linux process/Node hosts. Breaks DoD item 2 off macOS.

**Fix:** don't gate admin on same-uid peercred — per-tenant uid + fs namespace, or a
per-component bearer token on the socket, or restrict multi-tenant Linux to the microVM
tier. Document process/Node Linux tiers as single-tenant until then.

**STATUS accuracy:** STATUS is candid ("0700 is not isolation"; M7 "a hostd compromise
exposes every tenant's node keys"; integration "the Node tier on Linux has no per-tenant
confinement"). But the **E2E-MULTI isolation headline is macOS-specific** and should say so
loudly, since `LINUX-PLAN` presents Linux as first-class.

---

## RT-08 (High, Node tier) — Egress containment is advisory; raw sockets bypass ADR-019

**Claim:** objects' `fetch()` leaves from the object's node and is bounded by the ADR-019
egress guard (no loopback/LAN/metadata/internet).

**Reality:** for the Node runtime, egress enforcement is an **environment HTTP proxy**
(`HTTP(S)_PROXY`, `js/packages/node`). A hostile Node app opens raw TCP/UDP sockets or
installs its own `fetch` dispatcher and reaches loopback, the host LAN, link-local
`169.254.169.254`, and the internet directly — the ADR-019 guard is never consulted. The
microVM gVisor gateway would make this real, but Node is **rejected** in the microVM tier
(`runtime:node + tier:microvm` unsupported), so on every platform the Node tier's egress is
unenforced.

**Evidence:** STATUS M9 gap states it verbatim ("a malicious Node app can bypass the proxy
(raw sockets, its own dispatcher) and reach loopback, LAN and internet"). Grounded in
`js/packages/node` (proxy-via-env only) + `hostd/node.go`. SSRF to cloud metadata / host
services is the practical impact.

**Impact:** SSRF and egress-containment bypass for any Node workload; combined with RT-03 on
Linux it is also the delivery vehicle for the socket takeover.

**Fix:** enforce egress out-of-process (per-tenant netns + transparent proxy / microVM) for
the Node tier, or restrict it to single-tenant trusted apps. Don't present env-proxy as a
security boundary.

**STATUS accuracy:** honest in the M9 gap list; just not reflected in the top-line egress
claims.

---

## RT-04 (Medium) — One MAC key per VM lets a guest compromise forge attribution among its objects

The microVM tier ships `MAC_KEY` in the guest config (`env.MAC_KEY`, readable in-guest by
design). All objects in one VM share it, and `isolation:"object"` placement is not built.
A single V8 escape inside the guest forges the egress MAC for any object in that VM,
impersonating a sibling object's identity on egress (including a higher-privileged parent in
the same workload — compounds RT-01). Scope: one tenant's workload. STATUS M8 states this
("a V8 escape could impersonate the other objects in the same VM"). Fix: per-object MAC keys
or `isolation:"object"` VM placement.

## RT-05 (Medium) — Egress MAC has no replay/nonce cache and is not bound to the destination node

`gen/mac.go` verifies a 2-minute-skew HMAC over `class\nname\nmethod\nurl\nts\nnonce` with
**no nonce cache**. In the workerd tiers the key isn't tenant-visible, so this is latent;
in the microVM tier (RT-04, key readable) a captured or recomputed MAC can be replayed
within 2 minutes, and the MAC does not bind to *which* object node must originate the
request, only to the class/name claim. Fix: nonce cache + tighter skew, and bind the MAC to
the resolved egress node.

## RT-06 (Low/Medium) — `twd deploy --policy-apply` can widen the tenant tailnet from a bundle's twd.jsonc

`internal/policy.Merge/Apply` only *adds* tagOwners/grants, validates, and writes with
If-Match (sound against clobbering). But the additions are derived from the deploy's
`twd.jsonc`/preflight. A hostile or mistaken bundle run with `--policy-apply` injects new
`grants`/`tagOwners` into the live tenant policy — a privilege-widening supply-chain edge.
Mitigated by the explicit flag + validation; the operator should diff the printed snippet
before applying. Fix: show a destructive-change diff and require per-grant confirmation;
never let a bundle add `tagOwners`.

## RT-07 (Low/Medium) — Kill retraction is slower than the "<1 s" headline for netmap removal

In-flight flows RST in <1 ms (strong). But netmap removal on production control is bimodal
to **3–5 s**, and a *new direct* dial to a killed node's 100.x IP black-holes to the
caller's timeout (~5 s) rather than failing fast — a brief window where a killed object's
node is still joinable/addressable and clients hang. STATUS measures and discloses this
(M1 findings 2–3, M3/M8). Not "keeps serving" (the tombstone RSTs in-netmap peers), but the
"kill under 1 s" headline holds only for callers with an active flow. Fix: delay device
delete by ~the tombstone window, and have clients treat netmap-absence as dead.

---

## Regression tests

- `attacks/rt01_spawn_lim_test.go` — **executed**, fails without the skip.
- `attacks/rt02_audit_forgery_test.go` — skip-marked RT-02, encodes the invariant.
- `attacks/rt03_mgmt_socket_test.go` — skip-marked RT-03, dials the real listener.

All compile and `go vet` clean; skip-marked so CI stays green until the fixes land.

---

## Fix status (branch `rt-fixes`, 2026-11-08)

| ID | Status | Where |
|----|--------|-------|
| RT-01 | **Fixed**: node certificates (ADR-010) signed by mgmt at ready; receiving netd computes `CapMap ∩ lim`, fails closed | STATUS.md "RT fixes"; `rt01_spawn_lim_test.go` un-skipped |
| RT-02 | **Fixed**: tenant code produces only `tenant_note`/`source: tenant`; mgmt and `twd audit` enforce/show provenance | `rt02_audit_forgery_test.go` un-skipped |
| RT-03 | **Fixed**: per-host bearer token on the unix socket, rotated on `twd up`; peercred kept as an extra check | `rt03_mgmt_socket_test.go` un-skipped |
| RT-04 | **Partial**: per-object derived MAC keys; the root still lives in the guest, so `isolation: "object"` remains the real fix | STATUS.md |
| RT-05 | **Fixed** for what exists: nonce caches on egress MACs and the Node caller header; OBO assertions are still unimplemented | STATUS.md |
| RT-06 | **Fixed**: `--policy-apply` only adds `tag:twd*` tagOwners and twd-sourced grants, refuses any non-additive diff | `internal/policy` hostile table + fuzz tests |
| RT-07 | **Documented**: kill tier 1 (RST + tombstone) is the fast path; netmap retraction stays a control-plane property | STATUS.md |
| RT-08 | **Fixed/partial**: `@twd/node` `enforceEgress()` (cooperative); hostd reports `egress unenforced`, refuses Node-tier on multi-tenant hosts; docs say only Landlock/microVM is a boundary | `docs/node-tier.md` |

---

# Pass 2 (live) — 2026-11-08, branch `rt2-live`

Pass 1 worked from code on macOS and ran no live attacks. Pass 2 attacked the
**merged** system on real Linux. Environment: a dedicated Lima VM
`twdl-rt2-u24` (Ubuntu 24.04 arm64, kernel 6.8, **Landlock ABI 4**, vz, nested
virt, mounts nothing), the real `linux-arm64` release built on the Mac
(`make release`) and installed with `install.sh` as a user would. No personal
tailnet, no system tailscaled, no policy writes; `twd-dev` / `twdl-*` / the FV
worktree untouched. The microVM tier could **not** be exercised live: a
Mac-built release carries no microVM guest (`make vm-assets` is Linux-only and
needs the Firecracker toolchain + guest image; `twd doctor` confirms
"this release has no microVM guest … process tier only"). Those verdicts are
code-grounded and labelled so.

Evidence lives in `security/attacks/`: the pre-existing regression tests
(`rt01`/`rt02`/`rt03`), plus Pass-2 additions `rt01_cert_attacks_test.go`,
`rt05_replay_test.go`, `rt06_policy_json_tricks_test.go`,
`rt_sandbox_linux_test.go` (the Linux escape battery), and the live script
`rt03_live_mgmt_socket.sh`. The whole `./security/attacks` suite was
**cross-compiled to linux/arm64 and run inside `twdl-rt2-u24`**: PASS (the
`//go:build linux` battery only means anything there).

## Verdicts

| ID | Verdict | Live evidence |
|----|---------|---------------|
| RT-01 | **HOLDS** | node cert (`internal/nodecert`) signs `lim`; receiver computes `CapMap ∩ lim` (`netd/ingress.go:applyCert` → `internal/caps.Intersect`). Live: a child spawned `lim=[lookup_order]` loses `refund`; a cert for node `nA` is rejected for a re-minted `nB` (stale-cert reuse), an **expired** and a **future-iat** cert are rejected, a forged/other-key cert fails signature, an unparseable/omitting `lim` **fails closed** (empty caps), and a failed cert *fetch* withholds all caps (`netd/ingress.go:76`, `CapsWithheld`). `rt01_*_test.go` PASS on macOS and in the VM. |
| RT-02 | **HOLDS** | tenant code yields only `tenant_note`/`source:tenant`; `traceID`/`actingFor` are bound to the real request, platform fields dropped, the claim preserved only inside `detail` (`hostd.TenantAuditRecord`). mgmt's ingest (`mgmt.checkAuditSource`) refuses `tenant`+non-note, a `tenant_note` without `source:tenant`, and anything claiming `source:mgmt`. Both raw-POST shapes covered; `rt02_*` PASS live. |
| RT-03 | **HOLDS (live, real binary)** | Ran the real `twd-mgmt` on its unix socket (SQLite store, no tailnet) and attacked it as a tenant would (`rt03_live_mgmt_socket.sh`): bare same-uid connect → **401 host_token_required**; forged 64-char token → **403 host_token_invalid**; pre-fix `Twd-Host-ID` alone → **401**; even `/v0/healthz` → **401**; the 0600 token file's value → **200**. Socket is `srw-------` in a `drwx------` dir; token file `-rw-------`. On a Landlock host the socket is **doubly** out of reach: the battery shows AF_UNIX connects are refused (EPERM, seccomp) and `~/.twd` is unreadable (EACCES), so a workload can neither open the socket nor read the token. |
| RT-04 | **PARTIAL (by design; not exploited live)** | Per-object MAC keys are derived (`gen.ObjectMACKey`); a MAC under another object's key does not verify (`rt05_replay_test.go`). The root key still lives in the guest, so a single in-guest V8 escape can still forge siblings in the *same* VM — `isolation:"object"` placement remains the real fix. Not reachable without the microVM tier. |
| RT-05 | **HOLDS** | `gen.VerifyEgressOnce` + `gen.NonceCache`: a replayed `(class,name,nonce)` inside the skew window is refused, a fresh nonce is accepted, an out-of-skew tuple is refused even with a valid MAC, and a MAC under another object's key fails. `rt05_replay_test.go` PASS live. Residual (documented): the MAC is not bound to *which* egress node originates, latent while the key isn't tenant-visible (workerd tiers). |
| RT-06 | **HOLDS** | `internal/policy.Check`/`Merge`/`VerifyAdditive` refuse `src:*`, `autogroup:*`, groups, users, non-twd `src` tags, `dst:*`, non-twd `tagOwners`, extra grant fields, and any non-additive result (the base hostile table). Pass-2 adds: a **Unicode-homoglyph** tag (`tag:twԁ-…`) is refused, and a live policy with **duplicate `grants`/`tagOwners` keys** either is refused or stays verifiably additive. See RT2-L1 below for one Low gap. |
| RT-07 | **HOLDS (documented)** | Not re-measured live this pass. Kill tier 1 (RST + 10 s tombstone) is the fast path; netmap retraction stays a bimodal control-plane property, as STATUS/pass-1 disclose. |
| RT-08 | **HOLDS on Landlock ABI ≥ 4; cooperative elsewhere (as documented)** | The **live escape battery** (`rt_sandbox_linux_test.go`, run under twd's real tenant profile in the VM) refuses, before any effect: read/list/write outside the instance dir (EACCES), TCP to `1.1.1.1:443`, cloud metadata `169.254.169.254:80`, and another tenant's loopback port (EACCES), `bind` (EACCES), UDP/DNS (EPERM), AF_UNIX to a mgmt-shaped socket and to `tailscaled.sock` (EPERM), `socketpair(AF_UNIX,DGRAM)` (EPERM), `ptrace`/`process_vm_readv`/`kill`/`tgkill`/`kill(-1)` against another process (EPERM). Only the egress-proxy port and the instance dir are reachable. `twd doctor`'s own live Landlock probe agrees. `execve(/bin/cat)` is **allowed by design** (`systemRO` maps `/bin,/usr,/lib` read+execute so runtimes work) and is not an escape: the child inherits Landlock under `no_new_privs` (setuid bits ignored) + the seccomp filter and is confined identically. On macOS (Seatbelt, no TCP confinement) and Linux ABI < 4, egress is cooperative — `twd doctor` warns and hostd refuses a second process-tier tenant without `--insecure-shared-host`. |

## New findings

**RT2-L1 (Low) — `policy.Check` accepts a whitespace-/control-padded twd tag.**
`okTag` is `strings.HasPrefix(tag, "tag:twd")` with no boundary or
whitespace check, so a `src`/`tagOwners` entry like `"tag:twd-wl-x "`
(trailing space, tab or newline) passes `Check`. *Impact is bounded:* the
accepted string is still inside twd's own `tag:twd*` namespace, a grant to a
space-padded tag matches no real device, and every policy write is validated
by Tailscale before apply (BUILD-PLAN safety rule). So this is input-validation
leniency, not a cross-principal widening. *Fix:* require a boundary after the
prefix (exact `tag:twd` or `tag:twd-…`) and reject tags containing
whitespace/control characters. Regression: `TestRT2L1_CheckAcceptsWhitespacePaddedTag`
(skip-marked). No Critical/High/Medium new findings.

## Residual risk by tier

- **macOS process (Seatbelt).** Files and unix sockets are confined (so the
  RT-03 socket/token are unreachable), but Seatbelt does **not** confine TCP:
  the Node tier's egress is cooperative (`@twd/node enforceEgress()` is
  in-process defence only), so a hostile Node app can SSRF to loopback/LAN/
  metadata/internet. workerd-tier egress is still platform-enforced. Documented;
  treat multi-tenant macOS Node as trusted-only.
- **Linux process / Node (Landlock ABI ≥ 4).** Strong boundary — the live
  battery is clean, RT-03 is double-locked. Residuals: (a) ABI < 4 kernels
  can't restrict TCP, so Node egress is cooperative there (hostd warns,
  `node_egress`); (b) a user in a container-engine group or able to reach
  `tailscaled.sock` as a unix-listening Node app is root-equivalent for hostd
  itself — hostd refuses a second tenant unless `--insecure-shared-host`
  (`twd doctor host_sockets`). RT2-L1 above.
- **Linux microVM (Firecracker).** **Not exploited live this pass** (release
  without guest assets). Code review of the privileged paths found the
  expected controls intact: the vmhelper authorises each connection by
  **SO_PEERCRED uid** against `allowedUIDs`; its config must be root-owned and
  not group/world-writable; a volume image must be **owned by the caller uid**
  (no attaching another uid's image); a `(device,inode)` **busy map** blocks
  double-attach; volumes are **LUKS2** and the attach is **epoch-fenced**,
  persisted, checked before and after `cryptsetup open`, so a taken-over
  hostd's **stale-epoch** attach is refused (`CodeVolumeFenced`); a wrong key
  gives `CodeVolumeKey`. `twd host setup --undo` reverses the recorded manifest
  (unit, root-owned binaries, sysctl/hugepages, only the kvm-group members it
  added, `/etc/twd`), leaving each user's DO volumes. RT-04 (shared in-guest
  root MAC key) is the open gap. These should be re-verified with a live
  Firecracker run once a microVM-asset release is available.

## Supply chain / install (live)

The `linux-arm64` tarball's outer sha256 matched `dist/SHA256SUMS`; its
internal `SHA256SUMS` verified clean (12881/12881 files, no mismatch).
`install.sh` is no-sudo and confined to the prefix; `twd doctor` reports the
sandbox, host-socket and microVM posture honestly. (`workerd`'s mtime is a
fixed 1985 epoch — cosmetic.)
