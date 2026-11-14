<!-- GENERATED from api/openapi.yaml by `make api` (api/render). Do not edit; edit the spec. -->

# twd management API (1.0.0-pre)

_twd-mgmt ⇄ twd-hostd ⇄ twd CLI contract (/v1)_

This document is the **single source of truth** for `/v1`. Go server
interfaces and types (`mgmt/api/v1`), the hostd/CLI client
(`hostd/mgmtapi/v1`) and `docs/contracts/mgmt-api.md` are generated from
it (`make api`); none of them is hand-edited. `/v0` is gone: twd is
pre-release and `/v1` is a breaking replacement with no compatibility shim.

## Transport and authentication (tsnet-only in normal operation)

mgmt serves the API on **its own tsnet node** `twd-mgmt` (`tag:twd-mgmt`),
on `:80` and, when the tailnet has HTTPS, on `:443` with the node's
certificate (tailnet-only). The OIDC issuer (`/.well-known/...`, `/jwks`,
`/t/{tenant}/...`) is the only thing served over Funnel.

Every tailnet request is authenticated by **Tailscale WhoIs on the
connection**, never by a bearer token or API key:

- **host** — the peer node carries `tag:twd-host`. hostd runs its own
  tsnet node `twd-host-<hostID>`; the node is bound to the `hostID` it
  registered with (`PUT /v1/hosts/{id}`), and may only act on that host's
  records and on the namespaces it holds a lease for.
- **admin** — the peer's WhoIs `CapMap` holds the app capability
  `twd.dev/cap/admin` toward `tag:twd-mgmt` (granted by the base policy
  snippet to `autogroup:admin`; tenants may grant it to users, groups or
  tags such as a CI node). `twd-mgmt -admins <logins>` is an override.
- everyone else: `403 not_admin` with the grant `snippet` to add.

Each operation declares its role in `x-twd-role` (`admin`, `host`,
`admin|host`, `any`). The security scheme `tailnetWhoIs` documents this;
it is **not** a token scheme and no header is sent.

The unix socket + host token (RT-03) survives **only** behind
`--local-dev` (`twd dev`, hermetic tests). It is the `localHostToken`
scheme; `twd doctor` reports `transport: tsnet` in normal operation and
flags any local-socket use.

mgmt → hostd pushes (kill, assignments) go to the host's tsnet node and are
hostd's API, not this document's.

## Conventions

- JSON everywhere. Timestamps are unix **milliseconds** unless the field
  says otherwise (`AuditRecord.ts` is RFC 3339).
- **Pagination.** Every list returns `{"items": [...], "nextCursor":
  "..." | null}`. `?limit=` defaults to 50; 1–500 is accepted and anything
  else is `400 bad_request`. `?cursor=` is opaque; it encodes the
  stable sort key `(createdAt, id)` (audit: `(ts, chain, seq)`), so a page
  is stable under concurrent inserts. A cursor that does not decode is
  `400 bad_cursor`. Filters are query parameters and must be repeated
  unchanged with the cursor.
- **ETags.** Every single-resource `GET` sets a strong `ETag`
  (`"<sha256-prefix of the canonical JSON>"`). `PATCH`/`DELETE` accept
  `If-Match`; a stale value is `412 precondition_failed`. `If-None-Match`
  on `GET` answers `304`.
- **PATCH** bodies are partial JSON following **JSON Merge Patch (RFC
  7396)**: absent keys are untouched, `null` clears a field. Media type
  `application/merge-patch+json` (plain `application/json` is accepted).
- **DELETE** takes options as query parameters, never a body.
- **Async creates** answer `202 Accepted` with a `Location` header to poll.
- **Tombstones.** A killed object stays readable as `410 Gone` whose body
  is the typed error plus the kill record (`TombstoneError`).
- **Errors** are the DX.md catalog shape
  `{code, message, missing?, snippet?, doc_url, trace_id}` and also set
  `Twd-Error: <code>`; `identity_pending` sets `Retry-After: 1`.
- **Audit.** Every mutating operation is marked `x-twd-audit: true` and
  writes to mgmt's hash chain with the caller's actor (`deployedBy`,
  `by`): the WhoIs login for users, `<tag> (<stableNodeID>)` for tagged
  admin nodes, `host:<id>` for hosts, `Twd-Actor` or `local` on the
  local-dev socket.

## Decisions recorded here

- **Preflight is `POST /v1/deployments?dryRun=true`** (decided by Ross,
  2026-11-08): the request body is exactly a real deploy's; mgmt parses
  `twd.jsonc` from the bundle as a deploy would (`tailnet.tags`,
  `tailnet.objects.*.tags` → `srcTags`; `tailnet.connects`), checks grants
  and tagOwners against the policy cache and answers `200 PreflightResult`
  (missing grants with HuJSON snippets). Nothing is created, no lease is
  taken, and the only audit record is an optional `kind: preflight`.
  There is no separate preflight resource.
- **Audit verification is `GET /v1/audit/verification`**: the noun form.
  It is a read-only report, so a resource name fits, and it needs no
  custom-method (`:verb`) syntax that routers, generators and linters
  treat specially. No reason was found to prefer `:verify`.
- **Credentials and key grants are `POST`** (`…/credentials`,
  `…/keys`): each call *creates* a single-use grant (a fresh auth key, a
  fenced key release) and is audited, so a `GET` would be wrong.
- The only other RPC-shaped operations are diagnostics that produce a
  report: `POST /v1/tenants/{id}/wif-check` (a live token exchange) and
  `POST /v1/device-delete-runs` (execute the scheduled device deletes
  now). Both are documented as such.

## Route table

| Method | Path | Role | Audited | Operation | Summary |
|---|---|---|---|---|---|
| `GET` | `/v1/audit` | admin | no | `ListAudit` | Query audit records |
| `POST` | `/v1/audit` | host | yes | `AppendAudit` | Append a batch of host audit records |
| `GET` | `/v1/audit/verification` | admin | no | `GetAuditVerification` | Verify every audit chain |
| `GET` | `/v1/certificates/{stableNodeID}` | host\|admin | no | `GetCertificate` | Look up a node certificate by stable node ID |
| `GET` | `/v1/deployments` | admin | no | `ListDeployments` | List deployments |
| `POST` | `/v1/deployments` | admin | yes | `CreateDeployment` | Deploy a workload (async), or preflight it with dryRun |
| `GET` | `/v1/deployments/{id}` | admin\|host | no | `GetDeployment` | Read a deployment and its status |
| `DELETE` | `/v1/deployments/{id}` | admin | yes | `DeleteDeployment` | Undeploy |
| `GET` | `/v1/deployments/{id}/bundle` | host\|admin | no | `GetDeploymentBundle` | Download the raw bundle |
| `POST` | `/v1/device-delete-runs` | admin | yes | `CreateDeviceDeleteRun` | Run the scheduled device deletes now |
| `GET` | `/v1/healthz` | any | no | `GetHealthz` | Liveness |
| `GET` | `/v1/hosts` | admin | no | `ListHosts` | List hosts |
| `POST` | `/v1/hosts` | admin | yes | `CreateHost` | Add a host and mint its single-use join key |
| `GET` | `/v1/hosts/{id}` | admin\|host | no | `GetHost` | Read a host |
| `PUT` | `/v1/hosts/{id}` | host | yes | `RegisterHost` | Register or re-register a host (idempotent) |
| `DELETE` | `/v1/hosts/{id}` | admin | yes | `DeleteHost` | Drain and remove a host |
| `GET` | `/v1/hosts/{id}/assignments` | host\|admin | no | `ListHostAssignments` | List the deployments assigned to a host |
| `PUT` | `/v1/hosts/{id}/status` | host | no | `ReportHostStatus` | Heartbeat (replace the host's reported status) |
| `GET` | `/v1/objects` | admin\|host | no | `ListObjects` | List objects (with lineage) |
| `POST` | `/v1/objects` | host | yes | `CreateObject` | Create an object (async mint) |
| `GET` | `/v1/objects/{id}` | admin\|host | no | `GetObject` | Read an object (lineage, children, kill record) |
| `PATCH` | `/v1/objects/{id}` | host\|admin | yes | `UpdateObject` | Update an object (JSON Merge Patch) |
| `DELETE` | `/v1/objects/{id}` | admin\|host | yes | `DeleteObject` | Kill an object |
| `GET` | `/v1/objects/{id}/certificate` | host\|admin | no | `GetObjectCertificate` | Read the object's node certificate |
| `POST` | `/v1/objects/{id}/credentials` | host | yes | `CreateObjectCredential` | Mint a single-use auth key for the object's node |
| `GET` | `/v1/status` | admin | no | `GetStatus` | mgmt status |
| `GET` | `/v1/tenants` | admin\|host | no | `ListTenants` | List tenants |
| `POST` | `/v1/tenants` | admin | yes | `CreateTenant` | Create a tenant |
| `GET` | `/v1/tenants/{id}` | admin\|host | no | `GetTenant` | Read a tenant |
| `PATCH` | `/v1/tenants/{id}` | admin | yes | `UpdateTenant` | Update a tenant (JSON Merge Patch) |
| `DELETE` | `/v1/tenants/{id}` | admin | yes | `DeleteTenant` | Delete a tenant |
| `DELETE` | `/v1/tenants/{id}/objects` | admin | yes | `PurgeTenantObjects` | Purge terminal objects of a tenant |
| `POST` | `/v1/tenants/{id}/wif-check` | admin | no | `CheckTenantWIF` | Verify a wif tenant's trust credentials (diagnostic) |
| `POST` | `/v1/volumes/{id}/keys` | host | yes | `CreateVolumeKeyGrant` | Release a volume's dm-crypt data key to the lease holder |
| `GET` | `/v1/whoami` | any | no | `WhoAmI` | Who the caller is, as mgmt sees it |
| `GET` | `/.well-known/openid-configuration` | public | no | `GetOIDCConfiguration` | OIDC discovery (base issuer) |
| `GET` | `/jwks` | public | no | `GetJWKS` | Issuer signing keys |
| `GET` | `/t/{tenant}/.well-known/openid-configuration` | public | no | `GetTenantOIDCConfiguration` | OIDC discovery (per-tenant issuer `…/t/{tenant}`) |
| `GET` | `/t/{tenant}/jwks` | public | no | `GetTenantJWKS` | Per-tenant issuer signing keys (same key set) |

## Tenants

Customer tailnets. Admin only.

### `GET /v1/tenants` — List tenants

`ListTenants` · role **admin\|host** · audited: no

Hosts may list tenants too (they deny every tenant's `apiKeyFile` and `oauthClientFile` path to their workload sandboxes).

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `limit` | query | integer | no | Page size, 1–500 (default 50). Out-of-range values are `400 bad_request`. |
| `cursor` | query | string | no | Opaque page cursor from the previous page's `nextCursor`. Filters must be repeated unchanged. |

| Status | Body | Description |
|---|---|---|
| 200 | `TenantPage` | One page of tenants, ordered by `(createdAt, tenantID)`. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `POST /v1/tenants` — Create a tenant

`CreateTenant` · role **admin** · audited: yes

**Request** `application/json`: `TenantCreate`

| Status | Body | Description |
|---|---|---|
| 201 | `Tenant` | Created. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 409 | `Error` | The request conflicts with the resource's current state. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `GET /v1/tenants/{id}` — Read a tenant

`GetTenant` · role **admin\|host** · audited: no

Includes `oboPublicKey` (hostd pins it) and, in wif mode, `wif` (issuer and subjects).

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Tenant ID (`tn_…` when generated; one safe path element). |
| `If-None-Match` | header | string | no | Answer `304` when the `ETag` still matches. |

| Status | Body | Description |
|---|---|---|
| 200 | `Tenant` | The tenant. |
| 304 | `Error` | The `If-None-Match` validator still matches. No body. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `PATCH /v1/tenants/{id}` — Update a tenant (JSON Merge Patch)

`UpdateTenant` · role **admin** · audited: yes

Updatable: `mintMode`, `wifClientID`, `wifLifecycleClientID`,
`apiKeyFile`, `oauthClientID`, `oauthClientFile`, `registrationsPerMinute`,
`maxObjects`, `pool`, `tier`.
Switching mode or changing a client ID / credential file drops cached tokens.
`tailnet` and `controlURL` are immutable (`400 immutable_field`).

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Tenant ID (`tn_…` when generated; one safe path element). |
| `If-Match` | header | string | no | Only apply if the resource's current `ETag` matches; otherwise `412 precondition_failed`. |

**Request** `application/json` or `application/merge-patch+json`: `TenantPatch`

| Status | Body | Description |
|---|---|---|
| 200 | `Tenant` | The updated tenant. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| 412 | `Error` | `If-Match` did not match the current `ETag`. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `DELETE /v1/tenants/{id}` — Delete a tenant

`DeleteTenant` · role **admin** · audited: yes

`409 tenant_has_live_objects` while any object of the tenant is live,
unless `force=true`: then every live object is killed (cascade, device
deletes through the tenant credential), killed/failed objects are
purged, deployments and leases are released, and the whole cascade is
audited (`kind: tenant_delete`). The tenant's custody entries (OBO root,
volume keys) are removed last.

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Tenant ID (`tn_…` when generated; one safe path element). |
| `If-Match` | header | string | no | Only apply if the resource's current `ETag` matches; otherwise `412 precondition_failed`. |
| `force` | query | boolean | no | Cascade-kill live objects and purge before deleting. |

| Status | Body | Description |
|---|---|---|
| 202 | `TenantDeleteResult` | Accepted; the cascade runs. Poll `GET /v1/tenants/{id}` until `404`. |
| 204 | — | Deleted (nothing was live). |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| 409 | `Error` | Live objects exist and `force` was not set. |
| 412 | `Error` | `If-Match` did not match the current `ETag`. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `DELETE /v1/tenants/{id}/objects` — Purge terminal objects of a tenant

`PurgeTenantObjects` · role **admin** · audited: yes

Forgets the tenant's objects in the given terminal `state`s (default:
both `killed` and `failed`) so the same `(workload, namespace, name)`
can be created again. Pending device deletes for them run first. Live
objects are never touched (there is no `force` here; use `DELETE
/v1/objects/{id}` to kill). Each removal is audited (`kind: purge`).
Replaces `POST /v0/tenants/{id}/purge`.

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Tenant ID (`tn_…` when generated; one safe path element). |
| `state` | query | array of string: `killed` \| `failed` | no | Terminal states to purge. Repeatable. Any other value is `400 bad_request`. |

