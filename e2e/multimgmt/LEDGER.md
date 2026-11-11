# multi-mgmt loop ledger


FIX_MODE: fix

> **Resumed 2026-11-11 in fix mode** (Ross approved): clock skew is in
> scope. Fixes run before open hypotheses. Self-fencing (fix queue F4)
> was redefined by Ross on 2026-11-11 (ADR-028 §3, now Accepted) as
> guarded moves; self-fencing is deferred until state can move.

## Fix queue (fix mode: these come before open hypotheses, in order)

| ID | Fix | For | Regression test (must fail before, pass after) | Status |
|---|---|---|---|---|
| F1 | Postgres/SQLite clock for lease acquire/renew/expiry, host `last_seen`/liveness, expiry windows; `Store.Now` hook for tests | H10 | `TestMM_H10_LeaseClockSkew` (0–20 s, both directions) | **DONE**: fails before (same 10/15 s thresholds), passes after; `make test-go`, `make test-pg`, `make lint` green |
| F2 | Skew metric in `/v1/status`; not-ready above 2 s | H10 | new `TestMM_F2_SkewGate` | **DONE** (reports, doesn't refuse; see F2 note) |
| F3 | Claim before minting (`pending → minting`, stale claim takeover); custody name per key; the loser path only deletes its own entry | H02 | `TestMM_H02_DoubleMint` | **DONE**: before 14–23/40 broken (iter2), after 0/40 and 40 keys for 40 objects, 3/3 runs; + `TestStoreMintClaim` (SQLite + Postgres), `TestCustodyNamesPerKey` |
| F5 | When `pickHost` thinks the holder is dead but its lease is still valid: keep the holder, or return a clear error (not `lease held by another holder`) | H10 (I8 side) | `TestMM_F5_LeaseHeldExplains` | **DONE** |
| F6 | Replica registry (`replicas` table: stable per-state-dir ID, custody fingerprint, last_seen by the store clock). A replica refuses to start if file custody would be shared with another live replica, or if live replicas' custody fingerprints differ; the same check runs each reaper tick (status not ok, warning) | H11 | `TestMM_H11_FileCustodyPerReplica` becomes "second replica refuses with a clear error"; unit test for the registry; a single replica restarting within 30 s of a SIGKILL must still start | queued |
| F7 | Registration throttle state in the store (`tenant_throttle`), shared by every replica and timed by the store clock | H03 | `TestMM_H03_ThrottlePerReplica` (before: 45 in a 10 s window; after: 16 = the limit, 2/2); `TestStoreSharedThrottle` (SQLite + Postgres) | **DONE** |
| F8 | Persist each host's push target in the `hosts` row at registration, so any replica can push kills and assignments; keep the in-memory map as a cache | H06 | `TestMM_H06_KillPushAcrossReplicas` (before: via r1 heartbeat after ~1.5 s; after: pushed in 13–15 ms, 2/2), `TestMM_F8_PushTargetShared` | **DONE** |
| F9 | ADR-028 §3 (Ross, second interview): mgmt marks each deployment `fence` (= `BundleStateful`) and sends it in assignments; hostd's heartbeat-loss fencing undeploys only workloads with `fence` (a missing field means fence: safe against an older mgmt). `FenceAfter` 9 → 30 s, `NamespaceLeaseTTL` 15 → 45 s, with a startup check that fence < lease − margin. Fix the stale comment | correction (iter13) | `TestMM_F9_FenceStatefulOnly`, `TestFenceBelowLease` (hostd), `TestMM_F9_AssignmentFence` (mgmt) | **DONE** (fence selection unit-tested; undeploy and redeploy on reconnect not exercised end to end, since that needs workerd: scenario S4) |
| F10 | Durable device deletes: `objects.device_delete_at` (due ms / -1 claimed), claimed by one replica, swept by every replica's reaper, retried in 30 s on failure | H07 | `TestMM_H07_DeviceDeleteLostOnCrash` (before: device left; after: deleted by the surviving replica, 2/2); `TestStoreDeviceDelete` (SQLite + Postgres) | **DONE** |
| F11 | Store-arbitrated shared secrets: OBO root keys (and the WIF issuer signing key) are chosen once per tenant/issuer through a DB row (`INSERT … ON CONFLICT DO NOTHING` names the winning key ID); custody holds each candidate under a per-key name, losers delete theirs; readers follow the row. Issuer key moves from the state dir into custody | H13 | `TestMM_H13_OBORootKeyDiverges` (b): before 12/40 diverged, after 0/40 (5 runs); `TestMM_F11_OBORootChosenOnce` (40 two-replica races + legacy adoption, -race) | **DONE for OBO root keys**; the WIF issuer key is still per state dir (F11b, queued) |
| F11b | WIF issuer signing key: load or create through custody + `secret_choice` ("issuer") instead of `<state-dir>/issuer.key`, migrating an existing file key as the first offer | H13 | `TestMM_F11b_SharedIssuer` (4 concurrent first starts serve one JWKS; restart reloads it; an existing issuer.key is kept and shared). No e2e (needs Funnel and WIF) | **DONE** |
| F12 | (a) `CreateObject`: a UNIQUE violation on insert means another replica just created it; re-read by name and return that object (the same response as the idempotent path). (b) Share `id.secret` through custody + `secret_choice` (like F11b), adopting the first replica's existing file, so object IDs and hostnames don't depend on which replica created them | H14 | `TestMM_H14_ConcurrentSameNameCreate` (before 10/10 names broken; after 0/10 ×5 alone and after H13); `TestMM_F12_SharedIDSecret`, `_ExistingFileAdopted`, `_IdentityConflictStillRefused` | **DONE** |
| F13 | Host-reported deployment status stored in `deployments.host_status/host_error`; written with `UPDATE … WHERE` the value differs (no per-replica cache: another replica may have written since) | H08 | `TestMM_H08` (r1 said `assigned`, now `active`, 2/2), `TestMM_F13_DeploymentStatusShared` (alternating replicas) | **DONE** |
| F14 | Spawn budget enforced inside the insert transaction: lock the root object row (`FOR UPDATE`; SQLite's immediate transaction serialises), count live descendants, insert or refuse `spawn_budget_exceeded` | H04 | `TestMM_H04_SpawnBudgetRace`: before 3/20 failed, after 0/30; `TestStoreCreateChildBudget` (40 concurrent, maxLive 5 → exactly 5; SQLite + Postgres) | **DONE** |
| F4 | **Guarded moves** (ADR-028 §3, Ross 2026-11-11): Deploy refuses to move a workload with DO classes off a host whose lease lapsed unless the host was removed or the request carries `forceTakeover` (CLI `--force-takeover`; audited with the old host). Per-class `"fence": "never"` exempts a class. Workers (no DO classes) unaffected. Self-fencing deferred until state can move | H10/S4 | new `TestMM_F4_GuardedTakeover` (stateful refused, forced allowed + audited, Workers-only allowed, `fence: never` allowed, after `host rm` allowed) | next |

## Status

| Item | Priority | Status | Evidence / test |
|---|---|---|---|
| Harness | P0 | slice 1 built (PG container, fake TS API, fake setec, N real replicas, unix-socket client, pg_locks probe). + slice 4: scenario runner (`chaos_test.go`): client-side load balancing over running replicas with transport retry (stands in for the svc VIP), seeded 6-worker workload (create/spawn/credential+ready/kill-cascade/status, history.jsonl), host heartbeat loop, oracle I3/I5/I6/I7/I8/I9/I11 → report.json and op-codes.txt, pending-object diagnostics; replica state mutex. + slice 3: per-replica Postgres fault proxy (pass/delay/blackhole/reset, `pgproxy_test.go`), fake TS policy endpoint, configurable twd.jsonc. + slice 2: tenant/host/deploy setup, fake host heartbeat, fakeTS hold switch and per-object key counts. TODO: PG fault proxy, front proxy, fake hosts, workload gen, PG sampler, oracle | `e2e/multimgmt/harness_test.go` |
| H01 | P0 | **REFUTED** (3/3 runs) — latent fragility noted | `TestMM_H01_StartupAdvisoryLock` |
| H02 | P0 | **FIXED by F3** (was: every double-minted object lost its credential) | `TestMM_H02_DoubleMint` |
| H03 | P1 | **FIXED by F7** (was: 3 replicas minted 45 keys in 9 s against a 60/min limit, ≈2.8× the window limit) | `TestMM_H03_ThrottlePerReplica`, `TestStoreSharedThrottle` |
| H04 | P1 | **FIXED by F14**. Was CONFIRMED (intermittent: 3/20 runs, each 6 live descendants with maxLive 5; standalone 2/15, after H03 1/5) | `TestMM_H04_SpawnBudgetRace` (writes report.txt) |
| H05 | P2 | **CONFIRMED and FIXED** (F16): a credential long-poll on one replica saw another replica's mint only after the whole wait (10.004 s for a mint at 300 ms); now 506 ms | `TestMM_H05_CredentialWaitSeesOtherReplicasMint` |
| H06 | P1 | **FIXED by F8**. Was CONFIRMED (2/2): a kill served by a replica the host didn't register with isn't pushed; the host learns at its next heartbeat (≈1.5 s here; up to `HeartbeatEvery` = 3 s with real hostd). The kill response silently omits `pushed` | `TestMM_H06_KillPushAcrossReplicas` |
| H07 | P1 | **FIXED by F10**. Was CONFIRMED: with a device-delete delay, SIGKILL of the killing replica inside the window left the device forever (control deleted) | `TestMM_H07_DeviceDeleteLostOnCrash`, `TestStoreDeviceDelete` |
| H08 | P2 | **FIXED by F13**. Was CONFIRMED: after the host reported `active` to r0, r1 said `assigned` | `TestMM_H08_DeploymentStatusPerReplica`, `TestMM_F13_DeploymentStatusShared` |
| H09 | P2 | **CONFIRMED and FIXED** (F15): concurrent reapers double-killed 19/20 objects (two kill records, tombstone overwritten); now exactly one | `TestMM_H09_ConcurrentKillOnce` |
| H10 | P0 | **FIXED by F1** (in-process, SQLite-modelled DB clock; real Postgres clock covered by `TestStoreClockPostgres`). Host-side half is F4 | `mgmt/mm_h10_test.go` `TestMM_H10_LeaseClockSkew` |
| H11 | P0 | **FIXED by F6** (was: with `-custody file`, 6/12 credential releases failed on each replica) | `TestMM_H11_FileCustodyPerReplica` |
| H12 | P1 | open | |
| H13 | P1 | **FIXED** (OBO root keys by F11, WIF issuer key by F11b). Was CONFIRMED (OBO root: 12/40 tenants diverge on cold concurrent first use, 0/40 on normal create; WIF issuer key: per state dir, by code, not run) | `TestMM_H13_OBORootKeyDiverges` |
| H14 | P2 | **FIXED by F12**. Was CONFIRMED (2/2 runs, 10/10 names): concurrent same-name creates through 3 replicas → one 202 and two `409 exists`, not the idempotent existing object | `TestMM_H14_ConcurrentSameNameCreate` |
| H15 | P2 | **CONFIRMED and FIXED** (F15): a lowered `registrationsPerMinute` never reached an existing throttle; now applied on the mint worker's next attempt, on every replica | `TestMM_H15_ThrottleFollowsTenantLimit` |
| H17 | P2 | **CHANGED (FOR SHARE), effect not isolated** (iter30). Was open (from S6: fenced commits hold the workload's lease row FOR UPDATE across several round trips; a slow replica stalls everyone's credential/ready/renew for that workload; p99 3.6 s on fast replicas) | S6 locks.txt |
| H18 | P3 | **FIXED** (iter30): store unavailable, lock timeout or idle-transaction timeout → `503 store_unavailable` with Retry-After | `TestStoreUnavailableMapping` |
| H16 | P2 | **CONFIRMED and FIXED** (F17): 2/10 runs ended with 12 live objects against maxObjects 10; after 0/20 | `TestMM_H16_TenantQuotaRace`, `TestStoreTenantQuota` |
| S2 | — | **3/3 clean seeds** (1, 2, 3) after F18 | `TestMM_S2_RandomKills` |
| S3 | — | **3/3 clean seeds** (SIGSTOP 50 s > 45 s lease) | `TestMM_S3_StallLongerThanLease` |
| S4 | — | **3/3 clean seeds** (PG blackhole 30 s, one replica) | `TestMM_S4_PostgresPartitionOneReplica` |
| S5 | — | **3/3 clean seeds** (PG restart mid-load; failures confined to the declared outage) | `TestMM_S5_PostgresRestart` |
| S6 | — | **3/3 clean seeds** after the harness got load-balancer semantics (iter30); was failing I8 at 1.3–2.2% | `TestMM_S6_SlowPostgresOneReplica` |
| S1 | — | **3/3 clean seeds** (rolling SIGTERM restarts under load) | `TestMM_S1_RollingRestart` |
| S7 | — | **clean**: S7a 3/3 seeds (all replicas killed and cold-started at once, twice, under load); S7b passes (3 concurrent `OpenStore` migrating a pre-M7 Postgres store: once, 50/50 rows intact) | `TestMM_S7a_SimultaneousColdStart`, `TestStoreConcurrentLegacyMigration` (make test-pg) |
| S8–S11 | — | 0/3 seeds each | |

## Hypotheses (seeded from a code read on 2026-11-11, commit 84b4246)

Every hypothesis below is unverified. Each one says where to look, what
would go wrong, and which invariant it threatens.

- **H01 (P0, I8): the migration advisory lock leaks across pooled connections.**
  `mgmt/sqlstore.go:153` calls `pg_advisory_lock(7464)` and
  `pg_advisory_unlock` through `*sql.DB`, which hands out a different pooled
  connection per call. These locks belong to a session, so the unlock may run
  on a different connection than the lock. The unlock then fails without an
  error, and an idle pooled connection keeps holding the lock. A second
  replica, or a restart of one, would then block forever in `migrate`.
  - **Probe:** start replica 1, then start replica 2. Also restart replica 1
    while replica 2 is up.
  - **Likely fix:** pin one connection with `db.Conn(ctx)` for both lock and
    unlock, or use `pg_advisory_xact_lock` inside a single transaction.
- **H02 (P0, I3): double mint.**
  - Every replica's `Start` re-enqueues every `pending` object
    (`mgmt/service.go:156`).
  - `mintOne` (`mgmt/service.go:~1108`) checks then acts: it reads that the
    object is pending, calls `CreateKey`, writes custody with `Put`, then
    updates the object.
  - So two replicas can mint for the same object: two keys, and the
    second custody `Put` overwrites the first.
  - **Probe:** have pending objects in the DB, then start 3 replicas at once
    (S7). Repeat with S2 kills during a create burst.
  - **Likely fix:** claim the object with a conditional `pending → minting`
    (replica, claimed_at) before calling `CreateKey`, and expire stale
    claims.
- **H03 (P1, I4): throttles are per replica.**
  `m.throttles` lives in memory, so the real registration rate is N × 200/min.
  That breaks ADR-017 and risks 429s from control.
  - **Fix options:** a token bucket in Postgres; or divide the rate by the
    live replica count; or make one replica the minter, chosen by an advisory
    lock. That last one is leader election only for minting.
- **H04 (P1, I5): spawn budgets race.**
  `m.spawnMu` is per replica (already known: `STATUS.md:252`), so concurrent
  spawns on different replicas can exceed maxLive.
  - **Likely fix:** `SELECT … FOR UPDATE` on the root object row inside the
    transaction that counts and inserts.
- **H05 (P2, I8 latency): credential long-polls wait out their timeout.**
  `Credential` waits on `m.waiters` (`mgmt/service.go:1161`). If a host
  long-polls replica B while replica A does the mint, B is never notified and
  waits its full timeout. Measure create → ready p99 under `random` routing
  against `sticky-per-client` routing.
- **H06 (P1, I6): kill push only works from the replica the host registered
  with.**
  `m.pushURLs` lives in memory and is filled at host registration
  (`mgmt/service.go:407`). A kill served by another replica can't push, so
  delivery falls back to heartbeat revocations and the < 1 s kill SLO is lost.
  - **Fix:** persist push URLs in the hosts row.
- **H07 (P1, I6): device deletes are lost on a crash.**
  Device deletes are scheduled through `m.pending` in memory
  (`mgmt/service.go:1593`). If a replica crashes after a kill and before the
  delayed delete runs, the device is never deleted.
  - **Probe:** S2 timed right after kills; then check fake TS API devices at
    quiescence.
  - **Fix:** a durable outbox table.
- **H08 (P2, I10): deployment status depends on the replica.**
  `m.depStatus` lives in memory and is filled by heartbeats. The same
  deployment's status differs depending on which replica answers.
- **H09 (P2, I7): every replica's reaper races the others.**
  All replicas run the reaper. Look for duplicate kill audit records, error
  noise, and wasted work. It may be correct but wasteful.
- **H10 (P0, I1/I2): leases use each replica's own clock.**
  `AcquireLease` and `RenewLease` take `now` from the replica's own clock. If
  one replica's clock is ahead, it sees live leases as expired and can steal
  them, which breaks I1.
  - **Probe:** S8.
  - **Likely fix:** use Postgres `now()` inside the SQL (one clock), and keep
    the replica clock out of expiry decisions.
- **H11 (P0, config): file custody is per replica.**
  With the default `-custody file`, a key minted on replica A sits in A's
  state dir, so replica B can't release it. `-db postgres://` together with
  `-custody file` should be refused (or warned about loudly) whenever more
  than one replica might run. The same goes for OBO root keys (`m.oboKeys`)
  and the WIF issuer key in the state dir (see H13).
  - **Probe:** run the harness with file custody once to show the failure,
    then use fake setec.
- **H12 (P1, I8, design): hosts can't fail over to another replica.**
  - Hosts take a single `-mgmt` address (`cmd/twd-hostd/main.go:52`).
  - Replicas can't all register the tsnet name `twd-mgmt`.
  - `svc:twd-mgmt` (Tailscale Services) isn't wired up.
  - So without a VIP, hosts have no failover path. The harness front proxy
    stands in for the VIP. Record what the real fix needs.
- **H13 (P1): per-replica secrets diverge.**
  OBO root keys and the WIF issuer key are created or loaded per replica.
  Two replicas may each create a different OBO root key for the same tenant,
  or serve different issuer keys from the same issuer URL.
  - **Probe:** create a tenant through A, then sign through B; verify across
    replicas.

- **H14 (P2, I8/I10): each replica has its own `id.secret`.**
  `cmd/twd-mgmt/main.go` loads `id.secret` from each replica's local state
  dir. That key is the HMAC key for object IDs, which double as node
  hostnames (`mgmt/service.go:781`).
  - **Consequence 1:** an object's ID depends on which replica created it.
  - **Consequence 2:** `tenantLock` is per replica too. Two replicas creating
    the same (tenant, workload, ns, name) at once both miss
    `GetObjectByName`, then both insert. One hits the
    `UNIQUE (tenant_id, workload, namespace, name)` constraint, and that
    likely surfaces as a 500 instead of the idempotent 200 with the
    existing object. No split brain, because the constraint holds.
  - **Fix:** share the secret (custody/setec), and map a unique violation
    to "return the existing object".
  - **Probe:** concurrent create of the same name through 2 replicas.

## Findings

### H02: CONFIRMED (P0). A double mint destroys the object's credential.

**Repro.** Run `TestMM_H02_DoubleMint`:
1. Create 40 objects while the fake TS API answers 503, so they all stay
   pending.
2. SIGKILL the replica, then start 3 replicas at once.
3. Release the hold.

This failed 4 of 4 times. Each run made 61–68 keys for 40 objects; 14–23
objects were minted 2–3 times. **Every double-minted object** then had no
valid credential:
- `POST /v1/objects/{id}/credentials` → `404 custody: not found`;
- the object is stuck in `minted` and can never join.

The extra keys themselves were deleted, so no live duplicate keys leaked.

**Root cause (`mgmt/service.go`).**
- `Start` (`:156`) re-enqueues every pending object on every replica.
- `mintOne` (`:~1108`) checks the status, then calls `CreateKey`, with
  nothing to claim the object in between.
- The loser calls `custody.Put` under the **same name**
  (`custodyName(o)` = `twd/<tenant>/<object>/authkey`), overwriting the
  winner's key.
- Its fenced `UpdateObject` then finds status `minted`, so it sets
  `killed=true` and runs `api.DeleteKey` **and `custody.Delete(custodyName(o))`**
  (`:~1137`). That deletes the winner's credential.

**When it happens.** Any replica that starts while others are minting,
which includes every rolling restart. A single replica can't trigger it.

**Fix sketch** (any of the three stops the corruption; do all three):
1. **Claim before minting.** Conditional update
   `pending → minting(claimed_by, claimed_at)`. Only the claimer calls
   `CreateKey`. A claim older than about 30 s can be stolen.
2. **Key custody per key.** Use `twd/<tenant>/<object>/authkey/<keyID>`, and
   have `Credential` read the name for the row's `key_id`, so writers can't
   overwrite each other.
3. **Make the "not pending" path delete only its own custody entry**, never
   the object's.

**Follow-ups.**
- Does a stuck `minted` object without custody ever recover? KeyExpiry is
  5 min, and the remint path is `reason=rejoin_rejected`. Check this under
  I9 in the soak.
- Add an in-process unit regression test in `mgmt/` (two `Mgmt` on one PG
  store); the e2e test is the repro for now.

### H10: CONFIRMED (P0, conditional on clock skew). A replica's clock decides who holds a lease.

**Repro.** `go test -tags e2emultimgmt -run TestMM_H10 ./mgmt/` runs two
`Mgmt` replicas on one store, with offset clocks:
- h1 heartbeats every 2 s through one replica;
- a redeploy of the same workload is served by the other replica, 1 s
  after h1's last heartbeat;
- the skew is swept from 0 to 20 s, with the other replica either ahead or
  behind.

**Results** (deterministic, the same in both directions):

| Skew | Outcome | Invariant |
|---|---|---|
| ≤ 9 s | correct: the workload stays on h1 | — |
| 10–12 s | the redeploy is **refused** (`lease held by another holder`) although h1 is healthy | I8 |
| ≥ 15 s (≥ 12 s ahead on Postgres) | the workload **moves to h2** (epoch+1) although h1 is healthy | **I1** |

When the workload moves, h1 keeps serving its DO, with its local state,
until its next heartbeat comes back with `revoke`. Meanwhile h2 starts the
same DO with **empty** state. That's a brief two-instance window, plus the
state of every object in that workload lost on h2. Epoch fencing stops h1
from getting new credentials, but not from serving.

**Root cause.** Every expiry decision uses the calling replica's wall clock:
- `TouchHost` writes `last_seen = replica now` (`mgmt/sqlstore.go:523`);
- `Host.Live(now)` compares against `HostLeaseTTL` = 10 s
  (`mgmt/types.go:85`), which is the 10 s threshold;
- `AcquireLease`/`RenewLease` write and compare `expires_at` against
  replica `now` (`mgmt/sqlstore.go:946`, `:967`), with
  `NamespaceLeaseTTL` = 15 s, which is the 15 s threshold.

**Is it realistic?** NTP-synced machines are within milliseconds, so this
needs a broken clock: a VM resumed from suspend, a host with NTP blocked, or
a laptop replica. With `twd up` on Macs, sleep/wake is a plausible source.
Nothing detects or refuses skew today.

**Fix sketch.**
1. Make Postgres the only clock. Compute `expires_at`/`last_seen` with
   `now()` inside the SQL (the SQLite equivalent is fine for one replica),
   and compare with `now()` too; take replica `now` out of lease and
   liveness decisions.
2. Have `GET /v1/status` report each replica's skew against the database
   (`SELECT now()`), and refuse to serve beyond about 2 s.
3. Separately (also seen here): `pickHost` choosing a new host while the
   old lease is still valid gives a confusing "lease held by another
   holder". It should fall back to the lease holder, or say "the current
   host looks dead to this replica".

### F1: Postgres server time is the shared clock (fixes H10)

- **`mgmt/storeclock.go`:** `dbClock` samples Postgres `clock_timestamp()`
  at open and every 5 s. Between samples it extrapolates with the local
  **monotonic** clock (no round trip per call, immune to wall-clock steps),
  and it keeps extrapolating when a sample fails. SQLite uses the local
  clock, since there's only one replica.
- **`SQLStore`:** gains `Now()`, `ClockOffset()` (for F2) and `SetClock()`
  (tests). `Mgmt.now()` uses the store clock whenever the store provides
  one. That covers every lease, liveness, key-window, revocation and TTL
  decision (~31 call sites) with one change.
- **Still on the local clock** (not ownership decisions): the throttle
  token bucket, the policy cache max age, the JWT issuer `iat`/`exp`, and
  local timers (`After`).
- **Tests:**
  - `TestMM_H10_LeaseClockSkew`: the store clock is true time and the
    replica clocks are skewed. It passes from 0 to 20 s in both directions,
    and fails with `Mgmt.now()` reverted.
  - `TestDBClockExtrapolates` (in `make test`).
  - `TestStoreClockPostgres` / `TestStoreClockSQLite` (in `make test-pg`):
    `Mgmt.now()` follows the server even with the replica clock 1 h off.

### F2: skew is reported in `/v1/status` (warns, doesn't refuse)

**A deliberate change from ADR-028 §2's "not-ready".** After F1, the
replica's wall clock no longer drives any lease, liveness or expiry
decision. Refusing traffic on skew would cost availability and buy no
correctness.

What remains on the local clock:
- the WIF issuer JWT `iat`/`exp`, where skew can make Tailscale reject
  token exchanges;
- the throttle;
- the policy cache.

Those deserve a warning, not an outage. So F2 does the following:
- `GET /v1/status` gains `clock {source, ok, offsetMs, syncAgeMs, message}`;
- the top-level `ok` becomes `clock.ok`;
- `ok` is false beyond `MaxClockOffset` = 2 s, or when the last sample is
  older than `MaxClockSyncAge` = 30 s;
- `checkClock` logs a warning once per change between ok and not ok (at
  `Start` and every reaper tick).

The spec came first (`StatusClock` in `api/openapi.yaml`), then
`make api`. The regression test is `TestMM_F2_SkewGate` (in `make test`).

### F3: one mint per object (fixes H02)

1. **Mint claims.** New `objects.mint_claim` / `mint_claimed_at` columns.
   `ClaimMint` is a conditional UPDATE: it succeeds iff the object is
   pending with no key, and is unclaimed, claimed by this replica, or
   claimed more than 60 s ago (`MintClaimTTL`). Each replica gets a
   per-process ID (`rp_…`).
   - The claim covers **one attempt** and is released on every exit (a
     `defer`). The first version held it through 503 backoff: a SIGKILLed
     replica then blocked all 40 objects for the full 60 s (runs took
     66 s instead of 12 s). Releasing per attempt fixed that, and runs are
     back to 12–13 s.
2. **Custody named per key** (`…/authkey/<keyID>`). `Credential` falls back
   to the legacy `…/authkey` name for objects minted before the upgrade.
   Ready and kill delete both names.
3. **The losing path undoes only its own work:** its key and its own
   custody entry. It also bails if the row already has a `KeyID` (in case a
   claim expired mid-attempt). So correctness doesn't depend on the TTL;
   the TTL only prevents wasted registrations.
4. **Pending sweep.** Each replica sweeps pending objects at `Start` and on
   every reaper tick (deduplicated per replica via `m.queued`), so an
   object whose claimer died is retried once its claim goes stale.

Mint delay after a claimer crashes mid-attempt: at most `MintClaimTTL`
(60 s) plus one reaper interval.

### F5: a still-valid lease gets an explanation, not "lease held by another holder"

After F1, this happens only in one window: a host has stopped
heartbeating for more than 10 s (`HostLeaseTTL`), so placement treats it
as dead, but its 15 s lease hasn't expired yet.

The deploy now fails with `409 lease_held` (same catalog code), and:
- the message names the holder, the epoch, when this mgmt last saw it, and
  when the lease expires;
- `Retry-After` is set to the seconds left on the lease (a new
  `Error.RetryAfter` field, sent as a header by `writeErr`).

After the lease expires, the redeploy lands on a live host (asserted in
the test).

The hint says to pin a host with the deploy request's `host` field. The
CLI has no `--host` flag (only the API field), so the hint doesn't claim
one. Whether to add the flag is open.

### H11: CONFIRMED (P0, configuration). File custody is per replica.

**Repro.** `TestMM_H11_FileCustodyPerReplica`: two replicas with
`-custody file` (the default), and 12 objects created alternately through
each.

**Result.** Every credential fetch through the replica that did *not*
mint the object fails with `404 custody: not found`: 6 of 12 on each
replica, and none fail on both. A host that sticks to one replica fails to
get about half its credentials. The same applies to anything else stored
in each replica's own state dir:
- OBO root keys (`m.oboKeys`, loaded from custody): H13;
- the WIF issuer key: H13;
- `id.secret`: H14.

**Fix (F6).** Don't try to share file custody; detect it and refuse to
start. The stable replica ID matters because without it, a single replica
restarting after a crash would see its own old row and refuse.

### F6: replica registry (fixes H11 as a refusal)

**Registry.** A new `replicas` table holds `(id, custody, started_at,
last_seen)`. Each replica writes its row at `Start` and every reaper tick,
using the store clock. A clean `Close` sets `last_seen = 0`. A replica
counts as live if seen within `ReplicaLiveTTL` (30 s).

**Stable IDs.** The replica ID lives in `<state-dir>/replica.id`
(`Config.ReplicaID`). It's also the mint-claim owner, so a restarted
replica can immediately reclaim its own claims.

**`CustodyFingerprint`:**
- `file:<host>:<dir>`;
- `setec:<url>` (trailing `/` trimmed).

**`Start` refuses** (and clears its own row) in three cases:
- this replica uses file custody and another replica is live;
- a live replica uses file custody;
- live replicas' fingerprints differ.

The message names the other replica and says to use
`-custody setec -setec-url <same URL>`. A conflict that appears later is
logged once and reported in `/v1/status` as
`replicas {id, live, ok, message}`, and the top-level `ok` includes it
(spec first, `StatusReplicas`).

**Tests:**
- `TestMM_F6_ReplicaRegistry` (unit): file+file refused; a crash-restart
  with the same ID allowed; a clean shutdown frees the store; same setec
  allowed; a different setec refused; file joining setec refused.
- `TestMM_H11_FileCustodyPerReplica` (e2e) now asserts that r1 exits with
  a clear refusal, r0 is unaffected, and r0 restarts 105 ms after SIGKILL.
- `TestMM_H01` and `TestMM_H02` (setec) still pass.

**Caveat.** The registry refreshes on the reaper tick. A
`-reap-interval` of 30 s or more makes replicas look dead between ticks,
so conflicts that appear later go undetected. The startup check still
works.

**My mistake:** a YAML flow-mapping comma in the new schema description
split the field. The spec loader caught it in the `mgmt` tests, and I
quoted it.

### H03: CONFIRMED, then FIXED by F7. The registration throttle was per replica.

**Repro.** `TestMM_H03_ThrottlePerReplica`: 3 replicas, a tenant limited to
60/min (burst 6, then 1/s), and 45 objects created at once across the
replicas.

**Before.** 45 keys in about 9 s (5/s): all 45 inside one 10 s window,
against a limit of 16. That's three independent buckets (3×6 burst plus
3×1/s). Same result in 2 of 2 runs.

**F7.**
- The throttle state (tokens, AIMD rate, pause, failures) is a
  `ThrottleState` held in a `tenant_throttle` row.
- `UpdateThrottle` reads, applies and writes it in one transaction:
  `FOR UPDATE` on Postgres; SQLite's immediate transaction holds the
  write lock.
- The bucket logic is unchanged, and the in-memory path remains for
  stores without shared state.
- Shared throttles are timed by the store clock (`storeTimedClock`).
- If the store can't be read, `Reserve` fails closed (waits 1 s).

**After.** 45 keys in 45–48 s, with at most 16 in any 10 s window (the
limit exactly), in 2 of 2 runs. `TestStoreSharedThrottle` (SQLite and
Postgres) checks three things:
- 40 concurrent reservations from two replicas on a frozen clock get
  exactly one burst (6);
- a backoff on one replica pauses the other and halves the shared rate;
- a success on one is seen by the other.

**Side effect.** `TestMM_H02` now takes about 18 s instead of 9–13 s,
because a 503 backoff now pauses the whole tenant, not one replica. That's
correct for ADR-017.

- **H15 (P2, found while reading, not multi-replica-specific):**
  `Throttle.SetLimit` is never called, so `PATCH /v1/tenants/{id}` with a
  new `registrationsPerMinute` doesn't reach a throttle that already
  exists (until a restart). With F7 the limit isn't stored in the shared
  row, so each replica clamps to its own idea of the limit. Fix: call
  `SetLimit` on tenant update. Check that it's really missing.

### F4: guarded moves (ADR-028 §3, as decided by Ross)

**`guardTakeover`** runs in `Deploy` after placement and before
`AcquireLease`. It **refuses with `409 takeover_unconfirmed`** when all of
these hold:
- the workload's lease holder is a different host;
- that host is **still registered and not live**;
- the bundle is stateful.

The message names the host, when mgmt last heard from it, and the guarded
classes. It gives three ways out: `twd host rm <h> --drain`,
`--force-takeover`, or `"fence": "never"`.

It **allows** the move when:
- the old host was removed;
- the old host is live (the existing `lease_held` refusal applies there);
- the workload is stateless;
- every DO class is `"fence": "never"`;
- the deploy sets `forceTakeover`. A forced move is audited on the
  `deploy` record with `takeoverFrom` and `forced`.

**`BundleStateful`** is conservative. It counts as DO classes:
- `twd.jsonc` `tailnet.objects`, including `node:false` classes;
- `wrangler.jsonc`/`.json` `durable_objects`.

A `wrangler.toml`, or anything unreadable, counts as stateful.
`node_modules/` is ignored.

**Spec and CLI.**
- New `DeploymentCreate.forceTakeover` and catalog code
  `takeover_unconfirmed` (spec first).
- CLI: `twd deploy --force-takeover`.
- New `twd.jsonc` field: `tailnet.objects.<Class>.fence`.

**Found along the way.** A pinned move away from a **live** holder was
already refused (`lease_held`), but F5's message said "redeploy after the
lease expires", which never happens for a live, renewing holder. It now
says the host is live and to drain it first.

**Tests.**
- `TestMM_F4_GuardedTakeover` (7 cases) and `TestBundleStateful` (8 cases).
- The F5 test now uses a confirmed takeover; F5's waiting-out-the-lease
  case only arises after confirmation.

**Not run:** the real-tailnet e2e suites that redeploy across hosts.
Hermetic `make test-go` and `make test-pg` are green.

### CORRECTION (iteration 13): hostd already self-fences

What I told Ross on 2026-11-11, and wrote into ADR-028 as the premise for
"guarded moves now", was **wrong**: "hostd only stops serving on an
explicit revoke and keeps serving forever when cut off from mgmt". I had
searched only for `revoke`.

In fact, since M3 (`ede3258`):
- `hostd/host.go` `tick()` runs a heartbeat every 3 s (`HeartbeatEvery`).
- After `FenceAfter` = 9 s without a successful heartbeat (measured on the
  monotonic clock), it calls `fenceAll`, which **undeploys every workload,
  stateless Workers included**.
- When mgmt is reachable again, `syncAssignments` deploys them back.
- `FenceAfter` has no flag. Its comment ("below the 10 s lease TTL") is
  stale: `NamespaceLeaseTTL` is 15 s and `HostLeaseTTL` is 10 s.

**Consequences:**
- The split brain I described (a partitioned host serving forever) is
  already prevented, as long as `fenceAll` really stops serving, because
  9 s is less than the 15 s lease.
- The cost is availability: any mgmt or Postgres outage of more than 9 s
  stops **every** workload on **every** host. That includes a routine
  Postgres restart or failover, which is often 10–30 s; S5 will show it.
- Ross's decision ("hosts stay fail-static; Workers keep serving")
  contradicts the current code. ADR-028 §3 needs re-deciding with the
  facts. **Re-decided** by Ross in a second interview: fence stateful
  workloads only, at 30 s fence / 45 s lease (F9).
- F4 (guarded moves) stays valid: it prevents moving a stateful workload
  onto empty state before the old host is confirmed gone.

### F8: push targets persisted (fixes H06)

- **Storage.** A new `hosts.push_target` column (`Host.PushTarget`, `json:"-"`,
  so no API change). It holds `unix:///…` in local-dev mode, or
  `http://<node addr>:<port>` in tsnet mode, and is written by whichever
  replica serves the registration.
- **Lookup.** `pushTarget()` reads the per-replica cache first, then the
  row.
- **Stale caches.** `push()`, on failure, drops the cached entry, re-reads
  the row, and retries once if the target changed (the host re-registered
  through another replica).
- **Tests:**
  - `TestMM_F8_PushTargetShared`: B pushes to a host registered with A,
    and follows a re-registration after the old listener is gone.
  - e2e `TestMM_H06`: kills via either replica are pushed in 12–15 ms.

### F9: hosts self-fence stateful workloads only, at 30 s; lease 45 s (ADR-028 §3)

**mgmt.**
- `Deployment.Fence` is `BundleStateful(bundle)`, or true when there's no
  bundle.
- It's stored in a new `deployments.fence` column. Existing rows default
  to 1 (fence), the safe value.
- It's sent as `fence` in assignments (spec first).
- `NamespaceLeaseTTL` went from 15 s to 45 s.

**hostd.**
- Each workload remembers `noFence` from its assignment. A missing field
  (an older mgmt) means fence.
- Heartbeat loss now calls `fenceStateful`, which undeploys only
  `fences()` workloads (selection lives in `fenceTargets`). The next
  successful sync redeploys them, as before.
- `DefaultFenceAfter` is 30 s (was 9 s). `MaxFenceAfter` is 40 s, and `New`
  refuses anything larger.
- New flag: `twd-hostd -fence-after`.
- The stale "below the 10 s lease TTL" comment is replaced.

**Tests.**
- hostd: `TestMM_F9_FenceStatefulOnly` (stateful and older-mgmt are
  fenced, stateless isn't) and `TestFenceBelowLease` (`MaxFenceAfter` + 5 s
  ≤ `mgmt.NamespaceLeaseTTL`; default and maximum enforced).
- mgmt: `TestMM_F9_AssignmentFence` (Workers-only → false, DO → true,
  `fence:never` → false).
- The F4 and F5 tests were updated for the 45 s lease.

**Not run.** The real undeploy and redeploy on heartbeat loss needs
workerd. That's scenario S4 (Postgres blackhole for one replica) and an
mgmt outage scenario, both future.

**Behavior change to note.** Takeover of a dead host's workload
(confirmed, or stateless) now waits up to 45 s instead of 15 s. Ross
accepted this.

### H07: CONFIRMED, then FIXED by F10. Device deletes lost on a crash.

**Repro.** `TestMM_H07_DeviceDeleteLostOnCrash` with
`-device-delete-delay 3s` (`twd up` uses 8 s):
- **Control:** kill through r0 with r0 staying up. The device was deleted
  after about 3 s.
- **Crash:** kill through r0, then SIGKILL r0 0.5 s later. The device was
  **never** deleted: not by r1 (whose reaper kept running), and not by r0
  after its restart.

The delete existed only as an in-memory `time.AfterFunc`. A failed delete
wasn't retried either, which affects a single replica too.

**F10.**
- The kill writes `device_delete_at` (now + delay) before scheduling the
  local timer, which is kept for promptness.
- Each runner (the timer, `FlushDeletes` (forced), or any replica's reaper
  sweep via `DueDeviceDeletes`) first `ClaimDeviceDelete`s, which sets it
  to -1, so exactly one runs it.
- A retryable failure makes it due again in `DeviceDeleteRetry` (30 s);
  "unavailable" (no devices scope) doesn't retry.
- 404 already counts as success in `tsapi`.
- The inline (no-delay) delete inside `Kill` stays unaudited on its own,
  because the kill record carries `deviceDeleted`. My first version added
  a separate audit record, which `TestAPIEndToEnd` caught, so I kept the
  existing audit contract.

**After.** The crash case was deleted by r1's sweep in 2 of 2 runs.
`TestStoreDeviceDelete` passes on SQLite and Postgres.

### H13: CONFIRMED (P1). Per-replica secrets diverge.

**OBO root key (run).** `oboKey` does `Get` → not found → generate →
`Put` → re-`Get` → cache per replica. Two replicas racing can each keep a
different key: the last `Put` wins in custody, but the first replica has
already cached its own.

`TestMM_H13_OBORootKeyDiverges`, 3 replicas, 5 runs:
- **(a) normal create:** 0 of 40 tenants diverged. The create response
  makes the key on the creating replica first.
- **(b) cold concurrent first use** (the key absent from custody, as for
  tenants that predate OBO keys or after a custody loss; caches cold;
  first `GET /v1/tenants/{id}` on all replicas at once): **12 of 40
  tenants diverged** (5, 0, 3, 3, 1 per run).

The impact: OBO assertions signed by one replica don't verify against the
public key another replica publishes.

**WIF issuer key (by code, not run; needs Funnel and WIF).**
`LoadOrCreateIssuer(<state-dir>/issuer.key)` runs per replica, so each
replica would serve, and sign with, a different key behind one issuer URL.

**Fix: F11.**

### F11: OBO root keys are chosen once, through the store (fixes the OBO part of H13)

- **Arbiter table.** New `secret_choice (name, key_id, created_at)`;
  `ChooseSecret` inserts the first offer and does nothing on conflict, then
  returns the winner.
- **`oboKey`, uncached:** read the choice. If there's none:
  - if a pre-F11 key exists under `twd/<t>/obo-root`, offer `legacy`, so
    every replica adopts it unchanged;
  - otherwise put a new seed under `twd/<t>/obo-root/<obo_id>`, then offer
    it.

  A loser deletes its own candidate. Everyone loads the winner's seed.
- **Tenant delete** removes the chosen key, the legacy entry and the choice
  row.
- **e2e (b)** now clears the choice row and every custody entry (prefix),
  so the cold state is equivalent to the pre-fix probe. Against the old
  code the row delete is a no-op and the prefix delete covers the legacy
  name, so the before and after numbers are comparable: **12/40 → 0/40**.

### F11b: the WIF issuer key is shared through custody and `secret_choice`

`twd-mgmt` now calls `LoadSharedIssuer(ctx, url, <state-dir>/issuer.key,
store, custody)`:
- **Choice row:** `issuer:<url>`.
- **Candidate:** the replica's existing `issuer.key`, which keeps keys
  Tailscale may already trust, or a new P-256 key. It's stored at
  `twd/mgmt/issuer/<iss_id>`, the first offer wins, and losers delete
  theirs.
- **The old key file is left in place** and no new files are written.
  `LoadOrCreateIssuer` stays for callers outside `twd-mgmt`.

The test is `TestMM_F11b_SharedIssuer` (unit, `-race`). An end-to-end WIF
check needs Funnel on a real tailnet: run `make e2e-keyless` before
release.

### H14: CONFIRMED (P2). Concurrent same-name creates aren't idempotent across replicas.

**Repro.** `TestMM_H14_ConcurrentSameNameCreate`: 3 replicas, the same
`(namespace, objectName)` posted through all 3 at once, 10 names, 2 runs.

**Result.** Every name got one `202` and two `409 exists` (the UNIQUE
constraint maps to `ErrExists`; not a 500 as I'd guessed). A single
replica returns the existing object for a repeated create, because
`tenantLock` serialises lookup and insert. Across replicas, the lookups all
miss and only one insert wins.

**When it matters.** It needs creates for one object spread over replicas
at once: a host behind a load-balanced mgmt (`svc:twd-mgmt`) retrying, or
two callers touching a new DO together. There's no split brain, because
the constraint holds.

**Separately: `id.secret` is per state dir.** The same name created
through different replicas (e.g. after a purge) gets a different ID and
hostname. Fix in F12(b).

### F12: idempotent creates across replicas, and a shared ID secret (fixes H14)

**(a) Insert conflicts.** An insert hitting the UNIQUE constraint
re-reads by name and returns the existing object (or 410 if it's dead).
A lookup error other than not-found is surfaced as-is, not mislabelled.

**(b) Shared ID secret.** `twd-mgmt` loads the ID secret with
`LoadSharedIDSecret` (custody plus a `secret_choice` "id-secret" row,
adopting the first replica's existing `id.secret`). Object IDs and
hostnames no longer depend on which replica created them.

**Two regressions, caught and fixed before commit:**
1. **The second file-custody replica crashed instead of refusing.** With
   file custody, it found the chosen ID secret missing from its own
   custody and exited with `custody: id secret …: not found`, before F6's
   clear refusal could run; `TestMM_H11` caught it. Fix: `twd-mgmt` runs
   `PreflightReplica` (the F6 custody check, now `replicaConflictFor`)
   before loading any shared secret. The missing-secret error also
   explains itself now.
2. **The by-ID pre-check became racy.** Before (b), replicas derived
   different IDs, so a by-ID hit after a by-name miss meant a real
   collision. With a shared secret, a concurrent creator's row produces
   exactly that pattern. It showed up only when H14 ran after H13 (1–2
   names out of 10). Fix: compare identities; the same identity is
   idempotent, and a different one is still `identity_conflict`
   (`TestMM_F12_IdentityConflictStillRefused`, a new test, since no test
   covered genuine collisions).

**After.** The full multi-replica e2e suite (H01–H14) passes, as do
`make test-go` and `make test-pg`.

### F13: deployment status is shared (fixes H08)

- **Before:** host-reported status lived in each replica's memory. After
  the host reported `active` to r0, r1 said `assigned`, so `twd deploy`
  (which waits for `active`) could wait on the wrong replica behind a
  load-balanced mgmt.
- **Now:** `deployments.host_status/host_error`, written by `setDepStatus`
  with a single `UPDATE … WHERE (host_status<>? OR host_error<>?)`. Repeat
  heartbeats cost a lookup, not a write, and `deploymentStatus` reads the
  row. The `depStatus` map is gone.
- **My first version** skipped the write when this replica's cache already
  held the same value. That's wrong when the host alternates replicas:
  active via A, failed via B, active via A again would leave the store at
  `failed`. Replaced with the SQL comparison;
  `TestMM_F13_DeploymentStatusShared` covers that sequence.

### H04: CONFIRMED (intermittent). The spawn budget is exceeded by one.

The race the code suggested finally showed up. In the full suite
`TestMM_H04` failed once; I couldn't diagnose it at first because the test
only logged to stdout. It now writes `report.txt`. Targeted reruns:
- **standalone:** 2 of 15 runs failed;
- **after H03:** 1 of 5 failed;
- every failure was **6 live descendants with `maxLive` 5**, one too many.

**Cause.** `checkSpawn` runs `CountLive`, and the insert follows later.
Only the per-replica `tenantLock` sits in between, so two replicas can
both see 4 and both insert. **Fix: F14.**

### F14: spawn budget enforced atomically (fixes H04)

- `SQLStore.CreateChild(ctx, o, maxLive)` runs one transaction: lock the
  root row (`FOR UPDATE`; SQLite's immediate transaction holds the write
  lock), count live descendants, then insert or return `ErrSpawnBudget`.
- `CreateObject` uses it for spawns. The earlier `checkSpawn` count stays
  as a fast path with the friendly message; the transaction is the
  authority. A late refusal is `429 spawn_budget_exceeded` and is audited
  as `deny`, like the fast path.
- **Tests.** e2e H04 failed 0 of 30 (before: 3 of 20).
  `TestStoreCreateChildBudget` (40 concurrent spawns, `maxLive` 5) inserts
  exactly 5 on SQLite and Postgres.
- **Flaky test found along the way.** `TestV1Pagination` failed once in
  `make test-pg` ("audit walked 12, unpaged 13"). It walks the audit log
  while asynchronous `mint` records are still landing. That's a
  pre-existing race, probably widened by the extra round trips from F3 and
  F7. The test now waits for all 5 `mint` records before walking: 30 of 30
  pass, and `make test-pg` passed 4 of 4.
- **Not addressed, noted:** the tenant-wide `MaxObjects` quota
  (`CountLive` then insert) has the same shape across replicas. A
  per-tenant lock would serialise every create in a tenant. Tracked as H16.

### F15: one winner per kill (H09); throttle follows the tenant's limit (H15)

**H09.** `Kill` read the object, then updated it, without re-checking
inside the write. Two replicas' reapers, or a reaper and an operator,
could both "kill" the same object. In
`TestMM_H09_ConcurrentKillOnce` (2 replicas, 4 concurrent `Reap` passes, 20
TTL-expired objects), **19 of 20 got two `kill` audit records**, and the
second write overwrote the tombstone (`KillReason`, `KilledBy`,
`KillRoot`). The update function now returns `errAlreadyDead` when the row
is no longer live; that object is skipped silently, and the first kill's
tombstone and audit stand. After: 1 record per object.

**H15** (pre-existing, single replica too). `m.throttle(t)` cached the
throttle and never called `SetLimit`. In
`TestMM_H15_ThrottleFollowsTenantLimit`, lowering the limit to 6/min left
the throttle at 60/min. `throttle(t)` now applies a changed limit. The mint
worker re-reads the tenant on every attempt, so every replica follows a
`PATCH`.

### F16: credential long-polls see other replicas' mints (H05)

- **Before.** `Credential` waits on `m.changed(id)`, a per-replica
  notification, then re-reads at the deadline. hostd asks with `wait=10`.
  With F3, any replica may mint an object, so a host polling replica B
  while A minted waited the whole 10 s:
  `TestMM_H05_CredentialWaitSeesOtherReplicasMint` measured **10.004 s**
  for a mint at 300 ms. That's create-to-ready latency, not an error.
- **Now.** The wait also wakes every `credentialPoll` (250 ms) to re-read
  the object: **506 ms**. The cost is one `GetObject` per 250 ms per
  waiting request.

### F17: the tenant quota is enforced in the insert transaction (fixes H16)

- **Before.** `TestMM_H16_TenantQuotaRace` (3 replicas, quota 10, 9 used,
  30 concurrent creates, 5 ms DB delay) ended with **12 live objects in 2
  of 10 runs**: one extra per additional replica.
- **`SQLStore.CreateObjectChecked(o, maxObjects, maxLive)`.** One
  transaction:
  - lock the tenant row and refuse with `ErrQuota` when the tenant is full;
  - for spawns, lock the root row and refuse with `ErrSpawnBudget`;
  - then insert.

  Locks are always taken tenant first, then root. `CreateChild` (F14)
  delegates to it with no quota.
- **Wiring.** `CreateObject` uses it for every create. A late refusal is
  `429 object_quota_exceeded`, the same code as the fast path.
- **After.** H16 failed 0 of 20 and H04 0 of 10. `TestStoreTenantQuota`
  (40 concurrent creates, quota 7) inserts exactly 7 on SQLite and
  Postgres.
- **Trade-off.** Creates within one tenant are now serialised across
  replicas: one short transaction each. They were already serialised per
  replica.

### F18: throttle tokens are spent only on real mint attempts (found by S2 seed 3)

- **Symptom.** S2 seed 3 (60 s of random SIGKILLs under a create-heavy
  load) violated **I9**: 9 objects, then 7 on a rerun, were still `pending`
  120 s after quiescence. They were the last objects created; none had a
  claim or a key request. No warnings were logged.
- **Cause: F3 and F7 interacting.** Every replica's pending sweep queues
  every pending object, and `mintWorker` took a shared throttle token
  **before** `mintOne`, which then usually did nothing (another replica's
  claim, or already minted). With 3 replicas, up to about 2/3 of the
  tenant's shared budget was wasted, so a backlog drained at about a third
  of the configured rate.
- **Fix.** `mintOne(ctx, t, id, th)` now claims first, then waits for a
  token, then refreshes the claim (it may have gone stale during a long
  wait), then calls `CreateKey`. `Success` (the AIMD increase) counts only
  attempts that created a key.
- **After.**
  - S2 seeds 1, 2, 3 all pass, draining in 35–42 s (1036–1105 keys).
  - H03 is now 45 keys in **38.9 s**, which is the single-replica
    expectation (was 45–48 s).
  - H02 still passes.

### Oracle fix (my mistake)

The first S2 run reported I8 at 10%. Every one of those was
`503 identity_pending`, the documented retryable backpressure from
credential waits of 2 s while the registration queue drains. I8 now
counts transport errors and other 5xx, and reports `identity_pending`
separately.

### S3 and S4: clean, with one operational note

- **S3**, SIGSTOP of a replica for 50 s (longer than the 45 s namespace
  lease), then SIGCONT: all invariants hold over 3 seeds, with 2–3 of
  about 1,100 ops failing (0.2%).
- **S4**, a 30 s Postgres partition for one replica (traffic held, then
  released): all invariants hold over 3 seeds, with 0 failures.
- **Operational note from S3.** A stopped replica still *accepts* TCP
  connections (the kernel's accept queue), so every request routed to it
  waits out the client timeout (5 s here) before retrying elsewhere.
  Throughput roughly halved (about 1,100 ops in S3 against 2,200–2,400 in
  S4). A real `svc:twd-mgmt` VIP needs **active health checks** (e.g.
  `GET /v1/status` with a short timeout) to eject a stalled replica. A dead
  one is ejected by connection refusals anyway. Record this in the
  deployment docs when the VIP is built (H12).
- **Coverage gap.** The workload never moves a workload between hosts, so
  lease safety and fencing (I1/I2) aren't exercised by S2–S4; they're
  covered by the H10 unit test. S11 adds host failover.

### S5 and S6

**S5**, Postgres restarted mid-load: all invariants hold over 3 seeds. The
replicas recover by themselves; pgx re-dials. The first run "failed" I8 at
9%, but every failure was inside the restart (seconds 21–22, all
`500 internal`). The oracle now takes scenario-declared outage windows,
covering the restart through 10 s after Postgres returns. Outside them,
0.03–0.16% of ops failed. **H18:** a store outage surfaces as
`500 internal` rather than a retryable `503`.

**S6**, 200 ms–2 s of Postgres latency on one replica. **Correction:** I
first recorded "invariants hold, 3/3" and committed that before reading
seed 3's result. **Seed 3 failed I8** (2 of 133 ops = 1.5%, over the 1%
bound), and seed 2 passed narrowly at 0.85%. The counts are tiny, because
the cluster completed only about 117–175 ops in 60 s, against about 2,300
in S4. **The oracle doesn't check throughput, so this "passes".** The new
lock sampler (`locks.txt`) and a latency split (seed 1) show two causes:
1. **About half the ops first waited out a 5 s client timeout on the slow
   replica.** That's the routing/health-check issue from S3: a real VIP
   needs latency-aware outlier ejection. It's a harness and deployment
   matter, not mgmt.
2. **Lease-row contention (H17).** Of the ops served directly by *fast*
   replicas, p50 was 10 ms but p99 was 3.6 s. Every lock wait sampled was
   on the workload's `leases` row: fenced commits (`UpdateObject` with a
   `Fence`) lock it `FOR UPDATE` and hold it across several round trips;
   the slow replica showed `idle in transaction` while holding it. Fix
   idea: `FOR SHARE` for the fence read, so fenced commits don't serialise
   on each other, or a single-statement fenced update.

The audit chain head, my first suspect, never showed up in the samples.

### Iteration 30: S6 diagnosed. A harness artifact, plus defense in depth in mgmt.

**What the failures were.** Both I8 failures in S6 seed 3 were
`callAny` exhausting 4 attempts, each a 5 s timeout. I added per-attempt
traces (`attempts` in `history.jsonl`) to look inside.

**mgmt changes** (made before the traces were in):
- Postgres sessions get `lock_timeout` = 3 s and
  `idle_in_transaction_session_timeout` = 3 s (startup parameters;
  `withSessionLimits`), so a replica with a slow link can't hold row locks
  indefinitely.
- The fence read is `FOR SHARE` (H17).
- `isStoreUnavailable`: lock and idle-transaction timeouts, a shutting-down
  server, and lost or refused connections map to
  **`503 store_unavailable`** with `Retry-After` (H18). The spec was
  changed first.

**The traces then showed what was really going on.** With those changes,
the fast replicas answered 142 attempts at p90 15–19 ms (max 266 ms). The
failed ops were **4 random picks of the slow replica in a row**: the
harness retried the same dead backend.

**Harness fix (iter30).** `callAny` now behaves like a load balancer with
passive health checks: no retry on a backend already tried for this
request, and a backend that timed out is ejected for 10 s.
- S6 seeds 1, 2, 3: 0 failures, about 2,100 ops (was 117–180).
- S3 throughput: 2,384 ops (was about 1,100).

**A/B check, to be honest about attribution.** S6 seed 3 with the **mgmt
changes reverted** and the new harness **also passes** (0 of 2,197). So S6
passes because of the client and VIP behavior. The mgmt changes are kept
as defense in depth: they bound how long a sick replica can stall the
others, and H18 is a real fix. Their effect on S6 isn't demonstrated.

**Requirement for the real `svc:twd-mgmt` VIP (H12), from S3 and S6:**
retry on a different backend, plus passive and active health checks
(eject on timeouts, probe `GET /v1/status`).

## Notes

- **H01, latent fragility:** `migrate` takes and releases the advisory lock
  through the pool. It works only because `migrate` runs on one goroutine,
  so `database/sql` hands back the same idle connection. Any future
  concurrent query inside `migrate`, or a pool setting change, could break
  it. Cheap hardening: pin a connection with `db.Conn(ctx)`. That's not a
  bug today.
- **Harness deviation:** the fake TS API and fake setec are small standalone
  copies in `harness_test.go`, rather than `mgmt/mgmttest` promoted from
  `mgmt`'s internal test files. That avoids touching `mgmt` tests in
  propose mode.
- **Baseline:** a 3-replica simultaneous cold start on an empty database
  takes 407–410 ms to ready, and a restart into a live cluster about
  105 ms (M5 Pro, OrbStack Postgres 17).

## Run log

<!-- One line per run: UTC stamp | item | seed | verdict | invariants failed | artifact dir | note -->
2026-11-11 iter1 PLAN | Harness slice 1 + H01 | seed n/a | confirm H01 if any replica start/restart exceeds 10 s or lock 7464 is held after all are ready; refute if 3-way cold start, SIGTERM/SIGKILL restarts and 10 rapid restarts are all < 10 s with the lock free | I8
2026-11-11T16:32Z | H01 | n/a | HARNESS flaw | — | results/20261111T163257Z-H01 | binary built inside first start(): r1/r2 started 4.4 s late, so cold start wasn't simultaneous; fixed (build in newCluster)
2026-11-11T16:3xZ | H01 | n/a ×3 | REFUTED | none | results/*-H01 (3 runs) | cold start 3-way 407–410 ms; SIGTERM/SIGKILL/10 rapid restarts ~105 ms each; lock 7464 free at every check
2026-11-11 iter2 PLAN | H02 (+ harness: tenant/host/deploy setup, fake host heartbeat, fakeTS hold) | n/a | confirm if any object gets >1 key after 3 replicas start together with 40 pending objects; refute if 3 runs show exactly 1 key per object | I3
2026-11-11T16:37Z | H02 | n/a | CONFIRMED | I3 | results/20261111T163710Z-H02 | 60 keys/40 objects, 13 double-minted; added custody oracle
2026-11-11T16:38Z | H02 | n/a ×3 | CONFIRMED | I3 (custody) | results/20261111T1638{26,40,51}Z-H02 | 14–17 double-minted per run, all of them left without a credential (custody 404); first custody-oracle run discarded (it treated 201 as failure)
2026-11-11 iter3 PLAN | H10 (in-process: 2 Mgmt, one store, skewed clocks) | n/a | confirm if a healthy heartbeating holder is displaced by a redeploy through a skewed replica at any skew; report the threshold (ahead and behind); refute if no displacement up to 20 s | I1
2026-11-11T16:4xZ | H10 | n/a | CONFIRMED | I1, I8 | in-process (SQLite) | sweep 0–20 s: refused ≥10 s, displaced ≥15 s, ahead and behind. First run aborted at 10 s (a test bug: deploy errors were fatal); fixed to record outcomes
2026-11-11T16:4xZ | H10 | n/a | CONFIRMED | I1, I8 | in-process (Postgres twd-mm-pg) | refused ≥10 s; displaced ≥12 s ahead / ≥15 s behind (sub-cases share host rows: approximate)
2026-11-11 | LOOP STOPPED | — | I1 violated (H10) → stop rule | — | — | needs a decision: accept skew as out of scope (and resume), or fix H10 first. Open: H03–H09, H11–H14; harness slices 3+; scenarios S1–S11 not started
2026-11-11 iter4 | F1 | n/a | FIXED | — | in-process + make test-go/test-pg/lint | H10 test fails before (10/15 s), passes after; H01 e2e still green with the dbClock
2026-11-11 iter5 | F2 | n/a | DONE | — | make test-go/test-pg green; lint/api-check re-run after commit | status reports clock; warns at >2 s or a 30 s-stale sample
2026-11-11 iter6 | F3 | n/a ×6 | FIXED | — | results/*-H02 | first 3 runs correct but 66 s (claim held through backoff); after per-attempt release 3/3 correct in 12–13 s; make test-go/test-pg/lint green
2026-11-11 iter7 | F5 | n/a | DONE | — | make test-go/test-pg/lint green | first message referenced a nonexistent --host CLI flag; corrected
2026-11-11 iter8 PLAN | H11 | n/a | confirm if, with -custody file, any credential fetch fails on the replica that didn't mint; refute if all 12×2 succeed | I3, I8
2026-11-11 iter8 | H11 | n/a ×2 | CONFIRMED | I3, I8 | results/*-H11 | exactly half of credential releases fail on each replica (404 custody: not found)
2026-11-11 iter9 | F6 | n/a ×2 | FIXED | — | results/*-H11, H01, H02 | second file-custody replica refuses with a clear message; crash restart 105 ms; setec replicas unaffected; make test-go/test-pg green
2026-11-11 iter10 PLAN | H03 | n/a | confirm if keys created in any 10 s window exceed burst+rate×10 s (16) by >10% with 3 replicas; refute if ≤ 17.6 | I4
2026-11-11 iter10 | H03 → F7 | n/a ×2 + ×2 | CONFIRMED then FIXED | I4 | results/*-H03 | before: 45 keys in 9 s (window max 45, limit 16); after: 45 in 45–48 s, window max 16; make test-go/test-pg green
2026-11-11 iter11 PLAN | H04 | n/a | confirm if a root ends with > maxLive (5) live descendants after 40 concurrent spawns across 3 replicas; refute if ≤5 in 3 runs | I5
2026-11-11 iter11 | H04 | n/a ×6 | NOT REPRODUCED (open) | — | results/*-H04 | 0/2/5/20 ms PG delay: always exactly 5 live. At 20 ms, 23/40 spawns exceeded the 5 s client timeout (I8 latency: look at serialisation under DB latency). Paused here for Ross's self-fencing interview
2026-11-11 iter12 | F4 | n/a | DONE | — | unit tests; make test-go/test-pg green | the first test wrongly assumed a pinned move from a live holder is allowed (it's refused, which is safe); its message was fixed instead
2026-11-11 iter13 PLAN | H06 | n/a | confirm if a kill served by r1 reaches a host registered with r0 only via heartbeat or after > 1 s; refute if pushed within 1 s | I6
2026-11-11 iter13 | H06 | n/a ×2 | CONFIRMED | I6 | results/*-H06 | via r0: pushed in 12–15 ms; via r1: no push, heartbeat after ~1.5 s. While reading hostd's heartbeat found existing self-fencing (FenceAfter 9 s, all workloads): correction recorded, ADR-028 §3 to be re-decided
2026-11-11 iter14 | F8 | n/a ×2 | FIXED | — | results/*-H06; make test-go/test-pg green | kill via r1 now pushed in 13–15 ms (was heartbeat, ~1.5 s)
2026-11-11 iter15 | F9 | n/a | DONE | — | make test-go/test-pg green; e2e H01/H02/H06/H11 green | F4/F5 tests updated for the 45 s lease; my first edit left dead scaffolding (`_unusedFenceLoop`) and a duplicate import, both removed before commit
2026-11-11 iter16 PLAN | H07 | n/a | confirm if, with -device-delete-delay 3s, a kill followed by SIGKILL of that replica within the delay leaves the device undeleted (control: deleted without the crash) | I6
2026-11-11 iter16 | H07 → F10 | n/a ×3 | CONFIRMED then FIXED | I6 | results/*-H07 | crash case: device left (before), deleted by the surviving replica (after, 2/2); make test-go/test-pg green
2026-11-11 iter17 PLAN | H13 (OBO root key) | n/a | confirm if replicas report different OBO public keys for a tenant: (a) after a normal create, (b) after cold concurrent first use; refute if all agree in both | identity consistency
2026-11-11 iter17 | H13 | n/a ×5 | CONFIRMED | identity consistency | results/*-H13 | (b) 5/8, 0/8, 3/8, 3/8, 1/8; (a) 0/40. Issuer key by code inspection only
2026-11-11 iter18 | F11 | n/a ×5 | FIXED (OBO) | — | results/*-H13; make test-go/test-pg green | (b) 0/8 ×5; issuer key split out as F11b
2026-11-11 iter19 | F11b | n/a | DONE | — | unit (-race); make test-go/test-pg green | e2e WIF not run (needs Funnel)
2026-11-11 iter20 PLAN | H14 | n/a | confirm if concurrent same-name creates through 3 replicas give any non-2xx or differing object IDs; refute if all 2xx with one ID for 10 names × 2 runs | idempotency, I8
2026-11-11 iter20 | H14 | n/a ×2 | CONFIRMED | idempotency | results/*-H14 | 10/10 names: 202 + 2×409 exists; queued F12
2026-11-11 iter21 | F12 | n/a ×many | FIXED | — | full e2e suite + make test-go/test-pg green | two regressions found by existing e2e (H11 refusal, H13+H14 ordering) and fixed before commit
2026-11-11 iter22 PLAN | H08 | n/a | confirm if r1 reports a different deployment status than r0 after the host reports active to r0 only | I10
2026-11-11 iter22 | H08 → F13 | n/a | CONFIRMED then FIXED | I10 | results/*-H08 | first fix version had a stale-cache flaw (fixed before commit)
2026-11-11 iter22 | H04 | 20 runs | CONFIRMED (3/20) | I5 | results/*-H04 (report.txt) | appeared in the full-suite run after F13; not related to F13 (the race predates it)
2026-11-11 iter23 | F14 | 30 runs | FIXED | — | make test-go/test-pg green | found and fixed flaky TestV1Pagination (async mint audits); queued H16
2026-11-11 iter24 | H09, H15 → F15 | unit | CONFIRMED then FIXED | I6/I7, I4 | — | make test-go/test-pg green; e2e H02/H03 green
2026-11-11 iter25 | H05 → F16 | unit | CONFIRMED then FIXED | I8 latency | — | 10.004 s → 506 ms; make test-go/test-pg green
2026-11-11 iter26 PLAN | H16 | n/a | confirm if live objects exceed maxObjects (10) after 30 concurrent creates over 3 replicas (5 ms PG delay); refute if ≤10 in 10 runs | quota
2026-11-11 iter26 | H16 → F17 | 10 + 20 runs | CONFIRMED (2/10) then FIXED (0/20) | quota | results/*-H16 (report.txt) | H04 still 0/10; make test-go/test-pg green
2026-11-11 iter27 PLAN | harness slice 4 (client-side LB, seeded workload, host loop, oracle I3/I5/I6/I7/I8/I9/I11) + S2 seed 1 | 1 | pass if all invariants hold after 60 s of random SIGKILLs under load | I3 I5 I6 I7 I8 I9 I11
2026-11-11 iter27 | harness slice 4 + S2 | seeds 1,2,3 | seed 3 FAILED I9 → F18 → 3/3 PASS | I9 | results/*-S2-seed{1,2,3} | first run's I8 'violation' was an oracle misclassification (identity_pending); fixed
2026-11-11 iter28 PLAN | S3 (SIGSTOP 50 s > 45 s lease) and S4 (PG blackhole 30 s for one replica), seed 1 | 1 | pass if all invariants hold | I3 I5 I6 I7 I8 I9 I11
2026-11-11 iter28 | S3, S4 | seeds 1–3 each | 6/6 PASS | — | results/*-S3-*, *-S4-* | S3 throughput about halved by the stalled-but-accepting replica (needs VIP health checks)
2026-11-11 iter29 PLAN | S5 (PG restart mid-load), S6 (200 ms–2 s PG latency on one replica), seed 1 | 1 | pass if all invariants hold | I3 I5 I6 I7 I8 I9 I11
2026-11-11 iter29 | S5, S6 | seeds 1–3 each | S5 3/3 PASS (after outage windows), S6 3/3 invariants PASS with throughput finding | I8 | results/*-S5-*, *-S6-* (locks.txt) | new: PG lock sampler; H17 (lease-row contention), H18 (500 vs 503 during store outage)
2026-11-11 iter29 CORRECTION | S6 | seeds 2, 3 | seed 2 PASS (0.85%), seed 3 FAIL I8 (1.5%) | I8 | results/*-S6-seed{2,3} | the previous entry's '3/3' was written before reading seed 3's result
2026-11-11 iter30 | S6 diagnosis | seeds 1–3 + A/B | 3/3 PASS after harness LB semantics; mgmt: session limits, FOR SHARE fence, 503 store_unavailable (H18) | I8 | results/*-S6-* (attempts traces) | A/B: S6 passes without the mgmt changes too; recorded as defense in depth
2026-11-11 iter31 PLAN | S1 (rolling SIGTERM restarts), S7a (simultaneous kill+cold start ×2 under load), S7b (3 concurrent OpenStore on a pre-M7 Postgres store) | seeds 1–3 | pass if all invariants hold / migration once, rows intact | I3 I5 I6 I7 I8 I9 I11, migration
2026-11-11 iter31 | S1, S7a, S7b | seeds 1–3 | 7/7 PASS | — | results/*-S1-*, *-S7a-*; make test-pg | no product changes
