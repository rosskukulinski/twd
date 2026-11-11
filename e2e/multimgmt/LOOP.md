# Loop: multi-replica twd-mgmt on Postgres (chaos + invariants)

Run with: `/loop Follow e2e/multimgmt/LOOP.md` (self-paced, no interval).

You are validating that **N ≥ 2 twd-mgmt replicas sharing one Postgres** are
safe and live when replicas die, stall, lose the database, or disagree on the
time. twd-mgmt has **no leader election**: every replica serves the API, runs
the reaper, and runs mint workers, so correctness rests entirely on Postgres
conditional writes plus whatever in-memory state each replica keeps. Your job
is to find where that breaks, prove it with a reproducible test, and record it.

State lives in `e2e/multimgmt/LEDGER.md`. Read it first, every iteration.

## Hard rules (stop-the-line)

1. **Hermetic only.** Fake Tailscale API (`mgmt/api_test.go` `newFakeTS`),
   fake setec (`mgmt/custody_test.go` `fakeSetec`), testcontrol if a real
   tsnet node is needed. Never use a real tailnet or any key under
   `~/.config/tailnet-agents-design/`.
2. **Never push.** This repo is public. Commit locally to branch
   `mm-loop` only (create it from master on the first iteration). Never
   commit `e2e/multimgmt/results/` (gitignored by `e2e/*/results/`).
3. **Only kill what you started.** Every process and container you start is
   recorded in `e2e/multimgmt/results/pids` / named `twd-mm-*`. Kill by that
   record, never by name pattern. Remove `twd-mm-*` containers when a run ends.
4. **Budget:** one experiment per iteration, ≤ 10 min wall clock per run, ≤ 3
   reruns of the same seed. If a run hangs past budget, that is a finding
   (liveness), not a reason to retry forever.
5. **Mode.** `FIX_MODE` at the top of LEDGER.md is `propose` (default) or
   `fix`. In `propose`, write the failing regression test and a fix sketch in
   the ledger, but don't change production code. In `fix`, you may change
   `mgmt/` on `mm-loop`, but only with a regression test that failed before the
   change and passes after, and only if `make test` and `make test-pg` stay green.

## Each iteration

1. **Read LEDGER.md.** Pick exactly one item, in this order:
   a. Harness not built, or the last run was HARNESS-BROKEN → work on the
      harness (see "Harness").
   b. An open hypothesis (H-*), highest priority first → design the smallest
      experiment that confirms or refutes it. Prefer a Go test that runs two
      `mgmt.Mgmt` instances in-process on the same Postgres store (fast,
      deterministic). Fall back to real processes when the bug needs a real
      crash, a real connection pool, or real timing.
   c. A scenario (S-*) without 3 consecutive clean seeds → run it with the
      next seed.
   d. Everything green → soak: random scenario mix, new seed, 10 min.
2. **Write the plan in the ledger before running:** item, seed, what result
   would confirm or refute it, and which invariants you'll check.
3. **Run it.** Put artifacts in `e2e/multimgmt/results/<UTC-stamp>-<item>-<seed>/`:
   the op history (JSONL), replica logs, PG samples, and the invariant report.
4. **Classify the result:** `CONFIRMED` (with the violated invariant and evidence),
   `REFUTED`, `FLAKY` (differs across reruns of one seed: rerun up to 3,
   then record as a determinism finding), or `HARNESS-BROKEN`.
5. **For CONFIRMED:**
   - Write the smallest deterministic regression test: a unit test where
     possible, named `TestMM_<Hxx>_<slug>`. On Postgres it is gated by
     `TWD_TEST_POSTGRES_DSN`, as in `scripts/test-pg.sh`.
   - Add a `t.Skip("mm-loop: Hxx open")` only in `propose` mode, so `make test`
     stays green.
   - Record the root cause with `file:line` and a fix sketch.
   - Add any new hypotheses it suggests.
6. **Update LEDGER.md** (status table, run log line, new hypotheses). Commit
   on `mm-loop` with a message naming the item and verdict.
7. **Stop the loop and notify the user (PushNotification)** if any of these
   happen:
   - I1 or I2 is violated (split brain or fencing failure). These are
     design-level problems, so don't keep going.
   - The harness is HARNESS-BROKEN 3 iterations in a row.
   - Every hypothesis is resolved and every scenario has 3 clean seeds, plus
     2 clean soaks. Write a final summary at the top of LEDGER.md.

   Otherwise, schedule the next iteration in 60–120 s. During the soak phase,
   use 600 s.

## Harness (build once, in `e2e/multimgmt/`, build tag `e2emultimgmt`)

- **Postgres:** `postgres:17-alpine` in OrbStack, named `twd-mm-pg`; copy the
  start and readiness logic from `scripts/test-pg.sh`. Enable
  `log_lock_waits=on`, `deadlock_timeout=200ms` and
  `track_activity_query_size` large enough to see whole queries.
- **PG fault proxy:** a small in-test TCP proxy, one per replica, between that
  replica and Postgres. It supports `pass`, `delay(d)`, `blackhole` (accept
  connections but forward nothing) and `reset` (RST every connection). No new
  dependencies.
