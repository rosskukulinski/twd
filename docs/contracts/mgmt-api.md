# Contract: twd-mgmt ⇄ twd-hostd ⇄ twd CLI (v0, MVP)

Transport: HTTP+JSON. In production it is served on the operator tailnet (`svc:twd-mgmt`/tsnet), authenticated by WhoIs (`tag:twd-host` for hosts, a human/admin login for the CLI). In homelab/`twd up` and in tests it is a **unix socket** `~/.twd/run/mgmt.sock`, with peer creds checked against the owning uid. Every error body is `{"code","message","missing"?,"snippet"?,"doc_url"?,"trace_id"}` (DX.md catalog).

## Tenants (CLI → mgmt)
- `POST /v0/tenants` `{tailnet, controlURL?, mintMode:"apikey"|"wif", apiKeyFile? (homelab/test only), wifClientID?, registrationsPerMinute?:200, maxObjects?:1000}` → `{tenantID}`
- `GET /v0/tenants`, `GET /v0/tenants/{id}`

## Hosts (hostd → mgmt)
- `POST /v0/hosts/register` `{hostID, capacityNodes, tiers:["process","microvm"]}` → `{ok}`
- `POST /v0/hosts/{id}/heartbeat` `{liveNodes, leases:[{namespace, epoch}]}` → `{ok, revoke:[namespace...]}` (every 3 s; the host lease TTL is 10 s)

## Deployments (CLI → mgmt → hostd)
- `POST /v0/deployments` `{tenantID, workload, bundle (base64 tar of the built worker + twd.jsonc + wrangler.jsonc)}` → `{deploymentID, host, endpoint:{node, magicDNS}}`. mgmt picks a host (MVP: the single host per tenant namespace), acquires the namespace lease (epoch+1), and tells hostd via the next heartbeat response or `GET /v0/hosts/{id}/assignments`.
- `GET /v0/hosts/{id}/assignments` → `[{deploymentID, tenantID, workload, bundleURL, namespaces:[{class, tags, agent?}], epoch}]`

## Objects and identity (hostd → mgmt)
- `POST /v0/objects` `{tenantID, namespace, objectName, parent?:{objectID}, caps?, ttl?}` → `{objectID, status:"pending", tags}`. This is async (ADR-017). mgmt queues a mint subject to the tenant throttle. For a `parent`, it checks `caps ⊆ parent caps computed from policy` and the spawn budget (maxLive 64, depth 3).
- `GET /v0/objects/{id}/credential?epoch=N` → `{authKey}` (single-use, ≤5 min, tagged), released **only** if the caller host holds the namespace lease at epoch N. Otherwise 409 `{code:"lease_lost"}`.
- `POST /v0/objects/{id}/ready` `{nodeID, stableNodeID, magicDNS}`
- `POST /v0/objects/{id}/kill` `{cascade, reason}` → mgmt marks it killed, tells the hosts (via heartbeat `revoke` + a direct push if connected), deletes the device(s) via the tenant API, and records audit.
- `GET /v0/objects?tenant=&namespace=` → list with lineage.

## Audit
- `POST /v0/audit` (hostd/netd → mgmt) `{records:[{ts, objectID, kind:"allow"|"deny"|"spawn"|"kill"|"egress", dst?, grant?, code?, traceID, actingFor?, prevHash, hash, hostSig}]}`
- `GET /v0/audit?object=|human=|trace=` → records, with chain verification status.

## Leases (mgmt internal; Store interface)
`Acquire(ns, holder) → epoch`, `Renew(ns, holder, epoch)`, `Release`. Backends: SQLite (homelab) and Postgres. Fencing: every credential release and every storage commit carries the epoch; stale epoch → `lease_lost`.

## JS runtime surface (hostd platform wrapper ⇄ tenant code) — see RFC §6.1
`getCaller(request)` reads `request.cf.tailscale` (set by netd). `ctx.tailnet.fetch(url)` goes through the platform Egress (per-object attribution). `ctx.tailnet.spawn(ns, {name, caps, ttl})` calls the hostd local API, which calls `POST /v0/objects`, and returns `{stub, ready}`.

## v0 clarifications and additions (M2 implementation, backward compatible)
Everything above holds as written. These clarify ambiguities and add optional fields; v0 clients that ignore them keep working.

**Errors.** Every non-2xx also sets the header `Twd-Error: <code>`. `identity_pending` (503) sets `Retry-After: 1`.

**Leases.** MVP: one namespace lease per **(tenant, workload)**, named `"<tenantID>/<workload>"`, covering every DO class of the workload. That's why an assignment carries a single `epoch`. TTL 15 s. Every heartbeat renews it. A renew succeeds as long as `(holder, epoch)` still matches, even after expiry, because nobody else took over. Expiry only lets *another* host acquire, at epoch+1. Release keeps the row, so epochs never go backwards.

