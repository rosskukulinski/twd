# twd STATUS (living log)

Last update: 2026-11-07 23:55 PT. M0 and M1 by the first run; M2 (`twd-mgmt`) on branch `m2-mgmt`.

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
- Run `make test` in Lima `twd-dev` (Linux arm64).
