# twd STATUS (living log)

Last update: 2026-11-07 23:55 PT. M0 and M1 by the first run; M2 (`twd-mgmt`) on branch `m2-mgmt`.
Last update: 2026-11-08 00:10 PT. M0, M1 (main) and M3 (branch `m3-hostd`).
Last update: 2026-11-08 00:40 PT. M8 (microVM tier, Firecracker in Lima) on branch `m8-vm`: `make e2e-vm` PASS.
Last update: 2026-11-08 00:45 PT. M4 (`twd` CLI + integrated single-machine flow, E2E-MAC on this Mac) on branch `m4-cli`.
Last update: 2026-11-08 01:15 PT. M6 (merged with M5; release tarballs, `install.sh`, `twd doctor`, **E2E-MAC on `mbp`: PASS**) on branch `m6-release`.
Last update: 2026-11-08 01:10 PT. M5 (SDK conformance, `make test-sdk`) on branch `m5-sdk`: PASS on macOS and Lima.
Last update: 2026-11-08 01:20 PT. M9 (opt-in Node process tier + Vercel Workflow) on branch `m9-node`: `make test-node` PASS on macOS and Lima.
Last update: 2026-11-08 01:40 PT. M7 (E2E-MULTI: one host, tenant A on tailnet 1 + tenant B on tailnet-2) on branch `m7-multi`: `make e2e-multi` PASS (3 runs).
Last update: 2026-11-08 08:30 PT. L3: `make e2e-linux-reboot` PASS (clean reboot + power cut). Earlier, 08:15 PT, L3 (production-shaped microVM tier on Linux) on branch `l3-microvm`: `make e2e-vm` PASS (13 subtests), `make e2e-linux-vm` PASS from the installed release on fresh `twdl-kvm-u24`.
Last update: 2026-11-08 10:00 PT. Integration (Linux): L3 + L1 merged onto master (L2) on `int-linux`; redeploy-after-purge and lease_lost-retry fixes; full matrix green (macOS, real tailnets, twd-dev, Debian 12 / Ubuntu 24.04 arm64 with reboot + ephemeral / Ubuntu 24.04 amd64, e2e-linux-vm, e2e-linux-reboot); re-find delay diagnosed (WireGuard stale-session timeout, not DERP/netmap).
Last update: 2026-11-08 09:05 PT. L1: ephemeral nodes now re-mint on a rejected rejoin (netd + mgmt `/remint` + hostd); reboot survival and the ephemeral restart measured on Ubuntu 24.04 arm64; 5/5 supported distros PASS.
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
- The full v0 surface is served over a unix socket (0600 socket in a 0700 dir; connections whose peer uid, via `peercred`, isn't ours are closed). **RT-03 (fixed, see "RT fixes"):** peercred alone was the only check, so any same-uid tenant process was admin; every request now also needs the per-host token (`Twd-Host-Token`).
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
- ~~**Spawn caps are not enforced on the receiving side.** They are checked by mgmt but not yet written into the node cert (`CapMap ∩ lim`).~~ **Fixed in "RT fixes" (RT-01):** mgmt signs a node certificate at ready and the receiving netd computes `CapMap ∩ lim`. `actingFor` assertions and `forward()` act-chain re-signing are not implemented, and `ctx.tailnet.connect()` is a typed `plan_feature_missing`.
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
  - **(d)** `spawn` creates a `LookupAgent` with its own node and IP, `tag:twd-agent-support-sub`, and caps `{"example.com/cap/mcp":{"tools":["lookup_order"]}}` (narrower than the parent's `[lookup_order, refund]`, checked by mgmt against tailnet 1's real policy). The child answers on its own node. **Correction (red team RT-01):** at this point the narrower caps were only *recorded*; receivers still saw the child's full class-tag caps, so this check did not prove least privilege. Since "RT fixes" check (d) also asserts the denial (the child's `refund` → `cap_missing` while the parent's is admitted);
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
- **`make e2e-mbp`: PASS in 3 of 3 runs** (`e2e/real/results/m6-mbp-*.json`: `34cd1d`, `3feb1e`, `402770` on commit `e67b9d1`, clean tree; plus the final-commit run below). All 17 checks pass: copy (0600), install, doctor, up (interactive refused with `tailnet_required`), status, preflight refuses hello, deploy agent+mcp, **(a)** verified caller (the client's nodeId + `tag:ttest`, via the workload and the object's own node), **(b)** echo's WhoIs of the agent's fetch = the agent object's own node on mbp, **(c)** `lookup_order` allowed / `refund` → `cap_missing`, **(d)** spawn → `LookupAgent` with its own node, `tag:twd-agent-support-sub`, caps `[lookup_order]` (recorded, not yet enforced at the receiver at the time: RT-01), **(e)** `kill --cascade` 2 killed, 2/2 pushed, **(f)** audit chain `ttest-m6-<run>-client → …-supportagent-… → …-lookupagent-…` verified, `--human` works, cross_machine, system_tailscaled_not_used, down.
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
2. **0700 is not isolation on the process tier.** Every daemon runs as one uid, so before Seatbelt any tenant's workerd (after an RCE) could have read every tenant's node keys, mgmt's custody key, and the API key files, and could have used **mgmt's unix-socket admin API** (kill any tenant's objects). The Seatbelt profile closes all of these on macOS. **Red team RT-03:** off macOS nothing did, and the socket itself trusted same-uid peers; fixed independently of any sandbox by the per-host token ("RT fixes").
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
- **Node apps don't run in the microVM yet.** The guest image has no Node.js, and `runtime: node` + `tier: microvm` is rejected. That needs Node in `guest/build-image.sh` plus a guestd process mode; the gVisor gateway would then make egress enforcement real. Until then, a malicious Node app can bypass the proxy (raw sockets, its own dispatcher) and reach loopback, LAN and internet. It still can't use a tailnet identity or forge a caller (`docs/node-tier.md`). **Red team RT-08 / "RT fixes":** this is now surfaced as `egress unenforced` in `twd status`/`doctor`, Node-tier deploys are refused on multi-tenant hosts without `--insecure-shared-host`, and `@twd/node` `enforceEgress()` tunnels raw connects through the proxy (cooperatively).
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


## RT fixes: red-team findings RT-01…RT-08 (branch `rt-fixes`, 2026-11-08)

Fixes for `security/RED-TEAM.md`, independent of the Linux sandbox landing on `l2-landlock` (defence in depth; hostd's workload launch is untouched). The red team's regression tests in `security/attacks/` are un-skipped and run by `make test` (`./security/...`).

| Finding | Fix | Where | Test |
|---|---|---|---|
| **RT-01** spawn `lim` never bound to the child's identity (High) | ADR-010 node certificates: mgmt signs `{tenant, objectID, stableNodeID, tags, lim, parent, root, iat, exp}` with a per-tenant **OBO root key** (Ed25519 in custody) at `ready`; `GET /v0/certs/{stableNodeID}`; `GET /v0/tenants/{id}.oboPublicKey`. The receiving netd fetches/caches (60 s) the sender's certificate by WhoIs stable node ID, verifies it against the pinned tenant key, and sets `caller.capMap = CapMap ∩ lim` (`internal/caps`) before tenant code sees it; a lookup failure **fails closed** (caps withheld, `capsWithheld: true`, logged). The Node tier's `x-twd-caller` carries `callerObjectId` and `lim`. Agent template: `/delegate` spawns a same-tag child with `lim=[lookup_order]` and has it call the MCP server. | `internal/nodecert`, `internal/caps`, `mgmt/obo.go`, `netd/ingress.go` (`CertSource`), `hostd/certs.go`, `hostd/node.go`, `js/packages/tailnet/src/caller.ts` (`who.twd`, `who.capsWithheld`) | `security/attacks/rt01_spawn_lim_test.go`; `mgmt.TestNodeCertificates`; `hostd.TestSpawnLimEnforcedAtReceiver` (child refund → `cap_missing`, parent admitted, fail-closed when the cert fetch fails); e2e check (d) in `e2e/mac` and `e2e/multi` (`d_spawn` / `d_lim_enforced_{A,B}`) now asserts the **denial** |
| **RT-02** tenant-forgeable audit (High) | `ctx.tailnet.audit()` only produces `kind: tenant_note, source: tenant`; hostd assigns object, trace ID and `actingFor`; the tenant's claims survive only in `detail` (`claimedKind`, `parentHint`). All host records carry `source` in the hashed body; mgmt `POST /v0/audit` refuses a tenant source with any other kind (and `source: mgmt`); `GET /v0/audit` reports `source`; `twd audit` prints `~tenant_note … tenant note (unverified, written by tenant code)`. | `hostd/http.go` (`TenantAuditRecord`), `hostd/audit.go`, `mgmt/service.go` (`checkAuditSource`, `auditSource`), `cmd/twd/ops.go` | `rt02_audit_forgery_test.go`; `mgmt.TestAuditIngestProvenance`; `hostd.TestTenantAuditIsOnlyANote` |
| **RT-03** mgmt admin socket authorizes by same-uid peercred only (Critical) | Per-host bearer token: `twd-mgmt -host-token-file` mints a 256-bit token into a 0600 file under hostd's state dir (`<state>/hostd/mgmt.token`), rotated on every `twd up`; **every** request on the unix socket (host and admin endpoints, `/v0/healthz` too) needs `Twd-Host-Token`; hostd (`-mgmt-token-file`, re-read on change) and the CLI read the same file. Peercred stays as an additional check. No token configured → 503, fail closed. `Twd-Host-ID` keeps its meaning. | `mgmt/server.go` (`LocalHandler(h, token)`, `LoadOrCreateHostToken`), `cmd/twd-mgmt`, `hostd/mgmtapi/client.go`, `cmd/twd/{state,up}.go`, `docs/contracts/mgmt-api.md` | `rt03_mgmt_socket_test.go`; `mgmt.TestHostTokenOnUnixSocket`; `cmd/twd` `TestCLIEndToEnd`/`TestCLIMultiTenant` (the whole stack with the token) |
| **RT-08** Node tier: raw sockets bypass the egress proxy (High) | (a) `@twd/node` `enforceEgress()` (also from `installGlobalDispatcher()`; the `node-workflow` template calls it): raw `net`/`tls` connects become CONNECT tunnels through the proxy (netd's ADR-019 guard applies), direct DNS/UDP/other unix sockets fail with `forbidden_destination`. Cooperative, documented as such. (b) hostd reports `Egress: unenforced` for Node-tier workloads unless the sandbox confines TCP (Landlock ABI ≥ 4, L2) or a microVM; `twd status` shows it, `twd doctor` warns (`node_egress`); a Node-tier deploy on a host serving other tenants fails `egress_unenforced` unless `twd up --insecure-shared-host`. `docs/node-tier.md` says plainly what the boundary is. | `js/packages/node/src/index.ts`, `hostd/http.go` (`EgressEnforcement`), `hostd/workload.go`, `cmd/twd/{up,doctor}.go` | `@twd/node` `test/egress.test.js` (tunnel, typed refusal, loopback/unix/DNS/UDP refused); `e2e/node` `TestNodeTierSharedHostRefused` |
| **RT-04** one egress MAC key per VM (Medium) | Per-object keys derived from the instance root (`HMAC(root, "twd-egress-object-key-v1\n"+class+"\n"+name)`) in `platform.js` and `gen.ObjectMACKey`; hostd verifies a `(class, name)` MAC only under that object's key. **Partial:** the root key still lives in the guest/platform worker, so a full guest or platform compromise still derives sibling keys; `isolation: "object"` VM placement remains the real fix. | `hostd/gen/mac.go`, `hostd/gen/js/platform.js` | `gen.TestEgressMACPerObjectAndReplay`, `gen_test` (workerd ↔ hostd derivation) |
| **RT-05** no replay cache on the egress MAC / caller header (Medium) | hostd keeps a per-instance nonce cache for the MAC skew window (`VerifyEgressOnce`); `x-twd-caller` carries a `nonce` and `@twd/node` refuses a replayed signature within the 60 s window. OBO assertions (RFC §4.6) are still not implemented, so there is nothing to replay there; the node certificate is fetched server-side, not presented by the sender. | `hostd/gen/mac.go`, `hostd/http.go`, `hostd/node.go`, `@twd/node` | `gen.TestEgressMACPerObjectAndReplay`; `@twd/node` caller test "replayed" |
| **RT-06** `--policy-apply` widening (Low/Med) | `internal/policy.Check`: tagOwners keys must be `tag:twd*`, owned only by `tag:twd`/`autogroup:admin`/twd tags; grants need non-empty `src` of twd tags (never `*`, `autogroup:*`, groups, users) and `dst` of tags/`svc:` (never `*`/`autogroup:*`), fields only `src/dst/ip/app`. `VerifyAdditive` recomputes the diff after the merge and refuses (printing it) if anything existing was removed or modified or anything beyond the snippet was added. The e2e harness passes `Options{AllowTags: tag:ttest}` for its own test tag. | `internal/policy/policy.go` | `TestHostileSnippetsRefused` (20 hostile `twd.jsonc`-derived snippets), `TestVerifyAdditiveRefusesModification`, `FuzzMergeAdditive` (seeds run in `make test`; 2.4 M execs clean in 20 s) |
| **RT-07** kill → netmap removal 3–5 s (Low/Med) | Control-plane property, left as is. Kill **tier 1 (RST all flows + tombstone answering `object_killed`)** is the fast path and the measured headline (<1 ms for callers with a flow, ~1 ms for new connections during the tombstone window); tier 2 (device delete) is deferred by `--device-delete-delay` for exactly this reason. | — | existing `KillFailsFast`, e2e (e) |

Also in this branch: `twd up --allow-sibling-egress` / `twd-hostd -allow-sibling-egress` (objects may reach other objects on the same host over the tailnet; off by default per ADR-019 rule 2) so a single-host e2e can exercise receiver-side authorization between two of its own objects; `make test-go` includes `./security/...`.

### Verification (this Mac, 2026-11-08)
| Where | Target | Result |
|---|---|---|
| macOS (this Mac) | `make lint` | **PASS** (gofmt, vet incl. every e2e tag, check-secrets) |
| | `make test` (Go `-race`: netd, internal incl. new `caps`/`nodecert`/`policy` hostile+fuzz seeds, cmd, mgmt, hostd, gen, **`security/attacks` un-skipped**; JS 5+9+7) | **PASS** |
| | `make test-node` (incl. new `TestNodeTierSharedHostRefused`; `@twd/node` 9/9 incl. egress enforcement + replay) | **PASS** |
| | `make test-sdk` | **PASS** |
| | `make e2e-mac` (tailnet 1, prefix `ttest-rtf-b1b5c4`, `e2e/real/results/m4-20261108-092854-b1b5c4.json`) | **PASS** 53 s. (d): LookupAgent child as before **and** same-tag SupportAgent child with `lim=[lookup_order]` → MCP `refund` **`cap_missing`**, `lookup_order` ok, parent `refund` admitted (its tag is granted refund by the real policy); kill cascade 3/3 pushed, 410 `object_killed` ~11 ms. **No policy written**; 0 `tskey-` in the log; leftover check 0 devices/keys |
| | `make e2e-multi` (tailnet 1 + tailnet-2, prefix `ttest-rtf-1c3dc5`, `e2e/real/results/m7-20261108-092949-1c3dc5.json`) | **PASS** 55 s, 15/15 checks incl. `d_lim_enforced_A`/`_B` (refund `cap_missing`, parent admitted, per tenant) and `d_state_isolation` with the mgmt socket probe now carrying the host token (sandbox still denies it) and a token-less request answered **401**. No policy written on either tailnet; 0 `tskey-`; 0 leftovers |
| Linux / Lima | not run from this branch (the L2 worktree owns the VMs); nothing here changes how hostd launches workloads | — |

### RT fixes × Linux (merge of `int-linux`: L2 Landlock + L3 microVM/vmhelper + L1 release/systemd)
- **Node certificates and microVM senders.** The microVM tier's object nodes run in host netd (keys never enter the guest), so `serveIngress` → `applyCert` narrows a microVM sender's capMap exactly as for the process tier, and the guest-ingress path receives the already-narrowed `request.cf`. Cert fetches go through hostd's mgmt client (unix socket + host token), which works unchanged under the systemd user service.
- **Re-mint (L1).** `POST /v0/objects/{id}/remint` clears the object's certificate with its node IDs; the following `ready` issues a new certificate for the new stable node ID (the old one is unknown to `GET /v0/certs/` → receivers treat it as a non-object or, if cached, the cache expires in 60 s and a killed/deleted node has no flows anyway).
- **Host token and the service.** The supervisor starts mgmt with `-rotate-host-token`, so every mgmt start (`twd up`, `twd up --service` restart, a systemd/supervisor restart) mints a fresh token; hostd re-reads the 0600 file when it changes and the CLI per call, so a running hostd is never stranded (at most one heartbeat retried). `twd-vmhelper` never talks to mgmt; hostd's `GET /v0/volumes/key` uses the same token-bearing client.
- **RT-08 × L2.** `EgressEnforcement` reports `enforced` only under Landlock (TCP pinned to the proxy); Seatbelt is `unenforced` but counts as a process sandbox, so the Node-tier refusal (`egress_unenforced`) applies only to hosts with **no** sandbox serving other tenants; L2's `sharedHostCheck` (`sandbox_unavailable`) governs everything else. The `nodeprobe` fixture's `/net` sandbox probe uses the unpatched `net.connect` so L2's Landlock checks still measure the OS sandbox, not `enforceEgress()`.

**Verification after the `int-linux` merge (commits 06e1b5f…e3dcbf6, 2026-11-08):**

| Where | Target | Result |
|---|---|---|
| macOS | `make lint`, `make test`, `make test-node`, `make test-sdk` | **PASS** |
| macOS | `make e2e-mac` (`ttest-rtf-012eab`, `e2e/real/results/m4-20261108-093833-012eab.json`) | **PASS** 38 s; (d) child `refund` → `cap_missing`, `lookup_order` ok, parent admitted; no policy written; 0 `tskey-` |
| macOS | `make e2e-multi` (`TWD_E2E_NO_POLICY_WRITE=1`, `ttest-rtf-22c034`, `e2e/real/results/rtf-20261108-093912-22c034.json`) | **PASS** 43 s, 15/15 incl. `d_lim_enforced_{A,B}`; read-only policy check on both tailnets; 0 `tskey-` |
| Lima `twd-dev` (Ubuntu 24.04 arm64, Landlock ABI 4; fresh clone of `rt-fixes`) | `make test` | **PASS** (all packages incl. `security/attacks`, hostd 136 s; `TestDoctor` after restoring int-linux's Linux expectations) |
| | `make test-node` | **PASS**: `TestNodeTier` (Landlock checks via the raw-connect probe), `TestNodeWorkflow`, `TestNodeTierSharedHostRefused` (`Egress: enforced` under Landlock; the no-sandbox refusal and the `-insecure-shared-host` override) |
| | `make test-sdk` | **PASS** (89 s) |
| | `make e2e-vm` | **PASS**: TestSmoke, TestVolume, TestE2EVM (`e2e/vm/results/l3-20261108T174458Z.json`); microVM senders pass through the same `applyCert` path; no keys in the guest (29 needles) |

**Not done / honest limits.** RT-04 is partial (root MAC key still in the guest). The `@twd/node` enforcement is cooperative by construction. Same-host object→object calls still need `--allow-sibling-egress` (off by default) to go over the tailnet; the in-process policy-checked path of RFC §4.5 is not built. OBO per-request assertions (RFC §4.6) remain unimplemented.
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
- ~~**Unix sockets that aren't twd's, on ABI < 9**~~: closed in the follow-up below (workloads get no AF_UNIX sockets).
- ~~**Signals and abstract unix sockets on ABI < 6**~~: closed in the follow-up below (signals only to self/own group; no AF_UNIX sockets).
- **The "sandboxed peer" test is a heuristic:** more seccomp filters than the server. A legitimate client that itself runs under extra seccomp filters (e.g. the `twd` CLI inside a container while mgmt runs outside) is refused by mgmt's and hostd's sockets.
- **Child processes** of a Node app can't read their own `/proc/self/*` (the rules name the launcher's pid). Not hit by any test (Workflow included); an app that forks helpers that need `/proc/self/maps` would see `EACCES`.
- System dirs (`/usr`, `/etc`, `/opt`) are readable, as with Seatbelt; `/tmp` is **not** (stricter than Seatbelt). Apps can't do their own DNS (UDP is refused), by design.
- **Not run:** amd64 (builds and vets; the amd64 seccomp file is untested), kernels with Landlock ABI ≥ 5, other distros, and the sandbox **from an installed linux release tarball** (L1's matrix covers that; nothing new is shipped: the launcher is twd-hostd itself). ABI 1–3 kernels (FS-only, TCP unrestricted) are handled by best-effort downgrade and a doctor warning, but not tested.

### Follow-up: host unix sockets contained (lead review)
The gap in the first pass: on Landlock ABI < 9 a sandboxed workload could `connect()` to host unix sockets at known paths, such as `/var/run/tailscale/tailscaled.sock` (the host's own tailnet identity, BUILD-PLAN rule 1) or `/var/run/docker.sock` (root-equivalent). **Closed by taking unix sockets away from workloads entirely**, plus a shared-host refusal as defence in depth:
- **workerd needs no unix socket.** Its ingress listener is created by hostd and inherited as fd 3 (`workerd serve --socket-fd ingress=3`). Its three outbound services (`netd-egress`, `egress-wl`, `hostd`) are **hostd loopback TCP listeners** (`127.0.0.1:<ephemeral>`). Landlock pins workerd's connects to exactly those 3 ports. Each request carries a per-instance 256-bit token, injected by workerd's config (`injectRequestHeaders`, in the instance dir, unreadable to others); hostd checks the token in constant time and strips it. So other local processes, which can't connect under Landlock anyway, also can't use the ports on an ABI < 4 kernel. Status shows `LoopbackPorts`.
- **seccomp `NoUnixSockets`** (workerd, and Node apps in the default TCP listen mode): `socket(AF_UNIX, …)` → EPERM. `socketpair(AF_UNIX)` is allowed for STREAM and SEQPACKET only: those pairs are connected and can't be re-pointed. A DGRAM pair could be re-pointed at a host socket, so it is EPERM. workerd, Node 24, Vercel Workflow and the SDK apps all run unchanged. The Node app in `listen: "unix"` mode still creates its own unix socket, so that mode keeps AF_UNIX (gap below).
- **Signals (any ABI):** `kill` only to pid 0, self or −self (hostd now starts workerd in its own process group too, like Node). `tgkill`/`rt_sigqueueinfo`/`rt_tgsigqueueinfo` only to self. `tkill` and `pidfd_send_signal` are EPERM. A workload can no longer signal hostd, mgmt or another tenant's processes on ABI < 6.
- **Defence in depth (Linux):** `procsb.RiskyHostSockets` lists risky sockets this user can connect to and risky group memberships. It checks write permission via `access(W_OK)` and never connects. The sockets covered are tailscaled's, docker, containerd, podman, lxd/incus and rootless docker/podman under `$XDG_RUNTIME_DIR`. The groups are docker, podman, lxd and incus(-admin). If any is found:
  - hostd refuses a second tenant's process-tier deployment (`sandbox_unavailable`, naming them);
  - `twd tenant add` refuses up front;
  - `twd doctor` warns under the new `host_sockets` check, with the fix "run twd as a dedicated user that can't reach those sockets and isn't in those groups";
  - all three accept `twd up --insecure-shared-host` as the override.
- **Doctor's `sandbox` probe** now also checks that its profile can't open a unix socket.

**Tests, with fake listeners at the real paths.** In `twdl-sbx-u24`, sudo started a root-owned listener on each of `/var/run/tailscale/tailscaled.sock` and `/var/run/docker.sock` (mode 0666; it answers `HTTP 200 fake`). The VM has no real tailscaled or docker, and neither was touched. The listeners were removed afterwards.

| Check (listeners present) | Result |
|---|---|
| control: curl from the user, unsandboxed | reaches both (`fake`) |
| `procsb` `TestSandboxProbes`, under a workload profile | both sockets: **EPERM**; temp and guarded sockets EPERM; socketpair stream/seqpacket ok, dgram EPERM; `kill(self/0)` ok; kill of a sibling, of the parent test, of pid 1, and `kill(-1)`: EPERM; tgkill of a sibling: EPERM |
| hostd `TestLandlockHostSockets`: a **serving workerd**'s live profile | tailscaled.sock, docker.sock, hostd's push socket, a temp socket: **EPERM**; profile has `noUnixSockets` |
| `make test-node`: a **Node app**'s own `net.connect({path})` | tailscaled.sock, docker.sock, both hostds' push sockets: **EPERM**; TCP as before; Workflow passes |
| `make test-sdk` | PASS (8/8 SDK workloads under Landlock) |
| `twd doctor` | `sandbox ok`; `host_sockets warn` naming both sockets |
| `TestCLIMultiTenant` / `make e2e-multi` | `twd tenant add` refused: `sandbox_unavailable … can reach /var/run/tailscale/tailscaled.sock, /var/run/docker.sock` (expected: defence in depth); the e2e run still cleaned up to 0 devices |

**After the listeners were removed:**
- **`make test` PASS.** It includes `TestMultiTenant`, whose (d) now also shows A → B's 3 loopback service ports refused (`EACCES`) and B's ports refusing a token-less unsandboxed request. It also includes `TestSharedHostRiskySockets`.
- **`make e2e-multi` ×2 PASS** (`63a580`, `41967a`): 0 devices left, no policy written, 0 `tskey-`.
- **macOS:** `make test`, `make lint`, `make test-sdk` and `make test-node` PASS.

**Remaining gaps after the follow-up:**
- A Node app in `listen: "unix"` mode keeps AF_UNIX, so on ABI < 9 it can reach host sockets by path. The `host_sockets` refusal still protects shared hosts.
- **A Node app's child processes:**
  - they can't signal their own pid, because the filter names the app's pid, so `raise()`/`abort()` in a child fails;
  - the app can't signal its children by pid; `kill(0)` or the process group works.
  - No test hit this.
- ~~`host_sockets` makes `--insecure-shared-host` necessary on any Linux host where tailscaled is installed~~: resolved in follow-up 2 (exempt while every workload is denied AF_UNIX).

### Follow-up 2: tailscaled.sock exempted from the shared-host refusal when the sandbox makes it moot (lead decision)
- **Rule (Linux):** `procsb.ScanHostSockets` now separates the system tailscaled's socket from the *hard* cases (docker/containerd/podman/lxd/incus sockets, membership of their groups). Hard cases refuse a second tenant regardless (root-equivalent for twd-hostd itself; a dedicated user is the fix). `tailscaled.sock` refuses **only** if some workload could reach it: the host's sandbox isn't Landlock+seccomp (`twd-hostd` status `Sandbox`), or a Node app listens on a unix socket (`listen: "unix"` keeps AF_UNIX; `NodeInfo.Listen` is now in status). Applied consistently in hostd's `sharedHostCheck` (which also considers the app being deployed), `twd tenant add` (asks hostd's status) and `twd doctor`.
- **Doctor:** `host_sockets` is `info` ("reachable by this user, blocked for workloads by seccomp (AF_UNIX denied)") when exempted, `warn` with the reason otherwise; `info` is a new, non-failing status.
- **Tests** (hostd, with a stubbed scan via `Config.HostSockets`): `TestSharedHostTailscaledSandboxed` (sandbox on → tenant B runs; its workerd's live profile → tailscaled path: **EPERM**), `TestSharedHostTailscaledNoSandbox` (refused), `TestSharedHostTailscaledUnixListen` (tenant A runs nodeprobe with `listen: "unix"` → B refused, "listens on a unix socket"), plus the earlier `WithoutSandbox` and `RiskySockets` (docker) cases.
- **Measured in the VM with a real fake `/var/run/tailscale/tailscaled.sock`** (sudo, 0666, answering; no docker.sock): `TestCLIMultiTenant` passed, so **`twd tenant add` succeeded** and all (d) checks held; `twd doctor` → `host_sockets info …blocked for workloads by seccomp`; `TestLandlockHostSockets` → a serving workerd's profile gets **EPERM** on that path. Listener removed afterwards.
- **Docs:** docs/node-tier.md documents the child-process signalling limitation (a child can't `raise()` or signal its own pid; the parent can't `child.kill()` by pid, the group works) and the `listen: "unix"` caveat.
- **Green after the change** (fake socket removed): VM `make test`, `make test-node`, `make test-sdk`, `make e2e-multi` (`dfc01c`: 0 devices left, no policy written, 0 `tskey-`); macOS `make test`, `make lint`, `make test-sdk`, `make test-node`.


## L3: production-shaped microVM tier on Linux (DONE, branch `l3-microvm`)
Persistent encrypted DO storage, the Firecracker jailer, an unprivileged twd with a small privileged helper, and the e2e from the installed release on a fresh nested-virt VM.

### What's built
- **Privilege model: `twd-vmhelper`** (`cmd/twd-vmhelper`, `hostd/vm/helper`). twd-hostd runs as the user; the helper is the only root part.
  - systemd system service `twd-vmhelper.service`, root with `CapabilityBoundingSet=` NET_ADMIN SYS_ADMIN SYS_CHROOT SETUID SETGID CHOWN FOWNER DAC_OVERRIDE DAC_READ_SEARCH MKNOD KILL SYS_RESOURCE IPC_LOCK, `NoNewPrivileges`, `Delegate=yes` (per-VM cgroups live in its subtree).
  - Socket `/run/twd/vmhelper.sock` (root-owned, SOCK_SEQPACKET); every connection is authorized by **SO_PEERCRED uid** against `/etc/twd/vmhelper.json`.
  - **It never opens a path a client names.** hostd passes open fds (SCM_RIGHTS): kernel, rootfs, config drive, console log, volume image; it gets back fds for its TAP and the vsock listener. The jailer and Firecracker come from root-owned copies in `/usr/local/libexec/twd` (never from the user-writable `~/.twd`). One connection = one VM: the helper tears everything down when the VM exits or hostd hangs up.
  - No `ProtectSystem=`/`PrivateTmp=`: a private mount namespace can't bind-mount fds from the host namespace (measured: EINVAL).
  - As root without the service (tests, `make e2e-vm`), hostd runs the same helper code in-process.
- **Jailer** (Firecracker v1.17): per VM a chroot (`/var/lib/twd-vm/jail/firecracker/<id>/root`: kernel and rootfs bind-mounted read-only, config drive copied, the volume's device node, the vsock socket, `api.sock`), a dedicated uid/gid (3900000000+slot), its own netns (`fc0` TAP owned by the jail uid), cgroup v2 `cpu.max` = vCPUs and `memory.max` = guest RAM (when not on huge pages) + 256 MiB, and Firecracker's default seccomp filter (checked: `/proc/<pid>/status` `Seccomp: 2`, uid, cgroup).
- **Persistent DO storage**, per (tenant, workload): `~/.twd/hostd/tenants/<t>/workloads/<w>/do.img` (sparse, 1 GiB default), **LUKS2 opened with dm-crypt on the host** by the helper (loop over the passed fd, key on cryptsetup's stdin). The guest gets plaintext ext4 on `/dev/vdc` (Firecracker `cache_type: Writeback`, so guest fsyncs reach the host), mounted at workerd's DO dir. **The key never enters the guest.**
  - Key: a 256-bit DEK created once in **mgmt custody** (`twd/<tenant>/volumes/<workload>/dek`, sealed by the custody key), released by `GET /v0/volumes/key` only to the namespace lease holder at the current epoch (ADR-011), audited (`volume_key`). fakemgmt implements it too.
  - **Fencing:** a volume is attached to one VM at a time (in-memory by dev/inode + an exclusive flock held while attached → `volume_busy`), and never at an epoch older than the newest it was attached at (`/var/lib/twd-vm/volumes.json`, keyed by uid + LUKS UUID → `volume_fenced`). A wrong key → `volume_key`.
  - **Blue/green → serial for microVM workloads** (like the Node tier): requests wait at the gate; the old VM drains, its guest stops workerd, syncs and unmounts the volume (`shutdown` op), the helper releases it; then the new VM attaches it.
- **`sudo twd host setup --microvm`** (one time): root-owned binaries, helper config (the user's uid), the unit (enabled), adds the user to `kvm`, reserves huge pages (`/etc/sysctl.d/60-twd-microvm.conf`; default 4 VMs' worth, ≤ ¼ RAM). **`--undo`** removes all of it and restores `nr_hugepages` and `kvm` membership (recorded in `/etc/twd/setup.json`). Without setup, `tier: microvm` falls back to the process tier; `twd doctor` has a `microvm` check (Linux) that says why and how to fix it; `twd up` prints `tiers: process, microvm (…)`.
- **`twd host vmcheck <workload> [--keyscan]`** (hostd `POST /v0/vm/check` on its user-only socket): guest memory, DO volume fsync latency, and the no-keys scan (rootfs, config drive, guest files, full RAM snapshot taken by the helper) for `tskey-`, every node/machine private key in hostd's state, and the volume key; positive control = the instance MAC key in RAM. Output has needle names and counts only.
- **Release:** linux tarballs carry `bin/twd-vmhelper`, and with `make vm-assets` (in Lima) `share/twd/vm/{vmlinux, rootfs.ext4, manifest.json, firecracker, jailer}`. twd-hostd finds them next to itself. Microvm workloads' `twd logs` is the guest console.

### Test results
**`make e2e-vm`** (Lima `twd-dev`, root → in-process helper; fresh clone): **PASS**, TestSmoke + TestVolume + TestE2EVM 13/13 (`e2e/vm/results/l3-20261108T155228Z.json`). New: StorageWrite, Jailed, VolumeNeverDoubleMounted (second VM on probe's volume → `volume_busy`), NoKeysInGuest now with the volume key (29 needles, none in 3 disks / 74 files / 512 MiB RAM; the volume image doesn't contain the DO's plaintext), KillVMFailsFast now checks KV + SQLite **and a write acknowledged right before SIGKILL** survive, RedeployKeepsStorage. TestVolume: data across restart, `volume_busy`, `volume_fenced` (epoch 1 after 2), `volume_key`, key absent from RAM + image. The same two backend tests also pass **as the normal user through the systemd helper** in twd-dev.

**`make e2e-linux-vm`: PASS** (run `47c606`, `e2e/real/results/l3-linuxvm-20261108-080743-47c606.json`, release `0.1.0-…`): fresh `twdl-kvm-u24` (Ubuntu 24.04 arm64, vz, nested virt, 6 CPU / 10 GiB / 40 GiB, nothing mounted, no provisioning; tarball and key file via `limactl copy`). install.sh (0.7 s, no sudo) → doctor (`microvm: warn … not installed`, fix shown) → `sudo twd host setup --microvm` (1.0 s) → doctor `microvm: ok` → `twd up` (0.62 s) → agent + mcp with `"tier": "microvm"` → **checks (a)–(f) PASS** from this Mac (tsnet client + echo, `ttest-l3-<run>`) → both workloads jailed (uid, seccomp mode 2 checked via procfs) → storage across a **VMM SIGKILL** (30 SQLite writes, incl. the last acked one) and a **redeploy** → **no keys in either guest** (22/24 needles incl. volume key; none in rootfs, config drive, 70/49 guest files, 512 MiB RAM; MAC-key control found) → `twd down` → `sudo twd host setup --undo` (nothing left; kvm membership and huge pages restored; doctor back to warn). Cleanup: 6 test devices deleted, 0 left; 4 twd devices/keys, 0 left after down; VM clean; key file removed; **VM stopped, not deleted**. An earlier run (`bbc005`) failed only at `twd logs agent` (no host-side workerd.log for microVMs: fixed, logs = guest console) and one at `twd host vmcheck` arg parsing (fixed).

**Measurements** (nested virt on an M5 Pro; fresh VM unless noted):

| Metric | Value |
|---|---|
| InstanceStart → workerd ready (agent / mcp) | 744 / 703 ms (twd-dev hermetic: 557–666 ms) |
| Whole boot (helper: netns, jail, volume attach, jailer, API; guest) | 861 / 811 ms |
| Volume attach (loop + LUKS2 open; first boot also formats) | 93–95 ms first boot, 15–20 ms after |
| **RSS per VM** (Firecracker VmRSS + hugetlb guest RAM) | agent 175 → 193 MiB, mcp 159 → 171 MiB (hermetic hello/probe 118/150 MiB); guest workerd RSS 116 / 98 MiB |
| **Request latency** p50 / p95, this Mac → VM over tailnet 1 | MCP tools/list 1.49 / 3.34 ms; agent request with SQLite insert + setState 4.40 / 5.16 ms |
| Hermetic (twd-dev, tsnet) | object in VM 0.53 / 0.80 ms vs process 0.38 / 0.44; egress 1.43 / 1.80 vs 0.90 / 1.41 |
| **DO write latency** (hermetic, per request over tsnet) | KV put 0.59 / 2.80 ms, SQL insert 3.22 / 4.40 ms, KV get 0.55 / 0.87 ms |
| **Volume 4 KiB write + fdatasync in the guest** (virtio-blk → dm-crypt → image) | write p50 10 µs; fsync p50 2.9–3.2 ms, p95 3.8–4.4 ms (twd-dev 2.3–2.5 / 2.5–2.7 ms) |
| **Restart → serving** after VMM SIGKILL | 1.28 s (fresh VM, via tailnet); 0.94 s hermetic |
| Redeploy → serving (serial: drain, guest unmount, release, new VM, attach) | 1.74 s from `twd deploy` (CLI 1.57 s); hermetic 0.82 s, guest unmount + release ~110 ms |
| `twd host setup --microvm` / install.sh | 1.0 s / 0.7 s |

**`make e2e-linux-reboot`: PASS** (run `4920c5`, `e2e/real/results/l3-reboot-20261108-082536-4920c5.json`; same fresh `twdl-kvm-u24`, non-ephemeral nodes `twd-l3r-<run>-*`). An agent object `boot1` in a jailed microVM writes SQLite (11 calls); then the whole VM reboots **twice**: (1) clean `limactl stop` + start, (2) `limactl stop --force` (a power cut) + start. After each: `twd-vmhelper` is active via systemd, `twd up` again as the user, and the object **rejoins from saved node state** (same stable node ID `n4sx…` and IP, the run's device list unchanged: no new device), **all SQLite writes are present** (12, then 13), and the volume **re-opens** (dm-crypt `type: LUKS2`, helper log `formatted=false`) with the custody key. The main `make e2e-linux-vm` was re-run after these fixes: PASS (`5b1456`).

| Reboot timing | clean | power cut |
|---|---|---|
| kernel boot → twd-vmhelper active (systemd, VM clock) | 3.98 s | 4.63 s |
| kernel boot → ssh usable / `limactl start` returns | 5.2 s / 6.8 s | 6.3 s / 7.8 s |
| `twd up` (mgmt + hostd ready) | 0.59 s | 0.61 s |
| microVM InstanceStart → ready / volume attach | 0.80 s / 22 ms | 0.82 s / 22 ms |
| `twd up` → the object serving the Mac client over tailnet 1 | 17.4 s | 12.8 s |

The object-serving time is dominated by the client re-learning the rejoined node over the tailnet (netmap + path), not by twd: the workload VM is ready ~1 s after `twd up` (not investigated further).

Fixed on the way (found by the reboot test):
- **Serial deploys didn't rejoin saved objects.** The rejoin of objects from `objects.json` ran only on the blue/green path; microVM (and Node-tier) workloads deploy serially, so after a restart their objects never came back. `rejoinObjects` now runs on both.
- **Stale `supervisor.pid` after a reboot.** Boots are deterministic enough that the old supervisor's pid was the new `twd up` process: `already_running`. A pid now counts only if that process started before the pid file was written (Linux `/proc/<pid>/stat`).

### Findings
1. **Fd hygiene matters across jails.** `dup()` without CLOEXEC let a later VM's jailer inherit another VM's volume fd, holding its flock: the restarted VM got `volume_busy`. All dups are `F_DUPFD_CLOEXEC` and received SCM_RIGHTS fds are marked CLOEXEC.
2. **Acknowledged DO writes survive a VMM SIGKILL** (workerd commits before answering; `Writeback` cache turns guest flushes into host fsyncs). A plain file written through a workerd disk service without fsync did **not** survive (ext4 delayed allocation): durability is the app runtime's fsync, which workerd's DO storage does.
3. The jailer's pivot_root keeps bind mounts made beforehand in the host namespace; `/proc/<pid>/root` of the jailed VMM isn't readable by the user (only by root).
4. Huge pages are still decisive (STATUS M8); setup reserves them persistently.

### Known gaps (L3)
- After a reboot `twd up` is manual here (L1 adds `twd up --service`); the 12–17 s until a tailnet client reaches a rejoined object node is not twd-side.
- **Snapshot restore** (warm start) not built; boot is ~0.6–0.75 s to workerd ready.
- Serial blue/green means ~0.8–1.7 s of held requests on redeploy; no hot handover of the volume.
- The volume never shrinks/grows (fixed sparse size, `-vm-volume-mib`); no online backup (ADR-012's async backup is not built).
- The helper runs as root with a bounded capability set, not as a capability-only non-root binary; jailer + cryptsetup + mounts need CAP_SYS_ADMIN anyway. No `ProtectSystem` (see above).
- `kvm` group membership is added (as specified) but unused by twd: the helper opens `/dev/kvm` for the jail.
- amd64 microVM assets aren't built (L1 adds amd64 releases; the tier needs an x86 kernel + image).
- The cryptsetup/losetup/mkfs dependency: present on Ubuntu/Debian cloud images; setup fails with an install hint otherwise.


## L1: Linux release, doctor, systemd user service, E2E-LINUX matrix (DONE, branch `l1-linux-release`)
- **Releases:** `make release` now builds `darwin-arm64`, `linux-arm64` and **`linux-amd64`**. The Go binaries are built with `CGO_ENABLED=0`; amd64's workerd comes from npm `@cloudflare/workerd-linux-64` (integrity-checked against the lockfile).
  - `scripts/check-arch.mjs` checks each binary's ELF/Mach-O machine against the target.
  - `scripts/check-pure-js.mjs` proves the shared vendored `node_modules` holds no `.node`/ELF/Mach-O/PE files and no `os`/`cpu` packages, so one tree serves every target. The only native package in the closure is `@rolldown/binding-*` (agents → `@rolldown/plugin-babel`, used only by `agents/dist/vite.js`). It was already skipped, and no workerd bundle needs it. `core-js-pure` has a postinstall banner, which is never run.
  - `install.sh` maps `x86_64` → amd64 and refuses musl or glibc < 2.35 with a clear message. On bash it suggests `~/.profile` instead of creating `~/.bash_profile`, which would shadow `~/.profile` on Debian and Ubuntu.
  - `RELEASE_VERSION` is now expanded once (`:=`); an edit made during `make release` had renamed the e2e tarball.
- **`twd doctor` on Linux** (`cmd/twd/doctor_linux.go`). The checks:
  - `glibc` runs before `workerd`. It uses `getconf`, with `ldd` as a fallback and musl detection. It also reads workerd's ELF `GLIBC_` floor.
  - `kernel`.
  - `landlock`: `landlock_create_ruleset(NULL,0,VERSION)`, decoding ENOSYS and EOPNOTSUPP.
  - `user_namespaces`: informational; reports AppArmor's restriction.
  - `kvm`: opens `/dev/kvm` read-write and calls `KVM_GET_API_VERSION`; names the group in the fix.
  - `firecracker`, and the jailer.
  - `cgroups_v2`, plus what is delegated to the user manager.
  - `systemd`: the user manager, linger, and the `twd.service` state.
  - `microvm_tier`: available or unavailable, with every reason.

  The system-tailscaled check now works on Linux too. It finds twd's socket inodes in `/proc/<pid>/fd` and matches the peers of `ss -x` connections bound at `*tailscale*`. macOS still uses lsof. A hermetic test (`TestTailscaledHoldersDetects`, Linux) proves it flags a connection to another process's `…/tailscaled.sock`.
- **`twd up --service` / `twd down --service`** (`cmd/twd/service.go`):
  - `--service` writes `~/.config/systemd/user/twd.service`, which runs the same `twd __supervise` (`KillMode=mixed`, `TimeoutStopSec=180`, `Restart=on-failure`), and enables and starts it.
  - A plain `twd down` stops it through systemctl; the unit stays enabled. `twd down --service` disables and removes it.
  - It prints `sudo loginctl enable-linger <user>` guidance and never runs sudo. On macOS it is refused (`service_unsupported`).
  - `twd up` without `--service` refuses while the unit is active.
- **`make e2e-linux`** (`e2e/linux`, `scripts/e2e-linux{,-one}.sh`):
  - Each row gets a **fresh** Lima VM `twdl-<distro>` from the official template (4 CPU / 6 GiB / 30 GiB, `--plain --mount-none`). The test asserts nothing in `/proc/mounts` is virtiofs, 9p or sshfs.
  - The tarball and the key file go in with `limactl copy` (the key 0600, in a 0700 dir). They are removed by `t.Cleanup` and by the shell trap, even on failure.
  - The run covers `twd doctor`, checks (a)–(f) under `twd up --service`, then `systemctl --user restart` (new MainPID; healthy; the same workload IP answers the Mac), doctor while running, `twd down --purge --service`, and zero leftovers by ID.
  - At most 2 VMs run at once. Each is `limactl stop`ped (kept) when its row ends.
  - cliflow gained `UpArgs`, `Persistent` and `Slow` (the amd64 timeout scale, 6×).
- **Matrix (2026-11-08, release `0.1.0-bb3a3296`). Every row PASSED, with zero leftover devices and keys:**
  - Ubuntu 24.04 arm64, Debian 12 arm64, Ubuntu 22.04 arm64, Fedora 45 arm64 (vz);
  - Ubuntu 24.04 **amd64** (qemu x86-64 emulation).
  - Sandbox: **none (L2 pending)** on every row. The microVM tier is **unavailable** on every row (no /dev/kvm, no firecracker, not root).

  Typical arm64 numbers:

  | step | time |
  |---|---|
  | install.sh | 0.4–0.8 s |
  | up → ready | 0.64–0.70 s |
  | deploy → ready | 1.5–2.8 s |
  | first object node | ~1.6 s |
  | kill CLI | ~66–72 ms |
  | killed request → 410 | ≤ 1.7 ms |
  | Mac → VM | direct after ~0.19 s; tools/list 1.9–3.5 ms direct vs 10.7–16.6 ms DERP-only |
  | service restart → workload reachable | 3.8–6.7 s |

  amd64 (emulated): install 9.1 s, up 3.4 s, deploy 4.7–5.5 s, kill 319 ms, restart → reachable 13.1 s. The full table is in docs/INSTALL.md ("Linux support matrix"); the raw JSON is in `e2e/real/results/l1-linux/`.
- **Unsupported-host rows** (`TestUnsupportedHost`):
  - **Debian 11** (glibc 2.31, kernel 5.10): install.sh is refused; `bin/twd doctor` gives `✗ glibc` (with distro and fix), `! kernel` and `! landlock` (ENOSYS).
  - **Alpine 3.24** (musl): install.sh and doctor both refuse it, naming `/lib/ld-musl-aarch64.so.1`.
- **Verification:** `make test` is green on macOS and in Lima `twd-dev` (a clone of this branch, at `ccce991`, which includes the re-mint fix). `make lint` is clean.

### L1 findings and gaps
- **workerd's real glibc floor is 2.27, not 2.35.** The pinned workerd's ELF imports `GLIBC_2.27` at most, on both arm64 and amd64, and `workerd --version` runs on Debian 11 (glibc 2.31). The 2.35 floor (install.sh refusal, doctor `fail`) is kept as policy: it is the oldest glibc tested. The lead may want to lower it to 2.31 after a full run on Debian 11 / RHEL 9 (2.34).
- **Fixed: ephemeral nodes across a restart (re-mint on rejected rejoin).** First seen on the first e2e-linux run (`--ephemeral`): after `systemctl --user restart` both workload nodes hung in `tsnet.Up` for 2 min and failed with `bootstrap_failed … context deadline exceeded`. Cause: control removes an ephemeral device that logs out, so its saved `tailscaled.state` lands the backend in `NeedsLogin`, and `tsnet.Up` waits for `Running` and never reacts to that. The fix, commit `7f2e608` + follow-up:
  - **netd:** during a rejoin, `start()` watches the IPN bus next to `tsnet.Up`; `NeedsLogin` that persists for 2 s is a rejection. netd drops `tailscaled.state`, asks the KeySource for a key with `KeyRequest.Remint`, and joins again as the **same hostname** (`ObjectInfo.Reminted`). Persistent nodes whose state is accepted rejoin exactly as before, with no key. Hermetic `TestRejoinRejectedRemints` (testcontrol never refuses a node key by itself, so the test forces `NodeKeyExpired` and lifts it in the remint callback).
  - **mgmt:** `POST /v0/objects/{id}/remint` (host role, lease-fenced): a live object goes back to `pending` with its node IDs cleared, the **previous device is deleted** (control lists a logged-out ephemeral device for a while; a 404 is fine), an audit record `remint` is written, and the object is re-queued; `credential` then releases the new key and `/ready` records the new node ID. `TestRemintAfterRejectedRejoin`. Rejected for other hosts and stale epochs (`lease_lost`), and for killed objects.
  - **hostd:** `keySource.AuthKey` calls `Remint` before `Credential` when netd asks with `Remint`; fakemgmt has the route; the log line says `reminted=true`.
  - **Real tailnet (e2e-linux, Ubuntu 24.04 arm64, `--ephemeral`, `--service`):** agent + spawned sub-agent, then `systemctl --user restart twd`. All 3 nodes (workload, agent, sub-agent) were rejected and re-minted; the sub-agent was ready again as the same hostname with a new node ID **4.1 s** after the restart and answering this Mac **9.5 s** after it (new IP). `twd down --purge --service` then left **zero** devices or keys (the old devices were deleted by `remint`, not by the test's cleanup).
- **Reboot survival (measured, Ubuntu 24.04 arm64):** `sudo loginctl enable-linger` inside the VM, then `limactl stop` (1.1 s) and `limactl start` from the Mac. From the start of boot: ssh up **6.6 s**, `twd.service` active **6.6 s** (new MainPID), `twd status` healthy **6.6 s**, the mcp workload answering this Mac **8.6 s** (run 3) / **17.1 s** (runs 4 and 5); both live objects rejoined from saved state with their old node IDs, **0 re-minted**. The `e2e-linux` reference row (`ubuntu24-arm64`) now runs both of these phases every time (`TWD_E2E_REBOOT`, `TWD_E2E_EPHEMERAL`); results in `e2e/real/results/l1-linux/` (run `819d63`).
- **Gap found while doing this:** after `twd down --purge`, deploying the same workload again on the **same state dir** is refused: `bootstrap_failed: the workload node for agent is killed` (mgmt keeps the killed workload-node object and the deploy path looks it up by name). Real users hit this with `twd down --purge && twd up && twd deploy`. The e2e's ephemeral phase uses a fresh state dir to get past it. Not L1's code; needs a fix in mgmt/hostd deploy (create a new workload-node object when the previous one is killed).
- The rejection is logged twice per node (netd and hostd both say "saved node state rejected by control"); cosmetic.
- No VM had a system tailscaled installed, so the e2e's "no twd process holds tailscaled's socket" result is a negative only. The positive control is the hermetic `TestTailscaledHoldersDetects`.
- amd64 ran only under qemu TCG emulation (no x86 hardware here), so its numbers are slow by construction (timeouts scaled 6×; measured values recorded unscaled).
- Lima: `debian-11` genericcloud doesn't boot under `vmType: vz` (ssh never came up in 10 min), so that row uses qemu. The template locator is now `template:X` (Lima 2 syntax).
- Integration notes for the lead:
  - **L2** owns the `sandbox` check (still `skip` on Linux), and should change the `landlock` check's "sandbox pending" wording once Landlock lands; `release.json`'s `"sandbox":"none"` for linux too.
  - **L3:** `microvm_tier` looks for `~/.twd/vm/{vmlinux,rootfs.ext4}` and for firecracker/jailer on PATH, in `~/.twd/bin` or `/usr/local/bin`. It reports "not root" until `twd host setup --microvm` exists.
  - The new files (`doctor_linux.go`, `doctor_host.go`, `doctor_other.go`, `service.go`) are separate, to keep conflicts small. `doctor.go`, `up.go` and `cliflow.go` have small hunks.
- **Matrix VMs (all stopped, kept, not deleted):** `twdl-ubuntu24-arm64`, `twdl-debian12-arm64`, `twdl-ubuntu22-arm64`, `twdl-fedora-arm64`, `twdl-ubuntu24-amd64`, `twdl-debian11-arm64`, `twdl-alpine-arm64`.


## Integration (Linux: L2+L3+L1) (branch `int-linux`, merges `13b73af` (L3) + `e676bdc` (L1), fixes `7e6c53a`, `94e878f`)
Merge order L2 (already master `8dd8124`) → `origin/l3-microvm` → `origin/l1-linux-release`, resolved semantically; then the purge bug fix and the doctor coherence work. The lead fast-forwards master.

### Conflicts and semantic decisions
- **Textual (both sides kept):** `hostd.Config` (`InsecureSharedHost` + `VMVolumeMiB`), `StatusView` (`Sandbox` + `Tiers`/`VMVia`), `WorkloadStatus` (sandbox fields + `VMRSSKB`), `twd-hostd` flags, `cmd/twd/main.go` (`doctor` help + `host`), `fakemgmt` routes (`/v0/volumes/key` + `/remint`), `mgmtapi` types (`VolumeKey` + `RemintRequest`), Makefile `.PHONY`, `release.json` (`"sandbox":"landlock"` **and** `"microvm"`), STATUS. `e2e/cliflow` got `Object`/`WaitObjectReady`/`PeerIP` from both L1 and L3 as a silent duplicate (git auto-merged two copies); one kept, L1's `Objects()` too.
- **L2 × L3, deploy:** `sharedHostCheck` (no sandbox / container sockets / tailscaled reachable → one tenant only) runs for **process-tier deployments only**; microVM workloads are isolated by the jailer + VM and were already excluded from the "another tenant runs here" count. Node and microVM workloads deploy serially (L3), the shared-host check precedes both.
- **L2 × L3, doctor:** the `sandbox` check counts process-tier workloads only (skips `Tier == "microvm"`, as L2 wrote it); L3's `microvm` check and L1's `microvm_tier` summary were merged into **one `microvm_tier` row** (L1's name, L3's logic: KVM, the release's guest assets in `share/twd/vm`, the `twd-vmhelper` service answering this uid, and while twd runs, that hostd offers the tier and every microVM workload is jailed; plus "no cgroups v2" from L1). L1's `firecracker` row also looks in `/usr/local/libexec/twd` (L3's root-owned copies) and the release's `share/twd/vm`. L1's `landlock` row no longer says "sandbox pending (L2)": ABI ≥ 4 = filesystem + TCP rules, 1–3 = filesystem only (TCP by seccomp alone), missing = warn pointing at the `sandbox` check. `TestDoctor` and `e2e/linuxvm` updated to `microvm_tier`.
- **L2 × L3, `twd up`:** the host line now says each tier's isolation from hostd's status: `tier: process [Landlock+seccomp, landlock (ABI 4)]` / `[Seatbelt, dev-grade isolation]` / `[NO PROCESS SANDBOX: see twd doctor]`, and `, microvm [Firecracker, jailed, via twd-vmhelper]` when the tier is on (L1's "no per-tenant sandbox yet" text dropped).
- **L2 × L1:** `twd up --service` runs `twd __supervise` under the systemd **user** manager with no `NoNewPrivileges=`/`Protect*=`; Landlock + seccomp are applied by the sandbox launcher itself (`PR_SET_NO_NEW_PRIVS` on the workload only), so nothing conflicts. `twd doctor` runs as the same user the service runs as, so `host_sockets` (reachable sockets + group membership of *this user*) answers for the service context too. Measured below: `sandbox: ok` + `active N/N` in the e2e-linux rows under `--service`.
- **L1 × L3:** every object node, microVM tier included, is a host-side netd node (`t.reg.Create`), so the ephemeral re-mint path (netd rejoin → `KeyRequest.Remint` → hostd `keySource` → mgmt `/remint`) is tier-independent: a **VM restart never touches the node** (it lives in hostd, not the guest), and after a **host reboot** microVM objects rejoin or re-mint exactly like process-tier ones. `make e2e-linux-reboot` (persistent nodes: rejoin, same node ID) and the e2e-linux ephemeral phase (process tier: re-mint) are the measured cases; an ephemeral microVM object across a host reboot was not run separately.
- **INSTALL.md:** L1's general "Linux" section first, L3's microVM section after it; the "Isolation on Linux" box now describes Landlock + seccomp and the microVM tier (not "no sandbox yet"); the matrix caption says L1's rows ran before the sandbox landed and points at the rows below.

### Fix: redeploy after `twd down --purge` on the same state dir (L1's bug)
`twd down --purge && twd up && twd deploy` was refused with `bootstrap_failed: the workload node for agent is killed`. Cause: mgmt keeps killed objects as 410 tombstones (`CreateObject` → `killed`, `GetDeployment`'s endpoint reads the killed `_workload` object), object IDs are deterministic per (tenant, workload, class, name) and the row has a UNIQUE name, and the kill's **revocation** stays in `revocations`: even after the row is gone, a fresh hostd's first sync kills the recreated object ("revoked by mgmt"; seen as `tsnet: listener already open for tcp, :80` because the kill landed mid-join). Fix (mgmt + hostd + CLI):
- mgmt `POST /v0/tenants/{id}/purge` (admin): `FlushDeletes` (pending device deletes), then `Store.DeleteDeadObjects` removes the tenant's killed/failed objects **and their revocations** in one transaction; each removal is audited as **`purge`**; live objects are untouched; idempotent.
- hostd `POST /v0/purge` (user socket): forgets killed tombstones and rewrites `objects.json`; `create` also replaces a killed tombstone when mgmt hands out a new pending object of the same identity (defence in depth).
- `twd down --purge` (and `--ephemeral`'s implicit purge) calls both after the kills and reports `objectsPurged` ("N killed objects forgotten").
- **Tests:** mgmt `TestPurgeAfterKill` (killed workload node blocks create + endpoint `killed` → purge → create succeeds, revocation gone, live object kept, audit `create ×2, kill, purge`, 404 for an unknown tenant, second purge empty). CLI: `cliflow.RedeployAfterPurge` runs in `TestCLIEndToEnd` after the purging `twd down`: `twd up` + `twd deploy agent` on the **same state dir** → ready in ~1.0 s, then `twd down --purge` (2 killed, 2 forgotten); the leftover check covers the new node IDs. `e2e/linux`'s ephemeral phase now reuses the state dir instead of `state-eph` (it exercises the fix from the installed release on a real tailnet; see the row below).
- **Saved node state too.** A kill with a tombstone (`-kill-tombstone`, 10 s) removes the node's state dir only when the tombstone expires; `twd down` stops hostd first, so the dir survived and the redeployed identity found a dead node's state: control rejected it and the node **re-minted** (8.5 s join instead of 1.3 s; seen on the first Ubuntu 24.04 run). `netd.Registry.RemoveState` (refused for live objects) is called by hostd's purge for every forgotten object.
- **Second bug found by the same flow (fixed): a node whose mint was refused with `lease_lost` was never retried.** The fresh hostd's startup sync was still minting the *previous* deployment's workload node when the user's `twd deploy` ran `AcquireLease` (epoch +1); mgmt refused the credential at the stale epoch, hostd marked the node failed, and the new deployment's `ensure` had already seen it "live (joining)" and did not restart it: the workload stayed `active` with a dead entry node (`twd deploy`: `identity_pending … node minted` for 3 min). hostd now retries a failed join (1, 2, 4, 8, 16 s; 5 tries) while the workload is deployed, after refreshing the lease epoch from mgmt's assignments; mgmt's `lease_lost` error names the lease, holder, epoch, expiry and the caller's epoch. Test: hostd `TestNodeRetriesAfterLeaseLost` (fakemgmt `LeaseLostCredentials`: first credential call refused → ready after one retry, 2 calls). Hermetically the window is ~1 ms (fake TS API); on the real tailnet the mint takes ~0.6 s, which is why only the Linux row hit it.
- Finding (not fixed): a kill that lands while a node is joining leaves tsnet's `:80` listener open, so the join's retry fails with `listener already open`. Only reachable via the stale-revocation path, which purge now removes.

### Verification matrix
| Where | Target | Result |
|---|---|---|
| macOS (this Mac, 27.2) | `make test` | **PASS** on each commit incl. the final tree (`TestPurgeAfterKill`, `TestCLIEndToEnd` with `redeploy_after_purge`, `TestNodeRetriesAfterLeaseLost` after fixing a data race in its fakemgmt knob) |
| | `make lint` | **PASS** (final tree) |
| | `make test-sdk` | **PASS** (final tree) |
| | `make test-node` | **PASS** (final tree) |
| | `make test-pg` | **PASS** (`e676bdc`; the later commits don't touch the Postgres paths beyond `DeleteDeadObjects`, which runs on both stores through the shared SQL store and is covered by `TestPurgeAfterKill` on SQLite) |
| macOS, real tailnet | `make e2e-mac` | **PASS** run `m4-6469c4` (12/12; 6 test devices deleted, 0 left; 4 twd devices/keys, 0 left after down; policy not written) |
| | `make e2e-multi` | **PASS** run `m7-77c1e0` (13/13; tenant B on tailnet-2; 0 devices left in either; policy not written) |
| Release | `make release` | **PASS** ×3 (`e676bdc2`, `7e6c53aa`, final `94e878f6`): `twd-0.1.0-94e878f6-{darwin-arm64 (91 MB), linux-arm64 (99 MB), linux-amd64 (104 MB)}.tar.gz`; vendored node_modules pure JS (179 packages); 5/6/6 binaries arch-checked. `install.sh` ran from the amd64 tarball on Ubuntu 24.04 amd64 (row below: 8.8 s under qemu emulation) |
| Lima `twd-dev` (Ubuntu 24.04 arm64, copy of the tree, no mounts) | `make test` | **PASS** on `7e6c53a`'s tree (the first run failed only `TestDoctor`, which still expected L3's `microvm` row name; fixed). On the final tree, `go test -race ./hostd/ ./mgmt/ ./netd/ ./cmd/twd/`: hostd, mgmt, cmd/twd PASS; netd failed once while all four ran at the same time under `-race` (115 s, timing) and **PASS** alone (41 s) |
| | `make test-node` | **PASS** |
| | `make test-sdk` | **PASS** |
| | `make e2e-vm` | **PASS**: TestSmoke, TestVolume, TestE2EVM 13/13 (TierSelection … RedeployKeepsStorage) |
| Fresh Lima VMs, nothing mounted, tarball via `limactl copy` (reused stopped `twdl-*` VMs, cleaned by the harness; ≤ 2 running at a time; all stopped afterwards) | `make e2e-linux` **Debian 12 arm64** (`twdl-debian12-arm64`, kernel 6.1, glibc 2.36, **Landlock ABI 2**) | **PASS** run `l1-…-b11a55` (tarball `e676bdc2`): doctor 18 checks none failed (`sandbox` **warn**: ABI 2 = filesystem rules only, TCP by seccomp; `landlock ok`; `microvm_tier warn` no /dev/kvm); checks (a)–(f) under `twd up --service`; both workloads `process/landlock`; service restart → reachable 6.8 s; install 0.8 s, up 0.63 s, deploy→ready 1.5 s, kill CLI 89 ms, 410 in 1 ms, direct path after 177 ms; `twd down --purge --service`; 0 devices/keys left; VM clean |
| | `make e2e-linux` **Ubuntu 24.04 amd64** (`twdl-ubuntu24-amd64`, qemu TCG emulation; Landlock ABI 4) | **PASS** run `l1-…-cfc395` (tarball `7e6c53aa`): doctor none failed, `sandbox ok` (**amd64 seccomp filter proven on a real kernel**, L2's untested arch), both workloads `process/landlock`, `active 2/2`; install 8.8 s, up 3.2 s, deploy→ready 2.3–3.9 s, first object node 8.9 s, kill CLI 240 ms, 410 in 7 ms, restart → reachable 13.7 s; 0 devices/keys left; VM clean |
| | `make e2e-linux` **Ubuntu 24.04 arm64 with the reboot + ephemeral phases** (`twdl-ubuntu24-arm64`, kernel 6.8, Landlock ABI 4; `TWD_E2E_REBOOT=1 TWD_E2E_EPHEMERAL=1`) | **PASS** run `l1-…-0a0705` (tarball `94e878f6`, final code): doctor none failed, `sandbox ok` + `active 2/2`, `landlock ok` (FS + TCP), workloads `process/landlock`; install 0.36 s, up 0.73 s, kill 66 ms; **reboot:** enable-linger, `limactl stop` 1.1 s + start → ssh / `twd.service` active / healthy at 5.7 s, mcp answering this Mac at **16.3 s**, 2 objects rejoined with their old node IDs, 0 re-minted; **ephemeral phase on the SAME state dir after `twd down --purge --service`** (the fixed bug, from the installed release): `twd up --ephemeral --service` + `twd deploy` → agent ready, spawn 10.2 s (child mint + join on the real tailnet, within the new 40 s bound), `systemctl --user restart` → all 4 nodes rejected and **re-minted**, the sub-agent back as the same hostname with a new node ID in 5.2 s, answering this Mac at 5.2 s; `twd down --purge --service` 4 objects purged, 4 devices deleted, unit removed; 15 run devices deleted, 0 left; 0 twd devices/keys left; VM clean. Two earlier attempts on this row failed and led to fixes: `e676bdc2` (spawn > 10 s client timeout while the Mac ran e2e-multi + release + e2e-vm; and the 8.5 s re-mint from leftover node state) and `7e6c53aa` (the `lease_lost` dead-node bug above) |
| | `make e2e-linux-vm` (`twdl-kvm-u24`, nested virt; linux-arm64 release with microVM assets built in twd-dev) | **PASS** run `l3-linuxvm-590246` (23/23): install 0.67 s → doctor `microvm_tier warn … not installed` with the setup fix → `sudo twd host setup --microvm` 0.97 s → doctor `microvm_tier ok` (helper answers uid, jailer, LUKS2) → `twd up` 0.57 s (`tiers: process [Landlock+seccomp…], microvm [Firecracker, jailed, via twd-vmhelper]`) → agent + mcp in jailed microVMs (boot 0.81–0.91 s, InstanceStart→ready 0.70–0.78 s) → checks (a)–(f) from this Mac → storage across a VMM SIGKILL (serving again in 1.23 s, all 30 writes) and a redeploy (31 writes, attach 17 ms) → no keys in either guest (22/24 needles) → `twd down` → `setup --undo`; 6 test devices deleted, 0 left; 4 twd devices/keys, 0 left; VM clean |
| | `make e2e-linux-reboot` (`twdl-kvm-u24`; clean `limactl stop` + power cut) | **PASS** run `l3-reboot-447bf5` (final code): after each reboot `twd-vmhelper` active 3.8 / 4.6 s after kernel boot, ssh 5.4 / 6.5 s, `twd up` 2.2 / 2.0 s, microVM ready 0.77 s, volume re-attached (LUKS2) in 21 / 20 ms, the object back on the same node ID with every SQLite write; **`twd up` → the Mac client served: 17.1 s (clean reboot) vs 3.2 s (power cut)**; see the re-find diagnosis below; 0 devices left; VM clean |

### Diagnosis: the 12–17 s "client re-finds the node after reboot" delay (L3's open question)
Measured on `l3-reboot-447bf5` with the new client-side sampler (the test client's `Status().Peer[node]` every 250 ms from `twd up` until the first HTTP 200; the VM's `object node ready` time alongside):

| | clean reboot (1) | power cut (2) |
|---|---|---|
| node ready in the VM (`object node ready`, rejoined from saved state) | +2.3 s after `twd up` returned | +2.2 s |
| peer in the client's netmap | the whole time (same stable node ID; `online=true`, `relay="sea"`) | the whole time |
| client's WireGuard state at +0.3 s | `active=true`, **last handshake 14.7 s ago** (the pre-reboot session), no `curAddr` | `active=true`, last handshake 24.6 s ago |
| +2.0 s | `active=false` (session idle), still no path | same |
| new handshake + direct path (`curAddr` set) | **+15.9 s** | **+3.0 s** (handshake 0.7 s earlier, i.e. right as the node came up) |
| first HTTP 200 | +17.1 s | +3.2 s |

- **It is not DERP or netmap propagation.** The peer never leaves the client's netmap (a rejoin keeps the node key, so control sees the same device), it is `online=true` with a DERP home region throughout, and DERP would carry a handshake instantly. The VM side is done at +2.3 s.
- **It is the client's WireGuard stale session.** The rebooted node keeps its node key but has fresh WireGuard state, so the client's existing session (handshake 14.7 s old at +0.3 s, well under REKEY_AFTER_TIME) is useless but still trusted: data goes out on it, nothing comes back, and WireGuard only initiates a new handshake after KEEPALIVE_TIMEOUT + REKEY_TIMEOUT = **15 s** of silence. +0.3 s (first poll) + 15 s ≈ the +15.9 s handshake seen. In reboot 2 the client had already been retrying handshakes (its session to the node died during reboot 1's verification traffic and the VM was cut 30+ s later), so the first retry after the node came up (+2.2 s) succeeded at once: 3.2 s.
- Matches L1's reboot numbers too (8.6 s once, 17.1 s twice: whether the client's session had gone stale before the reboot decides it) and why L1's **ephemeral** restart answered in 9.5 s: a re-minted node is a *new* peer (new key), so the client starts a fresh session instead of waiting on a dead one.
- Not twd-side and not trivial: a client can't be told to re-handshake by the server; tailscale's own disco path discovery is what eventually rescues it. Options if it matters: (a) have twd's workload nodes **re-mint on host reboot** (new node key; costs a device churn and ~5 s instead of ~15 s), (b) clients using a short WireGuard persistent keepalive would notice in ≤ 10 s, (c) leave it: a reboot already means ≥ 6 s of downtime before twd even starts, and the node is correct and reachable over DERP for any *new* client the moment it is up (first-time clients saw it at +2–3 s in L3's e2e-linux-vm). Recorded, not fixed.

### Safety
- Only tailnet 1 and tailnet-2; **no policy written** (every run reported the rules already present, read-only). Every run deleted its own devices by ID and revoked its keys (0 left, see each row); no `tskey-` in any log or result (check-secrets in `make lint`). `sudo` only inside Lima VMs (`twd host setup --microvm`, `enable-linger`). Ross's personal tailnet untouched.
- **VMs:** all matrix VMs are stopped and kept (`twdl-ubuntu24-arm64`, `twdl-debian12-arm64`, `twdl-ubuntu24-amd64`, `twdl-kvm-u24`; the others untouched and stopped); `twd-dev` is running. Each row's harness cleaned the VM (`twd down --purge --service`, uninstall, `rm -rf ~/.twd-e2e`; `setup --undo` on the kvm VM). `twd-dev` has a copy of the tree in `/tmp/twd-int` (no mounts used).

### Gaps (integration)
- An ephemeral **microVM** object across a host reboot (re-mint on a VM-tier object) was argued from the code path (nodes are host-side netd nodes for every tier), not run.
- `make e2e-linux` on Ubuntu 22.04 and Fedora was not re-run (L1 ran them before L2; the sandbox on ABI 1 (22.04) is FS-only + seccomp and has no measured row yet).
- The kill-mid-join `listener already open` finding (above) and the hostd node retry's audit noise (each retry that fails is a `deny lease_lost` record) are left as is.
- `scripts/e2e-linux-one.sh` still cleans a `state-eph` dir that the test no longer creates (harmless).

## Final verification (master = Linux + RT fixes) (branch `final-verify`, 2026-11-08, this tree)
Tree: master `4a8e0d9` (Linux integration + `rt-fixes`) + master `63a4eb3` (`rt2-live`, security/ only, merged conflict-free) + the commits below. Run IDs of this pass carry the prefix `ttest-fv-` (test client + echo) / `twd-fv-` (nodes twd created); the e2e harnesses now honour `TWD_E2E_PREFIX` everywhere (mbp, linux, linuxvm, reboot too).

### Fixes made during the pass
- **CLI nits:** `twd <cmd> --help` / `-h` / `-help` (any position before `--`, also after global flags and for `host setup` / `tenant add`), `twd help <cmd>` and `twd --help` print usage to **stdout, exit 0** instead of `error [bad_request]: flag: help requested`. `twd init`'s usage and the global help list all five templates `hello|agent|mcp|ai-agent|node-workflow` and say that `node-workflow` needs Node.js on the deploying machine (also `templates/README.md`). `main` is now `run(args, stdout, stderr) int`; `cmd/twd.TestHelp` covers every command and spelling.
- **e2e check (d) tightened** (`e2e/cliflow`, mac + multi): besides the narrowed child's `refund` → `cap_missing` and `lookup_order` ok, the **unnarrowed parent's `refund` and `lookup_order` must be admitted** (`ok: true`), not merely "not cap_missing"; otherwise the child's denial would prove nothing about `lim`.
- **e2e-mbp harness regression (found by this pass, not a product bug):** the mbp test required every `twd doctor` row to be `ok`; L2/RT added two rows that are legitimately `skip` on macOS before `twd up` (`host_sockets`: Linux-only; `node_egress`: "twd is not running here"), so the first `make e2e-mbp` on this tree failed at doctor (`host_sockets is skip`). e2e-mbp had not been run since those merges. The rule now matches the Linux harness: no row may `fail`, and `binaries, workerd, bundler, twd_home_writable, state_dir_writable, api_key, sandbox, system_tailscaled_not_used` must be `ok`; the per-row summary is recorded (`doctor_<row>`).
- **Packaging gap (red team, pass 2): the Mac-built linux-arm64 tarball had no microVM guest.** `release.sh` only includes `share/twd/vm/` (vmlinux, rootfs.ext4 + manifest.json, firecracker, jailer) when `TWD_VM_ASSETS_arm64` points at a `make vm-assets` output, which is Linux-only; `make release` on the Mac shipped only `bin/twd-vmhelper`, `release.json` said `"microvm": false`, and `e2e-linux-vm` had been building its own tarball inside twd-dev, so the L3 row never tested the shipped artifact. Fix: `scripts/vm-assets-lima.sh` builds the assets on Linux directly or, on macOS, in Lima `twd-dev` (copy of the tree in `/tmp/twd-vm-assets`, `make deps && make vm-assets`, `limactl copy` back), **cached in `dist/vm-assets-arm64` by an input hash** (guest/, go.mod/go.sum, the in-repo packages twd-guestd depends on, the workerd version, the VM's kernel + Firecracker binaries). `make release` calls it (`RELEASE_VM_ASSETS=auto` default: include when a builder is available, else print a WARNING and ship without the tier; `=1` required; `=0` never; `TWD_VM_ASSETS_arm64` overrides), and `release.sh` prints `microVM guest assets (share/twd/vm): included | NOT included`. Measured: first build **5 s** (warm npm cache; the image is reproducible, same `rootfs_sha256 7845b9d1…` as L3's), later releases hit the cache; the linux-arm64 tarball grew from 104 MB to **162 MB**; `release.json` `"microvm": true`. linux-amd64 still has no guest (no amd64 kernel/Firecracker; documented). `e2e-linux-vm.sh` now uses the shipped `dist/` tarball when it has `share/twd/vm` (fallback: build in twd-dev). INSTALL.md says how to tell.

### Matrix (all on this tree; one run each unless noted)
| Where | Target | Result |
|---|---|---|
| macOS (this Mac, 27.2) | `make test` (`-race`, 13 Go pkgs + JS) | **PASS** 55 s |
| | `make lint` | **PASS** 8 s (re-run after the master merge: PASS) |
| | `make test-sdk` | **PASS** 31 s (15 subtests) |
| | `make test-node` | **PASS** 15 s |
| | `make test-pg` (Postgres 17, OrbStack) | **PASS** 17 s |
| | `go test ./security/...` | **PASS** (re-run after merging `rt2-live`: PASS; the new `rt_sandbox_linux_test` runs on Linux, see twd-dev) |
| macOS, real tailnets | `make e2e-mac` | **PASS** run `m4-4d99db`, 41 s, 12/12 checks; `twd up` → ready 1.22 s, deploy agent → ready 1.60 s, first object node 1.66 s, spawn 2.72 s, `/delegate` 1.64 s, kill CLI 67 ms / push 56 ms, request to a killed object 410 in 14.8 ms, `twd down --purge` 4.19 s (2 purged, 5 devices deleted); 7 test devices deleted by cleanup, **0 left**; policy not written. **(d):** same-tag child spawned with `lim=[lookup_order]` → `refund` **`cap_missing`**, `lookup_order` ok; **parent `refund` admitted** |
| | `make e2e-multi` ×2 | **PASS** `m7-a947b3` 68 s and `m7-16014d` 51 s (the second with the tightened (d)), 15/15 checks each: `d_lim_enforced_A`/`_B` (child `cap_missing`, parent `refund` ok=true, per tenant), cross-tenant reach by name/FQDN/IP `peer_not_visible` 8/8 in both directions (1–3 ms), `d_state_isolation`; **0 devices left in either tailnet**; policy not written |
| Release | `make release` | **PASS** ×4: 106 s (`599f151e`, before the vm-assets change: linux-arm64 104 MB, no guest), then `3630c898` / `9024aa02` with the guest (darwin-arm64 95 MB, linux-arm64 **162 MB** incl. `share/twd/vm`, linux-amd64 109 MB); 5/6/6 binaries arch-checked |
| `mbp` (M2 Pro, macOS 26.6.2, clean; release scp'd) | `make e2e-mbp` ×3 | #1 **FAIL** (harness: doctor strictness, above; mbp left clean) · #2 **PASS** `m6-mbp-3dd867` 65 s · #3 **PASS** `m6-mbp-913181` 85 s (tarball `3630c898`), 20/20 checks each. #2: install 5.5 s (again 6.1 s), doctor 10 rows none failed in 0.68 s (`sandbox=ok` Seatbelt enforces, `host_sockets=skip`, `node_egress=skip`), `twd up` → ready 1.59 s, deploy agent → ready 1.68 s, first object node 2.03 s, spawn 1.82 s, kill CLI 72 ms / push 58 ms, 410 via the workload in 19.7 ms, direct path after 9 ms; `seatbelt_release` PASS (2/2 workloads under Seatbelt from the installed release; profiles deny the API key file, hostd state, `~/.twd/bin`); (d) lim enforced via node certificates from the installed release; **0 devices/keys left, mbp clean** (#3: up 1.57 s, deploy 1.69 s, first node 2.42 s, spawn 3.85 s while twd-dev ran `make test -race` + the Ubuntu row's release build) |
| Lima `twd-dev` (Ubuntu 24.04 arm64; copy of the tree in `/tmp/twd-fv`, no mounts) | `make test` | **PASS** 164 s (15 Go pkgs under `-race`, incl. `internal/procsb` 11.7 s and `security/attacks` 5.2 s with the new Linux sandbox attack test; netd 41.6 s, hostd 147 s; **no netd flake** while e2e-mbp #3 ran on the Mac at the same time) |
| | `make test-node` | **PASS** 35 s |
| | `make test-sdk` | **PASS** 91 s |
| | `make e2e-vm` (Firecracker, nested virt, sudo inside the VM) | **PASS** 19 s: TestSmoke, TestVolume, TestE2EVM 13/13 |
| Fresh Lima VMs, nothing mounted, tarball via `limactl copy` (reused stopped `twdl-*` VMs, cleaned by the harness; ≤ 2 running at a time; stopped afterwards) | `make e2e-linux-vm` (`twdl-kvm-u24`, nested virt), **from the shipped Mac-built tarball** `twd-0.1.0-9024aa02-linux-arm64.tar.gz` (162 MB, `share/twd/vm` included; the script no longer builds its own) | **PASS** run `l3-linuxvm-9a5e6a`, 42 s, 23/23: install 0.9 s (firecracker, jailer, manifest, rootfs, vmlinux present) → doctor `microvm_tier warn` (helper not installed) → `sudo twd host setup --microvm` 1.0 s → doctor `microvm_tier ok` → `twd up` 0.69 s → agent + mcp in jailed microVMs (boot 0.97 s, InstanceStart→ready 0.85 s, guest RSS ~175 MB) → checks (a)–(f) from this Mac (deploy→ready 1.59 s, first object node 1.71 s, delegate 1.68 s, kill CLI 109 ms / push 88 ms, 410 in ≤ 13 ms) → storage across a VMM SIGKILL (serving again in 1.15 s, all 30 writes) and a redeploy (31 writes, attach 17 ms) → no keys in either guest (28 needles) → `twd down` 0.89 s → `setup --undo`; 0 devices left; VM clean, stopped |
| | `make e2e-linux-reboot` (`twdl-kvm-u24`; clean `limactl stop` + power cut; same tarball) | **PASS** run `l3-reboot-35b3fc`, 73 s: helper active 4.2 / 3.7 s after kernel boot, ssh 5.4 / 5.1 s, `twd up` 0.62 / 0.63 s, microVM ready 0.75 / 0.84 s, LUKS2 volume re-attached in 22 / 25 ms, the object back on the **same node ID** with every write; `twd up` → the Mac client served: **20.2 s (clean reboot) vs 6.4 s (power cut)**, the client-side stale-WireGuard-session effect diagnosed in the Integration section (not DERP, not twd); 0 devices left; VM clean, stopped |
| | `make e2e-linux` **Ubuntu 24.04 arm64 with the reboot + ephemeral phases** (`twdl-ubuntu24-arm64`, kernel 6.8, Landlock ABI 4; `TWD_E2E_REUSE_VM=1`) | run 1 (`l1-…-7ee2a8`, tarball `9024aa02`) **FAIL** at check (e), see "Flake" below (the product was fine: kill CLI 73 ms / push 64 ms; all earlier checks PASS; VM cleaned, 0 devices left). Run 2 (`l1-…-5d4d00`, tarball `d4e06750`, fixed harness) **PASS** 141 s: doctor 19 rows none failed (`sandbox ok` Landlock ABI 4 + seccomp, `host_sockets ok`, `landlock ok`, `microvm_tier` info: no /dev/kvm), both workloads `process/landlock`; install 0.56 s, `twd up --service` 0.66 s, deploy→ready 3.8 s, first object node 1.6 s, spawn 3.6 s, kill CLI 70 ms / push 61 ms, **410 in ≤ 1 ms** (via workload 3.7 ms), direct path after 178 ms; `systemctl --user restart` → healthy 3.7 s, answering this Mac 6.7 s; **reboot** (enable-linger, `limactl stop` 1.1 s + start): ssh / `twd.service` active / healthy at 6.7 s, mcp answering this Mac at **7.7 s**, 2 objects rejoined with their old node IDs, 0 re-minted; **ephemeral phase on the same state dir after `twd down --purge --service`** (`twd up --ephemeral --service` + redeploy): spawn 10.1 s, restart → sub-agent re-minted and ready in 4.1 s, answering this Mac at 9.5 s; `twd down --purge --service` 4 purged, unit removed; 16 run devices deleted, 0 left; 0 twd devices/keys left; VM clean, stopped |

### Flake investigated: check (e) "new request to killed narrowed sub-agent: 410 after 5.04 s" (Ubuntu 24.04 run 1)
- **What happened.** The narrowed SupportAgent child from check (d) had its node ready at 18:25:14.581 and `twd kill --cascade` killed it 184 ms later. The client had **never talked to that node** (check (d) only called `/delegate` on the parent and read the child's object record), so the first request to its IP after the kill had to set up a WireGuard session first. The 410 `object_killed` tombstone did arrive, after **5.04 s = one WireGuard handshake retransmit (REKEY_TIMEOUT, 5 s) + 40 ms**: the first handshake initiation to a peer that had joined ~200 ms earlier was lost (endpoint/DERP discovery for a brand-new peer), the second succeeded at once. The check's bound is 1 s, so the row failed. The kill itself was as fast as everywhere else (CLI 73 ms, push 64 ms), and the other two killed objects (which the client *had* talked to) answered 410 immediately.
- **Why it's the harness, not twd.** "Later calls fail fast (RST, not a timeout)" is meant to measure the killed object's refusal; for the narrowed child it also measured first-contact path setup, which no server can speed up. Every other run of the same flow (mac, mbp ×2, multi ×2, linuxvm) happened to get the first handshake through (12–81 ms).
- **Fix (harness):** check (d) now also calls the narrowed child **on its own node** before the kill (`GET /` must return the client's verified caller), which both asserts that it serves there and gives the client a live session, so (e) measures what it claims. Run 2 passed with 410 in ≤ 1 ms. The netd `-race` flake the integration run saw once under load did **not** reproduce: `make test` passed under `-race` on macOS (55 s) and in twd-dev (164 s, netd 41.6 s) while e2e-mbp ran at the same time.
- Timing context, not a failure: the rows that overlapped (mbp #3 with twd-dev `make test -race` + the Ubuntu release build; Ubuntu run 1 with e2e-linux-vm) show slower `twd up` / spawn numbers (e.g. up 2.7 s vs 0.66 s, spawn 3.8 s vs 1.8 s); every bound held.

### Safety
- Only tailnet 1 and tailnet-2; **no policy written** in any run (each reported the grants already present, read-only). Every run deleted its own devices by ID and revoked its keys (0 left, per row); prefixes `ttest-fv-*` / `twd-fv-*` (hostnames) and the harness run IDs above; no `tskey-` in any log or result (`check-secrets` in `make lint`). `sudo` only inside Lima VMs (`twd host setup --microvm`, `enable-linger`, `e2e-vm`). On `mbp` only `~/.twd` / `~/.twd-e2e`, no sudo; `mbp_clean: true` after each run. Ross's personal tailnet untouched.
- **VMs:** `twdl-ubuntu24-arm64` and `twdl-kvm-u24` were started (≤ 2 at a time), cleaned by their harness traps (`twd down --purge [--service]`, uninstall, `rm -rf ~/.twd-e2e`, `setup --undo` on the kvm VM) and **stopped** (kept); the other `twdl-*` VMs were not started; `twd-dev` is running with a copy of this tree in `/tmp/twd-fv` (and `/tmp/twd-vm-assets` from the release's guest build). The lead's red-team worktree/VMs (`twd-rt2*`, `twdl-rt*`) were not touched.

### Gaps (final verification)
- Not re-run on this tree: `make e2e-linux` on Debian 12, Ubuntu 22.04, Fedora and Ubuntu 24.04 amd64 (the integration rows for Debian 12 and amd64 ran on `e676bdc2`/`7e6c53aa`, before `rt2-live` and the harness changes; nothing in between touches the product's Linux paths except the release packaging).
- linux-amd64 ships without a microVM guest (no amd64 kernel/Firecracker build); documented.
- An ephemeral **microVM** object across a host reboot is still argued from the code path, not run.

## M10: keyless by default (branch `m10-keyless`, 2026-11-08) — ADR-026

Ross: "We shouldn't have to use an API key. We've got Tailscale identity built into the connection." The M4 flow (`--api-key-file`, CLI only on the local unix socket, interactive login refused) was a test-harness shortcut. M10 makes the keyless path the product flow and keeps API-key mode as a labelled alternative.

### What's built
- **`twd up` (no flags) is keyless.** mgmt's own tailnet node (`twd-mgmt`, `tag:twd-mgmt`) is joined **in the CLI** by tsnet interactive login (`cmd/twd/login.go`: the login URL is printed, the IPN bus is watched until Running, the netmap gives the tailnet name, DNS name, HTTPS and the funnel attribute), then the node's state dir (`<state>/mgmt/tsnet`) is handed to twd-mgmt, which re-opens it without any login (`twd-mgmt` fails fast with the login URL if a state ever needs one again, instead of hanging). Later `twd up` runs need no login. `--tailnet` is an override; the policy snippet (tagOwners `tag:twd`, `tag:twd-mgmt`, `tag:twd-host`; `nodeAttrs` funnel for `tag:twd-mgmt`; the `autogroup:admin → tag:twd-mgmt` grant with app cap `twd.dev/cap/admin`) is printed **before** the login URL. `https_required` / `funnel_required` stop with the exact console fix and keep the joined node. `--mgmt-authkey-file` is the unattended join (tests). The system tailscaled is still never used: mgmt's node is a tsnet node.
- **WIF minting is the default** (`cmd/twd/wif.go`, mgmt `PUT /v0/tenants/{id}/wif`, `POST …/wif-check`). mgmt serves its OIDC issuer over Funnel on its node (`-funnel -issuer-url https://<node>`); `twd up` prints the exact admin-console steps (issuer URL `https://twd-mgmt.<tailnet>.ts.net/t/tn_<tailnet>`, subjects `twd:tenant:<id>:mint|lifecycle`, scopes `auth_keys` / `devices:core`+`policy_file:read`, tag `tag:twd`, console URL; the console has no pre-fill for custom issuers, so that is stated), waits for the issuer to answer from the public internet, takes the client IDs from a prompt or `--wif-client-id` / `--wif-lifecycle-client-id`, creates (or re-keys) the tenant in mode `wif`, and runs `wif-check`: a fresh token exchange per credential plus a policy read with the lifecycle one. Only the public client IDs are stored. `twd up --yes` without IDs prints the steps, leaves the stack running and exits `wif_credential_required`; the next `twd up --wif-client-id …` completes. Without a lifecycle credential, `twd deploy` prints `preflight skipped` (mgmt answers `policy_unavailable`) and deploys. `--api-key-file` is "apikey mode"; a state dir is bound to one mode (`state_conflict` otherwise); `twd doctor` has a `mint_mode` row (`wif (keyless)` / `apikey` / linked) and `api_key` is `skip` in keyless mode; `twd status` shows the mode and the mgmt node.
- **Tailnet-authenticated API + `twd link`** (`mgmt/server.go`, `cmd/twd/link.go`). mgmt's tsnet node serves the API on `:80` and on `:443` with the node's HTTPS certificate (`ListenTLS`, tailnet-only; the Funnel listener serves only the issuer). **WhoIs** authenticates every tailnet request: `tag:twd-host` nodes are hosts; callers whose WhoIs CapMap holds **`twd.dev/cap/admin`** (any grant to `tag:twd-mgmt`: autogroup:admin from the base snippet, or a user/group/tag such as a CI node) are admins; everyone else gets `403 not_admin` with the grant snippet. `-admins` remains an override. `GET /v0/whoami` tells a caller who it is and where mgmt lives. `twd link twd-mgmt.<tailnet>.ts.net` (or bare `twd link`: MagicDNS discovery through the `:80` listener) calls whoami over the developer's own Tailscale connection (the CLI is not a node; plain HTTPS through the system tailscaled, which is exactly the identity we want), refuses non-admins with the snippet, and stores only the endpoint in `<state>/link.json`. Then `deploy` (bundled locally with the built-in esbuild, uploaded), `ls`, `kill`, `audit`, `status`, `init`, `dev` work remotely; `logs` and `tenant add` say `remote_unsupported`. `deployedBy` (new store column, `GET /v0/deployments` and `twd ls … by <actor>`) and the `deploy` audit record's `actingFor` carry the WhoIs actor (`login`, or `<tag> (<stableNodeID>)` for tagged nodes); kills by admins over the tailnet carry it too. The local unix socket + host token (RT-03) is unchanged for `twd up`'s own CLI and for hostd. A test transport (`twd link … --tsnet-dir … --tsnet-authkey-file … --tsnet-tags`) makes the CLI its own tsnet node, so tests never touch this Mac's tailscaled.
- **macOS `--service`**: `twd up --service` installs the LaunchAgent `~/Library/LaunchAgents/dev.twd.up.plist` (`cmd/twd/service_launchd.go`: RunAtLoad, KeepAlive on failure only so `twd down`'s SIGTERM is final, ExitTimeOut 180 s, per-user `gui/<uid>` domain, no sudo); `twd down --service` boots it out and removes the plist; `twd down` alone stops it through launchd. `twd up` prints a **sleep hint** (`pmset -a sleep 0 …` / `caffeinate -s`) unless sleep is already disabled; INSTALL.md says the same. Linux systemd is unchanged behind the same `installService/removeService/serviceStop` dispatch.
- **Policy snippets** (`internal/policy`) learned `nodeAttrs` (merge, additive verification, `Check`: only `funnel` on `tag:twd*` targets) and `Options.AllowSrcs` (the base snippet's `autogroup:admin` source; RT-06's refusal of non-twd sources otherwise stands).
- **Docs:** `docs/INSTALL.md` leads with "Keyless setup (recommended)" (host: `twd up` → snippet → login URL → trust credential → ready; developer machine: `twd link`, `twd init`, `twd dev`, `twd deploy`; why one console step remains), API-key mode is "Alternative"; `MORNING.md` command list; `docs/contracts/mgmt-api.md` "M10 additions".

### Decisions and honest limits
- **One login, not two.** The spec asked for mgmt and hostd to join. In the all-in-one `twd up`, hostd talks to mgmt over the local unix socket (host token), so a second tagged node would mean a second browser login for no functional gain; `tag:twd-host` is in the snippet and the WhoIs host path is implemented and tested (`mgmt.TestTailnetAuth`), but hostd does not yet run its own `tag:twd-host` node. The production shape (RFC §10: `twd host add` mints a single-use join key through mgmt, now possible via WIF) is the follow-up.
- **The one-time console step stays.** Creating new devices needs a control-plane credential; a connection's identity cannot mint. The trust credential is the keyless form (no secret anywhere, scoped to `auth_keys` for `tag:twd`, deletable in the console). Tailscale's console has no pre-fill or "connect a platform" flow for custom issuers (DX-fit T-C/T-J), so `twd up` prints the fields to paste. Interactive browser login can't be automated, so the e2e joins mgmt's node with an auth key minted by the test (the key never reaches twd).
- `twd tenant add` (a second tenant) is still apikey-only; keyless second tenants need a per-tenant issuer path and a second console step (the issuer already serves `/t/<tenant>`).
- Policy evaluation for admins is by the app cap only: there is no WhoIs signal for `autogroup:admin` membership itself, so "admins" means "whoever your policy grants `twd.dev/cap/admin` on `tag:twd-mgmt`", and the base snippet grants it to `autogroup:admin`.

### Test results (measured 2026-11-08, this Mac; results in `e2e/real/results/m10-*.json`)
- **`make test`: PASS** (Go `-race`, 13 packages incl. the new `mgmt.TestTailnetAuth` matrix: hosts bound to their hostID, `-admins` override, autogroup:admin via the cap, a tagged node via the cap, non-admins (a user, a twd object node) refused `not_admin` with the exact grant snippet, `deployedBy`/audit `actingFor` = the WhoIs actor; `TestWIFMintPath` with `wif-check` and `PUT …/wif`; `cmd/twd.TestCLILink` (the whole stack on testcontrol with mgmt's tsnet node joined by `--mgmt-authkey-file`, a second state dir linked through its own tsnet node → remote deploy 1.5 s / ls with `deployedBy` / kill / audit, `logs` and `tenant add` refused `remote_unsupported`, a fake non-admin mgmt → `not_admin` + snippet and no link saved, `--unlink`, 8.7 s); `TestPlistFor` (LaunchAgent content, XML escaping, `plutil -lint`); `internal/policy` nodeAttrs merge/additivity/hostile cases). **`make lint`: PASS** (incl. `vet -tags e2ekeyless`).
- **`make e2e-keyless` (tailnet 1, prefix `ttest-m10-`/`twd-m10-`): PASS 19/19 ×2** on runs `8c56ab` (3.5 min) and `2256b8` (2.5 min, final tree: Funnel first fetch 105 s, link 1.6 s, remote deploy→ready 1.7 s, first WIF-minted object node 1.6 s, remote kill CLI 1.65 s, down 0.53 s, 7 test devices deleted, **0 left, 0 untracked**; runs `2fd12c`/`fc6bb3` before it found two harness bugs, not product bugs: the CLI's tsnet dial fell back to the system resolver before the netmap had the peer (fixed: resolve MagicDNS peers through the CLI node, `cmd/twd/link.go`), and the audit JSON field is `object`). What it proved:
  1. `twd up --yes` keyless (mgmt node joined with a key the **test** minted, standing in for the browser login) → `wif_credential_required` with the console steps on stderr, the stack running and healthy, `twd.json` with `mintMode: wif`, **no `apiKeyFile`**, HTTPS true, issuer `https://<node>`; the issuer answered from the public internet (via 1.1.1.1) in **63 s / 158 s / 140 s** (Funnel's first certificate for a new node name; a production `twd-mgmt` pays this once).
  2. Trust credentials created via the API (mint: `auth_keys`, `tag:twd`; lifecycle: `devices:core`+`policy_file:read`, `tag:twd`) → `twd up --yes --wif-client-id … --wif-lifecycle-client-id …` completed in **1.9–2.0 s**: tenant `tn_tail7906d5` in mode `wif`, `wif-check` mint ✓, lifecycle ✓, **policy read ✓**; `twd doctor` `mint_mode ok wif (keyless)`, `api_key skip`.
  3. Local `twd deploy mcp` → ready in 1.5–2.8 s, its node **minted through the trust credential** (`tag:twd` owns `tag:twd-wl-mcp`).
  4. `twd link https://<mgmt node>` from a `tag:ttest` tsnet node (granted `twd.dev/cap/admin`): **admin by WhoIs in 1.6 s**, actor `tag:ttest (<stableNodeID>)`, tenant list shows `wif`.
  5. Remote `twd deploy agent` (bundled on the "developer" side, uploaded over HTTPS; preflight ran via the lifecycle credential's policy read) → ready 1.6 s; `twd ls` shows `by tag:ttest (<nodeID>)`; a `tag:ttest` caller got its verified identity via the workload (first request 1.6–3.3 s to a WIF-minted object node) and on the object's own node; remote `twd audit --all` verified with 3 `mint` records and the deploy's `actingFor` = the linked actor; remote `twd kill --cascade` 1.9–6.9 s CLI (device delete through the lifecycle credential), audit shows the kill.
  6. `tag:ttest-noadmin` (network grant, no cap) → `twd link` refused **`not_admin`** with the grant snippet naming `tag:ttest-noadmin → tag:twd-mgmt` and `twd.dev/cap/admin`; nothing saved.
  7. `twd down` (purge) 0.47 s; cleanup by ID: 6 test devices deleted, **0 left**, 2 trust credentials deleted, 0 twd devices/keys left. Run `8c56ab` left its ephemeral no-admin CLI node untracked (the ID never reaches a refused link) for 1 min; it was deleted by ID, and the harness now finds it by hostname (run `2256b8` confirms a clean cleanup).
- **`make e2e-mac`: PASS 12/12** (`m4-53b7c4`, 50 s: up 1.22 s, deploy→ready 2.0 s, first object node 1.67 s, spawn 3.3 s, kill CLI 67 ms, 410 in 1 ms, 0 leftovers; the harness check `up_interactive_refused` became `up_apikey_needs_tailnet`: `twd up --yes --api-key-file f` without `--tailnet` → `tailnet_required`). **`make e2e-multi`: PASS 15/15** (`m7-c74ce8`, 75 s, 0 leftovers in both tailnets).

### Policy change on tailnet 1 (recorded per BUILD-PLAN safety rule 2)
Applied once by `make e2e-keyless` run `2fd12c` (2026-11-08 13:30 PT) through `internal/policy.ApplyWith` (additions only; validated; `If-Match: "14e4bab4…"`; previous copy `~/.config/tailnet-agents-design/tailnet1_acl.pre-m10-2fd12c.hujson`). Added: tagOwners `tag:ttest-noadmin: ["autogroup:admin"]`; grants `autogroup:admin → tag:twd-mgmt` tcp:443,80 app `twd.dev/cap/admin`, `tag:ttest → tag:twd-mgmt` tcp:443,80 app `twd.dev/cap/admin`, `tag:ttest-noadmin → tag:twd-mgmt` tcp:443,80. `tag:twd-mgmt`/`tag:twd-host` tagOwners and the funnel nodeAttr were already there. Later runs found nothing missing and wrote nothing. **Ross's personal tailnet and the system tailscaled were never touched**; the API key was used only for policy, trust credentials, the test nodes' keys and cleanup by ID.

### Limitation stated plainly
An interactive browser login cannot be automated, so **the login-URL branch of `joinMgmtNode` (print the URL, wait for the browser) has not been run against a real control plane**; it follows tsnet's own `Up` logic (IPN bus, `BrowseToURL`, Running) but is untested beyond compiling. The e2e substitutes `--mgmt-authkey-file`, the flag the product also offers for unattended joins. Everything after the join (Funnel issuer, trust credential, WIF mint, WhoIs admin, remote CLI) ran for real.

## M11: REST v1 (spec-first) + tsnet-only operation (branch `m11-rest-tsnet`, 2026-11-08)

Ross: "Normal operation should be tsnet only." · "Please add pagination now." · "All POST seems like an anti-pattern in sound REST design." Pre-release, so `/v1` is a **breaking** replacement and `/v0` goes away with no shim. Process change from the lead: **spec first**; Ross reviews the spec before any handler is written.

### Phase 1 (DONE, this commit): the spec is the source of truth
- **`api/openapi.yaml`** (OpenAPI 3.1, ~2000 lines) specifies every `/v1` route and the public OIDC paths. Components: `Tenant`/`TenantCreate`/`TenantPatch`/`WIFInfo`/`WIFCheckResult`, `Host`/`HostRegistration`/`HostStatus`/`HostStatusResponse`/`HostPush`/`Lease`, `Deployment`/`DeploymentCreate`/`Assignment`/`ClassConfig`/`Endpoint`/`PreflightResult`, `Object`/`ObjectCreate`/`ObjectPatch`/`ObjectStatus`/`Tombstone`/`KillResult`, `Credential`, `NodeCertificate`(+`Payload`), `AuditRecord`/`AuditKind`/`AuditSource`/`AuditBatch`/`ChainStatus`/`AuditVerification`, `VolumeKeyGrant`, `Error`/`ErrorCode` (the full typed catalog, 35 codes, with examples per status)/`TombstoneError`, `Page` + typed `*Page` envelopes (`items`, `nextCursor: string|null`), `limit`/`cursor`/`If-Match`/`If-None-Match`/`epoch` parameters, `ETag`/`Location` headers, `Status`/`WhoAmI`/`Health`. Security: `tailnetWhoIs` (documented as WhoIs on the tsnet listener, not a token; `mutualTLS` is the nearest scheme type) and `localHostToken` (`--local-dev` only). Every operation carries `x-twd-role` (`admin` / `host` / `admin|host` / `any`); every mutating operation `x-twd-audit: true` (exceptions, recorded in the test: the 3 s heartbeat and the wif-check diagnostic). `operationId`s read as Go methods (`ListTenants`, `CreateObjectCredential`, `ReportHostStatus`, …).
- **Decisions recorded in the spec:** preflight = `POST /v1/deployments?dryRun=true` (same body and `twd.jsonc` parser as the deploy, `200 PreflightResult`, nothing written); audit verification = `GET /v1/audit/verification` (a noun, no `:verb` syntax); credentials and volume keys are `POST …/credentials` / `…/keys` (each call creates an audited single-use grant; remint = `?reason=rejoin_rejected`); `POST /v0/flush` → `POST /v1/device-delete-runs`; `POST …/purge` → `DELETE /v1/tenants/{id}/objects?state=`; heartbeat → `PUT /v1/hosts/{id}/status`; register → `PUT /v1/hosts/{id}` (201 first time, 200 after); ready → `PATCH /v1/objects/{id} {status: ready, …}`; kill → `DELETE /v1/objects/{id}?cascade&reason` → `202`, then `410` with the kill record (`TombstoneError`); undeploy → `DELETE /v1/deployments/{id}`; tenant delete `409 tenant_has_live_objects` unless `?force=true`; host delete `409 host_holds_leases` unless `?drain=true`; `PATCH` = JSON Merge Patch (RFC 7396) on `application/merge-patch+json`, `If-Match` → `412 precondition_failed`; volume ID is `<tenantID>:<workload>`.
- **Codegen:** `oapi-codegen` v2.8.2 pinned as a `go.mod` tool (no cgo). `make api` generates `mgmt/api/v1/api.gen.go` (types, `std-http-server`, **strict-server** interfaces, embedded spec; `nullable.Nullable[T]` for merge-patch fields so absent ≠ `null`), `hostd/mgmtapi/v1/client.gen.go` (typed client + `ClientWithResponses`) and **`docs/contracts/mgmt-api.md`** from the spec (`api/render`, kin-openapi; the old hand-written contract is replaced, `hostd-additions.md` carries a pointer). `make api-lint` runs Redocly 2.76.0 with `redocly.yaml` (warnings are errors: 0 issues). `make api-check` (now part of `make lint`) regenerates and fails on any diff, then runs `go test ./mgmt/api/...`.
- **Validation in tests:** `mgmt/api/v1/validate.go` wraps kin-openapi `openapi3filter` (`Validator.Middleware` checks every request and response of a handler against the spec; hermetic suites will wrap mgmt's handler with it in Phase 2). `spec_test.go` proves it rejects `limit=0/501`, bad ID patterns, a missing `epoch`, unknown enum values, `additionalProperties`, a `/v0` path, a response missing required fields, an unknown error code and an undocumented status, and accepts the valid cases; `TestSpecConventions` enforces operationId / role / audit / paging / merge-patch / `If-Match`+`412` / `202`+`Location` / no DELETE bodies on every operation.
- **Not done in Phase 1 (by instruction):** no handlers, no hostd/CLI/fakemgmt/e2e migration, no tsnet-only transport work, no store migrations. `/v0` still serves; the old `hostd/mgmtapi` (hand-written) is still what hostd compiles against; `hostd/mgmtapi/v1` is the generated replacement it will move to.

### Phase 2 (pending Ross's spec review)
Implement the strict-server interface in `mgmt` from the generated types; delete `/v0`; migrate hostd, fakemgmt, the CLI (`ls --limit/--all`, `kill` → DELETE, `tenant set` → PATCH, `tenant rm`, `undeploy`), the wrapper's spawn path and every e2e harness; `(created_at, id)` indexes for cursor pagination (SQLite + Postgres migrations); hostd's own `tag:twd-host` tsnet node, `twd host add` → WIF-minted single-use join key, `twd link --tsnet`, local socket only behind `--local-dev`, `twd doctor transport`.

### Phase 2 (this tree): implementation
**What's built.**
- **mgmt serves `/v1` from the generated interface** (`mgmt/v1.go` implements `v1.ServerInterface`; routing and parameter parsing are generated; a role gate reads every operation's `x-twd-role` from the embedded spec, so a route cannot exist without a role). `/v0` is gone (a typed 404). Pages are keyset-paginated on `(created_at, id)` (audit: `(ts, chain, seq)`) with opaque cursors; new indexes for both SQLite and Postgres (`addIndexes`, additive `addColumns` for tenant pool/tier/updated_at, host pool/push_port/created_at, deployment stopped_at, the object kill record `killed_at/killed_by/kill_cascade/kill_root` and `acting_for`). `PATCH` is JSON Merge Patch (absent ≠ `null`; `nullable.Nullable` in the generated types), `ETag`/`If-Match`/`If-None-Match` on every resource, `202 + Location` for creates, `410` with the kill record for tombstones, `DELETE` with query options and cascades (`tenant_has_live_objects` / `host_holds_leases` / `superseded` 409s). New service methods: `UpdateTenant`, `DeleteTenant(force)`, `CreateHost` (join key), `DeleteHost(drain)`, `Undeploy`, `PreflightBundle` (dryRun), `SetObjectTTL`, `Status`.
- **tsnet-only transport.** twd-mgmt needs `-tsnet-hostname`; the unix socket + host token exist only behind `-local-dev`. mgmt publishes its node's **loopback proxy** (`<state>/mgmt/loopback.json`, 0600): the `twd` CLI on the host dials mgmt's own tailnet IP through it, so WhoIs sees mgmt's node and admits it as the operator (no socket, no token). twd-hostd runs its **own node** `twd-host-<hostID>` (`tag:twd-host`): `twd up` mints a single-use join key through the tenant credential (`POST /v1/hosts`, WIF or API key) into `<state>/hostd/join.env`, hostd joins, registers with `PUT /v1/hosts/{id}` (the node is bound to the host ID), heartbeats `PUT …/status`, and serves the push API on its node (`-push-port 7474`); mgmt pushes kills there through its own node (`Config.TailnetDial`). Remote hosts: `twd host add <id>` → `twd-hostd join --token-file …`. `twd link --tsnet` runs an ephemeral CLI node with an interactive login (`<state>/cli`). `twd doctor` has a `transport` row (`ok tsnet:` expected; `--local-dev` warns; a stray mgmt socket fails). apikey mode is tsnet-only too: the API key mints mgmt's node key in-process. The policy snippet (`internal/policy.InfraSnippet`) adds `tag:twd-host → tag:twd-mgmt tcp:80,443` and `tag:twd-mgmt → tag:twd-host tcp:7474`.
- **Clients.** `hostd/mgmtapi` is an adapter over the generated `hostd/mgmtapi/v1` client (same exported surface; `Remint` returns the new credential); fakemgmt serves `/v1` and is wrapped by the spec validator in every hostd test; the CLI's `ls` pages (`--limit`, `--all`), `kill` is `DELETE`, `tenant set` is `PATCH`, new `tenant rm`, `undeploy`, `host add|ls|rm`. hostd's own push/status API moved to `/v1/...`. The hermetic control plane (`nettest.StartControl`) now models tags (`TagOwners` non-nil), so WhoIs sees `tag:twd-host` and `tag:twd-mgmt` in hermetic runs.
- **Tests.** `mgmt/v1_test.go`: pagination (bounds, bad cursor, stable walk under inserts, audit paging), merge-patch semantics, ETag/304/412, DELETE semantics (409/202/204/410 tombstone/superseded), a role matrix driven from `v1.OperationRoles()`, host join keys, status/whoami/no-v0; every mgmt API test runs under the kin-openapi validator. `e2e/m2mgmt` (hostd against the v0 twd-mgmt) is deleted with its Makefile target.

**Spec changes made during implementation** (regenerated, `make api-check` clean): `POST /v1/hosts` (join key), relaxed ID patterns (`dp_`/`tn_` are 16/8 hex), error code `delegation_exceeds_parent`, audit kinds `credential`/`obo_root`/`object_update`/`host_add`/`preflight`, `AuditRecord.detail` object|string, `limit` out of range is `400` (not clamped), `TombstoneError.tombstone` optional for a dead-parent `410 delegation_invalid`.

### Test results (measured 2026-11-08, this Mac + Lima twd-dev; results in `e2e/real/results/m10-*-93bf7b.json`)
- **`make test`: PASS** on this Mac (Go `-race`, 14 packages: `mgmt` incl. `TestV1*` and every API test under the kin-openapi validator, `cmd/twd` `TestCLIEndToEnd` 17 s / `TestCLILink --tsnet` 9 s / `TestCLIMultiTenant` / `TestDoctor`, `hostd` with the validated fakemgmt, `security/attacks` on v1) and **in Lima `twd-dev`** (Ubuntu 24.04: all packages PASS; `cmd/twd` 128 s). **`make lint`: PASS** (incl. `api-check`, every e2e tag vet, check-secrets). **`make test-pg`: PASS** (Postgres 17: store + API + `TestV1*` suites; it found and fixed one bug: `GetDeployment` missed the new `stopped_at` column, and a create-response race — `202` is now decided by "freshly created", not by re-reading the object).
- **Lima `twd-dev` `make e2e-vm`: PASS** (`TestSmoke`, `TestVolume`, `TestE2EVM` 8.6 s: the microVM tier against the v1 fakemgmt).
- **`make e2e-keyless` (tailnet 1, prefix `ttest-m10-93bf7b`/`twd-m10-93bf7b`): PASS 20/20** in 187 s, **tsnet only**: `twd up --yes` step 1 → `wif_credential_required`, mgmt up on its node, **host `pending`** (hostd waits for its join key), `transport "tsnet (loopback through mgmt's node)"`; trust credentials created; step 2 (`--wif-client-id …`) 8.1 s: wif-check mint ✓ lifecycle ✓ policy read ✓, then mgmt **minted the host join key through the trust credential** (`POST /v1/hosts`), hostd joined as `twd-host-h-mini` (`tag:twd-host`) and registered over the tailnet; `twd doctor` `transport ok tsnet: … hostd on its own tag:twd-host node …`, **no `mgmt.sock`**; `twd host ls` live; local WIF deploy (mcp) ready in 1.9 s; `twd link` from the tag:ttest node 1.6 s admin by WhoIs; remote deploy 1.8 s ready, remote kill CLI 1.8 s (pushed to the host's node through mgmt's node), audit verified, non-admin refused `not_admin`; `twd down` 0.7 s; cleanup by ID: 8 devices deleted (mgmt node, host node, CLI nodes, object nodes), **0 left, 0 untracked**, 2 trust credentials deleted. Funnel first fetch 107 s (the known first-certificate cost).
  - Two product bugs found by this run and fixed: (1) `twd status` said host `""` before the join (now `pending`); (2) the mint of the host join key was refused by the control plane (`tags [tag:twd-host] … not permitted`) because tailnet 1's `tag:twd-host` owners were `[autogroup:admin, tag:twd-mgmt]` from an earlier milestone and `internal/policy.Missing` never added owners to an *existing* tag. Merges are now additive at the owner level (`Missing`/`Merge`/`VerifyAdditive`), which also matters for real deployments whose policies predate a snippet change.

### Policy changes on tailnet 1 (recorded per BUILD-PLAN safety rule 2)
Applied by `make e2e-keyless` through `internal/policy.ApplyWith` (additions only; validated; `If-Match`; previous copies kept): run `a83272` (2026-11-08 22:05 PT) added the grant `tag:twd-mgmt → tag:twd-host tcp:7474` (kill push to the host's node; previous copy `~/.config/tailnet-agents-design/tailnet1_acl.pre-m10-a83272.hujson`); run `93bf7b` (22:06 PT, `If-Match "87d6044e…"`) added the owner `tag:twd` to `tagOwners["tag:twd-host"]` (previous copy `…/tailnet1_acl.pre-m10-93bf7b.hujson`). The grant `tag:twd-host → tag:twd-mgmt tcp:80,443` and the `tag:twd-mgmt`/`tag:twd-host` tagOwners were already present. **Ross's personal tailnet and this Mac's system tailscaled were never touched**; the API key was used only for policy, trust credentials, the test nodes' keys and cleanup by ID.
- **`make e2e-mac` (tailnet 1, apikey mode, run `8a7e51`): PASS 13/13** in 65 s, **tsnet only in apikey mode too**: the API key minted mgmt's node key in-process (no browser login), `POST /v1/hosts` minted the host join key, `transport_tsnet_only` passed (`doctor ok tsnet: … hostd on its own tag:twd-host node twd-host-twd-e2e-8a7e51`, no `mgmt.sock`), up 13.6 s (incl. both node joins), deploy→ready 4.6 s, first object node 1.9 s, kill CLI 156 ms, down 2.9 s; cleanup by ID: 9 devices deleted (5 object nodes + mgmt node + host node + 2 test nodes), **0 left, 0 untracked**.
- **`make e2e-multi` (tailnet 1 + tailnet-2, run `02d7b2`): PASS 16/16** in 97 s (tenant A on mgmt's/the host's tailnet, tenant B added with its API key; isolation checks a–f; the sandbox check now asserts **no mgmt unix socket**, and that mgmt's loopback credential and the host node's state are unreadable from a tenant's workerd). Cleanup by ID in both tailnets: **0 devices left** (verified read-only after the run: no `twd-*`/`ttest-*` device on either test tailnet). The first run of the day found a harness gap, not a product one: the multi harness tracked mgmt's and the host's own nodes on the overall results, not on tenant A's per-tailnet cleanup list, so those two nodes outlived the run; they were deleted by ID and the harness fixed.

### What is tsnet-only now, and what is not
- **tsnet only:** mgmt's API (its own node, WhoIs); hostd ↔ mgmt (hostd's own `tag:twd-host` node, bound at `PUT /v1/hosts/{id}`); kill/assignment pushes mgmt → host node; the host-local `twd` CLI (loopback proxy of mgmt's node → WhoIs = mgmt's node = operator); `twd link` (developer's Tailscale) and `twd link --tsnet` (ephemeral CLI node, interactive login); `twd host add` join keys (minted by mgmt through the tenant credential, single use, 10 min). Both mint modes: in apikey mode the API key mints mgmt's node key in-process.
- **not tsnet (by design, behind a flag):** `twd up --local-dev` / `twd-mgmt -local-dev` / `twd-hostd -local-dev` keep the unix socket + per-host token for `twd dev` and hermetic tests (`twd doctor` warns); `twd dev` itself runs fakemgmt on a socket. The OIDC issuer is Funnel (public by necessity). `twd link --tsnet`'s interactive branch was not exercised against a real control plane (the e2e joins the CLI node with a test-minted key; the login path is the same `joinMgmtNode` code `twd up` uses).

### Gaps (M11)
- `twd tenant add` for a keyless second tenant is still apikey-only (unchanged from M10).
- Remote hosts (`twd host add` + `twd-hostd join`) were exercised only through the same-machine join path (`twd up` writes the join file) — the join subcommand compiles and shares the code path but has not been run on a second machine; `make e2e-mbp`/`e2e-linux` (release tarballs) were not re-run this session.
- Hosts may list tenants (`GET /v1/tenants`, role `admin|host`) so they can deny every tenant's `apiKeyFile` path to their sandboxes; the response carries key-file *paths*, never keys.

## M12: OAuth client mint mode, the self-hosted default (branch `m12-oauth`, 2026-11-09)

Ross, installing on his prod tailnet: WIF needs Funnel + HTTPS + a public OIDC issuer just to get a secretless `auth_keys` credential; move to an **OAuth client** (scope `auth_keys`, tag `tag:twd`, never expires; the mechanism of the k8s operator / GitHub Action) as the default. WIF stays (hosted / multi-tenant), API key stays as the explicit fallback.

### What's built
- **Mint mode `oauth` in mgmt** (`mgmt/broker.go`, `internal/tsapi/oauth.go`). Tenant fields `oauthClientID` (public) + `oauthClientFile` (absolute path; the secret is never stored or returned). The file (`TS_OAUTH_CLIENT_ID=…`, `TS_OAUTH_CLIENT_SECRET=tskey-client-…`) is **re-read at every token exchange** and refused if `mode & 0o077 != 0` (at tenant create/patch and at every exchange). Client-credentials grant at `POST /api/v2/oauth/token`; the access token is cached until ~90 % of its life; a **401 invalidates it and the request is retried once** with a fresh exchange (`tsapi.Client.Tokens`). Keys are minted exactly as in apikey mode (single use, preauthorized, ≤ 5 min, tagged). Device deletes use the same token: a **403 → `device_delete_unavailable`** in `KillResult.warnings` (code, objects, fix) and in the kill / `device_delete` audit detail; the kill itself never fails; later kills warn up front; the tombstone reports `deviceDeleted: false`. A tag refusal at the host join key mint → **`403 policy_missing_tag_owners`** with the tagOwners snippet. Secrets/tokens never logged (tests assert it; `netd.Redact` covers `tskey-client-`). hostd's sandbox denies every tenant's `oauthClientFile` (and `twd up` adds it to `-sandbox-deny`).
- **`twd up --oauth-client-file <file> [--service] [--yes]`** (`cmd/twd/up.go`, `cmd/twd/oauth.go`): no browser, no Funnel, no issuer, no HTTPS requirement. Order: 0600 check (`credential_file_insecure`) → token exchange (`oauth_exchange_failed`) → tailnet from the client's devices (`GET /tailnet/-/devices`; else from mgmt's node netmap; `--tailnet` overrides) → **preflight before any mint**: if the client can read the policy, `tag:twd` must own `tag:twd-mgmt` and `tag:twd-host` (`policy_missing_tag_owners` + snippet otherwise); if it can't, the first mint's `requested tags … are invalid or not permitted` is translated to the same typed error → the client mints mgmt's node key, mgmt mints the host's → tenant created (or PATCHed when the client/file changed). The browser (`--wif`) flow now prints the policy and, in a terminal, **asks for confirmation before any login URL is requested**; a tag refusal during the join is `policy_missing_tag_owners`.
- **Mode selection:** `--oauth-client-file` → oauth; `--api-key-file` → apikey; `--wif` (or `--wif-client-id`) → wif; none on a fresh state → **`mint_mode_required`** listing the three options, OAuth first (no browser flow starts); none on a configured state → that state's mode (file/tailnet reused). Two modes at once → `bad_request`; a different mode than the state's → `state_conflict`.
- **Policy:** `policy.InfraSnippet` now has `tag:twd-mgmt: [autogroup:admin, tag:twd]`; `operatorSnippet()` = InfraSnippet + the `autogroup:admin → tag:twd-mgmt tcp:443,80 app twd.dev/cap/admin` grant, **no nodeAttrs**; `keylessSnippet()` (wif) = that + `funnel` on `tag:twd-mgmt`. Both pass the RT-06 checker with `keylessPolicyOptions()` and merge additively (`TestOperatorSnippet`).
- **Transport:** mgmt already served `:80` always and `:443` with HTTPS certs; it now **prefetches its HTTPS certificate** in the background (retries ×4; without Funnel nothing else requested it, and the first `twd link` over https timed out after 34 s in run `e6d03b`). `twd link <name>` tries **https, then http** and records `scheme` in `link.json`; an explicit `http://`/`https://` is used as given.
- **Doctor:** `mint_mode ok oauth: OAuth client <id> from <file> …`; **warn** if the file mode is looser than 0600; `api_key` is `skip` in oauth mode.
- **Spec** (`api/openapi.yaml`, `make api`, `make api-check` clean): `MintMode` enum `oauth`; `oauthClientID`/`oauthClientFile` on `Tenant`/`TenantCreate`/`TenantPatch`; `KillResult.warnings` + `KillWarning`; error code `policy_missing_tag_owners` (403).
- **Docs:** `docs/INSTALL.md` "Recommended: OAuth client" first (policy, console steps, file, `twd up`), "Alternative: workload identity federation (`twd up --wif`)" second, "Last resort: API-key mode" last; troubleshooting for the new codes; `MORNING.md` command list; `install.sh` hint.

### Safety fix found on the way (product bug)
`twd up`/`twd down` checked *any* installed LaunchAgent/systemd unit, not the one for their state dir. This Mac now runs Ross's real twd as `dev.twd.up` (`~/.twd`), so every hermetic/e2e `twd up --state-dir /tmp/…` failed `service_running`, and **a test's `twd down` would have stopped Ross's service** (`serviceStop()`). Now `serviceOf(s)` only counts a unit whose `--state-dir` is `s.Dir`, and `down --service` only removes its own. Ross's service was never stopped (checked `launchctl print`: running throughout). Also: `TestCLILink`'s "auto-discovery fails" case resolved `twd-mgmt` through this Mac's MagicDNS and **reached Ross's real mgmt on the personal tailnet** with one read-only `GET /v1/whoami` (the link was saved only in a temp dir, nothing written anywhere); the test now sets `TWD_LINK_DISCOVER_NAME` to an unresolvable name.

### Test results (2026-11-09, this Mac)
- **Hermetic:** `internal/tsapi` `TestLoadOAuthFile` (0600 enforcement, ID from the secret, mismatch, no secret in errors/`%#v`, redaction), `TestOAuthSourceCacheRefreshRetry` (cache, refresh near expiry, 401 → one retry, persistent 401 → exactly one retry, wrong secret, mode loosened later), `TestIsTagsNotPermitted`; `mgmt.TestOAuthMintPath` (spec-validated: create refusals, no secret in responses, host + object keys via OAuth, 1 exchange cached, revoked token → retry, kill with devices:core deletes, without → `device_delete_unavailable` in result + audit + tombstone, deferred delete audit with code + fix, `policy_missing_tag_owners` + snippet, group-readable file refused, no secret/token in logs); `cmd/twd.TestOperatorSnippet`, `TestModeChoiceAndTypedErrors`; **`cmd/twd.TestCLIOAuth`** (real binaries on testcontrol + fake API with an OAuth token endpoint: 0644 → `credential_file_insecure`; policy without `tag:twd` owners → `policy_missing_tag_owners`, **0 keys minted, mgmt not joined**; then the whole (a)–(f) scenario in oauth mode, 3 exchanges, 0 API-key requests, doctor ok/warn, no secret in logs) 12.6 s.
- **`make test`: PASS** (Go `-race`, all packages, + JS). **`make lint`: PASS** (incl. `api-check`, `vet -tags e2eoauth`, check-secrets).
- **`make e2e-oauth` (tailnet 1, prefix `ttest-m12-`/`twd-m12-`): PASS 25/25**, run `5d5b9a` (98 s; `e2e/real/results/m12-oauth-20261109-000700-5d5b9a.json`). A real OAuth client was created with the API key (`keyType: client`, scopes `auth_keys`, `devices:core`, `policy_file:read`, tags `tag:twd`), its secret written only to a 0600 file, and deleted at the end. `twd up --oauth-client-file` with **no `--tailnet`** (discovered from the devices endpoint): no browser, no Funnel, no issuer, twd.json has no `apiKeyFile` and no `issuerURL`, mgmt.log has no `tskey-client-` and no Funnel listener; up → ready 9.0 s (mgmt 3.8 s); checks **(a)–(f)** pass (deploy → ready 1.8 s, first object node 6.3 s, spawn 1.8 s, kill CLI 155 ms, 410 in ≤ 1 ms, audit chain verified); `twd doctor` `mint_mode ok oauth`; mgmt's HTTPS certificate prefetched; `twd link <name>` (no scheme) → **https** 1.8 s, `twd link http://<name>` → **http** 1.6 s (plain :80, WhoIs admin), `ls` over http; remote deploy (mcp2) 8.7 s, `ls` deployedBy = the WhoIs actor, remote kill with the device delete scheduled (devices:core, no warning), audit verified with the kill record; `twd down` 0.7 s; cleanup by ID: 10 devices, **0 left, 0 untracked, 0 twd-named, OAuth client deleted**. Earlier runs (all cleaned up to 0): `e6d03b` link fell back to http after a 34 s TLS timeout (no cert yet → added the prefetch), `e27628`/`0c46f9` harness checked the audit by the workload name instead of the object ID, `144160` ACME `CreateOrder: 404` on the first prefetch (→ retries; the harness accepts the http fallback when no certificate is issued).
- **`make e2e-keyless` (wif, now `twd up --wif`): PASS 20/20** run 2 (153 s, Funnel first fetch 76 s, 0 left). Run 1 (`84a679`) failed `wif_exchange_failed`: Tailscale answered 403 to the first token exchange right after the trust credentials were created (the issuer had become public after 178 s); rerun clean. Not a code change in the WIF path; recorded as a flake (the M10 runs saw none).
- **`make e2e-mac`: PASS** (61 s, apikey mode, 0 left). **`make e2e-multi`: PASS 16/16** (103 s, 0 left in both tailnets).

### Policy change on tailnet 1 (BUILD-PLAN safety rule 2)
Run `e6d03b` (2026-11-08 23:56 PT) via `policy.ApplyWith` (additions only, validated, `If-Match "07e9f510…"`, previous copy `~/.config/tailnet-agents-design/tailnet1_acl.pre-m12-e6d03b.hujson`): added owner **`tag:twd` to `tagOwners["tag:twd-mgmt"]`**. Later runs wrote nothing. No other tailnet's policy was read or written; Ross's personal tailnet was touched only by the hermetic test bug described above (one read-only whoami to his twd-mgmt, now fixed).

### Gaps (M12)
- `twd tenant add` (a second tenant) is still apikey-only; oauth for a second tenant is a PATCH away in the API (`mintMode: oauth`, `oauthClientFile`) but has no CLI flag yet.
- The interactive confirmation before the WIF login URL and the translation of a join-time tag refusal were exercised only by unit tests (no real browser login can be automated).
- Not exercised for real: an OAuth client **without** `devices:core` (hermetic only), and a real `policy_missing_tag_owners` (would need breaking tailnet 1's policy); both covered hermetically.
- `twd up` without `policy_file:read` can't check grants; it reports that and relies on the mints proving tag ownership. Without that scope `spawn()` answers `policy_unavailable` (same as WIF without a lifecycle credential).

## Tier-aware placement (2026-11-13)

- **Bug:** `pickHost` ignored `Host.Tiers`, and mgmt never read twd.jsonc `"tier"`. A `"tier": "microvm"` workload (or a tenant with default tier `microvm`) could be placed on a process-only host. hostd's `tierFor` then **fell back to the process tier with only a log warning**: a silent isolation downgrade on mixed clusters.
- **Fix (`mgmt/service.go`):** `placementTier` = twd.jsonc `"tier"`, else the tenant's `tier`. `process` and `runtime: "node"` place anywhere. Eligible hosts are live, in the tenant's pool and list the tier (a host listing no tiers is process-only). Ties on free capacity go to the lower host ID (deterministic across replicas).
  - No eligible host → `503 no_host` naming the missing tier and the fix (`sudo twd host setup --microvm`).
  - `--host` without the tier, or a live lease holder without the tier the workload now asks for → new `409 tier_unavailable` with the fix (`twd undeploy` then deploy, or pick another host). Stickiness never keeps a workload on the wrong tier.
- **Tests:** `mgmt/tier_test.go` (`TestPlacementTier` 10 cases, `TestPlacementTierStickyHolder`). `make test`, `make lint`, `make test-pg`: PASS.
- **Not changed:** hostd's fallback (`hostd/workload.go` `tierFor`) still downgrades with a warning when a microvm assignment reaches a host without a VM backend (e.g. the backend vanished after registration, or `twd dev`). Making it refuse is a follow-up.

## Tailscale Services for workloads and mgmt replicas (2026-11-13)

Design and behavior: [docs/services.md](docs/services.md).

- **Mechanism (`internal/tssvc`):** advertise + an *empty* serve-config TCP handler + a tsnet fallback TCP handler. VIP flows are delivered in-process with the caller's real source address, so WhoIs (and so all of netd's attributed ingress) works through the VIP. tsnet's `ListenService` was not usable: it forwards to a host loopback socket (caller = `127.0.0.1`, or a forgeable PROXY header).
- **Workloads:** twd.jsonc `tailnet.service` (`true` = `svc:<workload>`, or a name). This needs `tailnet.tags`. mgmt (`mgmt/services.go`):
  - creates the definition (`tcp:80`, owner comment `managed by twd: tenant=… workload=…`) before the lease;
  - refuses anyone else's Service, and a name that collides with a device's MagicDNS name (`409 service_conflict`);
  - reports a credential without Services scope as `502 service_api_failed`;
  - preflights `autoApprovers.services` (`serviceUnapproved`, `serviceSnippet`);
  - carries `service` in assignments and `endpoint.service` in deployments.
- **Workloads, host side:** hostd (`hostd/service.go`) advertises while the deployment is active, the instance passes probes (2 s; 2 failures withdraw) and the workload node is ready. It withdraws on undeploy, fencing, a move, a redeploy without a Service, and kill. netd serves Service flows on their own `http.Server`, so a withdraw stops keep-alives on Service connections only. netd egress resolves Service names from the object's netmap (MagicDNS extra records, routed by a peer's AllowedIPs) under the sibling rule.
- **mgmt replicas (H12):** `twd-mgmt -tsnet-service svc:twd-mgmt` (`mgmt.ServiceGate`, health = store answers). A hostname equal to the Service label is refused at start.
- **Tests:**
  - hermetic, all passing in `make test` (Go `-race` + JS): `internal/tssvc` (routing guard), `netd` `TestService` (testcontrol emulating Service hosts: VIP ingress identity, egress by name/bare name/VIP, sibling refusal, withdraw, kill), `hostd` `TestWorkloadService` (advertise; caller via VIP, with the remote address asserted to be the VIP; SIGSTOPped workerd withdrawn in 5.4 s and re-advertised 0.5 s after SIGCONT; redeploy without a Service withdraws and closes pooled connections), `mgmt` `TestServiceDeploy` (8 cases, spec-validated) and `TestServiceGate`;
  - real (tailnet 1, production control): `TestRealService`, E2E-SVC (`make e2e-svc`) and H12 `TestRealMgmtService` (`make e2e-mm-svc`). The numbers are in docs/services.md. All PASS, with 0 leftover devices or keys.
- **Found while testing:** a workload node and a Service with the same name share one MagicDNS name, and MagicDNS answers the device first. The first hostd test "passed" through the node, not the VIP. Tests now assert the remote address is the VIP, and mgmt refuses such names.
- **Not done:** one workload on several hosts at once (still one lease per workload). Also unverified: whether app caps granted to a `svc:` destination reach WhoIs, and a real hostd and `twd link` through `svc:twd-mgmt`.
- **Test tailnet policy (tailnet 1, backed up first to `tailnet1_acl.pre-svc-*.hujson`):**
  - `autoApprovers.services` for `svc:ttest-tssvc`, `svc:twd-mgmt`, `svc:ttest-mgmt` and `svc:ttest-svc-hello`;
  - tagOwners `tag:twd-wl-svc`;
  - grants `tag:ttest` → those Services, `tag:twd-host` → `svc:twd-mgmt`, and `tag:ttest` ↔ `tag:twd-wl-svc`.