**Hosts.**
- `POST /v0/hosts/register` also takes `auditPubKey?` (base64 ed25519; when set, `hostSig` is verified).
- Heartbeat `leases[].namespace` may be the lease name, the workload name, or a DO class name. `revoke` echoes the entries that are no longer held.
- New: the heartbeat response adds `revokeObjects:[objectID…]`, the objects killed in the last 10 min whose namespace this host holds. They repeat on every heartbeat until the window passes, so a host's kill must be idempotent. The "direct push" is not implemented; this field is the delivery path.
- On the unix socket a host names itself with the header `Twd-Host-ID: <hostID>` (or `?host=`). Over tsnet it is the WhoIs node bound at register. Without it, credential release is fenced on the epoch alone.

**Tenants.**
- The request also takes `tenantID?` and `wifLifecycleClientID?`; the latter is the optional lifecycle credential (`devices:core`) for kill tier 2 and the reaper.
- In wif mode the response adds `wif:{issuer, mintSubject, lifecycleSubject}`. Subjects are `twd:tenant:<id>:mint|lifecycle`.
- The JWT audience is Tailscale's generated default, `api.tailscale.com/<clientID>`. Policy reads for spawn checks use the lifecycle credential in wif mode, so it also needs `policy_file:read` when spawn is used.

**Deployments.**
- mgmt reads `twd.jsonc` from the bundle: a tar, optionally gzipped, taking the shallowest `twd.jsonc`. It uses `tailnet.objects.<Class>.{tags, agent, ttl}` and `tailnet.spawn.{maxLive ≤64, maxDepth ≤3}`.
- The request may instead pass `namespaces:[{class,tags,agent?,ttl?}]`, and may pin `host`. The response adds `epoch`.
- `bundleURL` is a mgmt path, `GET /v0/deployments/{id}/bundle`, which returns the raw bundle bytes.
- Assignments add `lease`, `tailnet` and `controlURL`.

**Objects.**
- `objectID` = `<class-slug>-<b32(hmac)[:10]>`, which is also the node hostname. The create response adds `hostname`.
- Create is idempotent by `(tenant, namespace, objectName)`, and a killed name returns 410 `killed`.
- `caps` is `{capName: value}` or `{capName: [values]}`. `caps ⊆ parent` uses receiver merge semantics over every grant whose `src` matches a parent tag, across all `dst`: arrays union, numbers take the max, objects merge key-wise, bools OR, and requesting `false` is always allowed.
- Every child tag must be the parent's tag or be owned by it in `tagOwners`; otherwise 403 `delegation_invalid`. The error `missing` is `{src,dst,cap,field}`, e.g. `field: caps["acme.com/cap/mcp"].tools[1]`.
- A spawn budget overrun is 429 `spawn_budget_exceeded`.
- `ttl` is `"10m"` or a number of seconds. It defaults to the class `ttl`. A spawned child with a TTL gets an **ephemeral** key.
- The request also takes `actingFor?` and `traceID?`, which feed audit.
- `GET …/credential?epoch=N&wait=S`: `wait` defaults to 10 and is capped at 30 seconds. It long-polls a pending mint.
  - The response adds `expiresAt` (ms), `tags`, `hostname`, `ephemeral` and `controlURL`.
  - Errors: 409 `lease_lost`, 503 `identity_pending`, 410 `killed`, 409 `already_joined` (rejoin from saved state), 502 `bootstrap_failed`.
  - An expired unused key is revoked and re-minted automatically.
- `POST …/ready` also takes `epoch?`, which fences the commit. If the object was killed during the join, it returns 410 `killed` and mgmt deletes the device that just appeared.
- The kill response is `{killed:[ids], devicesDeleted:[ids], errors?:{id:msg}}`. `cascade` kills the subtree below the given object. Hosts may kill objects only in namespaces they hold.
- Added `GET /v0/objects/{id}` (with `children`), `GET /v0/hosts` and `GET /v0/healthz`.

**Audit hash chain** (each host has its own chain, `host:<hostID>`; mgmt's chain is `mgmt`):
```
body    = the record JSON object without "hash" and "hostSig" (it includes "prevHash"; "" for the first record)
canon   = body with keys sorted at every level, no whitespace, numbers as written, no HTML escaping
hash    = lowercase hex(SHA-256(canon))
hostSig = base64(ed25519.Sign(hostKey, utf8(hash)))
```
- `POST /v0/audit` requires `kind` and `hash` on every record, and responds `{ok, accepted, chain:{chain, ok, records, brokenAt?, error?}}`.
- `GET /v0/audit` also filters by `tenant=`, `chain=` and `limit=`, and responds `{records, chains:[status…], verified}`. `GET /v0/audit/verify` covers every chain.
- The store rejects UPDATE/DELETE on audit rows (triggers). Verification detects modified, removed, reordered and truncated records.

## M4 additions (twd CLI; backward compatible)
**Workload entry node (closes hostd gap 1).** If `twd.jsonc` has `tailnet.tags`, mgmt declares the namespace `_workload` with those tags for every deploy, so a Worker-only bundle (no `tailnet.objects`, e.g. `hello`) deploys. `POST /v0/objects {namespace:"_workload", objectName:<workload>}` resolves to *that workload's* latest deployment (not the tenant's latest `_workload`). Its object ID slug is the workload name (`agent-<b32>`). Assignments carry it as `workloadTags` and omit it from `namespaces`. Classes with `"node": false` (e.g. `TwdBudget`) are not namespaces and need no tags. A bundle with neither `tailnet.tags` nor `tailnet.objects` is still a 400.

