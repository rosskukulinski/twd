# twd STATUS (living log)

Last update: 2026-11-07 23:55 PT. M0 and M1 by the first run; M2 (`twd-mgmt`) on branch `m2-mgmt`.
Last update: 2026-11-08 00:10 PT. M0, M1 (main) and M3 (branch `m3-hostd`).
Last update: 2026-11-08 00:40 PT. M8 (microVM tier, Firecracker in Lima) on branch `m8-vm`: `make e2e-vm` PASS.
Last update: 2026-11-08 00:45 PT. M4 (`twd` CLI + integrated single-machine flow, E2E-MAC on this Mac) on branch `m4-cli`.
Last update: 2026-11-08 01:10 PT. M5 (SDK conformance, `make test-sdk`) on branch `m5-sdk`: PASS on macOS and Lima.
Last update: 2026-11-08 01:40 PT. M7 (E2E-MULTI: one host, tenant A on tailnet 1 + tenant B on tailnet-2) on branch `m7-multi`: `make e2e-multi` PASS (3 runs).

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

## M2: `twd-mgmt` (DONE, branch `m2-mgmt`)
Code is in `mgmt/`, `cmd/twd-mgmt`, `internal/tsapi` (extended) and `e2e/real/mgmt`. The contract is implemented as written. The backward-compatible clarifications and additions are recorded in `docs/contracts/mgmt-api.md` § "v0 clarifications and additions"; **hostd should read that section**.

**Store** (`mgmt.Store`, one SQL implementation, two dialects)
- Backends: SQLite via `modernc.org/sqlite` (pure Go; `CGO_ENABLED=0 GOOS=linux GOARCH=arm64` builds) and Postgres via pgx.
- Covers tenants, hosts, deployments (bundle included), objects (lineage: parent, root, depth), leases, the audit log, audit chain heads and revocations.
- **Leases** are single conditional writes: `INSERT … ON CONFLICT DO UPDATE … WHERE expired OR same holder … RETURNING epoch`.
  - Acquire takes epoch+1. Renew and Release only succeed at `(holder, epoch)`. Release keeps the row, so epochs stay monotonic.
  - Fenced commits (`UpdateObject(…, &Fence{lease, holder, epoch})`) check the lease inside the same transaction, using `FOR UPDATE` on Postgres. Credential release goes through this path.
- **Audit** is append-only, enforced by UPDATE/DELETE triggers on both backends.
  - It is hash-chained per chain (`mgmt`, `host:<id>`) using canonical JSON with sorted keys, SHA-256, and an ed25519 `hostSig` for host chains.
  - The head table detects truncation.

**Mint broker**
- `apikey` mode reads the tenant's `apiKeyFile` at use time and never stores the key.
- `wif` mode:
  1. Signs an ES256 JWT (≤5 min, `iss=<base>/t/<tenant>`, pinned subject `twd:tenant:<id>:mint|lifecycle`, `aud=api.tailscale.com/<clientID>`).
  2. Exchanges it at `POST /api/v2/oauth/token-exchange` and caches the access token.
  3. Mints with `POST /keys`.
- The **OIDC issuer** serves discovery and the JWKS at both the root and `/t/<tenant>/`. It can run on a local listener (`-issuer-listen`) or via tsnet `ListenFunnel :443` (`-funnel`). The key is P-256, PKCS#8, 0600.
- Keys are single-use, preauthorized, tagged with the object's class tags and valid for ≤300 s. They are ephemeral for spawned children with a TTL.
- A key goes into custody and is released at most 15 s before it expires. An expired unused key is revoked and re-minted.
- Kill deletes devices with the lifecycle credential and revokes unused keys.

**Throttle**
- A per-tenant token bucket with default `registrationsPerMinute` 200 and burst rate/10.
- On a 429 or 5xx it halves the effective rate (floor 1/min) and pauses for `Retry-After`, or else a jittered exponential delay from 1 s up to 60 s. Each success adds 1/min back (AIMD).
- The mint queue is asynchronous, one worker per tenant. Pending objects are re-queued on restart.

**Spawn checks**
- The tenant policy comes from `GET /acl` and is cached with its ETag, revalidated with `If-None-Match` at most every 10 s. It is never written. If a refresh fails, the cached copy is kept (fail static).
- `caps ⊆ effective(parent)`: every app-cap grant whose src matches a parent tag, over every dst, merged with receiver semantics. Errors name the field (`delegation_exceeds_parent`).
- The child's tags must be owned by the parent's tags (`delegation_invalid`).
- Budgets: depth ≤ 3 and live descendants per root ≤ 64, both lowerable from `twd.jsonc` (`spawn_budget_exceeded`). Checks are serialised per tenant within a replica.

**Reaper, kill, custody**
- The reaper runs every 10 s and kills, with cascade, any live object past its TTL (`ttl_expired`) or whose parent is dead (`parent_dead`).
- Kill (optionally cascading over the subtree):
  1. marks the object killed;
  2. queues `revokeObjects` for the lease holder's heartbeats;
  3. deletes the device(s) (tier 2);
  4. revokes unused keys and wipes custody;
  5. writes one audit record per object (reason, requested root, parent, whether the device was deleted).
  A join that races a kill gets 410 on `/ready`, and its new device is deleted.
- **Custody:** `FileCustody` uses AES-256-GCM with one file per secret, the name bound as AAD, and the key in `custody.key` (0600, refused if group- or world-readable). `SetecCustody` is a stub client (put+activate, get, delete) behind the same interface.

