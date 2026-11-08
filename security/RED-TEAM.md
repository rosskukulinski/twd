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
