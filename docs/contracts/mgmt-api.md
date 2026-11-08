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
