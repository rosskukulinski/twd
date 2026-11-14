# Tailscale Services

twd uses [Tailscale Services](https://tailscale.com/kb/1552/tailscale-services)
in two places:

- **workloads:** a workload's entry node (its default export) can host a
  Service. Callers then use a stable name and VIP that don't change when the
  workload node is re-minted, redeployed or moved to another host;
- **mgmt replicas:** several `twd-mgmt` replicas on one Postgres can host one
  Service (e.g. `svc:twd-mgmt`), so hosts reach whichever replica is healthy
  (H12 in `e2e/multimgmt/LEDGER.md`).

Durable Object instances never host Services. A DO is single-instance, and
its node *is* its identity: per-object egress attribution, `twd kill`, and
spawn lineage all hang off that node.

## How it works (`internal/tssvc`)

`tsnet.Server.ListenService` forwards Service traffic to a host loopback
socket. The backend then sees `127.0.0.1` as the caller, or has to trust a
PROXY header that any local process could forge. twd authenticates every
request with WhoIs on the connection's source address, so it can't use that.
Instead, twd:

1. advertises the Service in the node's prefs (`AdvertiseServices`);
2. adds an **empty** TCP handler for the Service port to the serve config.
   Netstack then intercepts flows to the VIP, but serve has nothing to do
   with them (no `TCPForward`, no HTTP), so they fall through to tsnet's
   fallback TCP handlers;
3. registers a fallback handler that hands VIP flows (any destination that
   isn't the node's own address, on an advertised port) to an in-process
   listener.

The `net.Conn` keeps the caller's real tailnet address, so netd's attributed
ingress (WhoIs, `request.cf.tailscale.caller`, cap narrowing) works exactly
as it does on the node's own address. Kill RSTs Service flows like any
other flow.

## Workloads

```jsonc
// twd.jsonc
{
  "name": "support",
  "tailnet": {
    "tags": ["tag:twd-wl-support"],   // required: Service hosts are tagged nodes
    "service": true                    // svc:support; or "svc:helpdesk" / "helpdesk"
  }
}
```

**What `twd deploy` does:**

- **Preflight** (dry run): checks that the policy's
  `autoApprovers.services` lets the workload tags host the Service. If it
  doesn't, control would never route the Service to the node, so the deploy
  is refused with the entry to add:
  ```jsonc
  "autoApprovers": { "services": { "svc:support": ["tag:twd-wl-support"] } }
  ```
- **Service definition:** mgmt creates it (port `tcp:80`, the attributed
  HTTP ingress) with the comment `managed by twd: tenant=<id> workload=<name>`.
  - A redeploy adopts its own definition.
  - A Service with any other comment is refused with `409 service_conflict`.
    One tenant or workload can never take over another's name on a shared
    tailnet. To hand an existing Service to a workload, set its comment to
    that exact string.
  - A name that would share its MagicDNS name with an existing device is
    also refused, because MagicDNS answers the device first. This check is
    best effort: it's skipped if the credential can't list devices.
- **Credential:** apikey mode needs nothing extra. An OAuth client needs
  **Services › Write**; without it the deploy fails with
  `502 service_api_failed` and says so. WIF uses the lifecycle credential.
- **Output:** the deploy prints
  `✓ service svc:support · http://support.<tailnet>/`, and
  `GET /v1/deployments/{id}` has `endpoint.service`.

**Callers need a grant to the `svc:` name**, for example
`{"src": ["tag:agent"], "dst": ["svc:support"], "ip": ["tcp:80"]}`.
(Not yet verified: whether app capabilities granted with a `svc:`
destination show up in the workload's WhoIs CapMap. The real-tailnet runs
also had tag-to-tag grants carrying the same caps.)

**When the host advertises the Service (`hostd/service.go`).** Tailscale
doesn't health-check Service hosts, so each host gates its own
advertisement. It advertises only while all of these hold:

- the deployment is active;
- its instance (workerd, or the Node app) answers health probes, every 2 s
  with a 1.5 s timeout;
- the workload node is ready.

It withdraws the Service:

- on undeploy, and so on self-fencing and when mgmt moves the workload to
  another host;
- after 2 failed probes in a row (one good probe brings it back);
- on a redeploy without `tailnet.service`;
- with the node, on `twd kill` or a close.

Withdrawing removes the serve-config entry, so the node stops intercepting
the VIP at once. It also turns off keep-alives on Service connections: idle
ones close, and active ones answer `Connection: close`. Callers' pooled
connections therefore re-resolve instead of sticking to a host that no
longer serves. Requests already in flight finish; a kill RSTs them.
`twd-hostd`'s status shows `Service {name, advertised, healthy, reason}` per
workload.

**The definition outlives the deployment.** `twd undeploy` and `twd down`
withdraw the advertisement but keep the definition, so the VIP stays stable
for the next deploy. Delete it in the admin console (or via the API) when
the workload is gone for good.

**Egress to Services.** An object's `fetch("http://support.<tailnet>/")`
(or the bare `http://support/`, or the VIP itself) resolves from that
object's own netmap: the Service's MagicDNS record, routed by a peer that
has the VIP in its AllowedIPs. Host DNS is never used. The sibling rule
still applies: if every host of the Service is a sibling object on the same
host, the call is refused with `forbidden_destination`, because same-host
calls go through the platform.

**Not done here:** running one workload on several hosts at once. A
workload still holds one namespace lease, so there is one host per
workload. Services make that host replaceable without changing the caller's
name; they don't make the workload multi-host.

## mgmt replicas (`svc:twd-mgmt`)

```sh
twd-mgmt -db-file pg.dsn -custody setec -setec-url … \
  -tsnet-hostname twd-mgmt-1 -tsnet-control-url https://controlplane.tailscale.com \
  -tsnet-service svc:twd-mgmt
# every host
twd-hostd -mgmt http://twd-mgmt.<tailnet> …
```

- **Naming:** each replica keeps its own node (`-tsnet-hostname`, e.g.
  `twd-mgmt-1`). mgmt refuses to start if the hostname equals the Service
  label, because MagicDNS would answer the node first.
- **What's served:** Service flows on `:80` reach the same
  WhoIs-authenticated handler as the node's own address. Verified:
  `/v1/whoami` through the VIP returns the caller's node ID. Not yet
  exercised end to end: a real `twd-hostd` registering and heartbeating
  through the VIP, and `twd link`. Both use the same WhoIs path.
- **Health gate:** `mgmt.ServiceGate` advertises while `Status` (the store)
  answers. It withdraws after 3 failed checks in a row (every 2 s, 2 s
  timeout), and one success restores it. On SIGTERM it withdraws before the
  listeners close. Withdrawing turns off keep-alives on the Service
  listener, so hosts' pooled connections move to another replica.
- **Policy:**
  ```jsonc
  "autoApprovers": { "services": { "svc:twd-mgmt": ["tag:twd-mgmt"] } },
  "grants": [
    {"src": ["tag:twd-host"], "dst": ["svc:twd-mgmt"], "ip": ["tcp:80"]},
    {"src": ["autogroup:admin"], "dst": ["svc:twd-mgmt"], "ip": ["tcp:80"], "app": {"twd.dev/cap/admin": [{}]}},
  ]
  ```
  Create the Service definition (`svc:twd-mgmt`, `tcp:80`) in the admin
  console. `twd up --policy-apply` doesn't write `autoApprovers`.
- **Limitation:** a replica that is stalled outright (SIGSTOP, a wedged
  process) can't withdraw itself. Callers time out and retry, and a later
  connection may land on it again until control sees the node go offline.
  This is the S3 case in the multimgmt ledger.

## Measured (tailnet 1, production control, 2026-11-13)

| Check | Result |
|---|---|
| `internal/tssvc` real (`TestRealService`) | VIP answered 3.5 s after advertise; WhoIs saw the caller's node ID through the VIP; after withdraw the next fresh request failed (within one 3 s timeout); re-advertise answered in 14 ms |
| E2E-SVC (`make e2e-svc`, twd CLI) | `twd deploy` created the definition (owner comment, VIPs). First call through the VIP 11.1 s after deploy, caller = the client node. Redeploy kept the same VIPs and was served again at once. `twd kill` of the workload node: the VIP stopped answering on the next request (≤ 3 s). `twd down`: 0 leftover devices/keys; definition kept |
| H12 real (`make e2e-mm-svc`, 2 replicas on Postgres; needs `TWD_MM_PG_DSN` pointing at a database of its own) | `svc:ttest-mgmt` answered 5.6 s after start; `/v1/whoami` through the VIP = the client node, admin. Serving replica's store reset → 5 consecutive answers from the other replica 8.2 s later; it re-advertised 0.5 s after its store came back. SIGTERM of the serving replica → steady on the other 7.7 s later |