**API and auth**
- The full v0 surface is served over a unix socket (0600 socket in a 0700 dir; connections whose peer uid, via `peercred`, isn't ours are closed).
- Optional tsnet listener `-tsnet-hostname` (`-tsnet-control-url` required) authenticates by WhoIs:
  - `tag:twd-host` nodes are hosts, bound to their registered hostID; a host can't act as another.
  - Untagged users are admins (optionally restricted with `-admins`).
  - Other tagged nodes are refused.

### Test results
- **`make test`** (hermetic, `-race`): **PASS** (netd, tsapi, mgmt, JS). The mgmt suite takes about 8 s and covers:
  - **Store fencing:** stale-epoch renew, release and fenced commit are rejected, and the commit is not applied; takeover after expiry; monotonic epochs after release; a 16-way acquire race produces exactly one winner.
  - **Throttle:** burst 20, then about 200 per simulated minute; Retry-After pause; exponential backoff; rate floor; AIMD recovery; Wait and cancel.
  - **Spawn ⊆ against the RFC §7.1 sample policy:** 10 allowed and 8 refused cases, each with the exact offending field; union and max across grants; a grandchild can't re-widen; tag ownership; ETag cache revalidation (304).
  - **Cascade-kill graph:** kill a mid-tree node and only its subtree dies, with 3 devices deleted; the root and siblings stay live until the root is killed, then the reaper takes its children as `parent_dead`.
  - **Audit:** a modified record is caught at its seq; a deleted tail and a gap are caught; a forged hostSig is caught.
  - **Issuer:** JWKS and discovery shape; our verifier and **go-jose independently** both verify the JWT (iss, sub, aud, ≤5 min); expired and tampered tokens are rejected; key mode 0600; reload keeps the kid.
  - **API end to end over the unix socket against a fake tsapi:** tenant → host → deploy (from a `twd.jsonc` bundle) → async mint → fenced credential (wrong epoch or wrong host gets 409) → ready → spawn tree → budgets → cascade kill → heartbeat `revokeObjects` → reaper → audit verify.
  - **Other cases:** two 429s with `Retry-After` slow the mint and lower the rate; TTL reaping; host audit ingest; tsnet WhoIs authorisation matrix; the full **WIF path** (fake token-exchange verifies our JWT against our JWKS); kill-while-joining; custody (file and setec); the error body always in catalog shape; no `tskey-` in the logs.
- **`make test-pg`** (new; throwaway `postgres:17-alpine` in OrbStack): **PASS**. The store tests ran on both backends and every API test ran on the Postgres store.
- **`make e2e-real`, mgmt part (`e2e/real/mgmt`, tailnet 1):**
  - **Mint → join → kill: 14/14 PASS** (2 runs).
    1. mgmt minted a `tag:ttest` key (spawn checked against tailnet 1's real policy, which was **read only**).
    2. A tsnet node joined with it as `ttest-m2-<run>-…`; the device is tagged `tag:ttest` and the key is ephemeral.
    3. A stale epoch got 409, and no second credential was released.
    4. Cascade kill from the root deleted the device, and it was **verified gone** (`GET /device` 404 and absent from the list).
    5. The host got `revokeObjects`, and the audit chain verified.
    - Numbers: join 1.41 s; kill API 0.29 s and 4.8 s (the same bimodal device-delete latency M1 saw); kill → device 404 0.43 s and 5.0 s.
  - **WIF: 12/12 PASS** (1 run).
    1. An ephemeral `tag:twd-mgmt` tsnet node served the issuer via Funnel. The first public fetch took **92.6 s** (cert issuance; the plan said ~55 s).
    2. The public JWKS verified an issued JWT.
    3. A **mint credential** (`auth_keys`, `tag:ttest`) and a **lifecycle credential** (`devices:core`) were created via the API, with audience = the generated default.
    4. Token exchange, then a mint, took 1.5 s.
    5. A node joined with the WIF-minted key.
    6. Kill deleted the device **using the lifecycle token** (0.29 s), and it was verified gone.
  - **Cleanup:** every run deleted exactly the device and key IDs it created, plus both federated credentials, leaving `devices_left: 0`. Afterwards the tailnet held no `ttest-m2` keys or federated keys. Nothing was deleted by prefix or tag sweep, because M3 shares the tailnet. **No policy was written.**
  - Results are in `e2e/real/results/m2-*.json` and `m2wif-*.json`.

### Findings
1. The WIF audience must be `api.tailscale.com/<clientID>`, Tailscale's generated default; the RFC said "the client ID". Federated credentials are deleted with `DELETE /keys/{id}`.
2. The first Funnel fetch for a new node name took about 93 s, not 55 s. Because the issuer origin must be stable (RFC §4.4), production must keep one long-lived mgmt node name. A per-run hostname re-pays cert issuance every time.
3. Device-delete latency on production control is still bimodal (about 0.3 s or about 5 s), matching M1.

### Not done / known gaps (M2)
- Nothing pushes revocations directly to hosts; the heartbeat `revokeObjects` field is the only path.
- No warm-key pool (RFC: 5 per tag set), no `nodeAttributes` (`custom:twd-*`) at mint, and no tier-3 posture kill.
- No node certificate, OBO root key or per-object assertion key (RFC §4.6), so `lim` is recorded in audit and in the object but not yet signed.
- Spawn budget checks are serialised per tenant *within one replica*. Several Postgres replicas could race the maxLive count; this needs a `SELECT … FOR UPDATE` on the root row.
- The `SetecCustody` client is a stub: it has never run against a real setec, and node state and DEKs are not stored there yet.
- Policy evaluation covers `src` tags and `*` only (not groups, users, autogroups or IP sets) and ignores `srcPosture`. That is enough for twd's tag→tag grants.
- In wif mode, policy reads use the lifecycle credential, which then needs `policy_file:read`; this hasn't been tested on a real tailnet.
- No Lima/Linux test run yet; the linux/arm64 build was only cross-compiled.

## Next steps
- M2 follow-ups: a per-root row lock for multi-replica budgets, a warm-key pool, node certificates (§4.6), `nodeAttributes` at mint, and a real setec.
- M3 `twd-hostd`: move `netd/testdata/workerd/platform.js` into the generated wrapper, add the egress MAC, and run the workerd supervisor.
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

## M8: microVM tier, Firecracker in Lima (DONE, branch `m8-vm`)
`make e2e-vm` **passes** inside Lima `twd-dev`: 9/9 subtests plus the VM smoke test, 4 consecutive green runs at ~5 s per suite. macOS `make test` and `make lint` stay green; `make test` is also green in Lima (Linux arm64; first Linux run of the process tier).

### What's built
- **Guest image (`guest/`, `make guest-image`).** `guest/build-image.sh` builds a 189 MiB read-only ext4 with no journal. It holds:
  - pinned **workerd 1.20261108.1**, the npm linux-arm64 binary at the lockfile version;
  - glibc 2.39 copied from the CI Ubuntu 24.04 rootfs (`~/fc/rootfs`), plus its CA bundle;
  - static **`twd-guestd`** as init.
  - No tailscale, tsnet or key material.
  - **Reproducible:** fixed UUID and hash seed, `SOURCE_DATE_EPOCH` mtimes, and ctimes/atimes pinned per inode with debugfs. Two builds gave the same sha256 (`6c91243…`). `manifest.json` records the input and output hashes.
- **`twd-guestd`** (`guest/twd-guestd`, PID 1):
  1. mounts proc, sys, devtmpfs and tmpfs;
  2. sets the clock from the config drive (egress MACs are timestamped);
  3. extracts the config drive (`/dev/vdb`, a raw tar of the generated instance dir plus `guest.json`);
  4. brings up `eth0` with static config via rtnetlink: 192.168.72.2/24, with a default route via .1;
  5. supervises workerd, restarting it up to 5 times;
  6. speaks NDJSON over **vsock** (CID 2, port 1024). It reports `boot`, `ready` (workerd healthy) and `workerd-exit`, and answers `stats`, `fsscan` and, in debug mode only, `probe`.
  - The contract is in `guest/proto`.
- **Host side (`hostd/vm`, Linux only; `hostd/sandbox` is the portable interface).** One Firecracker VM per workload instance, driven through its **API socket**:
  - machine-config, boot-source, rootfs (ro), config drive (ro), eth0 and vsock, then `InstanceStart`;
  - stop = SIGKILL of the VMM (arm64 has no ACPI shutdown; guest state is disposable);
  - **snapshot**: `MemoryDump` (pause, full snapshot, resume) is used by the key scan. Restore is not used.
- **Networking, tailvisor-style.** Each VM gets its **own network namespace** containing `fc0` (Firecracker's TAP) and `gv0` (hostd's TAP) on bridge `br0`. There is no IP, no IPv6 and no host listener in it.
  - hostd runs a **gVisor stack** on `gv0` (`fdbased` + ARP/IPv4/TCP). It is adapted from tailvisor `netd/stack.go`/`proxy.go` with BSD-3 attribution in `vnet_linux.go`. tailvisor's package wasn't imported as a library: it is one-VM-one-tsnet, has no go.mod, and needs gopacket.
  - The stack is the guest's only gateway. **Only `192.168.72.1:3128/3129/3130` exist**: facet egress (MAC'd object attribution), workload egress and host API. These are served by hostd's existing handlers, so netd maps each request to that object's `tsnet.Server.Dial` with the ADR-019 guard.
  - Every other TCP SYN, to any address or port, gets a **RST and is recorded**. UDP and ICMP aren't registered.
  - **Ingress:** netd (node listener, WhoIs, header strip, cf blob) → hostd routing → a reverse proxy that dials `192.168.72.2:8080` through the gVisor stack. That socket is the only one with `cfBlobHeader`.
  - **Per-object attribution inside one VM** is unchanged from M3: the facet's platform Egress sends `x-twd-class`/`x-twd-name` plus an HMAC over the TAP path, hostd verifies them, and egress leaves from the object's node.
- **Tier selection.**
  - Order: `"tier"` in twd.jsonc, then `Tenant.Tier` (mgmt contract addition, optional), then `hostd.Config.DefaultTier` (`twd-hostd -default-tier`), then `process`.
  - `microvm` falls back to `process` with a warning when there's no backend. That covers macOS, no KVM, not root, or no image.
  - Hosts register `tiers: ["process","microvm"]` when the backend is available. `WorkloadStatus` reports `Tier` and the VM boot info.
  - twd-hostd flags: `-vm-kernel -vm-rootfs -firecracker -vm-mem-mib -vm-vcpus -vm-hugepages`.
- **VM crash = workerd crash.** When the VMM exits, every flow into the guest is aborted, new requests get a typed 503 `upstream_unavailable`, and hostd reboots the VM with the bounded restart policy. Object nodes and their identities are untouched.

### Test results: `make e2e-vm` (hermetic: testcontrol + local DERP/STUN, fakemgmt, real hostd, real Firecracker v1.17.0, 2 vCPU / 512 MiB guests)
Final run: `e2e/vm/results/m8-20261108T083524Z.json`.

| Check | Result |
|---|---|
| Tier selection | hello and probe run in `microvm` (tenant tier); `probep` runs in `process` (twd.jsonc override) |
| `hello` in a VM | a tsnet client gets a verified caller (`nodeId` matches); the Worker's fetch from inside the guest is attributed to the workload node (echo WhoIs) |
| 3 DO objects in one VM | each gets its own node; direct ingress delivers the verified caller; **egress attributed to each object's own node 3/3**; forged `x-twd-*`/MAC headers from object a still leave as a |
| **No keys in the guest** | 26 needles: `tskey-`, the test auth key, and 12 node/machine private keys from host state, as text and raw bytes. **None** found in the rootfs, the config drive, 73 guest files (`fsscan`), or a **512 MiB full RAM snapshot**. Positive controls: the keys are found on the host, and the instance MAC key is found in the dump |
| **ADR-019: no host network or loopback** | raw TCP from the guest is refused for: gateway :22/:80/canary, Lima's IPs :22/canary, 10.0.2.2:22 (the Mac), 127.0.0.1, 169.254.169.254, 1.1.1.1, 192.168.72.3, and the echo node's 100.x IP. A canary listener on every host interface got **0 connections**. Through the platform path, egress to 127.0.0.1, the host IP and metadata gets 403 `forbidden_destination`; example.com gets `peer_not_visible` |
| Kill an object (mgmt) | streaming caller into the VM failed after **0.27 ms**; a new request failed in 0.2 ms (tombstone RST) |
| Kill the VM (SIGKILL the VMM) | streaming caller failed after **14 ms**; the next request got 503 `upstream_unavailable` in <1 ms; the VM was rebooted and serving the same object node again **0.87 s** after the kill |

**Measurements** (Lima on an M5 Pro; nested virtualization):

| Metric | Value |
|---|---|
| **Boot → ready**, InstanceStart → guest init | 173–188 ms |
| InstanceStart → workerd healthy in the guest | **0.60–0.62 s** |
| Whole `Boot()` (netns, TAPs, stack, VMM, API calls, boot, workerd) | 0.61–0.64 s |
| Deploy of 3 workloads → all active | 0.71 s |
| **Latency** (p50 / p95, 200 sequential requests over tsnet) | object in a VM 0.69 / 1.57 ms vs process tier 0.37 / 0.47 ms; object egress to echo 1.44 / 1.71 ms vs 0.80 / 0.89 ms; workload `hello` in a VM 1.35 / 2.30 ms |
| **RSS per VM** (Firecracker VmRSS + hugetlb) | 116 MiB (hello), 128 MiB (probe). Guest used 33/43 MiB, guest workerd RSS 49/63 MiB |
| Full memory snapshot (512 MiB) | 126–223 ms |

**`make e2e-real` microVM variant** (`TWD_E2E_TIER=microvm`, tailnet 1, production control, ephemeral `tag:ttest` `ttest-m8-972ff3-*`): **16/16 checks PASS** (`e2e/real/results/m8-20261108-003440-972ff3.json`).
- hello: verified caller, workload egress.
- 3 objects: own nodes, ingress, attributed egress.
- Kill: streaming caller TTF **0.53 ms**.
- VM InstanceStart → ready 0.60/0.62 s.
- **Cleanup:** 7 devices tracked, 7 deleted, 0 left, 0 untracked; 7 keys minted and revoked. No policy was read or written.
- The API key file was piped into Lima (`/tmp`, 0600), never printed, and deleted right after the run.
- As in M3, a *new* request to the killed node's IP on production black-holed for 5 s (M1 finding 3).

### Findings
1. **Huge pages are decisive under nested virt.** With 4 KiB pages, InstanceStart → workerd healthy took **3.7 s** for a trivial config and 6.5–7.3 s for the platform wrapper. With 2 MiB hugetlbfs pages it takes **0.43 s** and 0.60 s, because stage-2 faults dominate.
   - `HugePages: "auto"` (the default) uses them when `vm.nr_hugepages` has room.
   - `make e2e-vm` reserves 2048 pages with a runtime sysctl inside Lima (not persisted).
2. **1 vCPU is too few for workerd.** Its threads contend: p50 was 31 ms (object) and 50 ms (egress). With 2 vCPUs it is 0.7 ms and 1.4 ms. The default is now 2.
3. **`Pdeathsig` is per thread, not per process.** Creating the TAP inside a namespace needs a locked OS thread. Letting that thread exit once SIGKILLed *another* VM's Firecracker, because that thread had forked it. Fix: restore the namespace and unlock the thread, and drop `Pdeathsig`.
4. workerd won't start without a system CA store, even though it makes no TLS calls. The image ships the base CA bundle; `https://` egress is still originated by host netd.
5. Object names are per tenant *and class* (fakemgmt namespace = class). Two workloads of one tenant that share a class name share object names; the test uses distinct names.

### Known gaps (M8)
- **DO storage is guest tmpfs.** It is lost when the VM restarts or redeploys; `stord` over vsock (RFC §4.8) isn't built. Blue/green still works, but state isn't carried over.
- **No jailer.** Firecracker runs as root through `ip netns exec`, with no chroot, cgroups or seccomp profile beyond its built-in one. The whole tier needs root on the host (netns, TAP).
- **No snapshot restore** (warm empty VMs) and no wake-on-connection resume. Boot is about 0.6 s cold.
- One VM per workload instance. `isolation: "principal" | "object"` placement (RFC §4.8 residual risk) isn't implemented, so a V8 escape could impersonate the other objects in the same VM.
- `ctx.tailnet.connect()` and raw TCP over the TAP are not supported; HTTP(S) egress only, as in M1.
- The config drive and egress MAC key are readable inside the guest (by design: not a tailnet credential). A guest compromise can forge attribution among its own VM's objects only.
- The kernel is the Firecracker CI 6.1 build from `~/fc/vmlinux`, not built by `guest/`. The image builder takes glibc from `~/fc/rootfs` (`BASE_ROOTFS`).
- The tier is not offered on macOS (Virtualization.framework, per ADR-007, isn't built); microvm requests fall back to process there.
## M4: `twd` CLI and the integrated single-machine flow (DONE on this Mac, branch `m4-cli`)

### What's built
- **`twd up`** (`cmd/twd/up.go`). Non-interactive test mode: `twd up --tailnet <tailnet> --api-key-file <file> --yes` (tenant mint mode `apikey`; the key file path is stored, never the key).
  - It checks the key can read the tailnet's policy, writes `<state>/twd.json`, and starts a detached supervisor (`twd __supervise`, own session) that runs **twd-mgmt** (SQLite store, file custody) and **twd-hostd** (process tier) with restart-with-backoff (≤10 restarts / 5 min). Logs: `<state>/logs/{supervisor,mgmt,hostd}.log`.
  - Then it creates the tenant (`tn_<tailnet label>`), waits until the host is registered and heartbeating, and checks the base policy snippet (`tag:twd` tagOwners). It **prints** the snippet if missing; `--policy-apply` applies it (validate, then If-Match; `internal/policy`).
  - `--state-dir` (or `$TWD_STATE_DIR`) overrides `~/.twd`. Sockets go to `<state>/run`, or to `/tmp/twd-<hash>` when the state path is too long for macOS's 104-byte socket limit.
  - **It never uses the system tailscaled.** Every node is a tsnet node with an explicit control URL (production by default). Interactive mode (bootstrap from tailscaled) is **not built**: `twd up` without `--tailnet` fails with `tailnet_required` and the fix line.
  - `--ephemeral` and `--hostname-prefix` (default `twd-`) for tests and demos.
- **`twd down`** stops hostd first (nodes close in parallel), then mgmt (flushes pending device deletes). `--purge` (the default with `--ephemeral`) first kills every live root with `--cascade` (audit reason `twd down --purge`) and flushes the device deletes. **`twd status`** reports supervisor, mgmt, host heartbeat age, mint queue and rate, pending device deletes, workloads and objects; it exits 1 when unhealthy.
- **`twd init <dir> --template hello|agent|mcp`** copies a create-twd template and names the workload after the directory.
- **`twd deploy [dir]`**:
  1. **Preflight** via `POST /v0/tenants/{id}/preflight`: every declared `tailnet.connects` tag needs an `ip` grant from each of the app's tags, and every tag must be in `tagOwners`. If not, it prints the DX-style `› preflight: tag:a → tag:db tcp:80 … ✗ no grant` plus the HuJSON snippet and fails with `acl_denied` unless `--allow-missing-grants`.
  2. Bundles with create-twd's esbuild bundler (as hostd/gen expects). Apps outside the repo resolve `@twd/*`, `agents` and so on from twd's own `node_modules` via esbuild `nodePaths` (`TWD_NODE_PATH`).
  3. `POST /v0/deployments`, then polls `GET /v0/deployments/{id}` until the host reports `active` and the workload node is `ready`. `--var K=V` overrides wrangler vars; `--no-wait` returns early.
- **`twd kill <object> [--cascade] [--reason]`**: the object can be an ID, node name, MagicDNS name or `Class/name`. It prints tiers 1, 2 and 3 and the `revoked: principal → agent → sub-agent` chain.
- **`twd audit <object> | --human <principal> | --trace <id>`**: the object plus its descendants, both chains (mgmt and host), and `chain verified`.
- **`twd ls`** (`--all` includes killed objects), **`twd logs [workload] [--component mgmt|hostd|supervisor] [-n] [-f]`** (workerd's log for a workload).
- **`twd dev [dir]`**: the real hostd process tier against an embedded control plane (testcontrol + local DERP/STUN on 127.0.0.1) and an in-memory mgmt. A localhost proxy (`--port`, default 8787) forwards via a dev client node, so `getCaller()` sees a real caller. Edits reload (blue/green redeploy, mtime poll). `--once` is a smoke mode.
  - **Embedded policy evaluator: SKIPPED (stretch).** Every dev node sees every other and gets the same test caps. Mock peers from `twd.dev.jsonc` are not built either.
- **Every command takes `--json`.** Errors print in the DX.md catalog format (`error [code]: …`, detail lines, `fix:`, `trace · https://twd.dev/e/<code>`), or as JSON with `--json`. Keys are redacted from all output.
- **Not built** (M4 plan items outside tonight's scope): `twd host add/ls/doctor`, `twd tenant set`, `twd connect`/WIF onboarding from the CLI, interactive `twd up`.

### Gaps fixed (from M2/M3)
- **(a) Worker-only bundles and the workload node.** mgmt implicitly declares `_workload` from `tailnet.tags`. Objects in it resolve to their own workload's deployment (it used to be "the tenant's latest", which was wrong with two apps). `"node": false` classes need no tags. Assignments carry `workloadTags`.
- **(b) Kill push.** mgmt keeps each host's `pushURL` (unix only) and pushes every kill to the lease holder, in parallel, **before** the device delete; the heartbeat stays the fallback.
  - The device delete is deferred by `-device-delete-delay` (8 s under `twd up`, below hostd's 12 s tombstone).
  - The tombstoned node now **answers new requests with a typed `410 object_killed`** (netd `serveTombstone`) instead of an RST, and peers still have it in their netmaps. That fixes M3's "a new request to a killed object times out (5 s) on production".
  - Through the workload node, a call into a killed object is a typed `410 killed` instead of a 500: the runtime maps the platform's `[killed]` exception.
  - Pending deletes flush on mgmt shutdown and `POST /v0/flush`, so `twd down` leaves no devices.
- **(c) Also needed for the e2e:**
  - deployment status from heartbeats, and `GET /v0/deployments/{id}`;
  - assignment push on deploy;
  - hostd keeps its hostname prefix when mgmt names the node;
  - `actingFor` plumbing, so audit has a human or caller at the head of the chain. It comes from the platform: the first caller-bearing request's principal, with a host `principal` record when an RPC created the object first. Two bugs found on the way: the wire `user` is a string, and the Host DO Proxy answered unknown own properties with an RPC forwarder;
  - tsapi gained `GetPolicyHuJSON`, `ValidatePolicy`, `SetPolicy(If-Match)` and `GetKey`;
  - `mgmtapi.Client.DoHeaders`;
  - `object_killed` was added to the TS `TailnetError` catalog.

### Test results
- **`make test`: PASS** (netd, internal incl. the new `internal/policy`, cmd, mgmt, hostd with `-race`, gen, and JS 7/7 + 4/4).
  - New: `TestCLIEndToEnd` runs the **same scenario as E2E-MAC** through the real binaries against testcontrol and a fake Tailscale API, in about 8 s. It checks that no policy is written without `--policy-apply`, and that every device twd created is deleted by `twd down`.
  - New: `TestDevOnce` (`twd dev --once` → 200 with a verified caller), and the `internal/policy` merge/apply tests (comments and existing rules kept, idempotent, nothing written if validation fails).
  - `hostd` `KillFailsFast` now expects `410 object_killed`.
- `make lint`: PASS.
- **`make e2e-mac` (tailnet 1, production control, temp `--state-dir`, CLI only): PASS in 3 of 3 runs** (`e2e/real/results/m4-*.json`; the third on the final commit: up 0.99 s, deploy→ready 1.68 s / 1.54 s, first object node 1.63 s, kill CLI 71 ms, new request → object_killed 0.98 ms, 0 leftovers). Every check passed:
  - up, with interactive mode refused (`tailnet_required`);
  - status healthy;
  - preflight refuses `hello` (`tag:db`) with the snippet;
  - deploy `agent` (with `connects: [tag:ttest]`, preflight ✓) and `mcp`;
  - **(a)** the verified caller (the `tag:ttest` client's nodeId and tags) comes back, via the workload and via the agent object's own node;
  - **(b)** the private echo's WhoIs of the agent's fetch is **the agent object's own node**;
  - **(c)** `lookup_order` is allowed and `refund` gets `cap_missing`;
  - **(d)** `spawn` creates a `LookupAgent` with its own node and IP, `tag:twd-agent-support-sub`, and caps `{"example.com/cap/mcp":{"tools":["lookup_order"]}}` (narrower than the parent's `[lookup_order, refund]`, checked by mgmt against tailnet 1's real policy). The child answers on its own node;
  - **(e)** `twd kill --cascade` kills 2, pushes 2/2, and the CLI returns in **63–68 ms**. New requests to both killed nodes get **410 `object_killed` in 0.8–1.1 ms**; via the workload, 410 `killed` in 3–4 ms;
  - **(f)** `twd audit` shows `twd-e2e-<run>-client.tail7906d5.ts.net → …-supportagent-… → …-lookupagent-…` with spawn (with `lim`), mint, ready and kill records, and the chain verified; `--human` works.
  - **Cleanup:** after `twd down`, 0 of the 4 twd devices and 0 of the 4 twd keys remain (checked by ID: device 404, key consumed or revoked). Then the test's own client and echo were deleted by ID, and their 2 keys revoked: 0 devices left, 0 untracked.

| Timing (2 runs, this Mac → production control) | Value |
|---|---|
| `twd up` → ready (CLI wall time; mgmt healthy at ~0.83 s) | 1.20 s, 1.22 s |
| `twd deploy` → ready (workerd active at ~0.43 s; then mint + workload node join) | agent 6.96 s / 1.68 s · mcp 1.66 s / 6.36 s (bimodal join on production control) |
| First object node ready (first request → node ready) | 2.86 s, 1.62 s (first request itself served in 62–63 ms, before the node existed) |
| `spawn()` → child ready (includes the child's node join) | 9.6 s, 6.2 s |
| Kill: `twd kill --cascade` CLI wall time | 68 ms, 63 ms (mgmt API 56–58 ms, of which the push is 54–56 ms: netd's 50 ms `KillFlushDelay` after aborting the client's keep-alive flow) |
| Kill: a new request to a killed object | 410 `object_killed` in 0.8–1.1 ms |
| Agent egress to the private echo (warm) | 12–13 ms |
| `twd down` (purge + device deletes + stop) | 0.47 s, 4.3 s (device delete is bimodal, as in M1/M2) |

### Policy change on tailnet 1 (recorded per BUILD-PLAN safety rule 2)
- **When and how:** applied once, by `make e2e-mac` run `66f527` (2026-11-08 00:31 PT), through `internal/policy.Apply`. It read the policy with its ETag, merged **additions only** (comments and every existing rule kept), validated via `POST /acl/validate`, and wrote with `If-Match: "f80a31b1…"`. Later runs find nothing missing and don't write.
- **Backups:** the copy from before the change is in `~/.config/tailnet-agents-design/tailnet1_acl.pre-m4-66f527.hujson`; the original backup is untouched.
- **Added tagOwners:**
  - `tag:twd-wl-agent`, `tag:twd-wl-mcp` and `tag:twd-agent-support`, owned by `["autogroup:admin","tag:twd"]`;
  - `tag:twd-agent-support-sub`, owned by `["autogroup:admin","tag:twd","tag:twd-agent-support"]`.
- **Added grants** (all `tcp:80`):
  1. `tag:ttest` → the four twd tags above;
  2. `tag:ttest` → `tag:twd-wl-mcp` with app `example.com/cap/mcp: [{tools:[lookup_order]}]`;
  3. `tag:twd-wl-agent`, `tag:twd-agent-support` and `tag:twd-agent-support-sub` → `tag:ttest` (the echo);
  4. `tag:twd-agent-support` → `tag:twd-wl-mcp`, app `{tools:[lookup_order, refund]}`;
  5. `tag:twd-agent-support-sub` → `tag:twd-wl-mcp`, app `{tools:[lookup_order]}`.
- The snippet is `cliflow.Policy()` in `e2e/cliflow`. **Ross's personal tailnet was not touched**, and the system tailscaled was never used.

### Known gaps (M4)
- **Not yet run on `mbp`:** that needs M6 (release tarball, `install.sh`). twd finds `twd-mgmt`/`twd-hostd` next to itself, and workerd and create-twd via `$TWD_WORKERD`/`$TWD_CREATE_TWD`, next to the binary, or by walking up to a repo checkout. A release must ship `share/twd/create-twd` plus its `node_modules`. Deploy also needs `node` on the machine (esbuild).
- **`twd dev`** has no embedded policy evaluator and no mock peers (see above).
- **Interactive `twd up`** is refused (`tailnet_required`), and there is no `twd connect`, so WIF has to be set up through the API. `host add/ls/doctor` and `tenant set` are not built.
- **Kill:**
  - tier 3 (posture) is not available; the CLI prints `plan_feature_missing`;
  - after the tombstone window (12 s), a direct dial to the dead node's IP black-holes again, because the device is deleted and peers drop it (M1 finding 3). twd clients should treat netmap absence as dead;
  - the push path costs the 50 ms flush delay whenever the object had open flows.
- **Push URLs** live in mgmt memory: after a mgmt restart, kills fall back to the heartbeat until the host re-registers (hostd re-registers only on a 404).
- **Spawned sub-agents report `principal`** = the root's caller, not an `actingFor` assertion (RFC §4.6, still not implemented).
- **Linux/Lima not run** for M4.

## M5: SDK conformance, in-isolate, process tier (DONE, branch `m5-sdk`)

Full matrix: `docs/COMPAT.md`.

### What's built
- **`make test-sdk`** (`e2e/sdk`, build tag `sdk`, hermetic). Two twd-hostd hosts (each with fakemgmt, static mint) share one testcontrol + local DERP/STUN: host A runs the agent apps, host B the MCP servers, so agent → MCP calls cross the tailnet. A tsnet client drives everything over HTTP and WebSocket (`github.com/coder/websocket`, now a direct dependency). There is no real tailnet, no LLM and no key.
  - `apps/cfagents`: one `agents` `Agent` body exported twice, `StateAgent` (facet mode) and `PlainAgent` (plain-DO mode). It also serves an in-app `createMcpHandler` + `withTailnet` at `/mcp`.
  - `apps/vercel-mcp`: Vercel `mcp-handler` + `withTailnet` (`whoami`, `lookup_order`, `refund`).
  - Templates under test: `mcp` (host B), `agent`, and the new `ai-agent` (host A, deployed twice: once against each MCP server).
- **SDK versions:** `agents` **0.33.0** is still npm `latest` (2026-11-08) and runs on the pinned workerd 1.20261108.1 unmodified. Added (exact devDependencies): `ai` 7.0.165, `@ai-sdk/provider` 4.0.37, `@ai-sdk/provider-utils` 5.0.76, `@ai-sdk/mcp` 2.0.93, `mcp-handler` 2.3.1, `@modelcontextprotocol/client` 2.4.0.
- **Facet alarm delegation (twd fix).** workerd facets cannot set alarms ("Facets currently cannot set alarms"). Because the Agents SDK re-arms on every wake, one `schedule()` call in facet mode wedged the agent: every later request returned 500. The facet wrapper now redirects the facet's `ctx.storage.{set,get,delete}Alarm` through `TailnetApi` op `alarm` (facet objects only, props-attested) to its `Host_<Class>` DO. That DO owns the real alarm and its `alarm()` runs the facet's `alarm()` (`AlarmInvocationInfo` isn't serializable, so its fields are passed instead). A kill deletes the alarm. With this, **`schedule()`/`scheduleEvery()` work in both egress modes**, so the agent template no longer needs plain-DO mode for alarms.
- **Cross-workload identity collision (twd fix: fail closed).** mgmt keys objects by `(tenant, namespace = class, name)`. So two workloads of one tenant that use the same DO class and object name got the *same* mgmt object, and hostd handed the second app the first app's node: its egress (MCP calls) was attributed to the other app. hostd now refuses with a typed **`409 namespace_conflict`**, which is also in the TS catalog. The real fix is a mgmt namespace that includes the workload (contract change, M2).
- **`@twd/mcp` `mcpFetch(ctx, {headers, traceId})`.** A `fetch` for MCP clients (for example the `@ai-sdk/mcp` transport `fetch`):
  - it goes through `ctx.tailnet.fetch`, so it carries the object's or workload's identity and throws typed errors (`peer_not_visible`, `namespace_conflict`, …) instead of an opaque "HTTP 403";
  - it sends a W3C `traceparent` built from the caller's trace ID;
  - it emulates `redirect: "error"`, which is the `@ai-sdk/mcp` default and which workerd rejects.
  - Unit tested (mcp 5/5).
  - The Agents SDK's `addMcpServer` has no fetch hook. It already egresses from the agent's own node, but its errors are untyped.
- **Templates.**
  - New **`ai-agent`**: a `ToolLoopAgent` in a DO, one agent and one node per caller, with `/agents/<name>` and `/worker` routes. Its tools come from a twd MCP server via `createMCPClient` + `mcpFetch`, and it streams `toUIMessageStreamResponse()`. It ships a scripted `MockLanguageModelV4` in `src/model.ts`, with a comment on swapping in a provider through `ctx.tailnet.fetch`. Typed twd errors (including ones wrapped as an error's `cause`) render as catalog responses.
  - **`agent`** gains `routeAgentRequest`, `onConnect` with `getCaller(ctx.request)`, and a facet-mode `schedule()` route (`/remind`). The existing routes and responses are unchanged.
  - **`mcp`** gains a `whoami` tool.
  - `twd init` and `create-twd` now list `ai-agent`.

### Test results (measured 2026-11-08)
- **`make test-sdk`: PASS on macOS** (this Mac, 28.7 s) **and in Lima `twd-dev`** (Ubuntu arm64, cloned from `/Users/ross/dev/twd` at `m5-sdk`; 82.7 s, because node joins are slower there).

  | Check | Result |
  |---|---|
  | `routeAgentRequest`, `getAgentByName`, `getCaller` in `onRequest` (both modes) | ✅; an unknown namespace gets 400 |
  | `setState` / `onStateChanged` / `this.sql` (both modes, via the workload and via the object's own node) | ✅ |
  | `schedule(1)` + `scheduleEvery(1)` | ✅ both modes; once + 2× every fired in 1.9–2.0 s |
  | WebSockets: `onConnect` `getCaller`, `cf_agent_state` broadcast to a second connection | ✅ both modes |
  | WebSocket hibernation (14 s idle) | ✅ both modes: the DO was evicted (new boot ID), the sockets stayed open, connection state was intact |
  | `fetch()` egress | facet: the object's node; plain: the workload node |
  | `addMcpServer` → `mcp` template on host B | ✅ `lookup_order` allowed, `refund` `cap_missing`. The server's `whoami` = `a-cfagents-stateagent-mc1` (facet) / `a-cfagents` (plain) |
  | same-host `addMcpServer` | ❌ by design: netd `forbidden_destination` (the SDK reports "HTTP 403") |
  | `createMcpHandler` + `withTailnet`, `mcp-handler` + `withTailnet` | ✅ allow, deny (`cap_missing` JSON), `whoami` = the client |
  | `ToolLoopAgent` (mock model) → `mcp-handler` and → `createMcpHandler` | ✅ streamed UI message SSE (36–38 events); the MCP server saw the agent's own node (DO) or the workload node (Worker) |
  | typed errors through the MCP client | ✅ `403 peer_not_visible`, `409 namespace_conflict` |
  | Unsupported, documented by tests | `env.AI`, KV, Workflows, `env.BROWSER`, `env.LOADER` absent; `cloudflare:email` throws "not supported on twd"; `subAgent()` in a facet fails ("not found in worker exports") |
- **`make test`: PASS on macOS** (netd, internal, cmd, mgmt, hostd, gen, JS tailnet 7/7 and mcp 5/5). **Also PASS in Lima** (same packages, plus `hostd/vm`).
- `make lint`: PASS (now also vets `-tags sdk`).
- `package-lock.json` was regenerated with npm 11.19, because npm 11.6 (Node 25 on this Mac) had dropped the optional `@emnapi/*` entries and `npm ci` failed on Linux.

### Known gaps (M5)
- **Same-host object → object over the tailnet** is refused (RFC §4.5 platform path not built). An agent and its MCP server must be on different hosts.
- **The mgmt namespace is the class.** DO class names must be unique per tenant; hostd fails closed (`namespace_conflict`) until mgmt namespaces include the workload.
- **No on-behalf-of assertions** on MCP calls (RFC §4.6). Servers see the agent's node, not the human it acts for.
- `subAgent()` (facet sub-agents) does not work inside a facet. Use `ctx.tailnet.spawn`.
- Not tested: `@callable` RPC, `useAgent`, `AIChatAgent`, `McpAgent` (deprecated), and real LLM providers (no keys by rule).
- `addMcpServer` errors are untyped (there's no fetch hook to install `mcpFetch`).


## M7: multi-tenant hosts and E2E-MULTI (DONE on this Mac, branch `m7-multi`)

### What's built
- **Identity fix in mgmt (closes the M5 `namespace_conflict` finding).** Object identity is now **(tenant, workload, class, objectName)**.
  - `Object.Workload` is stored with a unique key on `(tenant_id, workload, namespace, name)`, and the object ID HMAC covers all four parts.
  - Two apps of one tenant with the same class and object name get distinct objects, nodes, hostnames and state dirs. The same names in another tenant never collide either, because the tenant is in the HMAC and in the state path.
  - `POST /v0/objects` takes `workload`, which hostd always sends. Without it:
    - a spawn inherits its parent's workload;
    - otherwise exactly one workload may declare the class, else 409 `namespace_ambiguous`.
  - An ID already held by another identity gets 409 `identity_conflict`.
  - **Store migration:** pre-M7 SQLite and Postgres stores are migrated in place when opened. Rows keep their IDs and take the workload from their deployment.
  - hostd keeps `namespace_conflict` only as a **defensive assertion**: it fails closed if mgmt ever returns another identity's object. Contract: `docs/contracts/mgmt-api.md` § M7. COMPAT.md is updated.
- **`twd tenant add <tailnet> --api-key-file <f> [--control-url] [--policy-apply] [--registrations-per-minute]`** is the CLI equivalent of `twd connect` in apikey mode.
  - It checks the key can read that tailnet's policy, creates the tenant in mgmt (`tn_<label>`) and records it in `twd.json` (key file path only).
  - It checks the base `tag:twd` snippet the same way `twd up` does.
  - It refuses a key file that already serves another tailnet (`state_conflict`).
  - `twd tenant ls` lists the tenants with role, throttle and objects.
  - `--tenant <tailnet|id|label>` is accepted by `deploy`, `ls`, `kill`, `audit` and `logs`; the default is the `twd up` tenant.
  - `twd audit --tenant X --all` returns every record of tenant X. Every audit query is scoped to the tenant on the server, and the CLI drops any record of another tenant.
  - `twd status` shows each tenant (mint queue, rate, objects) and each workload's tenant and sandbox. `twd down --purge` purges every tenant.
- **Per-tenant isolation on one host:**
  - **netd:** one registry per tenant, as before, with its own control URL. New: each registry's key source fetches credentials **only for that tenant's objects** and refuses others (unit test).
  - **workerd:** one process per (tenant, workload), never shared. The same workload name in two tenants gives two processes (pids are in status).
  - **State dirs:** `hostd/tenants/<tenantID>/{nodes,workloads}`, 0700 at every level. Tenant IDs are validated as one safe path element. DO storage lives under the tenant's own dir.
  - **Throttles:** mgmt's token bucket and AIMD backoff are per tenant. A test drains and backs off tenant A and checks that tenant B is unaffected.
  - **Process sandbox (new, macOS): Seatbelt.** On the process tier every daemon runs as one uid, so 0700 alone doesn't stop tenant A's workerd from reading tenant B's files. `twd-hostd -sandbox auto` (the default; `twd up` also passes `-sandbox-deny`) runs each workerd under `sandbox-exec` with a generated profile (`hostd/seatbelt.go`).
    - Denied: reads, writes and unix-socket connects to the home dir, hostd's data and run dirs, twd's state and run dirs (mgmt custody, **mgmt's admin socket**), and every tenant's API key file (from `GET /v0/tenants`).
    - Allowed: its own instance dir, its own workload's DO storage, and reading the workerd binary.
    - So a tenant's workerd can't read even its **own** tenant's node keys. The keys stay in hostd, as in the microVM tier.

### Test results (measured 2026-11-08, this Mac)
- **`make test`: PASS** (netd, internal, cmd, mgmt, hostd with `-race`, gen; JS tailnet 7/7 and mcp 5/5). New tests:
  - mgmt `TestStoreIdentityScope`, `TestStoreMigratesLegacyObjects` (SQLite; Postgres under `make test-pg`), and `TestAPIIdentityScope`:
    - 2 workloads × 1 class plus a second tenant give 3 distinct IDs and hostnames;
    - `namespace_ambiguous`; a spawn inherits its parent's workload; a cross-tenant parent is refused;
    - list filters; per-tenant throttles; tenant-scoped audit.
  - hostd `TestMultiTenant`: one hostd, **two testcontrol planes**, the same agent app in both tenants. It checks distinct IDs, workerd pids, instance dirs and DO dirs, and 0700 dirs. Seatbelt denies tenant B's state, node state, DO storage, hostd's audit key and the home dir. The by-name and by-IP refusal is typed `peer_not_visible`, and killing tenant A's agent leaves tenant B's serving.
  - hostd unit tests: `TestKeySourceRefusesOtherTenantsObjects`, `TestTenantIDsAreSafePathElements`, `TestSeatbeltProfile`.
  - cmd/twd **`TestCLIMultiTenant`**: the full E2E-MULTI scenario through the real binaries, against two testcontrols and a fake API, in about 9 s.
- **`make test-pg`: PASS**, including the Postgres legacy migration.
- **`make test-sdk`: PASS.** `SameClassAcrossWorkloadsFailsClosed` became `SameClassAcrossWorkloadsDistinctIdentities`: the two `ai-agent` workloads' `AiAgent/dup` get two nodes, and each one's MCP calls leave from its own node.
- **`make e2e-mac`: PASS** (tailnet 1; run `35f16c`, with Seatbelt on), so the single-tenant flow is unchanged.
- `make lint`: PASS (now also vets `-tags e2emulti`).
- **`make e2e-multi`: PASS in 3 of 3 runs** (`1678f2`, `d4aad9`, and `44d336` on commit `adf4cf2`; later commits change only docs and results; `e2e/real/results/m7-*.json`).
  - Setup: `twd up` with tailnet 1 = tenant A, then `twd tenant add tail2c3a48.ts.net` = tenant B. The same `agent` template directory (workload `agent`, `SupportAgent/a1`) was deployed to both. Test clients and echoes are tsnet `tag:ttest` nodes, each in its own tenant's tailnet, with prefix `ttest-m7-<run>-`.
  - **Tailnet-2's name** is discovered at run time. Its API key's device list was empty, so the name came from the MagicDNS name of a test client joined with an API-minted key. It is checked against an allow list, with the personal tailnet refused, before anything is written.

  | Check | Result (run `d4aad9`) |
  |---|---|
  | tenants | A `tn_tail7906d5` (operator) · B `tn_tail2c3a48` via `twd tenant add` (0.45 s); reusing A's key file for another tailnet → `state_conflict` |
  | (a) own agent, verified caller | each client's nodeId + `tag:ttest` seen via its workload and via its agent's own node; each agent's egress to its own echo = its own node (echo WhoIs) |
  | (b) same names, distinct identities | `agent:SupportAgent/a1` → A `supportagent-…` node `nPWS…` (`…tail7906d5.ts.net`) vs B `supportagent-…` node `nQy6…` (`…tail2c3a48.ts.net`); workload nodes, workerd pids, instance dirs and DO dirs all differ; each node's state is under its own tenant dir only |
  | (c) isolation A→B and B→A | tenant A's agent → B's echo (name, FQDN, IP), B's agent (name, FQDN, IP), B's workload (FQDN, IP): **8/8 typed `peer_not_visible` in 1–4 ms**; the same 8/8 for B→A |
  | (d) state dirs | all tenant dirs 0700. Under A's workerd profile: B's state dir, B's node state file, B's DO storage, **B's API key file**, A's own node keys and **mgmt's admin socket** are all denied, and A's own DO storage is readable (vice versa for B). The control check shows mgmt's socket is reachable outside the sandbox |
  | (e) kill A spares B | `twd kill --tenant A … --cascade`: 62 ms; A's agent → 410 `object_killed` in < 1 ms; B's agent and workload still 200 (3×), B object `ready`. Cross-tenant `kill`/`audit` of A's object as tenant B → `unknown_object` |
  | (f) audit per tenant | `twd audit --tenant A --all`: 11 records, all `tn_tail7906d5`, chain verified (includes the kill). Tenant B: 10 records, all `tn_tail2c3a48`, **no kill records**, chain verified |
  | cleanup | per tailnet: 0 twd devices and 0 valid twd keys after `twd down`; the test's own devices deleted by ID and keys revoked; 0 left, 0 untracked. An independent API listing afterwards shows 0 `ttest-m7` devices in either tailnet |

  | Timing (production control) | Value |
  |---|---|
  | `twd up` → ready | 1.56 s (2 runs) |
  | `twd tenant add` | 0.45 s |
  | `twd deploy` → ready (A / B) | 2.1 / 6.7 s and 4.8 / 1.8 s (bimodal join, as before) |
  | cross-tenant refusal (typed) | 1–4 ms |
  | `twd kill --tenant A` | 61–62 ms |
  | `twd down` (both tenants) | 4.0 s |

### Policy change on tailnet-2 (recorded per BUILD-PLAN safety rule 2)
- **When and how:** applied once, by `make e2e-multi` run `1678f2` (2026-11-08 01:27 PT), through `internal/policy.Apply`. It read the policy with its ETag, merged **additions only** (comments and every existing rule kept), validated via `POST /acl/validate` (also dry-run validated beforehand without writing), and wrote with `If-Match: "66bc1a17…"`. Later runs find nothing missing and don't write.
- **Backups:** the copy from before the change is in `~/.config/tailnet-agents-design/tailnet2_acl.pre-m7-1678f2.hujson`; the original backup is untouched.
- **Added:** exactly M4's tailnet-1 snippet (`cliflow.Policy()`, which `cliflow.MultiPolicy()` returns), so the two tenants have the same rules.
  - tagOwners: `tag:twd-wl-agent`, `tag:twd-wl-mcp` and `tag:twd-agent-support`, owned by `[autogroup:admin, tag:twd]`; `tag:twd-agent-support-sub`, owned by `[autogroup:admin, tag:twd, tag:twd-agent-support]`.
  - grants (tcp:80):
    1. `tag:ttest` → the four tags;
    2. `tag:ttest` → `tag:twd-wl-mcp` with app `{tools:[lookup_order]}`;
    3. the agent tags → `tag:ttest`;
    4. `tag:twd-agent-support` → `tag:twd-wl-mcp` `{tools:[lookup_order, refund]}`;
    5. `tag:twd-agent-support-sub` → `tag:twd-wl-mcp` `{tools:[lookup_order]}`.
- **Tailnet 1:** not written (it already had all of this from M4). **Ross's personal tailnet was not touched**, and the system tailscaled was never used.

### Findings
1. **Two tailnets on one host are isolated by netd's netmap-only egress guard, with no extra code.** A tenant's registry has only its own tailnet's netmap, so another tenant's names don't resolve and its IPs aren't peers: the answer is a typed `peer_not_visible` in about 1 ms, never a timeout. Cross-tenant IP collisions are possible in principle (both tailnets draw from CGNAT space). On production they didn't happen; on testcontrol they always happen (sequential numbering). The scenario skips, and notes, an IP that is also a node of the source tenant.
2. **0700 is not isolation on the process tier.** Every daemon runs as one uid, so before Seatbelt any tenant's workerd (after an RCE) could have read every tenant's node keys, mgmt's custody key, and the API key files, and could have used **mgmt's unix-socket admin API** (kill any tenant's objects). The Seatbelt profile closes all of these on macOS.
3. Tailnet-2's API key sees no devices on an empty tailnet, and the API has no "tailnet DNS name" field. The name is discovered from the first API-minted node's MagicDNS name, then checked against the allow list.

### Known gaps (M7)
- **Seatbelt is macOS-only**, and `sandbox-exec` is deprecated by Apple (it still works on macOS 27.2). On Linux, the process tier has 0700 dirs but no per-tenant confinement (no per-tenant uids, since that needs root); the microVM tier (M8) is the isolation answer there.
- A sandbox profile is built when a workerd starts. An API key file **outside `$HOME`** of a tenant added later is not denied to already-running workerds until they restart. Files under `$HOME` are always denied.
- netd isolation is **per registry, in one hostd process**. Per-tenant netd *processes* are not built, so a hostd compromise exposes every tenant's node keys.
- `twd tenant rm`, `twd connect` (WIF onboarding) and interactive mode are not built. Tenants are only added.
- With fakemgmt (tests), hostd names nodes `<workload>-<class>-<name>`, which is the same in two tailnets. That is harmless because tailnets are separate namespaces; real mgmt names nodes by the tenant-scoped ID.
- `GET /v0/tenants`, which the sandbox uses to deny every key file, is admin-only. It works on the local unix socket; a remote tsnet-mode mgmt would refuse it, and hostd falls back to the tenants it knows.
- Not run in Lima/Linux for M7 (the hermetic tests are portable; Seatbelt checks skip off macOS).
