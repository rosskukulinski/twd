# twd STATUS (living log)

Last update: 2026-11-08 00:10 PT. M0, M1 (main) and M3 (branch `m3-hostd`).

## M0: repo skeleton (DONE)
- Layout per RFC §15:
  - `cmd/{twd,twd-mgmt,twd-hostd,twd-netd}` (only `twd-netd` is real; the others are stubs that exit 2 and name their milestone);
  - `netd/`, `mgmt/` and `hostd/` (doc stubs);
  - `js/packages/{tailnet,mcp,create-twd}` as npm workspaces;
  - `templates/`, `e2e/{local,real}`, `scripts/`, `internal/tsapi`.
- One Go module, `github.com/rosskukulinski/twd`:
  - `tailscale.com v1.105.0-pre.0.20261107202901-503010831ecd`, which is exactly `.refs/tailscale` 5030108, fetched from the public proxy with no `replace`;
  - built with `GOTOOLCHAIN=local` (go1.27.3).
- workerd is pinned exactly to `1.20261108.1` (the prototype's version) as a devDependency in the root `package.json`, with a lockfile.
- Makefile targets:
  - `test`: hermetic, with `-race`;
  - `e2e-local`;
  - `e2e-real`: skips cleanly if `~/.config/tailnet-agents-design/tailnet1_api.env` is missing;
  - `lint`: gofmt, `go vet` for all build tags, and the secret scan;
  - `build`;
  - `e2e-vm` and `release` are stubs that exit 1.
- The pre-commit secret hook is installed in `.git/hooks`. `scripts/check-secrets.sh` runs in `make lint`. Test fixtures build fake keys at runtime, so no key-shaped literal is ever committed.

## M1: `netd` library + `twd-netd` binary (DONE)
What's in `netd/`, ported from `prototype/per-object-identity` and hardened:

**Registry**
- One `tsnet.Server` per object.
- `Create` is async. It returns in under 100 ms for 12 objects; joins go through a bounded queue (`JoinParallel`, default 16), and the test asserts the bound.
- Each object gets a 0700 state dir under `StateRoot/<id>/` plus `object.json`.
- `Rejoin()` restores every object from saved state with the same node ID and IP, and never calls the KeySource.
- `ControlURL` is required, so nothing can join production control by accident.

**Ingress**
- WhoIs runs with `OmitAuth=true`.
- Client identity headers are stripped: `x-twd-*`, `twd-*`, `cf-*`, `x-forwarded-*`, `forwarded`, `x-real-ip`, `tailnet-assertion`, `tailscale-user-*`, `proxy-authorization`.
- netd sets the `x-twd-cf` blob, `{tailscale:{caller:{nodeId,node,hostname,addrs,tags,user?,capMap}},objectId}`, and `x-twd-object`.
- `user` is set for untagged (human) callers only, per RFC §6.1.

**Egress (ADR-019)**
- Names resolve only from the object's own netmap: a bare name must match a MagicDNS first label, and a dotted name must match the full FQDN. Nothing is ever resolved via host DNS.
- An IP literal must be a tailnet IP *and* a peer in the netmap.
- Refused: self, same-host siblings (`AllowSiblings` opts in), host interface IPs, loopback, unspecified, link-local, multicast, 100.100.100.100, non-tailnet IPs, zoned addresses and `ExtraForbidden` prefixes.
- The destination is checked twice, once in the handler and again in the dialer.
- Refusals are typed JSON (`code, detail, object, destination, doc_url`) with a `twd-error` header: `peer_not_visible` and `forbidden_destination` are 403, `killed` is 410, `identity_pending` is 503.

**Kill**
1. Mark the object killed, so no new ingress or egress is admitted.
2. Close the listener, then abort every tracked flow in both directions with a real TCP RST (gVisor `Endpoint.Abort()`; a test guards the reflection).
3. Wait `KillFlushDelay` (50 ms) so the RSTs leave over WireGuard (see Findings 1).
4. Run the `OnDelete` hook (tier 2), then `Close()`, then remove the state dir.
   - `KillOptions.CloseFirst` reverses the order, and `Logout` is optional.
   - Running delete and Close concurrently was tried and made both contend at control (4–5 s each), so they run in sequence.

**Other**
- `Close()` closes all nodes in parallel (64-way) and keeps their state.
- KeySource: `StaticKey` (it never prints) and `MintFunc`. `internal/tsapi.MintSource` mints single-use, pre-authorized, 5-minute keys via the API and can revoke them all.
- `RedactingHandler` (slog) redacts `tskey-*` in messages, attributes, groups, errors and Stringers. Join errors and error bodies are redacted too.

**`twd-netd`**
- Flags: `-control-url`, `-state-dir`, `-run-dir`, `-upstream`, and either `-authkey-file` or `-mint-api-key-file -tailnet -tags [-delete-devices]`. `-tailnet` must be named explicitly.
- Sockets: `egress.sock` and `ctl.sock` (mode 0600, in a 0700 dir).
- Control API: `GET/POST /v1/objects`, `GET /v1/objects/{id}`, `POST /v1/objects/{id}/kill?reason=&logout=1&close_first=1`.
- SIGTERM closes all nodes in parallel, and they rejoin on the next start.

### Test results

#### `make test` (hermetic): PASS
Runs testcontrol, local DERP/STUN and stock workerd, offline except for the module and npm downloads, with `-race`. Measured on this Mac:

**Ingress identity reaches a stock workerd DO.** Client → object → platform entry → HostDO → Dynamic Worker facet, with the tenant class unmodified. The caller's nodeId, addrs, hostname, user and capMap (`example.com/cap/ttest`) all match, for 3 objects.

**Forged headers are stripped.** The tenant saw only `x-twd-object=a`.

**Per-object egress attribution.** Echo's WhoIs equals the object's own node, 3/3. A tenant forging `x-twd-object`, `x-twd-cf`, `Proxy-Authorization` or `cf` cannot change attribution, and none of those headers reach echo.

**SEC-4, 22 cases, all refused with a typed 403:**
- own IP, and own IP on another port;
- sibling by IP, short name and FQDN;
- host IP;
- 127.0.0.1, 127.1.2.3, ::1, ::ffff:127.0.0.1, 0.0.0.0;
- 169.254.169.254, fe80::1;
- 100.100.100.100;
- 1.1.1.1, 192.168.1.1;
- the decimal and hex loopback forms. workerd's WHATWG parser canonicalises these to 127.0.0.1, so they get `forbidden_destination`. Through a raw proxy client they stay names and get `peer_not_visible`;
- `localhost`, `example.com` and `metadata.google.internal`, which get `peer_not_visible`.

**`peer_not_visible`.** Applies to unknown names and to unknown 100.x IPs. Names that are in the netmap resolve, both short and FQDN.

**Kill fail-fast:**
- A streaming caller through workerd failed **0.39–0.46 ms** after the kill (limit 500 ms).
- An in-flight egress stream ended in **0.18–0.28 ms**.
- `OnDelete` received the nodeId, the state dir was removed, and later egress got a typed `killed`.

**Parallel join and close.** N=12 at par=4: all up in 1.66 s; parallel Close took **154 ms**.

**Rejoin from saved state.** Same node ID and IP, no key requested, about 370 ms per node.

**Other unit tests:** redaction, the StaticKey String, header stripping, config validation, idempotent concurrent Create, kill-while-pending, a typed and redacted join failure, and the tsapi client against an httptest server.

**JS:** `@twd/tailnet` `getCaller` has 2 node tests, both passing.

#### `make e2e-local`: PASS
Runs the `twd-netd` binary as a real process, plus workerd and local control. Covers identity, egress and SSRF, then SIGTERM → restart → rejoin from state (same node IDs, no key) → kill via `ctl.sock`.

#### `make e2e-real`: PASS (35/35 checks)
Runs on tailnet 1 with production control, using ephemeral `tag:ttest` keys minted via the API; every dial goes through tsnet.
- **Runs:** 24 this session.
  - 2 failed while I was developing. One asserted on a logout timeout, which became a note rather than a failure. The other was a black-holed RST, fixed by `KillFlushDelay`.
  - After the fix, 16 consecutive runs passed, including 6 of 6 on the final code.
- **Results:** the per-run JSON is in `e2e/real/results/`.
- **Cleanup:** every run deleted its devices and revoked its keys (`devices_left: 0` every time, 6–7 keys minted per run).
- **Policy:** no policy was read or written.

Numbers (median, with the range in brackets, over the last 6 runs unless noted):

| Metric | Value |
|---|---|
| Object join, production control (5 objects, par 16) | p50 1.53 s [1.50–1.56] |
| All 5 objects ready | 3.4–8.1 s; it rose as the session's registrations piled up |
| client → echo (tsnet baseline) | p50 0.19 ms, p95 0.24 ms |
| client → object (netd WhoIs + workerd DO + facet) | p50 0.33 ms, p95 0.40 ms |
| client → object → echo (egress end to end) | p50 0.69 ms, p95 1.02 ms |
| First request to a fresh object (cold DO) | 13–24 ms |
| Paths | client→echo and client→obj-a are `direct` (no DERP) once warm |
| **Kill: streaming caller time-to-failure** | **0.12–0.68 ms** in all 58 successful kills across 24 runs and every mode |
| Kill, default (delete, then Close): removal from a peer's netmap | about 206 ms in good runs, 3.1 s median in the last 6. Bimodal: ~0.2 s or 3–5 s |
| Kill, `CloseFirst`: netmap removal | 205 ms best, 4.75 s median (Close of an ephemeral node waits on its control-side logout) |
| Kill, `Logout`: netmap removal | 201 ms best, 1.97 s median; the LocalAPI logout hit its 1.5 s cap in 3 of 6 runs |
| Device-delete API call | 143–147 ms normally; 3–5 s outliers |
| Object → killed object via netd egress | typed `peer_not_visible` in about 4.5 ms, as soon as the object's netmap drops the node |
| New *direct* tsnet dial to a killed node's IP after netmap removal | **black-holes until the caller's timeout** (5.0 s, every run) |
| Parallel Close of the remaining nodes | 167–412 ms |

### Findings and decisions (new this run)
1. **The RST needs a moment to leave.** `Endpoint.Abort()` only queues the RST, and `tsnet.Close` tears netstack down first. With no delay, 1 of 3 production runs left the caller hanging. With a 50 ms `KillFlushDelay`, 16 of 16 runs passed.
2. **Closing an ephemeral tsnet node logs out first** (`LocalBackend.Shutdown`, best effort, up to 5 s). On production control that takes either about 150 ms or 3–5 s. Kill therefore deletes first by default and never runs delete and Close concurrently. Even so, removal from peers' netmaps on production control is bimodal (about 0.2 s, or 3–5 s), so the "kill under 1 s" target holds for callers, which fail on the RST in under 1 ms, but not always for netmap removal.
3. **Tier-1 kill is only fail-fast for in-flight flows.** A peer that dials a dead node's 100.x IP directly hangs until its own timeout, even after the netmap removal. Through netd egress, an object instead gets an immediate, typed `peer_not_visible`. Twd clients (the CLI and `@twd/tailnet`) should treat a node missing from the netmap as dead rather than dialing it.
4. **Control-side latency rose across the session:** `objects_all_ready` went from 3.4 s to about 8 s, and one join reached 6 s. That may be registration-rate shaping after ~150 registrations in 25 min (ADR-017), but it is unverified.

### Not done / known gaps (M1 scope)
- Egress supports HTTP and HTTPS only; CONNECT and raw TCP (`ctx.tailnet.connect`) are refused with a typed error.
- netd trusts `x-twd-object` from anyone who can reach `egress.sock` (a 0700 dir). The per-request MAC from RFC §4.8 comes with the M3 platform wrapper.
- No `Tailnet-Assertion` signing or verification (RFC §4.6).
- No registration throttle; that belongs to mgmt (M2).
- Egress calls `LocalClient.Status()` on every request, which is fine at the MVP's peer counts but should become a cached netmap view.
- The hermetic suite can't measure netmap removal, because testcontrol never drops nodes. Real numbers come from e2e-real only.
- Not run in Lima/Linux yet; macOS only.

## M3: `twd-hostd` process tier + JS runtime packages (DONE, branch `m3-hostd`)

### What's built
- **`cmd/twd-hostd` and `hostd/`.**
  - Registers (with `pushURL` and an ed25519 `auditPubKey`), heartbeats, and polls assignments every heartbeat or on push.
  - Self-fences after 9 s without a heartbeat.
  - On a lease revoke it adopts a newer same-host epoch; otherwise it fences.
  - For each assignment it:
    1. fetches and unpacks the bundle (`hostd/bundle`: tar, `twd.jsonc`, `wrangler.jsonc`, safe paths);
    2. generates the workerd config (`hostd/gen`);
    3. starts **one supervised workerd per tenant workload**, restarting it on a crash (bounded);
    4. health-checks it, then switches.
  - **Blue/green.** New requests wait at a gate while the old instance drains (≤10 s), then go to the new one. Both instances share DO storage, so they never serve at once.
    - Measured: 293 requests during a redeploy, **0 failed**.
    - The workload node and all object nodes are unchanged across the switch.
  - Supervision runs on the process tier on macOS. Linux is not run yet (see below).
- **Generated platform wrapper** (`hostd/gen/js/platform.js` plus generated modules):
  - entry: `cf.twd` routing (set only by hostd), `/_twd/o/<Class>/<name>/` on the workload node;
  - `Host_<Class>` DOs that run each tenant class **as a facet** in a Dynamic Worker, with `globalOutbound = Egress({props:{class,name}})`;
  - RPC forwarded to the facet through a Proxy (the Agents SDK `__unsafe_ensureInitialized` works);
  - `NsProxy` for DO bindings inside facets;
  - `WorkloadEgress` for the tenant service;
  - `TailnetApi`, which backs `ctx.tailnet`, with props-attested identity;
  - per-process **egress MAC** (HMAC: class, name, method, url, ts, nonce), verified by hostd (closes the M1 gap "netd trusts x-twd-object");
  - the platform's own globalOutbound is `deny`.
- **Tenant service.**
  - The app's default export runs here as a static workerd service. Its env has the real DO namespaces, and its egress is the workload node.
  - **Plain-DO mode** (`"egress":"workload"`): the class runs here and alarms work.
  - `"node": false` marks internal classes, such as the MCP budget DO.
- **Objects get their own nodes.**
  1. On the first request, `create()`/`getByName` or `spawn()`, hostd calls `POST /v0/objects` and then `GET credential?epoch`.
  2. netd adds the node asynchronously, then hostd calls `ready`.
  3. Until then, requests via the workload node reach the object in-process: the first request took 14–25 ms, before any node existed.
  4. Facet egress waits for the object's node (`identity_pending` after 30 s). It never falls back to another identity.
- **Kill.** Kills arrive by mgmt push, heartbeat `kill`/`revokeObjects`, or `ctx.tailnet.kill()`. hostd then:
  1. runs netd tier 1 (RST all flows);
  2. leaves the node as a **tombstone** for 10 s, so new connections get an RST (new `netd.KillOptions.Tombstone`);
  3. sets the platform kill flag, so the object returns 410 `killed` even in-process;
  4. writes an audit record.
  - Device deletion (tier 2) is done by mgmt.
- **Hibernation.** workerd evicts idle objects after ~10 s, and their nodes stay online; the next request wakes the object (`wakes` is counted). Measured: still in the client's netmap after 14 s idle, then woke.
- **Local host API** (`hostapi.sock`, reachable only by the platform): `ensure`, `spawn` (parent = the attested caller), `ready`, `kill` (self or descendants), `audit` forwarding, and `self`.
- **Audit.** A hash chain per host, using M2's canonical sorted-key SHA-256 and ed25519 `hostSig`. It is flushed with heartbeats and covers spawn, kill, egress deny, and app records.
- **`hostd/fakemgmt`.** An in-memory mgmt that matches the M2 contract clarifications:
  - one lease per (tenant, workload), and namespaces as class names;
  - `revokeObjects`, `Twd-Host-ID`, a 410 for killed names;
  - spawn checks (caps ⊆ parent, maxLive 64, maxDepth 3) and a registrations-per-minute token bucket;
  - mint modes `static` (testcontrol) and `apikey` (`internal/tsapi`);
  - optional push.
- **JS packages** (TypeScript 7.0.2, built to ESM in `dist/`):
  - **`@twd/tailnet`:** `getCaller`, `TailnetError` with the full typed catalog (statuses, `toJSON`/`toResponse`/`from`), `isTailnetError`, `fromResponse`, `tailnet(ctx)`.
    - The runtime provides `ctx.tailnet.fetch/forward/spawn/kill/audit/self` plus the class and handler wrappers.
    - hostd embeds an esbuild of `src/runtime.ts` (`make gen-js`; a test fails if it is stale).
  - **`@twd/mcp`:** `withTailnet(handler, {path})` via AsyncLocalStorage, and `caller()`.
    - `authorizeTool`: union of grants, max for numeric limits (a matching grant without the limit means unbounded), and `groups` gating.
    - Errors are thrown as `TailnetError` whose **message is the JSON body**, so MCP tool results carry `{"code":"cap_missing",...}`.
    - `budget()` is backed by the `TwdBudget` DO (`@twd/mcp/budget-do`).
  - **`create-twd`:**
    - templates `hello` (a Worker plus a private fetch), `agent` (an Agents SDK `Agent` with `setState`, `this.sql` and a `spawn` route) and `mcp` (`createMcpHandler` + `withTailnet` + `authorizeTool` + `budget`);
    - `create-twd build`, a wrangler-like esbuild bundler (workerd conditions, `node:*` builtins, `cloudflare:email` stubbed).
- **SDK versions (verified on workerd 1.20261108.1, no bump needed):** `agents` 0.33.0, `@modelcontextprotocol/server` 2.4.0, `@modelcontextprotocol/sdk` 1.32.2 (agents' peer) and `zod` 4.6.7.
  - `agents/mcp/server`'s `createMcpHandler` takes an **MCP SDK v2** factory (`McpServer` from `@modelcontextprotocol/server`). The v1 `McpServer` path is deprecated upstream and not used.
  - `cloudflare:email` is missing from stock workerd, so the bundler stubs it. Email is unsupported.
- **Docs:**
  - `docs/egress-modes.md`: the facet vs plain-DO trade-off, including alarms, attestation, the getByName caveat, and why switching modes resets storage;
  - `docs/contracts/hostd-additions.md`: hostd additions plus two gaps for M2.

### Test results (measured 2026-11-07/08, this Mac)
- **`make test`: PASS.** It covers netd, tsapi, hostd (with `-race`), the gen integration tests (real workerd, fake hostd sockets), and the JS tests (tailnet 7/7, mcp 4/4). Hostd results from `TestHostd`:

  | Check | Result |
  |---|---|
  | deploy `hello` → a tsnet client calls the workload node | verified caller in the response; the Worker's private fetch is attributed to the workload node (echo WhoIs) |
  | 3 DO objects | each gets its own node and IP; direct ingress caller OK; **egress attributed to its own node** 3/3; `ctx.tailnet.fetch` attributed too, with a typed throw for `peer_not_visible` and `forbidden_destination` (self) |
  | forged headers | client `x-twd-cf`/`x-twd-object`/`cf-*`/`x-forwarded-for`/`tailnet-assertion` stripped, tenant saw only `x-twd-object`; tenant-forged egress headers changed nothing and none reached echo |
  | kill → fast failure | streaming caller failed **1.0 ms** after the mgmt kill; a new request was **refused in 1.1 ms** (tombstone); via the workload, 410 `killed` |
  | `mcp` template | allowed `lookup_order` succeeds; `refund` → `isError` with `cap_missing` JSON (`missing.tool`, `have.tools`) |
  | `agent` template | `Agent` `setState`/`this.sql` persist across requests via the Worker and the object's own node; agent egress leaves from its own node; `spawn()` of a sub-agent works |
  | spawn | child gets its own node, lineage is recorded in mgmt, MagicDNS name returned |
  | plain-DO mode | alarm fires; egress = workload node; ingress via own node |
  | hibernation | node stays online, object wakes |
  | blue/green | 0 failures |
  | audit | spawn/kill/deny records present, chain verified |
  | `TestKillViaHeartbeat` (no push) | delivered in 143 ms at a 200 ms heartbeat |
- **`make test-m2mgmt`: PASS.** hostd ran against the **real twd-mgmt** built from `origin/m2-mgmt` (`git archive`; nothing checked out), with testcontrol and a fake Tailscale API.
  - Workload node, 3 objects with attributed egress, kill via heartbeat `revokeObjects`: the stream failed **172 ms** after the kill (300 ms heartbeat), and the device was deleted by mgmt.
  - The audit chain, including the host's ed25519 signatures, **verified**.
- **`make e2e-real` (`TestRealHostd`, tailnet 1): PASS, 14/14 checks**, 1 run (`e2e/real/results/m3-*.json`). It used fakemgmt in apikey mode, production control and ephemeral `tag:ttest` `ttest-m3-<run>-*` nodes.
  - hello: verified caller, egress = workload node.
  - 3 objects:
    - reachable via the workload in 14–26 ms before their nodes existed;
    - nodes ready 1.6 s (a) and 6.8 s (b, c) after the first request;
    - egress attributed 3/3 (9–50 ms).
  - Kill: streaming caller TTF **0.91 ms**; 410 via the workload.
  - **Cleanup:** 7 devices tracked, 7 deleted, 0 left, 0 untracked; 7 keys minted and revoked. No policy was read or written.
  - **Not fast on production:** a *new* request to the killed object's IP timed out (5 s) instead of failing fast. mgmt deletes the device right after the push, the peer drops the node from its netmap, and then the dial black-holes (M1 finding 3). The tombstone only helps while the peer still has the node in its netmap, which is always true on testcontrol. A fix is either to delay the device delete by about the tombstone time, or to have clients treat netmap absence as dead.
- `make lint`: PASS.

### Contract notes (see `docs/contracts/hostd-additions.md`)
- hostd and fakemgmt follow M2's clarifications: lease `<tenantID>/<workload>`, namespace = class, `revokeObjects`, `Twd-Host-ID`, canonical audit hash with ed25519, `epoch` on ready.
- **Additions (optional):** `pushURL` and the push API, heartbeat `deployments`, `kill`, assignment `workloadTags`, `GET /v0/deployments/{id}`, `mintMode:"static"` (fakemgmt).
- **Gaps M2 must close:**
  1. The workload entry node: hostd uses namespace `_workload`, which M2 needs declared explicitly, and M2 rejects bundles with no `tailnet.objects`.
  2. Kill delivery waits for a heartbeat without push.
- Also changed: M1 e2e-real cleanup is now **ID-based** (`tsapi.Tracker`) with a per-run `ttest-m1-<run>` prefix, because suites share tailnet 1. Re-run after the change: PASS; 7 tracked, 7 deleted, 0 left.

### Known gaps (M3)
- **Linux/Lima not run**; macOS only.
- **Facets and DO bindings.** No alarms in facet mode (use plain-DO mode). Facet env DO bindings expose only `getByName`, which must be awaited. `newUniqueId()` objects get no node.
- **Spawn caps are not enforced on the receiving side.** They are checked by mgmt but not yet written into the node cert (`CapMap ∩ lim`). `actingFor` assertions and `forward()` act-chain re-signing are not implemented, and `ctx.tailnet.connect()` is a typed `plan_feature_missing`.
- **Persistence and rejoin.** Object metadata persists (`objects.json`) and nodes rejoin on redeploy. Ephemeral nodes can't rejoin after a close, because tsnet logs them out.
- **Not built yet:**
  - egress MAC replay protection (no nonce cache; timestamp skew only);
  - same-host object→object calls over the tailnet (refused as siblings by netd); in-process DO RPC works and is not policy-checked;
  - cron triggers, WebSocket hibernation across deploys, internet egress (`egress.internet` is parsed but everything is denied), and `forward()` signing.

## Next steps
- M2 `twd-mgmt`: SQLite and Postgres leases with epoch fencing, the mint broker (API-key mode first, reusing `internal/tsapi`), the registration throttle and the audit log.
- M2 follow-ups for hostd: implicit `_workload` namespace, kill push or long-poll, delayed device delete for fail-fast new requests.
- M4 `twd` CLI on top of `create-twd build` + the deployments API; `twd up` runs twd-mgmt + twd-hostd.
- Run `make test` in Lima `twd-dev` (Linux arm64).
