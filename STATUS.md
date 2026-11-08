# twd STATUS (living log)

Last update: 2026-11-07 23:55 PT. M0 and M1 by the first run; M2 (`twd-mgmt`) on branch `m2-mgmt`.
Last update: 2026-11-08 00:10 PT. M0, M1 (main) and M3 (branch `m3-hostd`).
Last update: 2026-11-08 00:40 PT. M8 (microVM tier, Firecracker in Lima) on branch `m8-vm`: `make e2e-vm` PASS.
Last update: 2026-11-08 00:45 PT. M4 (`twd` CLI + integrated single-machine flow, E2E-MAC on this Mac) on branch `m4-cli`.
Last update: 2026-11-08 01:15 PT. M6 (merged with M5; release tarballs, `install.sh`, `twd doctor`, **E2E-MAC on `mbp`: PASS**) on branch `m6-release`.
Last update: 2026-11-08 01:10 PT. M5 (SDK conformance, `make test-sdk`) on branch `m5-sdk`: PASS on macOS and Lima.
Last update: 2026-11-08 01:20 PT. M9 (opt-in Node process tier + Vercel Workflow) on branch `m9-node`: `make test-node` PASS on macOS and Lima.
Last update: 2026-11-08 01:40 PT. M7 (E2E-MULTI: one host, tenant A on tailnet 1 + tenant B on tailnet-2) on branch `m7-multi`: `make e2e-multi` PASS (3 runs).
Last update: 2026-11-08 02:05 PT. Integration: master (M6 + M5 + M9, `57dd519`) merged into `m7-multi`; the full matrix passes on `4cce30d` (macOS, real tailnets, mbp from the release, Lima).

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

## M6: release packaging + E2E-MAC on a different, clean Mac (`mbp`) (DONE, branch `m6-release`)

### What's built
- **`make release`** (`scripts/release.sh`) → `dist/twd-<version>-{darwin,linux}-arm64.tar.gz` + `dist/SHA256SUMS`. Version = `VERSION` (0.1.0) + `-<commit>` (+`-dirty`). Each tarball (87 MB darwin / 93 MB linux; 331 MB unpacked) is self-contained:
  - `bin/twd`, `twd-mgmt`, `twd-hostd`, `twd-netd`: `CGO_ENABLED=0` (SQLite is modernc), `-trimpath`, `-X main.version`. Linux binaries are static ELF.
  - `bin/workerd` from npm `@cloudflare/workerd-<os>-arm64@1.20261108.1` (`npm pack`, cached in `dist/cache`), **sha512-checked against `package-lock.json`**.
  - **esbuild: built into `twd`** instead of a separate binary. `internal/esbundle` is the Go twin of `create-twd/lib/build.mjs` (esbuild 0.28.2 Go API, same options and plugin: workerd conditions, Node builtins → `node:*`, `cloudflare:email` stub). A test checks it is **byte-identical** to `build.mjs` for all three templates. `twd deploy` no longer needs Node.js (the M4 gap) on any machine.
  - `share/twd/create-twd` (templates, build.mjs) and `share/twd/node_modules`: the templates' runtime closure (`@twd/tailnet`, `@twd/mcp`, `agents`, `@modelcontextprotocol/{server,sdk}`, `zod`: 167 packages, 57 MB), copied from the tested repo `node_modules` by `scripts/vendor-node-modules.mjs` (no network; native `os`/`cpu` packages dropped). `twd init` + `twd deploy` work fully offline.
  - `install.sh`, `VERSION`, `SHA256SUMS` (every file), `INSTALL.md`, `share/twd/release.json`.
  - **macOS signing:** every darwin binary is re-signed ad-hoc (`codesign --force -s -`, verified) at build time on this Mac. **Quarantine:** an scp'd tarball on mbp has **no xattrs** (checked every run), so `xattr` isn't needed for scp. `install.sh` still clears `com.apple.quarantine` on its own copy in case the tarball came from a browser. **workerd 2026-11-08 runs on macOS 26.6.2** (mbp).
