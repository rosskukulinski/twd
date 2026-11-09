> **Note (M11):** this file describes the v0 era. The mgmt API is now `/v1`, specified in `api/openapi.yaml`; `mgmt-api.md` is **generated** from it (`make api`). The hostd-side facts below (audit hash input, push API, platform wrapper sockets) still hold; the `/v0` route names are historical and map to `/v1` per the spec's "Replaces …" notes.

# Contract: twd-hostd additions (M3, backward compatible)

These extend `mgmt-api.md` (v0 + the M2 "v0 clarifications"). All of them are optional: M2's twd-mgmt (origin/m2-mgmt b08b5e4) works with twd-hostd unmodified (`make test-m2mgmt`). The one exception is the workload entry node; see gap 1.

## What hostd sends and expects (it matches M2)
- **Host header.** Every request carries `Twd-Host-ID: <hostID>`.
- **Register.** `auditPubKey` is base64 ed25519. `hostSig` = base64(ed25519.Sign(key, utf8(hash))). The hash is the canonical sorted-key hash of the record JSON without `hash`/`hostSig`.
- **Heartbeat.**
  - `leases:[{namespace:"<tenantID>/<workload>", epoch}]`, one per workload.
  - Kills arrive in `revokeObjects`, and hostd's kill is idempotent.
  - If mgmt revokes a lease while a newer assignment for the same workload is on this host (a redeploy bumped the epoch), hostd adopts the new epoch instead of fencing itself.
- **Objects.**
  - `POST /v0/objects` `namespace` is the **DO class name**.
  - hostd proposes `hostname`. If the response carries `hostname`, hostd uses it; M2 uses the object ID.
  - `GET credential?epoch=N&wait=10` is retried while it returns `identity_pending`, 429 or 503.
  - `POST ready` carries `epoch`. A 410 `killed` from ready makes hostd kill the node locally.

## Additions (optional fields)
| Where | Field | Meaning |
|---|---|---|
| register | `pushURL` | `unix:///…/hostd.sock`. When set, mgmt may push `POST /v0/push/kill {objectID, reason}` and `POST /v0/push/assignments {}`. This is the fast kill path (~1 ms tier 1 vs ≤ one heartbeat). M2 does not push yet (gap 2) |
| heartbeat req | `deployments:[{deploymentID,status,error?}]` | `starting\|active\|stopped\|failed` |
| heartbeat resp | `kill:[{objectID,reason}]` | Same as `revokeObjects`, with a reason (fakemgmt only) |
| assignment | `workloadTags` | Tags of the workload entry node (twd.jsonc `tailnet.tags`) |
| deployment | `GET /v0/deployments/{id}` | `{deploymentID, host, endpoint, status}` |
| tenant | `mintMode:"static"` | fakemgmt only: a fixed key for hermetic testcontrol runs |

## Gaps that need an M2 change
1. **Workload entry node.** Every deployment gets a node for its default export: the "hello" node, whose egress is the Worker's identity.
   - hostd creates it as `POST /v0/objects {namespace:"_workload", objectName:<workload>}`.
   - M2 resolves namespaces by class, so today the deploy must declare `namespaces:[{class:"_workload", tags:[…]}, …classes]` explicitly. `make test-m2mgmt` does this.
   - M2 rejects bundles that have no `tailnet.objects` ("no namespaces"), so a plain Worker like `hello` can't be deployed to M2 as-is.
   - **Ask:** mgmt implicitly declares `_workload` with `tailnet.tags` for every deployment.
2. **Kill latency.** With heartbeat delivery only, `twd kill` → tier 1 waits up to one heartbeat (3 s).
   - **Ask:** support `pushURL` (above) or a long-poll `GET /v0/hosts/{id}/events`.
   - `ctx.tailnet.kill()` from inside an app is immediate on its own host either way.

## hostd ⇄ platform wrapper (local, per workerd instance; not a network API)
`hostapi.sock`, which only the trusted platform worker can reach (as the `HOSTD` binding).

**Ops.** `POST /v1/{ensure,spawn,ready,kill,audit,self}` with JSON `{as?:{class,name}, …}`. `as` is the calling object, attested by the platform's `TailnetApi` props.

**Errors.** Typed `{code, message, detail, doc_url}` with a `twd-error` header.

**Egress sockets.**
- `egress-obj.sock` (facets): `x-twd-class`, `x-twd-name`, `x-twd-ts`, `x-twd-nonce` and `x-twd-mac`.
  - The MAC is HMAC-SHA256 under a per-process key over `class\nname\nmethod\nurl\nts\nnonce`, with ±2 min skew allowed.
- `egress-wl.sock`: the Worker and plain-DO objects. This socket is the workload node's identity.

## M4 status of the gaps
Both gaps are closed in twd-mgmt (see `mgmt-api.md` § "M4 additions"): `_workload` is implicit from `tailnet.tags`, and mgmt pushes kills to `pushURL`.

**M4 hostd changes.**
- Hostnames: when mgmt names the node (M2: the object ID), hostd still prepends `-hostname-prefix` (`twd-` by default under `twd up`; `twd-e2e-<run>-` in E2E-MAC).
- `actingFor`: the platform sends the first caller-bearing request's principal (the untagged caller's login, else the caller's node name) with `ensure`; hostd passes it to `POST /v0/objects`. A spawn acts for `principal` if given, else the parent's. If the object already existed (an RPC such as the Agents SDK's `__unsafe_ensureInitialized` came first), hostd writes a host audit record `kind:"principal"`.
- Kill tombstone: the killed node's ingress port now answers every new request with `410 {"code":"object_killed"}` (`twd-error: object_killed`, `Connection: close`) for the tombstone window, instead of a bare RST. Via the workload node, a call into a killed object is a typed `410 killed` (the runtime maps the platform's `[killed]` exception) instead of a 500.