**Kill push (closes hostd gap 2).** `POST /v0/hosts/register` accepts `pushURL` (only `unix:///…`; kept in memory, hosts re-register on restart). Kill pushes `POST /v0/push/kill {objectID, reason}` to the lease holder for every killed object, in parallel, *before* any device delete (bounded by 2 s per push; heartbeat `revokeObjects` stays the fallback). Deploy pushes `POST /v0/push/assignments`.
- `twd-mgmt -device-delete-delay D` defers tier 2 by D after the push, so the host's tombstone (hostd `-kill-tombstone` > D) answers *new* connections with a typed `410 object_killed` while peers still have the node in their netmaps (M1 finding 3). Pending deletes run at once on `Close` (SIGTERM) and on `POST /v0/flush`. Each one writes an audit record `kind:"device_delete"`.
- The kill response adds `pushed:[ids]`, `pushMs`, `devicesDeleting:[ids]`; the kill audit detail adds `pushed` and `deviceDeleteIn`.

**Deployments.** The heartbeat's `deployments:[{deploymentID,status,error}]` is now read (in memory). New `GET /v0/deployments/{id}` → `{deploymentID, tenantID, workload, host, epoch, status:"assigned"|"starting"|"active"|"failed"|"superseded"|…, error, namespaces, endpoint:{objectID, node, magicDNS, status}}` (endpoint = the `_workload` object). `GET /v0/deployments?tenant=` lists the latest deployment per workload.

**Preflight.** `POST /v0/tenants/{id}/preflight {srcTags, connects, port?}` → `{ok, missing:[{src,dst,snippet}], missingTagOwners, tagOwnersSnippet, unchecked}`. A `connects` entry that is a tag (or `*`) needs a grant with an `ip` part whose `src` matches each src tag (or `*`); names are reported `unchecked`. Every src tag must be in `tagOwners`. Read-only (policy cache).

**Ops.** `GET /v0/status` → `{ok, pendingDeviceDeletes}`; `POST /v0/flush` runs pending device deletes now.

**Audit.** `kind:"principal"` (host chain): an object learned whom it acts for after it was created (its first caller-bearing request came after an RPC). `twd audit` uses create/spawn/principal `actingFor` as the head of the chain.

## M7 additions: identity scope and multi-tenant hosts (backward compatible)

**Object identity is `(tenant, workload, namespace = class, objectName)`.** Before M7 it was `(tenant, namespace, objectName)`, so two workloads of one tenant with the same DO class and object name shared one object and one node (M5 finding; hostd failed closed with `namespace_conflict`).
- `POST /v0/objects` takes an optional `workload`. hostd always sends it. Without it, a spawn takes its parent's workload, the `_workload` namespace takes `objectName`, and otherwise exactly one workload of the tenant may declare the class: if several do, the answer is **409 `namespace_ambiguous`**. An unknown `(workload, class)` is 404 `unknown_namespace`. For `_workload`, `workload` must equal `objectName` (400 otherwise).
- Create is idempotent by `(tenant, workload, namespace, objectName)`. The object ID (= node hostname) is `<class-slug>-<b32(hmac(tenant, workload, namespace, name))[:10]>`. If that ID is already taken by another identity, the answer is 409 `identity_conflict`, and mgmt never hands out the existing node.
- Objects carry `workload`. `GET /v0/objects?tenant=&workload=&namespace=` filters by it. The create audit record's detail adds `workload`.
- **Store migration.** A store from before M7 (no `workload` column) is migrated in place when it is opened (SQLite and Postgres): the table is renamed aside and copied back, with the workload taken from each object's deployment (`_workload` objects: their name). IDs are kept, so existing nodes keep their identity.
- hostd keeps `namespace_conflict` as a **defensive assertion**: if mgmt ever returns an object hostd already holds for a different `(tenant, workload, class, name)`, it refuses rather than share the node.

**Multi-tenant hosts.** One host serves any number of tenants, each on its own tailnet. Per tenant:
- its own netd registry, whose key source only fetches credentials for that tenant's objects;
- its own workerd process per workload (never shared);
- its own state dir `tenants/<tenantID>/{nodes,workloads}`, 0700 at every level (tenant IDs must be one safe path element);
- its own registration throttle and mint queue in mgmt.

On macOS, each process-tier workerd runs under Seatbelt (`twd-hostd -sandbox auto|seatbelt|none`, plus `-sandbox-deny`). The profile denies reads, writes and unix-socket connects to the home dir, hostd's data and run dirs, twd's state dir, mgmt's socket and every tenant's API key file (`GET /v0/tenants`), except the instance's own dir and its own workload's DO storage. Host status (`GET /v0/status` on hostd) reports each workload's `PID`, `Sandbox`, `SandboxProfile` and `DOStorage`.