| Status | Body | Description |
|---|---|---|
| 200 | `PurgeResult` | Purged. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `POST /v1/tenants/{id}/wif-check` — Verify a wif tenant's trust credentials (diagnostic)

`CheckTenantWIF` · role **admin** · audited: no

A live diagnostic, not a state change: one fresh token exchange per
credential and a policy read with the lifecycle one
(`policy_file:read`). It is a `POST` because it performs work on the
control plane; it creates nothing. `400 bad_request` for non-wif tenants.

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Tenant ID (`tn_…` when generated; one safe path element). |

| Status | Body | Description |
|---|---|---|
| 200 | `WIFCheckResult` | The check report. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

## Hosts

twd-hostd instances. A host writes its own record; admins read.

### `GET /v1/hosts` — List hosts

`ListHosts` · role **admin** · audited: no

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `limit` | query | integer | no | Page size, 1–500 (default 50). Out-of-range values are `400 bad_request`. |
| `cursor` | query | string | no | Opaque page cursor from the previous page's `nextCursor`. Filters must be repeated unchanged. |
| `live` | query | boolean | no | Only hosts that heartbeated within the host lease TTL (10 s). |

| Status | Body | Description |
|---|---|---|
| 200 | `HostPage` | One page of hosts, ordered by `(createdAt, hostID)`. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `POST /v1/hosts` — Add a host and mint its single-use join key

`CreateHost` · role **admin** · audited: yes

`twd host add`: creates the host record (status `pending`) and mints a
**single-use, pre-authorized auth key for `tag:twd-host`** (10 min)
through the operator tenant's trust credential (wif) or API key
(apikey). `twd-hostd join --token <key>` joins the operator tailnet
with it as `twd-host-<hostID>`, then `PUT /v1/hosts/{id}` binds that
node to the record. Calling it again for a host that has not bound a
node yet mints a fresh key (`200`); a bound host is `409 exists`
(delete it first). The key is never logged. Audited
(`kind: host_add`).

**Request** `application/json`: `HostCreate`

| Status | Body | Description |
|---|---|---|
| 200 | `HostJoin` | The pending host exists; a fresh join key. |
| 201 | `HostJoin` | Created; the join key. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | Unknown operator tenant. |
| 409 | `Error` | The request conflicts with the resource's current state. |
| 502 | `Error` | The control plane refused the mint (`bootstrap_failed`). |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `GET /v1/hosts/{id}` — Read a host

`GetHost` · role **admin\|host** · audited: no

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Host ID (one safe path element). |
| `If-None-Match` | header | string | no | Answer `304` when the `ETag` still matches. |

| Status | Body | Description |
|---|---|---|
| 200 | `Host` | The host. |
| 304 | `Error` | The `If-None-Match` validator still matches. No body. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `PUT /v1/hosts/{id}` — Register or re-register a host (idempotent)

`RegisterHost` · role **host** · audited: yes

The host's own record. Over the tailnet the WhoIs node (`tag:twd-host`)
is bound to this `hostID` on first registration; a different node using
the same ID is `403 host_identity_mismatch`. Re-registering with the
same node replaces capacity, tiers, `auditPubKey` and `push`. Hosts
re-register on every restart.

A host that was created through `twd host add` exists before its first
`PUT` (status `pending`); the `PUT` activates it.

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Host ID (one safe path element). |

**Request** `application/json`: `HostRegistration`

| Status | Body | Description |
|---|---|---|
| 200 | `Host` | Re-registered. |
| 201 | `Host` | First registration of this host ID. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | Another node owns this host ID, or the caller is not a `tag:twd-host` node. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `DELETE /v1/hosts/{id}` — Drain and remove a host

`DeleteHost` · role **admin** · audited: yes

`409 host_holds_leases` while the host holds any namespace lease,
unless `drain=true`: then its deployments are marked `draining`, its
objects are killed (cascade), leases are released and the record goes.
Pushes to the host stop. The host's tsnet device is deleted through the
operator credential when mgmt has one; otherwise it is reported in
`devicesLeft`.

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Host ID (one safe path element). |
| `If-Match` | header | string | no | Only apply if the resource's current `ETag` matches; otherwise `412 precondition_failed`. |
| `drain` | query | boolean | no | Kill the host's objects and release its leases first. |

| Status | Body | Description |
|---|---|---|
| 202 | `HostDeleteResult` | Accepted; the drain runs. Poll `GET /v1/hosts/{id}` until `404`. |
| 204 | — | Removed (it held nothing). |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| 409 | `Error` | The host holds leases and `drain` was not set. |
| 412 | `Error` | `If-Match` did not match the current `ETag`. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `GET /v1/hosts/{id}/assignments` — List the deployments assigned to a host

`ListHostAssignments` · role **host\|admin** · audited: no

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Host ID (one safe path element). |
| `limit` | query | integer | no | Page size, 1–500 (default 50). Out-of-range values are `400 bad_request`. |
| `cursor` | query | string | no | Opaque page cursor from the previous page's `nextCursor`. Filters must be repeated unchanged. |

