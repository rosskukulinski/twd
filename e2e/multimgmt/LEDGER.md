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
| F4 | **Guarded moves** (ADR-028 §3, Ross 2026-11-11): Deploy refuses to move a workload with DO classes off a host whose lease lapsed unless the host was removed or the request carries `forceTakeover` (CLI `--force-takeover`; audited with the old host). Per-class `"fence": "never"` exempts a class. Workers (no DO classes) unaffected. Self-fencing deferred until state can move | H10/S4 | new `TestMM_F4_GuardedTakeover` (stateful refused, forced allowed + audited, Workers-only allowed, `fence: never` allowed, after `host rm` allowed) | next |

## Status

| Item | Priority | Status | Evidence / test |
|---|---|---|---|
| Harness | P0 | slice 1 built (PG container, fake TS API, fake setec, N real replicas, unix-socket client, pg_locks probe). + slice 3: per-replica Postgres fault proxy (pass/delay/blackhole/reset, `pgproxy_test.go`), fake TS policy endpoint, configurable twd.jsonc. + slice 2: tenant/host/deploy setup, fake host heartbeat, fakeTS hold switch and per-object key counts. TODO: PG fault proxy, front proxy, fake hosts, workload gen, PG sampler, oracle | `e2e/multimgmt/harness_test.go` |
| H01 | P0 | **REFUTED** (3/3 runs) — latent fragility noted | `TestMM_H01_StartupAdvisoryLock` |
| H02 | P0 | **FIXED by F3** (was: every double-minted object lost its credential) | `TestMM_H02_DoubleMint` |
| H03 | P1 | **FIXED by F7** (was: 3 replicas minted 45 keys in 9 s against a 60/min limit, ≈2.8× the window limit) | `TestMM_H03_ThrottlePerReplica`, `TestStoreSharedThrottle` |
| H04 | P1 | **not reproduced yet** (exactly 5/5 live in 6 runs at 0/2/5/20 ms PG delay); the race exists in code (per-replica `tenantLock`; `CountLive` then insert, no re-check); something narrows the window, cause not found | `TestMM_H04_SpawnBudgetRace` |
| H05 | P2 | open | |
| H06 | P1 | open | |
| H07 | P1 | open | |
| H08 | P2 | open | |
| H09 | P2 | open | |
| H10 | P0 | **FIXED by F1** (in-process, SQLite-modelled DB clock; real Postgres clock covered by `TestStoreClockPostgres`). Host-side half is F4 | `mgmt/mm_h10_test.go` `TestMM_H10_LeaseClockSkew` |
| H11 | P0 | **FIXED by F6** (was: with `-custody file`, 6/12 credential releases failed on each replica) | `TestMM_H11_FileCustodyPerReplica` |
| H12 | P1 | open | |
| H13 | P1 | open | |
| H14 | P2 | open | |
| H15 | P2 | open (pre-existing, single replica too) | |
| S1–S11 | — | 0/3 seeds each | |

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