- **Replicas:** N=3 real `twd-mgmt` processes, each with `-local-dev`, its own
  `-state-dir` and `-socket`, and the same `-db postgres://…` (through its
  proxy). Use `-tsapi-url` pointing at a shared fake TS API, and
  `-custody setec -setec-url` pointing at a shared fake setec. Promote
  `newFakeTS` and `fakeSetec` into an importable `mgmt/mgmttest` package.
  The fake TS API must count `CreateKey` calls and devices per object.
- **Front:** an in-test L7 proxy standing in for `svc:twd-mgmt`. It sends each
  request to a replica chosen by policy: `random`, `sticky-per-client`,
  `pin(i)`, or `avoid(i)`. Every response carries the replica index so the
  history records which replica served it.
- **Hosts:** fake hosts by default (goroutines that register, heartbeat every
  2 s, renew leases, fetch credentials, report ready, and obey
  revoke/revokeObjects). They record every lease epoch they believe they
  hold, with timestamps. Add one real `twd-hostd` only for the S11 smoke test.
- **Faults:** SIGKILL, SIGSTOP/SIGCONT (a paused process is the classic
  zombie-writer case), graceful restart, a PG proxy fault per replica,
  `docker restart twd-mm-pg`, and clock skew. Skew needs a hidden test-only
  flag; add `-test-clock-offset` behind the `e2emultimgmt` build tag or an
  env var that's refused unless `-local-dev` is set. Record that in the ledger.
- **Workload generator (seeded):** a mix of deploy, create, spawn (trees up
  to maxDepth), credential, ready, kill (with and without cascade), audit
  ingest, and GETs. Each op is a JSONL record: op, args, start, end, replica,
  status, body summary.
- **PG sampler (every 250 ms):** sessions waiting on a lock
  (`pg_stat_activity` where `wait_event_type='Lock'`) with
  `pg_blocking_pids`, `pg_locks` grouped by relation and mode, the
  `pg_stat_database.deadlocks` delta, connections per application, and the
  longest transaction age.
- **Oracle:** checks the invariants below from the op history, the fake
  host records, fake TS API counters, and direct SQL on Postgres. It writes
  `report.json` with pass/fail and evidence for each invariant.

## Invariants

| ID | Invariant |
|---|---|
| I1 | **Lease safety:** for each lease, intervals in which a host believes it holds (lease, epoch) never overlap between hosts beyond the documented TTL slack. Epochs only increase. |
| I2 | **Fencing:** a credential release or fenced commit for (lease, epoch) only succeeds while that epoch is current. A stale holder (e.g. after SIGSTOP) gets refused. |
| I3 | **Exactly one mint:** at most one live auth key per object. Every extra key is deleted. The custody value matches the key the object used. |
| I4 | **Throttle:** the aggregate `CreateKey` rate per tenant, across all replicas, stays ≤ `registrationsPerMinute` (ADR-017), with 10% tolerance. |
| I5 | **Spawn budgets:** live descendants ≤ maxLive and depth ≤ maxDepth at every sample, under concurrent spawns spread across replicas. |
| I6 | **Kill:** killed objects are terminal. The owning host gets the kill within 1 s via push, or within `RevocationWindow` via heartbeat; record which path was used. At quiescence, no devices are left for killed objects. |
| I7 | **Audit:** `GET /v1/audit/verification` passes. No forks, gaps or duplicate kill/mint records per chain. |
| I8 | **Availability:** with one replica dead, stalled or partitioned, ≥ 99% of front requests succeed within 5 s. No request hangs past the client timeout. A replica (re)starting while others run reaches ready in < 10 s. |
| I9 | **Convergence:** within KeyExpiry + 30 s of quiescence, no object is stuck `pending`/`minted` and nothing the reaper should kill is still live. |
| I10 | **Cross-replica reads:** a write acknowledged by replica A is visible to a read on B (deployment status, object status, lease). |
| I11 | **Postgres health:** zero deadlocks. Lock waits over 1 s are reported with the blocking query. The connection count stays bounded after every reconnect storm. |

## Scenarios (each needs 3 clean consecutive seeds)

| ID | Scenario |
|---|---|
| S1 | Rolling graceful restart of every replica under steady load |
| S2 | SIGKILL a random replica every 5–20 s; restart it after 3 s |
| S3 | SIGSTOP one replica for 20–40 s (longer than the 15 s lease TTL) while it serves a hot tenant, then SIGCONT |
| S4 | PG blackhole for one replica for 30 s; the others keep going |
| S5 | `docker restart twd-mm-pg` mid-load, with transactions in flight |
| S6 | 200 ms–2 s PG latency on one replica, so lock hold times grow |
| S7 | All 3 replicas cold-start at once: (a) empty DB; (b) a DB at the pre-M7 schema (migration race) |
| S8 | Clock skew of +5 s / −5 s / +20 s on one replica |
| S9 | Hot tenant: 500 concurrent create/spawn spread across replicas (audit-head and throttle contention) |
| S10 | Kill storm: overlapping cascade kills on the same trees from different replicas |
| S11 | Host failover: a host dies, its leases expire, and the workload is redeployed through a different replica (one real twd-hostd) |