| Status | Body | Description |
|---|---|---|
| 200 | `AssignmentPage` | One page of assignments, ordered by `(createdAt, deploymentID)`. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `PUT /v1/hosts/{id}/status` — Heartbeat (replace the host's reported status)

`ReportHostStatus` · role **host** · audited: no

Every 3 s. Renews the host lease (TTL 10 s) and every namespace lease
in `leases` (TTL 15 s; a renew succeeds while `(holder, epoch)` still
matches). The reply lists the leases that are no longer held
(`revoke`) and the objects killed in the last 10 min in namespaces
this host holds (`revokeObjects`; they repeat until the window passes,
so the host's kill must be idempotent). Replaces
`POST /v0/hosts/{id}/heartbeat`. Not audited (3 s cadence).

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Host ID (one safe path element). |

**Request** `application/json`: `HostStatus`

| Status | Body | Description |
|---|---|---|
| 200 | `HostStatusResponse` | Renewed. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | The host never registered (`host_not_registered`). |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

## Deployments

Deploys of a workload bundle to a host.

### `GET /v1/deployments` — List deployments

`ListDeployments` · role **admin** · audited: no

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `limit` | query | integer | no | Page size, 1–500 (default 50). Out-of-range values are `400 bad_request`. |
| `cursor` | query | string | no | Opaque page cursor from the previous page's `nextCursor`. Filters must be repeated unchanged. |
| `tenant` | query | string | no |  |
| `workload` | query | string | no |  |
| `host` | query | string | no |  |
| `latest` | query | boolean | no | Only the newest deployment of each `(tenant, workload)` (what `twd ls` shows). |

| Status | Body | Description |
|---|---|---|
| 200 | `DeploymentPage` | One page of deployments, ordered by `(createdAt, deploymentID)`. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `POST /v1/deployments` — Deploy a workload (async), or preflight it with dryRun

`CreateDeployment` · role **admin** · audited: yes

mgmt reads `twd.jsonc` from the bundle (a tar, optionally gzipped;
the shallowest `twd.jsonc`): `tailnet.tags` (the `_workload` entry
node), `tailnet.objects.<Class>.{tags, agent, ttl, node}`,
`tailnet.spawn.{maxLive ≤ 64, maxDepth ≤ 3}`, `tailnet.connects`,
`tier`. A bundle with neither `tailnet.tags` nor `tailnet.objects` is
`400 bad_bundle`. mgmt also validates `wrangler.jsonc`
`triggers.crons` (cron triggers: at most 10, no duplicates, each a
valid five-field UTC expression that can fire); an invalid one is
`400 bad_bundle`.

**Deploy** (`dryRun` absent or false): picks a host (`host` pins one),
acquires the `(tenant, workload)` lease at `epoch+1`, stores the
bundle, pushes the assignment to the host and answers `202` with
`Location: /v1/deployments/{id}`. Poll it until `status` is `active`
or `failed`.

**Preflight** (`dryRun=true`): the same body and parser, but mgmt
only checks the policy: every src tag must be in `tagOwners`, and
each `tailnet.connects` entry that is a tag (or `*`) needs a grant
with an `ip` part whose `src` matches every src tag. Names are
reported `unchecked`. Nothing is written, no lease is taken, and the
answer is `200 PreflightResult`. `503 policy_unavailable` when mgmt
cannot read the policy (wif tenant without a lifecycle credential);
the CLI then prints `preflight skipped` and deploys.

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `dryRun` | query | boolean | no | Preflight only; see the description. |

**Request** `application/json`: `DeploymentCreate`

| Status | Body | Description |
|---|---|---|
| 200 | `PreflightResult` | `dryRun=true`: the preflight report. Nothing was deployed. |
| 202 | `Deployment` | Accepted; the host is starting the workload. |
| 400 | `Error` | Bad body or bundle. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | Unknown tenant. |
| 503 | `Error` | No live host can take the workload, or the policy cannot be read (dryRun). |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `GET /v1/deployments/{id}` — Read a deployment and its status

`GetDeployment` · role **admin\|host** · audited: no

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Deployment ID (`dp_…`). |
| `If-None-Match` | header | string | no | Answer `304` when the `ETag` still matches. |

| Status | Body | Description |
|---|---|---|
| 200 | `Deployment` | The deployment, with the host-reported status and the entry node. |
| 304 | `Error` | The `If-None-Match` validator still matches. No body. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `DELETE /v1/deployments/{id}` — Undeploy

`DeleteDeployment` · role **admin** · audited: yes

Kills every object of the deployment (cascade; pushed to the host, then
device deletes), releases the `(tenant, workload)` lease, marks the
deployment `stopped` and audits it (`kind: undeploy`). The record stays
readable (`status: stopped`) for history; a later deploy of the same
workload supersedes it. Only the latest deployment of a workload can be
undeployed (`409 superseded` otherwise).

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Deployment ID (`dp_…`). |
| `If-Match` | header | string | no | Only apply if the resource's current `ETag` matches; otherwise `412 precondition_failed`. |
| `reason` | query | string | no |  |

| Status | Body | Description |
|---|---|---|
| 202 | `KillResult` | Accepted; the kills are in flight. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| 409 | `Error` | The request conflicts with the resource's current state. |
| 412 | `Error` | `If-Match` did not match the current `ETag`. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `GET /v1/deployments/{id}/bundle` — Download the raw bundle

`GetDeploymentBundle` · role **host\|admin** · audited: no

Hosts fetch the bundle named by `Assignment.bundleURL`. Only the lease holder (or an admin) may read it.

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Deployment ID (`dp_…`). |

| Status | Body | Description |
|---|---|---|
| 200 | string (binary) | The bundle bytes (tar, optionally gzipped) exactly as uploaded. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

## Objects

DO instances, agents and sub-agents, each with its own tailnet node.

### `GET /v1/objects` — List objects (with lineage)

`ListObjects` · role **admin\|host** · audited: no

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `limit` | query | integer | no | Page size, 1–500 (default 50). Out-of-range values are `400 bad_request`. |
| `cursor` | query | string | no | Opaque page cursor from the previous page's `nextCursor`. Filters must be repeated unchanged. |
| `tenant` | query | string | no |  |
| `workload` | query | string | no |  |
| `namespace` | query | string | no | DO class name, or `_workload`. |
| `state` | query | array of `ObjectStatus` | no | Repeatable. Default is every state, tombstones included. |
| `parent` | query | string | no | Direct children of this object ID. |
| `root` | query | string | no | The whole tree under this root object ID. |

| Status | Body | Description |
|---|---|---|
| 200 | `ObjectPage` | One page of objects, ordered by `(createdAt, objectID)`. Hosts see only the namespaces they hold. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `POST /v1/objects` — Create an object (async mint)

`CreateObject` · role **host** · audited: yes

hostd calls this on first `create()`/`spawn()`. The identity is
`(tenant, workload, namespace = DO class, objectName)`; the object ID
`<class-slug>-<b32(hmac)[:10]>` is also the node hostname.

- Idempotent by identity: an existing live object is returned with
  `200`; a new one is `202` with `Location: /v1/objects/{id}` and
  `status: pending`. A killed identity is `410 killed`
  (`TombstoneError`); an ID collision with another identity is
  `409 identity_conflict`.
- `namespace: "_workload"` is the workload entry node; `workload`
  must equal `objectName` (`400`). An unknown `(workload, class)` is
  `404 unknown_namespace`; without `workload`, several workloads
  declaring the class is `409 namespace_ambiguous`.
- `parent` makes it a spawn: `caps ⊆ parent` (receiver merge
  semantics over the policy; `403 delegation_invalid` with
  `missing: {src, dst, cap, field}`), every child tag owned by the
  parent's (`tagOwners`), and the spawn budget (`maxLive`,
  `maxDepth`; `429 spawn_budget_exceeded`). A spawn with a `ttl` gets
  an ephemeral key.
- The mint is queued behind the tenant throttle
  (`registrationsPerMinute`) and quota (`maxObjects`;
  `429 object_quota_exceeded`).
Spawn checks are unchanged from v0.

**Request** `application/json`: `ObjectCreate`

| Status | Body | Description |
|---|---|---|
| 200 | `Object` | The object already existed (idempotent create). |
| 202 | `Object` | Accepted; the mint is queued. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | Delegation refused, or the host does not hold the namespace. |
| 404 | `Error` | Unknown tenant or namespace. |
| 409 | `Error` | `identity_conflict` or `namespace_ambiguous`. |
| 410 | `TombstoneError` | The object was killed. The body is the typed error plus the kill record; it stays until purged. |
| 429 | `Error` | Spawn budget or tenant quota. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `GET /v1/objects/{id}` — Read an object (lineage, children, kill record)

`GetObject` · role **admin\|host** · audited: no

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Object ID (`<class-slug>-<b32(hmac)[:10]>`, also the node hostname). |
| `If-None-Match` | header | string | no | Answer `304` when the `ETag` still matches. |

| Status | Body | Description |
|---|---|---|
| 200 | `Object` | The object. |
| 304 | `Error` | The `If-None-Match` validator still matches. No body. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| 410 | `TombstoneError` | The object was killed. The body is the typed error plus the kill record; it stays until purged. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `PATCH /v1/objects/{id}` — Update an object (JSON Merge Patch)

`UpdateObject` · role **host\|admin** · audited: yes

Two callers, two subsets:

- **host** (lease holder): `{status: "ready", nodeID, stableNodeID,
  magicDNS, epoch}` reports the node joined. `epoch` fences the
  commit (`409 lease_lost`). mgmt then signs the **node certificate**
  (`{v, tenant, objectID, stableNodeID, tags, lim?, parent?, root,
  iat, exp}`, 24 h, the tenant's OBO root key) and stores it; the
  `ready` audit detail adds `cert: true` and `lim`. If the object was
  killed during the join the answer is `410 killed` and mgmt deletes
  the device that just appeared. Replaces `POST /v0/objects/{id}/ready`.
- **admin**: `{ttl}` sets or clears (`null`) the TTL; the reaper
  applies it.

Any other field, or a host sending admin fields (and vice versa), is
`400 immutable_field`.

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Object ID (`<class-slug>-<b32(hmac)[:10]>`, also the node hostname). |
| `If-Match` | header | string | no | Only apply if the resource's current `ETag` matches; otherwise `412 precondition_failed`. |

**Request** `application/json` or `application/merge-patch+json`: `ObjectPatch`

| Status | Body | Description |
|---|---|---|
| 200 | `Object` | The updated object (`status: ready` carries `cert`). |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| 409 | `Error` | The caller does not hold the namespace lease at this epoch. |
| 410 | `TombstoneError` | The object was killed. The body is the typed error plus the kill record; it stays until purged. |
| 412 | `Error` | `If-Match` did not match the current `ETag`. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `DELETE /v1/objects/{id}` — Kill an object

`DeleteObject` · role **admin\|host** · audited: yes

Marks the object (and with `cascade=true` every descendant) killed,
pushes the kill to the lease-holding host (tier 1, ≤ 2 s per push;
the heartbeat's `revokeObjects` is the fallback), then deletes the
device(s) through the tenant credential (tier 2; deferred by
`-device-delete-delay` so the host's tombstone answers
`410 object_killed` while peers still have the node). Audited
(`kind: kill`, `detail.by`, `pushed`, `deviceDeleteIn`).

Hosts may kill only objects in namespaces they hold (`403`).
A later `GET` is `410 Gone` with the kill record. Replaces
`POST /v0/objects/{id}/kill`.

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Object ID (`<class-slug>-<b32(hmac)[:10]>`, also the node hostname). |
| `If-Match` | header | string | no | Only apply if the resource's current `ETag` matches; otherwise `412 precondition_failed`. |
| `cascade` | query | boolean | no | Also kill the subtree below this object. |
| `reason` | query | string | no |  |

| Status | Body | Description |
|---|---|---|
| 202 | `KillResult` | Accepted; tier 1 is done or pushed, device deletes are scheduled. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| 410 | `TombstoneError` | The object was killed. The body is the typed error plus the kill record; it stays until purged. |
| 412 | `Error` | `If-Match` did not match the current `ETag`. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `GET /v1/objects/{id}/certificate` — Read the object's node certificate

`GetObjectCertificate` · role **host\|admin** · audited: no

The certificate mgmt signed at `ready` (re-issued on fetch when missing
or within 1 h of expiry). The receiving netd verifies it against the
tenant's `oboPublicKey` and narrows the caller's caps to `cert.lim`;
a certificate that fails verification, names another node or cannot
be fetched makes netd withhold the caller's caps. `410 killed` for
tombstones. Replaces `GET /v0/certs/{nodeID}`; see also
`GET /v1/certificates/{stableNodeID}` for lookup by node.

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Object ID (`<class-slug>-<b32(hmac)[:10]>`, also the node hostname). |

| Status | Body | Description |
|---|---|---|
| 200 | `NodeCertificate` | The certificate. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| 409 | `Error` | The object has no node yet (`not_ready`). |
| 410 | `TombstoneError` | The object was killed. The body is the typed error plus the kill record; it stays until purged. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `POST /v1/objects/{id}/credentials` — Mint a single-use auth key for the object's node

`CreateObjectCredential` · role **host** · audited: yes

Creates one single-use, tagged auth key (≤ 5 min) and releases it
**only** to the host that holds the namespace lease at `epoch`
(`409 lease_lost` otherwise). Long-polls a pending mint for `wait`
seconds (default 10, max 30; `503 identity_pending` + `Retry-After: 1`
after that). An expired unused key is revoked and re-minted
automatically. `Cache-Control: no-store`.

`reason=rejoin_rejected` is the **re-mint**: the saved node state was
rejected by control, so mgmt deletes the old device, clears the node
certificate and mints again under the same hostname. Replaces
`GET /v0/objects/{id}/credential` and `POST …/remint`. Audited
(`kind: mint` / `remint`). The key is never logged.

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Object ID (`<class-slug>-<b32(hmac)[:10]>`, also the node hostname). |
| `epoch` | query | integer (int64) | yes | The namespace lease epoch the caller believes it holds. Fenced in the store (`409 lease_lost`). |
| `wait` | query | integer | no | Seconds to wait for a pending mint. |
| `reason` | query | string: `rejoin_rejected` | no | `rejoin_rejected` requests a re-mint; omitted for a first join. |

| Status | Body | Description |
|---|---|---|
| 201 | `Credential` | The key. Single use; it expires at `expiresAt`. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| 409 | `Error` | `lease_lost` (wrong epoch or holder) or `already_joined` (the node exists; rejoin from saved state, or re-mint with `reason`). |
| 410 | `TombstoneError` | The object was killed. The body is the typed error plus the kill record; it stays until purged. |
| 502 | `Error` | The control plane refused the mint (`bootstrap_failed`); the object is `failed`. |
| 503 | `Error` | Still minting (`identity_pending`). |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

## Credentials

Single-use auth keys and node certificates.

### `GET /v1/certificates/{stableNodeID}` — Look up a node certificate by stable node ID

`GetCertificate` · role **host\|admin** · audited: no

The receiver-side lookup: "is the peer that just connected a twd
object, and what is its `lim`?" `404 not_found` when no live object
of the tenant has that node (the receiver then applies the WhoIs caps
unchanged: the peer is not a twd object); `410 killed` for a tombstone.

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `stableNodeID` | path | string | yes |  |
| `tenant` | query | string | yes | The receiving object's tenant (certificates are per tenant). |

| Status | Body | Description |
|---|---|---|
| 200 | `NodeCertificate` | The certificate. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| 410 | `TombstoneError` | The object was killed. The body is the typed error plus the kill record; it stays until purged. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

## Audit

Hash-chained, append-only audit log.

### `GET /v1/audit` — Query audit records

`ListAudit` · role **admin** · audited: no

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `limit` | query | integer | no | Page size, 1–500 (default 50). Out-of-range values are `400 bad_request`. |
| `cursor` | query | string | no | Opaque page cursor from the previous page's `nextCursor`. Filters must be repeated unchanged. |
| `object` | query | string | no | Records about this object ID. |
| `human` | query | string | no | Records whose `actingFor` is this principal (the human at the head of a chain). |
| `trace` | query | string | no |  |
| `tenant` | query | string | no |  |
| `chain` | query | string | no | `mgmt` or `host:<hostID>`. |
| `kind` | query | array of `AuditKind` | no |  |
| `since` | query | string | no | Records at or after this time (RFC 3339 or unix ms). |
| `until` | query | string | no |  |

| Status | Body | Description |
|---|---|---|
| 200 | `AuditPage` | One page, ordered by `(ts, chain, seq)`, with per-chain verification of the records returned. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `POST /v1/audit` — Append a batch of host audit records

`AppendAudit` · role **host** · audited: yes

Append-only. Each host has its own chain `host:<hostID>`; every
record carries `prevHash`, `hash` and `hostSig`
(`hash = hex(sha256(canonical body without hash/hostSig))`,
`hostSig = base64(ed25519.Sign(hostKey, utf8(hash)))`, verified
against the registered `auditPubKey`). `kind` and `hash` are required
on every record. Provenance (RT-02): `source: tenant` is allowed only
with `kind: tenant_note` and vice versa; `source: mgmt` is refused;
a missing source means `host`. The store refuses UPDATE/DELETE on
audit rows. Mutating by nature; it *is* the audit.

**Request** `application/json`: `AuditBatch`

| Status | Body | Description |
|---|---|---|
| 201 | `AuditAppendResult` | Appended; the chain status after the append. |
| 400 | `Error` | A record is malformed, breaks the chain, or claims a forbidden source. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `GET /v1/audit/verification` — Verify every audit chain

`GetAuditVerification` · role **admin** · audited: no

Recomputes every hash and link of every chain and checks each chain head; detects modified, removed, reordered and truncated records.

| Status | Body | Description |
|---|---|---|
| 200 | `AuditVerification` | The verification report (`ok` is the AND over chains). |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

## Volumes

dm-crypt data keys for DO volumes (microVM tier).

### `POST /v1/volumes/{id}/keys` — Release a volume's dm-crypt data key to the lease holder

`CreateVolumeKeyGrant` · role **host** · audited: yes

Creates a key grant: the 256-bit data key of the `(tenant, workload)`
DO volume (custody name `twd/<tenant>/volumes/<workload>/dek`,
created on first use) is released only to the host holding the
workload's lease at `epoch` (`409 lease_lost`). Audited
(`kind: volume_key`). `Cache-Control: no-store`. The key is never
logged. Replaces `GET /v0/volumes/key`.

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `id` | path | string | yes | Volume ID `<tenantID>:<workload>` (one volume per workload; `:` is a legal path character). |
| `epoch` | query | integer (int64) | yes | The namespace lease epoch the caller believes it holds. Fenced in the store (`409 lease_lost`). |

| Status | Body | Description |
|---|---|---|
| 201 | `VolumeKeyGrant` | The grant. |
| 400 | `Error` | Malformed request, body, filter or cursor. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| 404 | `Error` | No such resource (or none the caller may see). |
| 409 | `Error` | The caller does not hold the namespace lease at this epoch. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

## Operational

Health, status, identity, maintenance.

### `POST /v1/device-delete-runs` — Run the scheduled device deletes now

`CreateDeviceDeleteRun` · role **admin** · audited: yes

Kill tier 2 is deferred by `-device-delete-delay`; this creates a run
that executes every pending delete immediately (what `twd down` does
before stopping mgmt). Each delete is audited (`kind: device_delete`).
Replaces `POST /v0/flush`.

| Status | Body | Description |
|---|---|---|
| 201 | `DeviceDeleteRun` | The run's outcome. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `GET /v1/healthz` — Liveness

`GetHealthz` · role **any** · audited: no

Any authenticated principal (hosts included). On the local-dev socket the host token is still required (fail closed).

| Status | Body | Description |
|---|---|---|
| 200 | `Health` | Alive. |
| 401 | `Error` | No identity on the connection (WhoIs failed), or the local-dev socket was used without `Twd-Host-Token`. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `GET /v1/status` — mgmt status

`GetStatus` · role **admin** · audited: no

| Status | Body | Description |
|---|---|---|
| 200 | `Status` | Counters and the transport mgmt is serving on. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

### `GET /v1/whoami` — Who the caller is, as mgmt sees it

`WhoAmI` · role **any** · audited: no

`twd link` uses it to learn the endpoint and refuse non-admins early. Any authenticated principal, hosts included.

| Status | Body | Description |
|---|---|---|
| 200 | `WhoAmI` | The caller's principal and mgmt's self-description. |
| 403 | `Error` | The caller is authenticated but lacks the role, or a host acts outside its own records/namespaces. |
| default | `Error` | Unexpected error. The body is always the typed catalog shape; `Twd-Error` carries the code. |

## Oidc

The WIF issuer, served over Funnel. Public, unauthenticated.

### `GET /.well-known/openid-configuration` — OIDC discovery (base issuer)

`GetOIDCConfiguration` · role **public** · audited: no

| Status | Body | Description |
|---|---|---|
| 200 | `OIDCConfiguration` | The discovery document. |
| 404 | `Error` | No such resource (or none the caller may see). |

### `GET /jwks` — Issuer signing keys

`GetJWKS` · role **public** · audited: no

| Status | Body | Description |
|---|---|---|
| 200 | `JWKS` | The JWK set. |
| 404 | `Error` | No such resource (or none the caller may see). |

### `GET /t/{tenant}/.well-known/openid-configuration` — OIDC discovery (per-tenant issuer `…/t/{tenant}`)

`GetTenantOIDCConfiguration` · role **public** · audited: no

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `tenant` | path | string | yes |  |

| Status | Body | Description |
|---|---|---|
| 200 | `OIDCConfiguration` | The discovery document with `issuer = <base>/t/{tenant}`. |
| 404 | `Error` | No such resource (or none the caller may see). |

### `GET /t/{tenant}/jwks` — Per-tenant issuer signing keys (same key set)

`GetTenantJWKS` · role **public** · audited: no

| Parameter | In | Type | Required | Description |
|---|---|---|---|---|
| `tenant` | path | string | yes |  |

| Status | Body | Description |
|---|---|---|
| 200 | `JWKS` | The JWK set. |
| 404 | `Error` | No such resource (or none the caller may see). |

## Components

### Security schemes

#### `localHostToken` (apiKey)

`--local-dev` only (`twd dev`, hermetic tests): the unix socket
`~/.twd/run/mgmt.sock` with the 256-bit token from
`~/.twd/hostd/mgmt.token` (RT-03). Every request needs it, `/v1/healthz`
included; a mgmt without a token answers `503 host_token_unconfigured`.
The host names itself with `Twd-Host-ID`; admins may set `Twd-Actor`.
Never used in normal operation; `twd doctor` flags it.

#### `tailnetWhoIs` (mutualTLS)

**Not a token.** The caller's identity is the Tailscale node behind the
connection, resolved by WhoIs on mgmt's tsnet listener. No header is
sent. Roles: `host` = the peer carries `tag:twd-host` (bound to a
hostID at registration); `admin` = the peer's WhoIs CapMap holds
`twd.dev/cap/admin` toward `tag:twd-mgmt`. `mutualTLS` is the closest
OpenAPI scheme type (identity comes from the transport, not from a
credential in the request); the actual mechanism is WireGuard node
identity + WhoIs, and the `:80` listener is tailnet-only plaintext.

### Schemas

#### `Assignment`

What a host must run; `bundleURL` is `GET /v1/deployments/{id}/bundle`.

| Field | Type | Required | Description |
|---|---|---|---|
| `bundleURL` | string (uri-reference) | yes |  |
| `controlURL` | string (uri) | no |  |
| `createdAt` | integer (int64) | yes |  |
| `deploymentID` | string | yes |  |
| `epoch` | integer (int64) | yes |  |
| `fence` | boolean | no | Stop serving this workload when no heartbeat to mgmt has succeeded for the host's fence window (30 s): it has Durable Objects that must not run on two hosts (ADR-028 §3). Stateless workloads and `"fence": "never"` classes are false and keep serving. Hosts treat a missing field as true. |
| `lease` | string | yes |  |
| `maxDepth` | integer | no |  |
| `maxLive` | integer | no |  |
| `namespaces` | array of `ClassConfig` | yes |  |
| `tailnet` | string | yes |  |
| `tenantID` | string | yes |  |
| `tier` | `Tier` | no |  |
| `workload` | string | yes |  |
| `workloadTags` | array of string | no |  |

#### `AssignmentPage`

Extends `Page`.

| Field | Type | Required | Description |
|---|---|---|---|
| `items` | array of `Assignment` | no |  |

#### `AuditAppendResult`

| Field | Type | Required | Description |
|---|---|---|---|
| `accepted` | integer | yes |  |
| `chain` | `ChainStatus` | yes |  |

#### `AuditBatch`

| Field | Type | Required | Description |
|---|---|---|---|
| `records` | array of `AuditRecord` | yes |  |

#### `AuditKind`

Host chain (netd/hostd): `allow`, `deny`, `egress`, `spawn`, `kill`,
`principal`, `tenant_note` (source `tenant`, unverified). mgmt chain:
`tenant_create`, `tenant_update`, `tenant_delete`, `host_register`,
`host_delete`, `deploy`, `undeploy`, `create`, `mint`, `remint`,
`ready`, `kill`, `device_delete`, `purge`, `volume_key`, `reap`,
`preflight` (optional; dryRun deploys), `host_add`, `credential`, `obo_root`, `object_update`.

Enum: `allow`, `deny`, `egress`, `spawn`, `kill`, `principal`, `tenant_note`, `tenant_create`, `tenant_update`, `tenant_delete`, `host_register`, `host_delete`, `deploy`, `undeploy`, `create`, `mint`, `remint`, `ready`, `device_delete`, `purge`, `volume_key`, `reap`, `preflight`, `host_add`, `credential`, `obo_root`, `object_update`

#### `AuditPage`

Extends `Page`.

| Field | Type | Required | Description |
|---|---|---|---|
| `chains` | array of `ChainStatus` | yes | Verification status of every chain that contributed to this page. |
| `items` | array of `AuditRecord` | no |  |
| `verified` | boolean | yes | AND over `chains[].ok`. |

#### `AuditRecord`

One hash-chained record. Hash input is the record without `hash` and
`hostSig`, keys sorted at every level, no whitespace, numbers as
written, no HTML escaping. `seq` and `chain` are assigned by mgmt.

| Field | Type | Required | Description |
|---|---|---|---|
| `actingFor` | string | no |  |
| `chain` | string | no | (read-only) `mgmt` or `host:<hostID>`. |
| `code` | string | no |  |
| `detail` | object \| string | no | Structured detail (mgmt records) or free text (host `tenant_note`s). |
| `dst` | string | no |  |
| `grant` | string | no |  |
| `hash` | string | yes |  |
| `hostSig` | string | no | base64 ed25519 over utf8(hash); host chains only. |
| `kind` | `AuditKind` | yes | Host chain (netd/hostd): `allow`, `deny`, `egress`, `spawn`, `kill`, `principal`, `tenant_note` (source `tenant`, unverified). mgmt chain: `tenant_create`, `tenant_update`, `tenant_delete`, `host_register`, `host_delete`, `deploy`, `undeploy`, `create`, `mint`, `remint`, `ready`, `kill`, `device_delete`, `purge`, `volume_key`, `reap`, `preflight` (optional; dryRun deploys), `host_add`, `credential`, `obo_root`, `object_update`. |
| `objectID` | string | no |  |
| `prevHash` | string | yes | `""` for the first record of a chain. |
| `seq` | integer (int64) | no | (read-only) |
| `source` | `AuditSource` | no | `mgmt` for the mgmt chain; `host` for netd/hostd security records; `tenant` for a `tenant_note` written by tenant code (unverified content). |
| `tenantID` | string | no |  |
| `traceID` | string | no |  |
| `ts` | string (date-time) | yes |  |

#### `AuditSource`

`mgmt` for the mgmt chain; `host` for netd/hostd security records; `tenant` for a `tenant_note` written by tenant code (unverified content).

Enum: `mgmt`, `host`, `tenant`

#### `AuditVerification`

| Field | Type | Required | Description |
|---|---|---|---|
| `chains` | array of `ChainStatus` | yes |  |
| `ok` | boolean | yes |  |
| `verifiedAt` | integer (int64) | yes |  |

#### `Caps`

`{capName: value | [values]}`; see `caps ⊆ parent` semantics in `CreateObject`.

#### `ChainStatus`

| Field | Type | Required | Description |
|---|---|---|---|
| `brokenAt` | integer (int64) | no | seq of the first bad record |
| `chain` | string | yes |  |
| `error` | string | no |  |
| `ok` | boolean | yes |  |
| `records` | integer | yes |  |

#### `ClassConfig`

One DO namespace from `twd.jsonc` `tailnet.objects`.

| Field | Type | Required | Description |
|---|---|---|---|
| `agent` | boolean | no |  |
| `class` | string | yes |  |
| `tags` | array of string | yes |  |
| `ttl` | string | no | `10m`-style duration or seconds. |

#### `Credential`

A single-use auth key. Never logged; `String()` redacts it in Go.

| Field | Type | Required | Description |
|---|---|---|---|
| `authKey` | string (password) | yes | `tskey-auth-…`, single use. |
| `controlURL` | string (uri) | no |  |
| `ephemeral` | boolean | yes |  |
| `expiresAt` | integer (int64) | yes | unix ms |
| `hostname` | string | yes |  |
| `reminted` | boolean | no | True when `reason=rejoin_rejected` deleted the old device and re-minted. |
| `tags` | array of string | yes |  |

#### `Deployment`

| Field | Type | Required | Description |
|---|---|---|---|
| `createdAt` | integer (int64) | yes |  |
| `deployedBy` | string | no | The actor who deployed (see Conventions). |
| `deploymentID` | string | yes |  |
| `endpoint` | `Endpoint` | yes | The workload entry node (`_workload` object). |
| `epoch` | integer (int64) | yes |  |
| `error` | string | no |  |
| `host` | string | yes |  |
| `maxDepth` | integer | no |  |
| `maxLive` | integer | no |  |
| `namespaces` | array of `ClassConfig` | yes |  |
| `status` | `DeploymentStatus` | yes |  |
| `tenantID` | string | yes |  |
| `tier` | `Tier` | no |  |
| `updatedAt` | integer (int64) | no |  |
| `workload` | string | yes |  |
| `workloadTags` | array of string | no |  |

#### `DeploymentCreate`

| Field | Type | Required | Description |
|---|---|---|---|
| `bundle` | string | yes | tar (optionally gzipped) of the built worker + `twd.jsonc` + `wrangler.jsonc`. |
| `forceTakeover` | boolean | no | Confirm that the workload's current host, which has stopped heartbeating, is gone for good, so a stateful workload may move off it (ADR-028 §3). Without it such a move is refused with `takeover_unconfirmed`. Audited (`takeoverFrom`, `forced`). |
| `host` | string | no | Pin a host. |
| `namespaces` | array of `ClassConfig` | no | Override the classes read from `twd.jsonc` (tests). |
| `tenantID` | string | yes |  |
| `workload` | string | yes |  |

#### `DeploymentPage`

Extends `Page`.

| Field | Type | Required | Description |
|---|---|---|---|
| `items` | array of `Deployment` | no |  |

#### `DeploymentReport`

| Field | Type | Required | Description |
|---|---|---|---|
| `deploymentID` | string | yes |  |
| `error` | string | no |  |
| `status` | `DeploymentStatus` | yes |  |

#### `DeploymentStatus`

Enum: `assigned`, `starting`, `active`, `draining`, `stopped`, `failed`, `superseded`

#### `DeviceDeleteRun`

| Field | Type | Required | Description |
|---|---|---|---|
| `deleted` | array of string | yes | Object IDs whose devices were deleted. |
| `errors` | object | no |  |

#### `Endpoint`

The workload entry node (`_workload` object).

| Field | Type | Required | Description |
|---|---|---|---|
| `magicDNS` | string | yes |  |
| `node` | string | yes | hostname |
| `objectID` | string | no |  |
| `status` | `ObjectStatus` | yes | `pending` → `minted` → `ready`; terminal: `killed` (tombstone, `410`), `failed`. |

#### `Error`

The DX.md error catalog shape. `code` is stable and machine-readable;
`missing` names what is absent (grant, cap, field, quota); `snippet`
is policy HuJSON to paste when a grant is missing; `doc_url` is
`https://twd.dev/errors/<code>`; `trace_id` correlates with audit.

| Field | Type | Required | Description |
|---|---|---|---|
| `code` | `ErrorCode` | yes | The catalog. Status in parentheses. \| code \| status \| when \| \|---\|---\|---\| \| `bad_request` \| 400 \| malformed body, parameter or filter \| \| `bad_cursor` \| 400 \| `cursor` does not decode \| \| `bad_bundle` \| 400 \| bundle has no `twd.jsonc`, neither `tailnet.tags` nor `tailnet.objects`, or invalid `triggers.crons` \| \| `immutable_field` \| 400 \| PATCH touches a field the caller may not change \| \| `no_identity` \| 401 \| WhoIs failed for the connection \| \| `host_token_required` \| 401 \| local-dev socket without `Twd-Host-Token` \| \| `host_token_invalid` \| 403 \| local-dev socket, wrong token \| \| `not_admin` \| 403 \| tailnet caller without `twd.dev/cap/admin` (`snippet` given) \| \| `forbidden` \| 403 \| role ok, but outside the caller's records / namespaces \| \| `host_not_registered` \| 403 \| `tag:twd-host` node that never did `PUT /v1/hosts/{id}` \| \| `host_identity_mismatch` \| 403 \| host ID bound to another node \| \| `delegation_invalid` \| 403 \| child tag not owned by the parent's, parent unknown (404) or dead (410) \| \| `delegation_exceeds_parent` \| 403 \| spawn caps ⊄ parent (`missing: {src, dst, cap, field}`) \| \| `policy_missing_tag_owners` \| 403 \| the control plane refused to mint a key for twd's own tags (`snippet` gives the tagOwners to add) \| \| `not_found` \| 404 \| — \| \| `unknown_tenant` \| 404 \| — \| \| `unknown_namespace` \| 404 \| `(workload, class)` not declared \| \| `exists` \| 409 \| create of an existing ID \| \| `lease_lost` \| 409 \| epoch/holder fence failed \| \| `lease_held` \| 409 \| another host holds the lease \| \| `store_unavailable` \| 503 \| the store is unreachable, restarting or contended (a row-lock or idle-transaction timeout); retry after `Retry-After` \| \| `takeover_unconfirmed` \| 409 \| the workload has Durable Objects and its host stopped heartbeating but is still registered: it may still be serving them. `twd host rm <host> --drain`, or redeploy with `--force-takeover` \| \| `already_joined` \| 409 \| credential for an object that has a node \| \| `identity_conflict` \| 409 \| object ID collision across identities \| \| `namespace_ambiguous` \| 409 \| class declared by several workloads, `workload` missing \| \| `not_ready` \| 409 \| certificate requested before `ready` \| \| `superseded` \| 409 \| undeploy of a non-latest deployment \| \| `tenant_has_live_objects` \| 409 \| tenant delete without `force` \| \| `host_holds_leases` \| 409 \| host delete without `drain` \| \| `killed` \| 410 \| tombstone (`TombstoneError`) \| \| `precondition_failed` \| 412 \| `If-Match` mismatch \| \| `object_quota_exceeded` \| 429 \| tenant `maxObjects` \| \| `spawn_budget_exceeded` \| 429 \| `maxLive` / `maxDepth` \| \| `bootstrap_failed` \| 502 \| control plane refused the mint \| \| `host_token_unconfigured` \| 503 \| local-dev mgmt without a token (fail closed) \| \| `identity_pending` \| 503 \| mint still queued (`Retry-After: 1`) \| \| `no_host` \| 503 \| no live host for the deployment \| \| `policy_unavailable` \| 503 \| mgmt cannot read the tenant policy \| \| `internal` \| 500 \| — \| |
| `doc_url` | string (uri) | yes |  |
| `message` | string | yes |  |
| `missing` | object | no | What is missing. Shape depends on `code` (e.g. `{src, dst, cap, field}` for `delegation_invalid`). |
| `snippet` | string | no | Policy HuJSON to add (a grant or tagOwners entry). |
| `trace_id` | string | yes |  |

#### `ErrorCode`

The catalog. Status in parentheses.

| code | status | when |
|---|---|---|
| `bad_request` | 400 | malformed body, parameter or filter |
| `bad_cursor` | 400 | `cursor` does not decode |
| `bad_bundle` | 400 | bundle has no `twd.jsonc`, neither `tailnet.tags` nor `tailnet.objects`, or invalid `triggers.crons` |
| `immutable_field` | 400 | PATCH touches a field the caller may not change |
| `no_identity` | 401 | WhoIs failed for the connection |
| `host_token_required` | 401 | local-dev socket without `Twd-Host-Token` |
| `host_token_invalid` | 403 | local-dev socket, wrong token |
| `not_admin` | 403 | tailnet caller without `twd.dev/cap/admin` (`snippet` given) |
| `forbidden` | 403 | role ok, but outside the caller's records / namespaces |
| `host_not_registered` | 403 | `tag:twd-host` node that never did `PUT /v1/hosts/{id}` |
| `host_identity_mismatch` | 403 | host ID bound to another node |
| `delegation_invalid` | 403 | child tag not owned by the parent's, parent unknown (404) or dead (410) |
| `delegation_exceeds_parent` | 403 | spawn caps ⊄ parent (`missing: {src, dst, cap, field}`) |
| `policy_missing_tag_owners` | 403 | the control plane refused to mint a key for twd's own tags (`snippet` gives the tagOwners to add) |
| `not_found` | 404 | — |
| `unknown_tenant` | 404 | — |
| `unknown_namespace` | 404 | `(workload, class)` not declared |
| `exists` | 409 | create of an existing ID |
| `lease_lost` | 409 | epoch/holder fence failed |
| `lease_held` | 409 | another host holds the lease |
| `store_unavailable` | 503 | the store is unreachable, restarting or contended (a row-lock or idle-transaction timeout); retry after `Retry-After` |
| `takeover_unconfirmed` | 409 | the workload has Durable Objects and its host stopped heartbeating but is still registered: it may still be serving them. `twd host rm <host> --drain`, or redeploy with `--force-takeover` |
| `already_joined` | 409 | credential for an object that has a node |
| `identity_conflict` | 409 | object ID collision across identities |
| `namespace_ambiguous` | 409 | class declared by several workloads, `workload` missing |
| `not_ready` | 409 | certificate requested before `ready` |
| `superseded` | 409 | undeploy of a non-latest deployment |
| `tenant_has_live_objects` | 409 | tenant delete without `force` |
| `host_holds_leases` | 409 | host delete without `drain` |
| `killed` | 410 | tombstone (`TombstoneError`) |
| `precondition_failed` | 412 | `If-Match` mismatch |
| `object_quota_exceeded` | 429 | tenant `maxObjects` |
| `spawn_budget_exceeded` | 429 | `maxLive` / `maxDepth` |
| `bootstrap_failed` | 502 | control plane refused the mint |
| `host_token_unconfigured` | 503 | local-dev mgmt without a token (fail closed) |
| `identity_pending` | 503 | mint still queued (`Retry-After: 1`) |
| `no_host` | 503 | no live host for the deployment |
| `policy_unavailable` | 503 | mgmt cannot read the tenant policy |
| `internal` | 500 | — |

Enum: `bad_request`, `bad_cursor`, `bad_bundle`, `immutable_field`, `no_identity`, `host_token_required`, `host_token_invalid`, `not_admin`, `forbidden`, `host_not_registered`, `host_identity_mismatch`, `delegation_invalid`, `delegation_exceeds_parent`, `policy_missing_tag_owners`, `not_found`, `unknown_tenant`, `unknown_namespace`, `exists`, `lease_lost`, `lease_held`, `takeover_unconfirmed`, `store_unavailable`, `already_joined`, `identity_conflict`, `namespace_ambiguous`, `not_ready`, `superseded`, `tenant_has_live_objects`, `host_holds_leases`, `killed`, `precondition_failed`, `object_quota_exceeded`, `spawn_budget_exceeded`, `bootstrap_failed`, `host_token_unconfigured`, `identity_pending`, `no_host`, `policy_unavailable`, `internal`

#### `Health`

| Field | Type | Required | Description |
|---|---|---|---|
| `ok` | boolean | yes |  |

#### `Host`

| Field | Type | Required | Description |
|---|---|---|---|
| `auditPubKey` | string | no | base64 ed25519; verifies `hostSig`. |
| `capacityNodes` | integer | yes |  |
| `createdAt` | integer (int64) | yes |  |
| `dnsName` | string | no | `twd-host-<hostID>.<tailnet>`. |
| `hostID` | string | yes |  |
| `lastSeen` | integer (int64) | yes | unix ms of the last heartbeat |
| `leases` | array of `Lease` | no | Namespace leases the host currently holds. |
| `liveNodes` | integer | yes |  |
| `nodeID` | string | no | Stable node ID of the host's `tag:twd-host` tsnet node (bound at registration). |
| `pool` | string | no |  |
| `push` | `HostPush` | no | Where mgmt pushes kills and assignment changes. Over the tailnet, `port` names hostd's push listener on **its own tsnet node** (the WhoIs node; mgmt never pushes anywhere else). `url` is accepted only on the local-dev socket and only as `unix:///…`. |
| `status` | `HostStatusEnum` | yes | `pending`: created by `twd host add`, never registered. `live`: heartbeated within 10 s. `stale`: registered, no recent heartbeat. `draining`: being deleted. |
| `tiers` | array of `Tier` | yes |  |
| `updatedAt` | integer (int64) | yes |  |

#### `HostCreate`

| Field | Type | Required | Description |
|---|---|---|---|
| `hostID` | string | yes |  |
| `pool` | string | no |  |
| `tenantID` | string | no | The operator tenant whose credential mints the key (default: the tenant whose tailnet is mgmt's own). |
| `tiers` | array of `Tier` | no |  |

#### `HostDeleteResult`

| Field | Type | Required | Description |
|---|---|---|---|
| `devicesLeft` | array of string | no | Devices mgmt could not delete (no operator credential); delete them in the console. |
| `hostID` | string | yes |  |
| `killed` | array of string | yes |  |
| `leasesReleased` | array of string | yes |  |

#### `HostJoin`

A single-use join key for a host's `tag:twd-host` node. Never logged.

| Field | Type | Required | Description |
|---|---|---|---|
| `authKey` | string (password) | yes |  |
| `controlURL` | string (uri) | no |  |
| `expiresAt` | integer (int64) | yes | unix ms |
| `host` | `Host` | yes |  |
| `hostname` | string | yes | `twd-host-<hostID>` |
| `tags` | array of string | yes |  |
| `tailnet` | string | no |  |

#### `HostPage`

Extends `Page`.

| Field | Type | Required | Description |
|---|---|---|---|
| `items` | array of `Host` | no |  |

#### `HostPush`

Where mgmt pushes kills and assignment changes. Over the tailnet,
`port` names hostd's push listener on **its own tsnet node** (the
WhoIs node; mgmt never pushes anywhere else). `url` is accepted only
on the local-dev socket and only as `unix:///…`.

| Field | Type | Required | Description |
|---|---|---|---|
| `port` | integer | no |  |
| `url` | string | no | `unix:///path/hostd.sock`; local-dev only. |

#### `HostRegistration`

| Field | Type | Required | Description |
|---|---|---|---|
| `auditPubKey` | string | no |  |
| `capacityNodes` | integer | yes |  |
| `pool` | string | no |  |
| `push` | `HostPush` | no | Where mgmt pushes kills and assignment changes. Over the tailnet, `port` names hostd's push listener on **its own tsnet node** (the WhoIs node; mgmt never pushes anywhere else). `url` is accepted only on the local-dev socket and only as `unix:///…`. |
| `tiers` | array of `Tier` | yes |  |

#### `HostStatus`

The heartbeat body.

| Field | Type | Required | Description |
|---|---|---|---|
| `deployments` | array of `DeploymentReport` | no |  |
| `leases` | array of `Lease` | yes |  |
| `liveNodes` | integer | yes |  |

#### `HostStatusEnum`

`pending`: created by `twd host add`, never registered. `live`: heartbeated within 10 s. `stale`: registered, no recent heartbeat. `draining`: being deleted.

Enum: `pending`, `live`, `stale`, `draining`

#### `HostStatusResponse`

| Field | Type | Required | Description |
|---|---|---|---|
| `assignmentsChanged` | boolean | no | A hint to re-list assignments (set when a push could not be delivered). |
| `revoke` | array of string | yes | Lease entries that are no longer held. |
| `revokeObjects` | array of string | yes | Object IDs killed in the last 10 min in namespaces this host holds; kill idempotently. |

#### `JWKS`

| Field | Type | Required | Description |
|---|---|---|---|
| `keys` | array of object | yes |  |

#### `KillResult`

| Field | Type | Required | Description |
|---|---|---|---|
| `devicesDeleted` | array of string | yes |  |
| `devicesDeleting` | array of string | no | Device deletes scheduled (`-device-delete-delay`). |
| `errors` | object | no |  |
| `killed` | array of string | yes |  |
| `pushMs` | number | no |  |
| `pushed` | array of string | no | Objects whose host acknowledged the tier-1 push. |
| `warnings` | array of `KillWarning` | no | Non-fatal findings. The kill itself succeeded (tier 1 push, killed status, tombstone). |

#### `KillWarning`

| Field | Type | Required | Description |
|---|---|---|---|
| `code` | string: `device_delete_unavailable` | yes | `device_delete_unavailable`: the tenant's credential may not delete devices (an OAuth client without `devices:core`); the killed node is unreachable but its device stays in the admin console until removed. |
| `fix` | string | no | How to add the missing scope. |
| `message` | string | yes |  |
| `objects` | array of string | no |  |

#### `Lease`

| Field | Type | Required | Description |
|---|---|---|---|
| `epoch` | integer (int64) | yes |  |
| `expiresAt` | integer (int64) | no |  |
| `namespace` | string | yes | Lease name `<tenantID>/<workload>` (a workload or DO class name is also accepted on input). |

#### `MintMode`

`oauth`: a Tailscale OAuth client (scope `auth_keys`, tag `tag:twd`; optionally `devices:core` and `policy_file:read`) whose secret stays in a 0600 file on the mgmt host (the default for self-hosted installs). `wif`: keyless, mints through a WIF trust credential (mgmt's OIDC issuer over Funnel; hosted / multi-tenant). `apikey`: a Tailscale API key file (explicit fallback, tests). `static`: fakemgmt only (testcontrol accepts any key).

Enum: `oauth`, `wif`, `apikey`, `static`

#### `NodeCertificate`

| Field | Type | Required | Description |
|---|---|---|---|
| `cert` | string | yes | `<b64url payload>.<b64url sig>` |
| `objectID` | string | yes |  |
| `oboPublicKey` | string | yes | base64 Ed25519 |
| `payload` | `NodeCertificatePayload` | yes | `internal/nodecert.Payload`; signed with the tenant's OBO root key (Ed25519). |
| `stableNodeID` | string | yes |  |
| `status` | `ObjectStatus` | yes | `pending` → `minted` → `ready`; terminal: `killed` (tombstone, `410`), `failed`. |
| `tenantID` | string | yes |  |

#### `NodeCertificatePayload`

`internal/nodecert.Payload`; signed with the tenant's OBO root key (Ed25519).

| Field | Type | Required | Description |
|---|---|---|---|
| `exp` | integer (int64) | yes | unix seconds (24 h after iat) |
| `iat` | integer (int64) | yes | unix seconds |
| `lim` | `Caps` | no | `{capName: value \| [values]}`; see `caps ⊆ parent` semantics in `CreateObject`. |
| `objectID` | string | yes |  |
| `parent` | string | no |  |
| `root` | string | yes |  |
| `stableNodeID` | string | yes |  |
| `tags` | array of string | yes |  |
| `tenant` | string | yes |  |
| `v` | integer | yes |  |

#### `OIDCConfiguration`

| Field | Type | Required | Description |
|---|---|---|---|
| `claims_supported` | array of string | no |  |
| `id_token_signing_alg_values_supported` | array of string | yes |  |
| `issuer` | string (uri) | yes |  |
| `jwks_uri` | string (uri) | yes |  |
| `response_types_supported` | array of string | yes |  |
| `subject_types_supported` | array of string | yes |  |

#### `Object`

| Field | Type | Required | Description |
|---|---|---|---|
| `actingFor` | string | no | The human principal at the head of the chain, when known. |
| `caps` | `Caps` | no | `{capName: value \| [values]}`; see `caps ⊆ parent` semantics in `CreateObject`. |
| `cert` | string | no | The node certificate `<b64url payload>.<b64url sig>` once ready. |
| `children` | array of string | no |  |
| `createdAt` | integer (int64) | yes |  |
| `deploymentID` | string | yes |  |
| `depth` | integer | yes |  |
| `ephemeral` | boolean | yes |  |
| `host` | string | no | The lease holder when known. |
| `hostname` | string | yes | The node hostname (hostd may prepend its `-hostname-prefix`). |
| `killReason` | string | no |  |
| `lease` | string | yes |  |
| `lineage` | array of string | yes | Object IDs from the root down to this object (inclusive). |
| `magicDNS` | string | no |  |
| `namespace` | string | yes | DO class name, or `_workload`. |
| `nodeID` | string | no |  |
| `objectID` | string | yes |  |
| `objectName` | string | yes |  |
| `parent` | string | no | Parent object ID (spawn). |
| `root` | string | yes | Root of the spawn tree (itself for a root). |
| `stableNodeID` | string | no |  |
| `status` | `ObjectStatus` | yes | `pending` → `minted` → `ready`; terminal: `killed` (tombstone, `410`), `failed`. |
| `tags` | array of string | yes |  |
| `tenantID` | string | yes |  |
| `tombstone` | `Tombstone` | no | The kill record of an object. Kept until the tenant's terminal objects are purged. |
| `ttl` | integer (int64) | no | seconds |
| `updatedAt` | integer (int64) | yes |  |
| `workload` | string | yes |  |

#### `ObjectCreate`

| Field | Type | Required | Description |
|---|---|---|---|
| `actingFor` | string | no | The principal this object acts for (feeds audit). |
| `caps` | `Caps` | no | `{capName: value \| [values]}`; see `caps ⊆ parent` semantics in `CreateObject`. |
| `hostname` | string | no | The hostname hostd proposes; mgmt answers with the one to use. |
| `namespace` | string | yes | DO class name, or `_workload`. |
| `objectName` | string | yes |  |
| `parent` | `ObjectRef` | no |  |
| `tenantID` | string | yes |  |
| `traceID` | string | no |  |
| `ttl` | string \| integer | no | `10m`-style duration or seconds. Defaults to the class `ttl`. |
| `workload` | string | no | Always sent by hostd; see resolution rules in `CreateObject`. |

#### `ObjectPage`

Extends `Page`.

| Field | Type | Required | Description |
|---|---|---|---|
| `items` | array of `Object` | no |  |

#### `ObjectPatch`

JSON Merge Patch. Hosts send the `ready` group; admins send `ttl`.

| Field | Type | Required | Description |
|---|---|---|---|
| `epoch` | integer (int64) | no | Fences the ready commit. |
| `magicDNS` | string | no |  |
| `nodeID` | string | no |  |
| `stableNodeID` | string | no |  |
| `status` | string: `ready` | no |  |
| `ttl` | integer \| null (int64) | no | Admin. Seconds; `null` clears. |

#### `ObjectRef`

| Field | Type | Required | Description |
|---|---|---|---|
| `objectID` | string | yes |  |

#### `ObjectStatus`

`pending` → `minted` → `ready`; terminal: `killed` (tombstone, `410`), `failed`.

Enum: `pending`, `minted`, `ready`, `killed`, `failed`

#### `Page`

The generic page envelope (`Page<T>`); every concrete `*Page` schema
below is this shape with a typed `items`. `nextCursor` is `null` on
the last page.

| Field | Type | Required | Description |
|---|---|---|---|
| `items` | array of any | yes |  |
| `nextCursor` | string \| null | yes | Pass as `?cursor=` for the next page; `null` when there is none. |

#### `PreflightMissing`

| Field | Type | Required | Description |
|---|---|---|---|
| `dst` | string | yes |  |
| `snippet` | string | yes | The grant to add (HuJSON). |
| `src` | string | yes |  |

#### `PreflightResult`

| Field | Type | Required | Description |
|---|---|---|---|
| `missing` | array of `PreflightMissing` | yes |  |
| `missingTagOwners` | array of string | yes |  |
| `ok` | boolean | yes |  |
| `srcTags` | array of string | no | The tags checked (workload + object tags from the bundle). |
| `tagOwnersSnippet` | string | no |  |
| `unchecked` | array of string | no | `tailnet.connects` entries that are names, not tags. |

#### `PurgeResult`

| Field | Type | Required | Description |
|---|---|---|---|
| `purged` | array of string | yes | Object IDs forgotten. |

#### `SelfInfo`

| Field | Type | Required | Description |
|---|---|---|---|
| `dnsName` | string | no | `twd-mgmt.<tailnet>` |
| `https` | boolean | yes |  |
| `issuer` | string (uri) | no |  |
| `tailnet` | string | no |  |
| `transport` | `Transport` | no | `tsnet` in normal operation; `local-dev` when the unix socket is serving. |

#### `Status`

| Field | Type | Required | Description |
|---|---|---|---|
| `clock` | `StatusClock` | no | The shared clock this replica uses for leases, host liveness and expiries (ADR-028): the store's clock. `ok` is false (and the top-level `ok` with it) when this replica's wall clock is more than 2 s from the store clock, or the store clock has not been sampled for 30 s. Ownership decisions do not use the wall clock, so this is a warning, not a refusal; local-clock users (WIF issuer tokens, throttles) may misbehave. |
| `hosts` | object | yes |  |
| `listeners` | array of string | no | e.g. `tsnet:80`, `tsnet:443`, `funnel:443`, `unix:~/.twd/run/mgmt.sock`. |
| `objects` | object | yes | Count per `ObjectStatus`. |
| `ok` | boolean | yes |  |
| `pendingDeviceDeletes` | integer | yes |  |
| `replicas` | `StatusReplicas` | no | The mgmt replica registry (replicas sharing this store). `ok` is false (and the top-level `ok` with it) when this replica conflicts with another live one: file custody shared between replicas, or replicas with different custody. A replica refuses to start in that state; a conflict that appears later (a replica started with an older version, or a registry row edited) is reported here and logged. |
| `tenants` | integer | yes |  |
| `transport` | `Transport` | yes | `tsnet` in normal operation; `local-dev` when the unix socket is serving. |
| `version` | string | no |  |

#### `StatusClock`

The shared clock this replica uses for leases, host liveness and expiries (ADR-028): the store's clock. `ok` is false (and the top-level `ok` with it) when this replica's wall clock is more than 2 s from the store clock, or the store clock has not been sampled for 30 s. Ownership decisions do not use the wall clock, so this is a warning, not a refusal; local-clock users (WIF issuer tokens, throttles) may misbehave.

| Field | Type | Required | Description |
|---|---|---|---|
| `message` | string | no |  |
| `offsetMs` | integer | yes | Store clock minus this replica's wall clock at the last sample. |
| `ok` | boolean | yes |  |
| `source` | string: `postgres` \| `local` | yes | `postgres`: the server clock, sampled every 5 s; `local`: SQLite, one replica. |
| `syncAgeMs` | integer | yes | Milliseconds since the last successful sample (0 for `local`). |

#### `StatusReplicas`

The mgmt replica registry (replicas sharing this store). `ok` is false (and the top-level `ok` with it) when this replica conflicts with another live one: file custody shared between replicas, or replicas with different custody. A replica refuses to start in that state; a conflict that appears later (a replica started with an older version, or a registry row edited) is reported here and logged.

| Field | Type | Required | Description |
|---|---|---|---|
| `id` | string | yes | This replica's stable ID (`<state-dir>/replica.id`). |
| `live` | integer | yes | Replicas seen within the last 30 s, this one included. |
| `message` | string | no |  |
| `ok` | boolean | yes |  |

#### `Tenant`

| Field | Type | Required | Description |
|---|---|---|---|
| `apiKeyFile` | string | no | `apikey` mode: absolute path, read at use; the key is never stored or returned. |
| `controlURL` | string (uri) | no | Non-default control plane (testcontrol, headscale). |
| `createdAt` | integer (int64) | yes |  |
| `maxObjects` | integer | yes |  |
| `mintMode` | `MintMode` | yes | `oauth`: a Tailscale OAuth client (scope `auth_keys`, tag `tag:twd`; optionally `devices:core` and `policy_file:read`) whose secret stays in a 0600 file on the mgmt host (the default for self-hosted installs). `wif`: keyless, mints through a WIF trust credential (mgmt's OIDC issuer over Funnel; hosted / multi-tenant). `apikey`: a Tailscale API key file (explicit fallback, tests). `static`: fakemgmt only (testcontrol accepts any key). |
| `oauthClientFile` | string | no | `oauth` mode: absolute path of the 0600 file with `TS_OAUTH_CLIENT_ID=…` / `TS_OAUTH_CLIENT_SECRET=tskey-client-…`, read at use on the mgmt host (a looser mode is refused). |
| `oauthClientID` | string | no | `oauth` mode: the OAuth client's public ID (`TS_OAUTH_CLIENT_ID`). The secret is never stored or returned. |
| `oboPublicKey` | string | no | base64 Ed25519 key that verifies this tenant's node certificates; hostd pins it. |
| `pool` | string | no | Host pool the tenant's deployments are placed on (hosts with a matching `pool`; empty = any host). |
| `registrationsPerMinute` | integer | yes |  |
| `tailnet` | string | yes | MagicDNS suffix of the customer tailnet. |
| `tenantID` | string | yes |  |
| `tier` | `Tier` | no |  |
| `updatedAt` | integer (int64) | yes |  |
| `wif` | `WIFInfo` | no | Present in wif mode. |
| `wifClientID` | string | no | Public client ID of the mint trust credential (`auth_keys`). |
| `wifLifecycleClientID` | string | no | Public client ID of the lifecycle credential (`devices:core`, `policy_file:read`). |

#### `TenantCreate`

| Field | Type | Required | Description |
|---|---|---|---|
| `apiKeyFile` | string | no |  |
| `controlURL` | string (uri) | no |  |
| `maxObjects` | integer | no |  |
| `mintMode` | `MintMode` | yes | `oauth`: a Tailscale OAuth client (scope `auth_keys`, tag `tag:twd`; optionally `devices:core` and `policy_file:read`) whose secret stays in a 0600 file on the mgmt host (the default for self-hosted installs). `wif`: keyless, mints through a WIF trust credential (mgmt's OIDC issuer over Funnel; hosted / multi-tenant). `apikey`: a Tailscale API key file (explicit fallback, tests). `static`: fakemgmt only (testcontrol accepts any key). |
| `oauthClientFile` | string | no |  |
| `oauthClientID` | string | no |  |
| `pool` | string | no |  |
| `registrationsPerMinute` | integer | no |  |
| `tailnet` | string | yes |  |
| `tenantID` | string | no | Optional; generated (`tn_…`) when absent. |
| `tier` | `Tier` | no |  |
| `wifClientID` | string | no |  |
| `wifLifecycleClientID` | string | no |  |

#### `TenantDeleteResult`

| Field | Type | Required | Description |
|---|---|---|---|
| `deploymentsStopped` | array of string | no |  |
| `devicesDeleting` | array of string | no |  |
| `killed` | array of string | yes |  |
| `purged` | array of string | no |  |
| `tenantID` | string | yes |  |

#### `TenantPage`

Extends `Page`.

| Field | Type | Required | Description |
|---|---|---|---|
| `items` | array of `Tenant` | no |  |

#### `TenantPatch`

JSON Merge Patch over the updatable fields. `null` clears an optional field.

| Field | Type | Required | Description |
|---|---|---|---|
| `apiKeyFile` | string \| null | no |  |
| `maxObjects` | integer | no |  |
| `mintMode` | `MintMode` | no | `oauth`: a Tailscale OAuth client (scope `auth_keys`, tag `tag:twd`; optionally `devices:core` and `policy_file:read`) whose secret stays in a 0600 file on the mgmt host (the default for self-hosted installs). `wif`: keyless, mints through a WIF trust credential (mgmt's OIDC issuer over Funnel; hosted / multi-tenant). `apikey`: a Tailscale API key file (explicit fallback, tests). `static`: fakemgmt only (testcontrol accepts any key). |
| `oauthClientFile` | string \| null | no |  |
| `oauthClientID` | string \| null | no |  |
| `pool` | string \| null | no |  |
| `registrationsPerMinute` | integer | no |  |
| `tier` | `Tier` | no |  |
| `wifClientID` | string \| null | no |  |
| `wifLifecycleClientID` | string \| null | no |  |

#### `Tier`

Enum: `process`, `microvm`

#### `Tombstone`

The kill record of an object. Kept until the tenant's terminal objects are purged.

| Field | Type | Required | Description |
|---|---|---|---|
| `by` | string | yes | The actor (see Conventions). |
| `cascade` | boolean | yes | True when this object died as part of a cascade. |
| `deviceDeleteAt` | integer (int64) | no | unix ms when the scheduled device delete runs; absent once done. |
| `deviceDeleted` | boolean | yes | Tier 2 done (false while the delete is still scheduled). |
| `killedAt` | integer (int64) | yes | unix ms |
| `namespace` | string | yes |  |
| `objectID` | string | yes |  |
| `objectName` | string | yes |  |
| `reason` | string | no |  |
| `root` | string | yes | The object the kill was issued on. |
| `tenantID` | string | yes |  |
| `workload` | string | yes |  |

#### `TombstoneError`

`410 Gone`: the error plus the kill record. `tombstone` is absent only when the 410 is `delegation_invalid` for a spawn whose parent is dead (the parent's record is then at `GET /v1/objects/{parent}`).

Extends `Error`.

| Field | Type | Required | Description |
|---|---|---|---|
| `tombstone` | `Tombstone` | no | The kill record of an object. Kept until the tenant's terminal objects are purged. |

#### `Transport`

`tsnet` in normal operation; `local-dev` when the unix socket is serving.

Enum: `tsnet`, `local-dev`

#### `VolumeKeyGrant`

A fenced release of a volume's data key. Never logged.

| Field | Type | Required | Description |
|---|---|---|---|
| `epoch` | integer (int64) | yes |  |
| `grantedAt` | integer (int64) | no |  |
| `key` | string (password) | yes | 32 bytes. |
| `tenantID` | string | yes |  |
| `volumeID` | string | yes |  |
| `workload` | string | yes |  |

#### `WIFCheckResult`

| Field | Type | Required | Description |
|---|---|---|---|
| `issuer` | string (uri) | yes |  |
| `lifecycle` | string: `ok` \| `absent` \| `error` | yes | `absent`: no lifecycle client ID (kill tier 2, reaper device deletes, spawn checks and preflight are off). |
| `lifecycleError` | string | no |  |
| `mint` | boolean | yes | The mint credential's token exchange succeeded (its `auth_keys` scope is only proven by the first real mint). |
| `mintError` | string | no |  |
| `mintMode` | `MintMode` | yes | `oauth`: a Tailscale OAuth client (scope `auth_keys`, tag `tag:twd`; optionally `devices:core` and `policy_file:read`) whose secret stays in a 0600 file on the mgmt host (the default for self-hosted installs). `wif`: keyless, mints through a WIF trust credential (mgmt's OIDC issuer over Funnel; hosted / multi-tenant). `apikey`: a Tailscale API key file (explicit fallback, tests). `static`: fakemgmt only (testcontrol accepts any key). |
| `policyRead` | boolean | yes | The lifecycle credential read the policy (`policy_file:read`). |
| `policyReadError` | string | no |  |

#### `WIFInfo`

Present in wif mode.

| Field | Type | Required | Description |
|---|---|---|---|
| `issuer` | string (uri) | yes |  |
| `lifecycleSubject` | string | yes |  |
| `mintSubject` | string | yes |  |

#### `WhoAmI`

| Field | Type | Required | Description |
|---|---|---|---|
| `actor` | string | yes |  |
| `admin` | boolean | yes |  |
| `hostID` | string | no |  |
| `kind` | string: `user` \| `node` \| `host` \| `local` | yes |  |
| `login` | string | no |  |
| `mgmt` | `SelfInfo` | yes |  |
| `nodeID` | string | no |  |
| `tags` | array of string | no |  |

