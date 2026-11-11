# multi-mgmt loop ledger

FIX_MODE: propose

## Status

| Item | Priority | Status | Evidence / test |
|---|---|---|---|
| Harness | P0 | slice 1 built (PG container, fake TS API, fake setec, N real replicas, unix-socket client, pg_locks probe). + slice 2: tenant/host/deploy setup, fake host heartbeat, fakeTS hold switch and per-object key counts. TODO: PG fault proxy, front proxy, fake hosts, workload gen, PG sampler, oracle | `e2e/multimgmt/harness_test.go` |
| H01 | P0 | **REFUTED** (3/3 runs) — latent fragility noted | `TestMM_H01_StartupAdvisoryLock` |
| H02 | P0 | **CONFIRMED** (4/4 runs), and worse than hypothesised: every double-minted object loses its credential | `TestMM_H02_DoubleMint` |
| H03 | P1 | open | |
| H04 | P1 | open | |
| H05 | P2 | open | |
| H06 | P1 | open | |
| H07 | P1 | open | |
| H08 | P2 | open | |
| H09 | P2 | open | |
| H10 | P0 | open | |
| H11 | P0 | open | |
| H12 | P1 | open | |
| H13 | P1 | open | |
| H14 | P2 | open | |
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
