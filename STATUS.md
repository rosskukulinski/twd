# twd STATUS (living log)

Last update: 2026-11-07 23:15 PT. Scope of this run: M0 and M1 only.

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

## Next steps
- M2 `twd-mgmt`: SQLite and Postgres leases with epoch fencing, the mint broker (API-key mode first, reusing `internal/tsapi`), the registration throttle and the audit log.
- M3 `twd-hostd`: move `netd/testdata/workerd/platform.js` into the generated wrapper, add the egress MAC, and run the workerd supervisor.
- Run `make test` in Lima `twd-dev` (Linux arm64).