- **`install.sh`**: installs into `~/.twd` (`--prefix`/`$TWD_HOME`), refuses root, checks platform and `SHA256SUMS`, stages and swaps `bin/` and `share/` (idempotent: a re-run is an upgrade/repair; state files in `~/.twd` are kept), smoke-runs `workerd --version` and `twd version` before swapping, and **prints** the PATH line (it doesn't edit shell profiles). `--uninstall [--purge] [--force]` removes it (refuses while a supervisor runs, unless `--force`).
- **`twd doctor`** (new) checks: binaries; workerd runs (version vs the pin; quarantine warning); **the built-in bundler builds every template offline**; `~/.twd` and the state dir are writable (it creates nothing); **the API key works** (`--tailnet/--api-key-file` or from `twd up`; reads the policy; warns on a non-0600 file; never prints the key); and **the system tailscaled is not used** (explicit tailnet + control URL, and `lsof` on the running twd processes shows no tailscaled LocalAPI socket; it never contacts tailscaled or reads its state). `twd version` is new too.
- **`make e2e-mbp`** (`e2e/mbp`, tag `e2embp`; skips without the tailnet-1 key file or `ssh mbp`). From this Mac (`mini`):
  1. reads tailnet 1's policy and checks the scenario's grants exist (**no policy write in M6**);
  2. scp's the darwin tarball and pipes `tailnet1_api.env` over ssh stdin into `~/.twd-e2e/` (umask 077, checked `0600`);
  3. on mbp: extract, `install.sh` (twice: idempotence), `twd doctor --tailnet … --api-key-file …` (all 7 ok);
  4. on mbp: `twd up --tailnet tail7906d5.ts.net --api-key-file ~/.twd-e2e/tailnet1_api.env --yes --state-dir ~/.twd-e2e/state` (+ `--ephemeral --hostname-prefix twd-m6-<run>-`), `twd init` agent/mcp/hello, `twd deploy`;
  5. the `tag:ttest` client and echo (`ttest-m6-<run>-*`) run **on this Mac** and run checks (a)–(f) exactly as `e2e/mac` (the same `e2e/cliflow` scenario, now with an ssh `Exec` hook; CLI wall times are measured **on mbp** with a perl wrapper, so ssh is not counted);
  6. `twd doctor` again while twd runs (lsof: no tailscaled socket), `twd down`, uninstall, remove `~/.twd-e2e`, then leftovers by ID.
  - Cleanup is in `t.Cleanup` **and** a shell trap (`scripts/e2e-mbp.sh`) that, on any exit, runs `twd down --purge` if still running, stops `~/.twd/bin/*` processes, uninstalls, and deletes `~/.twd-e2e` (the key file). Verified with a deliberately corrupt tarball (run `9bad25`, FAIL as intended): mbp left with no `~/.twd`, no `~/.twd-e2e`.
  - **Cross-machine proof:** the mcp workload node's direct endpoint, as seen from this Mac, is mbp's LAN address; the host ID is mbp's (`h-rosss-macbook-pro`), the client runs on `mini`.
  - **DERP vs direct:** the main client got a direct LAN path from its first ping. So a second client in a child process with `TS_DEBUG_ALWAYS_USE_DERP=1` (the knob is process-wide) repeats the measurement over DERP only (region `sea`); it is tracked and deleted like the others.

### Test results
- **`make e2e-mbp`: PASS in 3 of 3 runs** (`e2e/real/results/m6-mbp-*.json`: `34cd1d`, `3feb1e`, `402770` on commit `e67b9d1`, clean tree; plus the final-commit run below). All 17 checks pass: copy (0600), install, doctor, up (interactive refused with `tailnet_required`), status, preflight refuses hello, deploy agent+mcp, **(a)** verified caller (the client's nodeId + `tag:ttest`, via the workload and the object's own node), **(b)** echo's WhoIs of the agent's fetch = the agent object's own node on mbp, **(c)** `lookup_order` allowed / `refund` → `cap_missing`, **(d)** spawn → `LookupAgent` with its own node, `tag:twd-agent-support-sub`, caps `[lookup_order]`, **(e)** `kill --cascade` 2 killed, 2/2 pushed, **(f)** audit chain `ttest-m6-<run>-client → …-supportagent-… → …-lookupagent-…` verified, `--human` works, cross_machine, system_tailscaled_not_used, down.
  - **Cleanup every run:** 4/4 twd devices gone and 4/4 twd keys consumed/revoked after `twd down` (by ID); the test's 2–3 nodes deleted by ID and keys revoked; 0 untracked `ttest-m6-<run>-*`/`twd-m6-<run>-*` devices; mbp has no `~/.twd`, `~/.twd-e2e` or twd processes left.
- **`make test`: PASS** (new: `internal/esbundle` byte-identity + error tests, `TestDoctor`). **`make lint`: PASS** (vets `e2embp`).
- **linux-arm64 tarball:** `install.sh --prefix` + `twd doctor` in Lima `twd-dev`: all ok (bundler offline, workerd runs). The full e2e was **not** run from the Linux tarball. The extracted copy is left in the VM's `/tmp/twd-m6-lima`.

| mbp (M2 Pro, macOS 26.6.2), 3 runs | Value |
|---|---|
| scp tarball (87 MB, LAN) | 3.0–3.2 s |
| extract · `install.sh` (sha256 of every file + copy) · re-run | 1.4 s · 5.1–5.2 s · 5.6–5.8 s |
| `twd doctor` (7 checks incl. API call and 3 template builds) | 0.56–0.62 s |
| `twd up` → ready (mgmt healthy at 1.08 s) | 1.57–1.59 s |
| `twd deploy` → ready (build + workerd active ≤1.1 s, then mint + node join) | agent 1.69 / 5.84 / 1.68 s · mcp 1.59 / 1.60 / 6.19 s (the bimodal production join seen in M4) |
| First object node ready (first request → node ready) | 1.69 / 1.78 / 1.73 s (the first request itself: 137–182 ms) |
| `spawn()` → child ready | 1.8 / 6.9 / 4.6 s |
| `twd kill --cascade` CLI wall time on mbp (mgmt API 58–60 ms, push 56–58 ms) | 73 / 68 / 72 ms |
| New request to a killed object, from this Mac | 410 `object_killed` in 7.8–9.3 ms (via workload 410 in 6.7–7.9 ms) |
| Cross-machine, **direct** (LAN): disco ping · HTTP MCP `tools/list` | 3.7–4.1 ms · 10.9–12.2 ms (median of 10) |
| Cross-machine, **DERP only** (sea): disco ping · HTTP `tools/list` | 10.9–11.0 ms · 18.0–18.9 ms (median of 11) |
| Agent egress on mbp → echo on this Mac (warm) | 24–40 ms |
| `twd down` (purge + device deletes) · uninstall + cleanup | 0.5–3.1 s · 2.3 s |

### Safety
- **Ross's personal tailnet was not touched.** mbp's Tailscale.app was never used, queried or read: the test never runs `tailscale`, and twd on mbp runs only tsnet nodes of tailnet 1 (`twd doctor`'s lsof check passed in every run).
- **No policy change in M6** (read-only check that M4's grants exist). No sudo on mbp; nothing installed outside `~/.twd` and `~/.twd-e2e`, and both are removed at the end. No Homebrew installs.
- Keys: the key file goes over ssh stdin (never on a command line), mode 0600, deleted at the end even on failure. No `tskey-*` in logs or results (check-secrets).

### After merging master (M5) into `m6-release` (commit `9e3c50b`)
- The release now vendors **every dependency any template declares** (computed from the templates' `package.json`): `@ai-sdk/mcp @ai-sdk/provider @modelcontextprotocol/server @twd/mcp @twd/tailnet agents ai zod` → 177 packages, 71 MB. `twd doctor` and the esbundle byte-identity test cover all four templates, `ai-agent` included.
- e2e-mbp gained `init_ai_agent_offline`: `twd init --template ai-agent` on mbp (no npm) and doctor bundles it offline. It isn't **deployed** on mbp: its tags (`tag:twd-wl-ai`, `tag:twd-ai-agent`) aren't in tailnet 1's policy, and M6 writes no policy.
- **`make e2e-mbp`: PASS 2 of 2 on `9e3c50b`** (clean tree; runs `bf4d28`, `e61800`), 18/18 checks each, 0 leftovers, mbp clean. Numbers are in line with the table above: install 5.5–5.6 s, up 1.57–1.63 s, deploy→ready agent 3.74 / 1.58 s and mcp 1.61 / 1.60 s, first object node 1.79 / 1.71 s, spawn 6.8 / 3.6 s, kill CLI 69 / 69 ms, killed → 410 in 6.8 / 7.8 ms, direct HTTP 11.1 / 12.3 ms vs DERP 18.8 / 18.8 ms, direct ping 3.7 / 4.1 ms vs DERP 11.1 / 11.4 ms.
- Total: **5 green e2e-mbp runs** (3 before the merge, 2 after) plus 1 deliberate failure-path run.
- `make test` and `make lint`: PASS after the merge.

### Known gaps (M6)
- **No separate esbuild binary in the tarball:** esbuild is linked into `twd` (Go API, same version). That is simpler and needs no Node; `create-twd build` (the JS CLI) still needs Node + npm esbuild.
- **Signing:** ad-hoc only (no Developer ID, no notarization). A browser-downloaded tarball would hit Gatekeeper; `install.sh` clears quarantine, but notarization is the real fix for distribution.
- **The vendored `node_modules`** cover the templates' imports (`agents`, MCP, zod, `@twd/*`). An app importing other npm packages needs its own `node_modules` (resolved from the app dir first). There is no `twd add`/npm-free package install.
- **The linux-arm64 tarball** is install-and-doctor tested in Lima only; E2E-VM/E2E-MAC weren't run from it.
- **`install.sh` takes ~5 s**, mostly `shasum` over 331 MB of files; acceptable, but could verify the tarball hash only.
- Interactive `twd up`/`twd connect` (WIF) are still not built (M4 gaps), so a fresh Mac needs an API key file.
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
- ~~Not run in Lima/Linux for M7~~: run after the integration merge (see "Integration (M7 + master)"); Seatbelt checks skip off macOS.

## M9: opt-in Node process tier + Vercel Workflow (DONE, branch `m9-node`)
`make test-node` **passes on macOS** (Node 25.1) **and in Lima `twd-dev`** (Linux arm64, Node 24.22; fresh clone of `m9-node`). It is hermetic: testcontrol + local DERP, fakemgmt, real hostd, real node. `make test` stays green on macOS and in Lima; `make lint` is green. Design and caveats are in `docs/node-tier.md`.

### What's built
- **`"runtime": "node"` in twd.jsonc** (`hostd/bundle`: `App.Runtime`, `App.Node`).
  - The bundle is the built app dir; `main` comes from twd.jsonc or twd-bundle.json. Also parsed: `node.env`, `listen` (`tcp`/`unix`), `health` and `args`.
  - Rejected: `tailnet.objects` (ADR-022: one identity per process), `tier: microvm`, reserved `TWD_*` env and non-flag args.
  - The real mgmt accepts it unchanged; it only needs `tailnet.tags`.
- **hostd Node tier** (`hostd/node.go`).
  - **Process.** One supervised `node` process per deployment; the identity is the **workload node** (no per-object nodes). It runs in its own process group with a clean env: hostd's environment is not inherited, and a canary test checks this.
    - App data dir: `<data>/tenants/<t>/workloads/<w>/node`.
    - Logs go to `app.log`, which `twd logs <workload>` reads via the new `WorkloadStatus.LogFile`.
    - Status reports `Runtime` and `Node{PID,Starts,ReadyMs,Addr,ProxyAddr,LogFile}`.
    - `twd-hostd -node` (or `$TWD_NODE`) picks the binary; the default is PATH.
  - **Ingress.** netd (WhoIs, strip) → hostd → app on 127.0.0.1:PORT (or a unix socket), with `x-twd-caller: v1.<claims>.<HMAC-SHA256>`. The per-process key `TWD_CALLER_KEY` is fresh on every start, and hostd also drops any inbound `x-twd-*`/`twd-*`.
  - **Egress.** A per-process HTTP proxy on loopback with fresh Basic credentials, exported as `HTTP(S)_PROXY`/`TWD_EGRESS_PROXY` with `NODE_USE_ENV_PROXY=1`. `NO_PROXY` covers only the app's own port and the proxy.
    - Absolute-form requests go to netd egress.
    - **CONNECT** tunnels go through new `netd.Registry.DialEgress`: the same ADR-019 guard, tracked so a kill RSTs them. Node's built-in `fetch` uses CONNECT even for `http://`.
  - **Supervision.** Restart with exponential backoff (250 ms → 10 s, reset after 30 s up), never giving up; requests get 503 `upstream_unavailable` while the app is down. Deploys are **stop-then-start**, because durable runtimes assume one process per data dir.
- **`@twd/node`** (`js/packages/node`, 3 node tests):
  - caller: `verifyCaller`/`getCaller`/`requireCaller`/`signCaller`/`CallerError`;
  - egress: `fetch` (absolute form via the proxy), `dispatcher(undici)` and `installGlobalDispatcher(undici)`;
  - `proxyUrl`.
- **Build.** `create-twd build-node` (`lib/build-node.mjs`).
  - It esbuilds `main` for Node into `dist/server.mjs`.
  - If `workflows/` exists, it runs Vercel Workflow's `StandaloneBuilder` (the builder behind `workflow build`), ships `.well-known/workflow/v1/flow.mjs`, and applies the SDK's step-mode SWC transform to app code, as the rollup, Vite and Nitro integrations do.
  - The `twd` CLI's `deploy`/`dev`/`init` handle `runtime: node` (no wrangler.jsonc).
- **Template `node-workflow`** (`create-twd`; `twd init --template node-workflow`). It is a plain `node:http` server with a `"use workflow"` `supportWorkflow`:
  1. an AI SDK **`WorkflowAgent`** with a scripted mock `LanguageModelV4` (no keys; serializable via `Symbol.for("workflow-serialize")`) calls the **twd MCP** tools `lookup_order` and `refund` over the tailnet as `"use step"` functions;
  2. then `sleep("3s")`;
  3. then a `createHook` approval, resumed by `POST /hooks/<token>`.
  - The World is **`@workflow/world-local`** (filesystem), in the deployment data dir, and the server calls `world.start()` at boot to re-enqueue in-flight runs.
  - Versions: `workflow` 5.4.0, `@workflow/world-local` 5.2.1, `@ai-sdk/workflow` 2.0.97, `ai` 7.0.165, pinned in `e2e/node/package.json` (kept out of the root workspace, so `make test` doesn't pull ~264 MB).

### Test results: `make test-node`
Result files: `e2e/node/results/m9-*.json`.

| Check | Result |
|---|---|
| Verified caller | `/whoami` over the tailnet: `nodeId` = the client's; capMap present; deploy `--var` reaches env |
| Forged header via tailnet | client-sent `x-twd-caller` and `x-twd-cf` are stripped; the app still sees the real caller |
| Forged header bypassing netd (direct to the app port) | none, forged, wrong key: **401 `no_identity`**; `getCaller` → null |
| Env hygiene | `TWD_*`/proxy vars present; hostd's canary env var **absent** |
| Egress attribution | echo's WhoIs = the deployment's node for `@twd/node` fetch, global `fetch` (env proxy → CONNECT), `node:http` (env proxy, absolute form) and undici `ProxyAgent` |
| SSRF via proxy | 127.0.0.1 (the proxy's own port), ::1, 169.254.169.254, a host LAN IP, the app's own tailnet IP, 1.1.1.1 and `localhost`, via absolute form, raw CONNECT and global fetch: all refused, typed 403 `forbidden_destination` / `peer_not_visible` (global fetch over a refused CONNECT throws) |
| Proxy auth | no credentials or a wrong token: **407** |
| Supervision + logs | crash (`exit 3`) → restarted and serving; the rotated caller key still verifies; `app.log` has the app's output and no `tskey-` |
| **Vercel Workflow resumes after a kill** | SIGKILL during the durable sleep → hostd restarts the process → the run reaches the hook; SIGKILL again at the hook → restart → `resumeHook` → **completed**. Steps ran in **3 different PIDs** |
| **MCP from the workflow, authorized by caller caps** | the workflow node holds `{"tools":["lookup_order"]}`: `lookup_order` OK (`shipped`); `refund` → **`cap_missing`**, and the denial's caller chain names the workflow's own node |

**Measurements** (macOS M5 Pro, Node 25.1 / Lima, Node 24.22):

| Metric | macOS | Lima |
|---|---|---|
| **RSS per Node deployment**, plain app (`nodeprobe`, after undici loaded) | 68–106 MiB | 87 MiB |
| **RSS per Node deployment**, Workflow + AI SDK (`node-workflow`) | **293–321 MiB** | 293 MiB |
| **Cold start**, spawn → health, plain app | 45–48 ms | 45 ms |
| **Cold start**, spawn → health, `node-workflow` (8 MB server + 22 MB flow bundle) | 444–538 ms | 587 ms |
| Deploy → first 200 over the tailnet, plain app (includes node join) | 0.41–0.47 s | 3.1 s |
| Build + deploy `node-workflow` (StandaloneBuilder ~2.7 s) | 3.1–3.4 s | 4.2 s |
| Crash → serving again (250 ms backoff + start) | 308–321 ms | 327 ms |
| Workflow: kill → restarted (backoff + start) | 733–748 ms | 844 ms |
| **Workflow resume time**: restart → first post-sleep step ran (recovery + replay) | **564–638 ms** | 757 ms |
| Workflow: `resumeHook` → run completed | 69–78 ms | 113 ms |
| Egress p1 through the proxy (sdk / global CONNECT / http / undici) | 6–7 / 5–9 / 1–2 / 10–13 ms | 8 / 9 / 1 / 14 ms |

### Findings
1. **Importing `undici` silently disables env proxies.** Measured with Node 25.1 and undici 8.12: loading the `undici` package installs a plain `Agent` as the global dispatcher, so every later global `fetch()` goes **direct**. The test reached 1.1.1.1 that way before the fix. `@twd/node` `installGlobalDispatcher(undici)` restores the proxy. The Workflow bundles carry their own undici, so the template uses `@twd/node` `fetch`. This is the concrete reason the process tier is not an enforcement boundary.
2. **Node's built-in `fetch` uses CONNECT for `http://` targets** with `NODE_USE_ENV_PROXY`, so the M1 "CONNECT refused" behaviour made env proxies useless. CONNECT is now tunnelled for the Node tier via `DialEgress`, under the same ADR-019 checks.
3. **Node's global `http` agent applies env proxies too.** An explicit request *to* the proxy was itself proxied and its URL mangled (the target host got the proxy's port). `@twd/node` uses its own `http.Agent`, and `NO_PROXY` includes the proxy.
4. **Vercel Workflow needs two non-obvious things outside a framework:**
   - `world.start()` at boot, or in-flight runs are never recovered after a crash;
   - **real paths** when computing workflow IDs. macOS `/tmp` → `/private/tmp` made the IDs differ between the SDK's bundler and the app transform, giving `WorkflowNotRegisteredError`.
   - world-local also reads `<bundle dir>/../package.json` for its version, so the build writes one.
5. A Workflow app is **~3× the RSS** of a plain Node app: ~300 MiB against the research doc's ~100 MB for `ai` alone, because of the 22 MB `flow.mjs` plus the `node:vm` workflow sandbox. That is ~100× an isolate. One process per deployment (ADR-022) is right; per-object would not be.

### Gaps / follow-ups (M9)
- **Node apps don't run in the microVM yet.** The guest image has no Node.js, and `runtime: node` + `tier: microvm` is rejected. That needs Node in `guest/build-image.sh` plus a guestd process mode; the gVisor gateway would then make egress enforcement real. Until then, a malicious Node app can bypass the proxy (raw sockets, its own dispatcher) and reach loopback, LAN and internet. It still can't use a tailnet identity or forge a caller (`docs/node-tier.md`).
- **Ingress is HTTP only** (no raw TCP/WebSocket upgrade tested). The app's loopback port is reachable by other local processes; the HMAC header makes that safe only if the app verifies.
- **Replay window:** a captured `x-twd-caller` verifies for 60 s (no nonce cache); exposure is local-only.
- **World:** only `@workflow/world-local` is tested. It is single-process and its queue is in memory, with recovery on `start()`. `@workflow/world-postgres` (5.1.1), the SDK's self-hosting option, is not wired or tested (no Postgres in Lima); it would also allow blue/green deploys for Node.
- **Deploy restarts:** Node deploys are stop-then-start, so there is a short gap (requests wait at the gate). Whether a step in flight during a deploy is retried was not tested (recovery after SIGKILL was).
- No CPU or memory limits (cgroups) on the node process; no per-deployment Node version pinning (it uses hostd's `node`).
- `docs/COMPAT.md` lives on `m5-sdk` (not this branch). To avoid an add/add conflict, the M9 compatibility table is in `docs/node-tier.md` § Compatibility; append it to COMPAT.md when the branches merge.
- `twd dev` / `twd deploy` of `node-workflow` outside the repo need the app's own `npm install` (the Workflow builder resolves from the app dir). `@twd/node` is not published, so it resolves via twd's `node_modules` (`TWD_NODE_PATH`).

## Integration (M7 + master) (DONE, branch `m7-multi`, commit `4cce30d`)
`git merge origin/master` (`57dd519`: M6 release/esbundle/doctor/e2e-mbp, M5 SDK, M9 Node tier) into `m7-multi`: merge commit `a219532`, then fixes in `4cce30d`. Master fast-forwards to `origin/m7-multi`.

### Conflicts and semantic decisions
- **Textual (9 files), all kept from both sides.**
  - `hostd.Config`: `Sandbox`/`SandboxDeny` (M7) and `NodeBin` (M9). `instance`: `doStorage`/`profile` and `node`.
  - `WorkloadStatus`: `PID`/`Sandbox`/`SandboxProfile`/`DOStorage` and `Runtime`/`Node`/`LogFile`.
  - `cmd/twd`: the `--tenant` usages plus `doctor`/`version`. `twd logs <workload>` filters by tenant **and** uses `LogFile` (`app.log` for runtime node).
  - Tests: `TestCLIMultiTenant` and `TestDoctor` both kept. Makefile: `e2e-multi`, `e2e-mbp` and `test-node`; lint vets `e2emulti` and `e2embp`.
- **(a) The Node tier runs sandboxed per tenant on macOS.** Node apps are tenant code, so `hostd/seatbelt.go` now wraps node exactly like workerd (`instance.sandboxed`). The profile:
  - read-write: only the instance dir and the app's own data dir. That data dir is now `tenantDir(<t>)/workloads/<w>/node`, so it is per tenant and 0700;
  - read-only: the node binary and its install prefix (`…/bin/node` → `…`). This is needed because nvm puts node under the denied home dir;
  - denied: the home dir, hostd/twd state and run dirs, mgmt's socket, and every tenant's key file.
  - `make test-node` runs its hosts with `sandbox auto`, as twd-hostd does by default. **Measured on macOS:** the nodeprobe app gets **EPERM** on hostd's data dir (a canary file, list, write), on the tenants dir and on `$HOME`, and its own data dir is read-write. **Vercel Workflow (world-local, steps, sleep, hook, kill/resume) passes under Seatbelt.** Unit test: `TestSeatbeltProfileNodePrefix`.
- **(b) Seatbelt from an installed release on a clean Mac.** The installed `~/.twd/bin/workerd` is under the denied home dir. It is allowed as a read-only literal; the instance dir and DO dir are allowed below the denied state dir; no repo paths are involved.
  - `make e2e-mbp` gained `seatbelt_release`. On mbp (macOS 26.6.2), every workload reports `sandbox seatbelt`. Under each workload's **live profile**, `cat` of the API key file, `ls` of `~/.twd-e2e/state/hostd` and `ls ~/.twd/bin` all fail, while the same commands succeed unsandboxed. Only exit codes are used.
- **(c) `twd doctor` has a `sandbox` check.**
  - It runs `workerd --version` under a generated twd profile that denies `$HOME`, and checks that a denied canary is unreadable.
  - While twd runs, it reports `active: N/N` process-tier workloads (workerd and Node) under Seatbelt, and warns if any isn't.
  - `skip` off macOS: Linux has 0700 dirs only, so use `tier: microvm` there.
- **(d) Release.** M7 needs nothing new in the tarball: `sandbox-exec` is part of macOS, and profiles are generated at runtime. `release.json` now records `"sandbox"`. `INSTALL.md` documents Seatbelt, the doctor check and `twd tenant add`.
- **Linux:** Seatbelt code is runtime-gated (`ResolveSandbox` → none). Its tests skip off macOS, and the Linux builds and tests don't depend on it (Lima: green).

### Fixes needed in shared code (both pre-existing on master `57dd519`)
1. **`make release` (and so `make e2e-mbp`) failed.** After M9, release vendoring took every template's deps, including `node-workflow`'s Workflow packages, which live only in `e2e/node/node_modules`. The error was `can't resolve @ai-sdk/workflow`.
   - The new `scripts/template-roots.mjs` takes all deps of the workerd templates, and only `@twd/*` for Node-tier templates. This matches the lead's reconciliation: Node-tier apps need Node and their own `npm install` to build.
   - Result: 178 packages, 70.8 MB. Test: `internal/release` `TestTemplateRoots`.
2. **`make e2e-vm` `KillObjectFailsFast` failed** on master too (checked with a fresh master clone in Lima). The test enables `KillTombstone`, so a new request gets the M4 typed **410 `object_killed`**, but the M8 assertion expected a dial error. It now accepts either, under 1 s. Measured: 410 in 0.41 ms.

### Verification matrix (all on `4cce30d`; the later commit changes only STATUS and results)
| Where | Target | Result |
|---|---|---|
| macOS (this Mac, 27.2) | `make test` | **PASS** (incl. `internal/release`, `TestCLIMultiTenant`, `TestDoctor` with `sandbox: ok`, `TestMultiTenant`, Seatbelt unit tests) |
| | `make lint` | **PASS** |
| | `make test-sdk` | **PASS** |
| | `make test-node` | **PASS** (TestNodeTier incl. new `Sandbox` subtest: seatbelt, 5/5 denied, own dir rw; TestNodeWorkflow under seatbelt) |
| | `make test-pg` | **PASS** (incl. Postgres legacy migration) |
| macOS, real tailnet | `make e2e-mac` | **PASS** run `99da11` (12/12; up 1.10 s, kill CLI 69 ms, 0 devices left) |
| | `make e2e-multi` ×2 | **PASS** `dc7767`, `ae780f` (13/13 each; tenant B on `tail2c3a48`; up 1.27–1.28 s, deploy→ready A/B ~1.58 s, kill A 63 ms; 0 devices left in either tailnet; **no policy written**) |
| | `make e2e-mbp` ×2 | **PASS** `a3ce5f`, `4e0d74` (20/20 each, release `0.1.0-4cce30d4`, clean tree; mbp macOS 26.6.2; doctor 8/8 ok incl. `sandbox` before up; `seatbelt_release` and `doctor_sandbox_active` 2/2; install 5.4–5.7 s, up 1.41–1.57 s, kill CLI 68 ms; 0 devices left; mbp clean) |
| Lima `twd-dev` (Linux arm64, fresh clone of `m7-multi` @ `4cce30d`) | `make test` | **PASS** (98 s; Seatbelt checks skip) |
| | `make test-node` | **PASS** (`Sandbox` subtest skips: no Seatbelt on Linux) |
| | `make test-sdk` | **PASS** |
| | `make e2e-vm` | **PASS** (all 9 subtests; `KillObjectFailsFast` new request → 410 `object_killed` in 0.41 ms) |

Earlier runs, kept for the record: on merge commit `a219532`, `make e2e-mac` (`8c1592`) and `make e2e-multi` ×2 (`1dc500`, `4ba728`) passed, and `make e2e-mbp` failed in `make release` (fix 1). On a dirty tree with fix 1, e2e-mbp passed once (`acd614`).

### Safety
- Policy: **no writes** (both tailnets already had the scenario's rules, as e2e-multi reports). Ross's personal tailnet and mbp's Tailscale.app were not touched (doctor's lsof check passed).
- Every run deleted its own devices by ID and revoked its keys (0 left). No `tskey-` in results (check-secrets).

### Gaps (integration)
- `sandbox-exec` is deprecated by Apple; it works on macOS 26.6.2 (mbp) and 27.2. The doctor check is the canary for a future macOS.
- The Node tier's read-only node prefix means an app can read its own Node install. If `node` isn't under a `bin/` dir, only the binary itself is allowed; a node that loads libraries from under `$HOME` from elsewhere would fail to start (not seen).
- The Node tier on Linux has no per-tenant confinement, same as workerd. `runtime: node` + `tier: microvm` is still rejected (M9 gap).
- Node-tier templates (`node-workflow`) are shipped in the release but not deployable offline: they need Node and `npm install`, as before (only `@twd/node` is vendored).


## L2: Linux process-tier sandbox, Landlock + seccomp (DONE, branch `l2-landlock`)
Parity with the macOS Seatbelt sandbox (M7) for the Linux process tier, measured in a fresh Lima VM **`twdl-sbx-u24`** (Ubuntu 24.04 arm64, `vz`, 4 CPU / 6 GiB, kernel 6.8.0-147, **Landlock ABI 4**; `apparmor_restrict_unprivileged_userns=1`, so no user namespaces; Go 1.27.3, Node 24.22). The VM mounts this worktree; tests ran on an rsync'd VM-local copy so the Mac's `node_modules` stay darwin. It is **stopped, not deleted**.

### What's built
- **`internal/procsb`: a re-exec launcher, no cgo** (`github.com/landlock-lsm/go-landlock` v0.11.0 with best-effort downgrade up to ABI 11; hand-built seccomp BPF, arm64 + amd64). hostd starts `<twd-hostd> __twd-sandbox-exec -f <instance>/sandbox.json -- workerd|node …`. The launcher runs from package `init` in any binary importing procsb (twd-hostd, twd, test binaries), applies the profile to itself and `execve`s the workload in place (same pid). Cost: ~2 ms per start (`workerd --version`: 1 → 3 ms).
  - **Landlock FS (deny by default):** read+exec `/usr /lib /lib64 /bin /sbin /etc /opt` (+ `/nix/store`), the runtime binary (workerd / node) and node's install prefix; a short list of global `/proc` and `/sys` files (cpuinfo, meminfo, cgroup limits), and the process's **own** `/proc/<pid>` entries except `net/` and `task/`; `/dev/null|zero|full|urandom|random`. Read-write (incl. refer and, at ABI ≥ 9, unix-socket connect) **only** the instance dir and its own DO-storage / Node data dir. So the home dir (`~/.twd`, API key files), hostd/mgmt state and run dirs, other tenants' dirs, node keys, `/tmp` and other processes' `/proc` (environ, cmdline, `/proc/net/unix`) are all `EACCES`.
  - **Landlock TCP (ABI ≥ 4):** bind/connect only the profile's ports. workerd: **none** (all its sockets are unix sockets in its own dir). Node: bind+connect its own app port, connect its egress proxy's port. Everything else, loopback included, is `EACCES` before a packet is sent.
  - **Scopes (ABI ≥ 6)** and **pathname unix-socket connects (ABI ≥ 9)** are requested too and apply automatically on newer kernels.
  - **`PR_SET_NO_NEW_PRIVS` + seccomp (TSYNC):** EPERM for ptrace, process_vm_readv/writev, pidfd_getfd, kcmp, kexec, (u)mount and the new mount API, pivot_root, chroot, bpf, perf_event_open, keyctl/add_key/request_key, module loading, unshare/setns, clone with namespace flags, open_by_handle_at, userfaultfd, fanotify, swap, reboot, syslog, `kill(-1)`; ENOSYS for clone3 and io_uring (glibc/libuv fall back). `socket()` only AF_UNIX, AF_INET/6 **TCP** (protocol 0/6: not MPTCP, which bypasses Landlock, nor SCTP), and NETLINK_ROUTE (not sock_diag, which lists unix socket paths); UDP/ICMP/raw/packet/vsock refused. Foreign syscall ABIs (32-bit compat, x32) are killed. **workerd/V8, Node 24 and Vercel Workflow all run under it** unchanged.
- **Unix sockets on Landlock ABI < 9 (what can and can't be restricted).** Landlock < 9 can't refuse `connect()` to a pathname unix socket. So:
  - twd's own servers refuse sandboxed peers themselves: `procsb.Guard` reads `SO_PEERCRED` and refuses a peer that has no_new_privs and **more seccomp filters than the server** (the launcher's filter is inherited by every descendant and can't be removed). Applied to **mgmt's admin socket**, **hostd's push socket** and twd-netd's sockets; each instance's `egress-obj/egress-wl/hostapi.sock` accepts a sandboxed peer only if it is that instance's own process or a descendant.
  - workerd's own `ingress.sock` (served by workerd, not twd) sits in an instance dir with an **unguessable name** (`i<n>-<64 random bits>`); a sandboxed process can't list the run dir, can't read `/proc/net/unix` or other processes' cmdlines, and can't open sock_diag, so it can't find another instance's socket.
- **hostd:** `-sandbox auto` (the default) is now `landlock` on Linux when the kernel has Landlock (`seatbelt` on macOS). Workloads report `Sandbox: "landlock"` and `SandboxProfile: …/sandbox.json`; `GET /v0/status` has the host's `Sandbox: "landlock (ABI 4)"`. `hostd.SandboxCommand(profile, cmd…)` runs a command under a workload's live profile on either OS (tests). `release.json` records `"sandbox":"landlock"` for linux tarballs.
- **No sandbox → no shared host.** If the process tier has no sandbox (Linux without Landlock, or `-sandbox none`), hostd refuses a second tenant's process-tier deployment with `sandbox_unavailable` unless `twd-hostd -insecure-shared-host` (`twd up --insecure-shared-host`); `twd tenant add` refuses the same way up front. The microVM tier is unaffected.
- **`twd doctor` `sandbox` check on Linux:** runs workerd under a generated Landlock profile launched by the `twd` binary itself, and checks that a file outside it and the home dir are unreadable and (ABI ≥ 4) that a TCP connect to 1.1.1.1 is refused; reports the ABI, the unix-socket mode, and while twd runs, `active: N/N` workloads under Landlock. Without Landlock it **warns loudly** (`NO PROCESS SANDBOX …`) with the fix; ABI < 4 warns that TCP isn't restricted.
- **Docs:** INSTALL.md isolation section (Linux is no longer "no sandbox"); docs/node-tier.md: on Linux with ABI ≥ 4 the egress proxy is **enforced**, closing the M9 "a malicious Node app can bypass HTTP_PROXY" hole there.

### Test results (measured 2026-11-08)
| Where | Target | Result |
|---|---|---|
| `twdl-sbx-u24` (sandbox on) | `make test` | **PASS** (incl. new `internal/procsb`, `TestMultiTenant` (d) under Landlock, `TestSharedHostWithoutSandbox`, `TestCLIMultiTenant` (d) under Landlock with `RequireSandbox`, `TestDoctor` `sandbox: ok`) |
| | `TWD_TEST_SANDBOX=auto go test -race ./cmd/... ./hostd/...` (every hostd test with the sandbox on) | **PASS** |
| | `make test-node` | **PASS**: `Sandbox` subtest now runs on Linux (`sandbox: landlock`), `TestNodeWorkflow` under Landlock |
| | `make test-sdk` | **PASS**, now under the host sandbox (checked: 8/8 SDK workloads `landlock`) |
| | `make e2e-multi` ×2 (tailnet 1 + tailnet-2, prefix `ttest-l2-<run>`) | **PASS** `c1b1e5`, `ac586c` (13/13 each; `e2e/real/results/l2-*.json`); 0 devices left in either tailnet; 0 `tskey-` in logs; **no policy written** (`TWD_E2E_NO_POLICY_WRITE`: read-only check, both tailnets already had the rules) |
| macOS (this Mac) | `make test`, `make lint`, `make test-sdk` (now under Seatbelt: 8/8), `make test-node` | **PASS** |

Attacks, all from inside a sandboxed workload or under its **live** profile (`procsb.ProbeCommand`: same rules, seccomp filter and no_new_privs, launched like the workload):

| Attack (tenant A's workerd / Node app) | Result (ABI 4) |
|---|---|
| list/read tenant B's state dir, node state, DO storage, instance dir, **API key file** | `EACCES` |
| read its **own** tenant's node keys, hostd's audit key, list `$HOME`, hostd's run dir, `/tmp`, `/proc` | `EACCES` |
| read B's `/proc/<pid>/environ` (the Node tier's caller key lives there), `/proc/self/net/unix` | `EACCES` |
| **mgmt's admin socket** (curl `--unix-socket … /v0/healthz`), hostd's push socket, B's `egress-wl/egress-obj/hostapi.sock` | connect completes (ABI < 9), the server closes it unanswered (`ECONNRESET`); unsandboxed control answers |
| **ptrace** B's workerd / a sibling, `process_vm_readv` | `EPERM` |
| Node app `net.connect` 1.1.1.1:80 / :443, a host-loopback listener | `EACCES` |
| TCP connect 1.1.1.1:443, bind 127.0.0.1:0, UDP to 1.1.1.1:53, `kill(-1, 0)` | `EACCES` / `EACCES` / `EPERM` / `EPERM` |
| own DO storage / own data dir (read, write, list); Node app's own port | allowed |
| serving requests (a–c, e, f of E2E-MULTI) and **egress via netd** (`EgressAttributed`: sdk, global fetch, `node:http`, undici; the echo's WhoIs is the deployment's node) | work under the sandbox |

### Gaps vs Seatbelt
- **Unix sockets that aren't twd's, on ABI < 9:** Landlock can't refuse the connect, so a sandboxed workload can reach other software's sockets **at a known path** (the D-Bus session bus, `docker.sock` if the user may use it, tailscaled's LocalAPI socket). Seatbelt also allows unix sockets outside its denied paths, but on macOS those mostly live under the denied home dir. Kernels with Landlock ABI 9 close this automatically (the profile already requests it); measured only on ABI 4 here.
- **Signals and abstract unix sockets on ABI < 6:** a workload can signal a same-uid process by pid (not `kill(-1)`) and connect to an abstract socket whose name it knows (names aren't discoverable: `/proc/net/unix` and sock_diag are blocked). Seatbelt allows signals too. ABI 6 scopes apply automatically.
- **The "sandboxed peer" test is a heuristic:** more seccomp filters than the server. A legitimate client that itself runs under extra seccomp filters (e.g. the `twd` CLI inside a container while mgmt runs outside) is refused by mgmt's and hostd's sockets.
- **Child processes** of a Node app can't read their own `/proc/self/*` (the rules name the launcher's pid). Not hit by any test (Workflow included); an app that forks helpers that need `/proc/self/maps` would see `EACCES`.
- System dirs (`/usr`, `/etc`, `/opt`) are readable, as with Seatbelt; `/tmp` is **not** (stricter than Seatbelt). Apps can't do their own DNS (UDP is refused), by design.
- **Not run:** amd64 (builds and vets; the amd64 seccomp file is untested), kernels with Landlock ABI ≥ 5, other distros, and the sandbox **from an installed linux release tarball** (L1's matrix covers that; nothing new is shipped: the launcher is twd-hostd itself). ABI 1–3 kernels (FS-only, TCP unrestricted) are handled by best-effort downgrade and a doctor warning, but not tested.
