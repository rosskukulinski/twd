# Containers and sandboxes: `ctx.container` on the microVM tier

ADR-032 (design repo) is the decision; this is how the core runtime works.

## Shape

```
DO (workerd, any tier) ── ctx.container (polyfill in the generated wrapper)
   └─ platform TailnetApi (attests the object) ── HOSTD binding ── hostd /v1/container…
        └─ container manager, keyed (tenant, workload, class, object)
             └─ hostd/vm BootContainer ── twd-vmhelper ── jailer ── Firecracker
                  guest: twd-guestd (PID 1) ── overlay(erofs image + scratch) ── ENTRYPOINT/CMD, execs
```

- **One VM per container, one container per object.** Its own jail uid,
  network namespace and cgroup (`cpu.max` carries fractional vCPUs,
  `memory.max`, `pids.max`); the parent's DO volume, volume key, egress MAC
  key and tailnet identity are never part of it.
- **Image:** an OCI image becomes a read-only erofs (`hostd/image`; per-tenant
  content-addressed cache, `public` scope for operator-marked bases). Each run
  gets a fresh sparse ext4 scratch made by the helper inside the jail: the
  overlay's upper layer, gone with the VM (disks are ephemeral).
- **Guest:** the same guest rootfs and twd-guestd as DO VMs, in container
  mode (`guest.json` "container"); the kernel is `guest/build-kernel.sh`'s
  (erofs). Every container process starts through `twd-guestd ctr-exec`: a
  re-exec in a new mount namespace that `pivot_root`s into the overlay and
  detaches the old root, so no container process sees guestd or its config.

## Host ⇄ guest protocol (guest/proto)

- Control: the existing vsock NDJSON channel (guest → host port 1024). New
  ops `exec`, `signal`, `resize`, `status`; events `ctr-ready`, `ctr-exit`,
  `exec-exit`, `log`. A connection now opens with a `hello` line.
- Exec stdio: one more guest-initiated vsock connection per exec, whose first
  line is `{"type":"stream","exec":<id>}`; then frames (1 byte kind, u32 BE
  length, payload): stdin / stdin-EOF (half-close) host → guest, stdout,
  stderr, exit (int32) guest → host. No helper change was needed: the
  helper's single vsock listener carries both kinds.

## getTcpPort: host stack dial, not vsock forwarding

`getTcpPort(p).fetch()` dials the guest's TCP port `p` through the VM's
existing userspace network stack (gVisor, `vnet.dialGuest`), exactly how
hostd reaches a DO guest's workerd. It needs no guest-side forwarder and no
new listener on the host. Nothing else can reach a container port: the
container has no tailnet node and no address outside its own VM's stack,
so only its parent object (through hostd, attested) gets in. A
server must listen on `0.0.0.0` (as on Cloudflare). Not listening →
Cloudflare's error string `the container is not listening`, which
`@cloudflare/containers` relies on.

## Egress: the sandbox egress proxy (ADR-032 D4)

A container guest's NIC ends at a per-run proxy in hostd's process
(`netd/sbxnet`), not at any tsnet server: it has no tailnet identity, no
egress MAC key, and none of the gateway services a DO guest gets (attributed
egress, workload egress, host API). The guest's `resolv.conf` names the
gateway; that is the only DNS server it has.

- **Deny-all by default.** DNS answers only names the container may reach;
  each gets a synthetic address from `198.18.0.0/15`, so the proxy knows the
  name of every flow without trusting a `Host` header or SNI. Every other
  name is NXDOMAIN and every other TCP flow is reset before its handshake
  completes. UDP (other than DNS), ICMP and IPv6 aren't served.
- **`interceptOutboundHttp(addr, fetcher)` / `interceptAllOutboundHttp` /
  `interceptOutboundHttps`.** `addr` is a host, `host:port`, an IP,
  `ip:port`, or `*.suffix` (no port: any port); at most **128** per
  container (Cloudflare's limit; re-registering a key replaces it). A
  matching request is delivered to the **parent object**: hostd sends it
  through the workload's current instance to the platform wrapper
  (`cf.twd.kind = "intercept"`, which only hostd can set), which calls the
  object's `__twd_intercept`; the polyfill runs it through the handler
  registered under that key. So whatever the handler fetches leaves with the
  object's own identity, egress policy and `lim` (facet mode: the object's
  node; plain-DO mode: the workload node). The request URL is what the
  container connected to; its `x-twd*`/`twd-*`/`cf-*` headers are dropped;
  the rest is untrusted input. Handlers live in the object's memory: one
  registered before the object restarted answers `502
  intercept_unavailable` until it is registered again
  (`@cloudflare/containers` re-registers on wake).
- **HTTPS interception** terminates TLS with a **per-container ephemeral
  CA** (ECDSA P-256, made at `start()`, valid for the run plus margin).
  Its key stays in hostd's memory and is dropped with the run; only the
  certificate enters the guest, at
  `/etc/cloudflare/certs/cloudflare-containers-ca.crt` (Cloudflare's path)
  and appended to the image's trust bundles where they exist
  (`/etc/ssl/certs/ca-certificates.crt`, `/etc/pki/tls/certs/ca-bundle.crt`,
  `/etc/ssl/cert.pem`, resolved inside the container root). Another
  container's CA does not verify it, and a client that doesn't trust it sees
  the interception. `interceptAllOutboundHttp` is HTTP only; TLS to a host
  with no HTTPS intercept is never terminated.
- **`enableInternet: true`** reaches only hosts on the workload's
  `tailnet.egress.internet` allowlist (names, `*.suffix`, IPs or CIDRs),
  spliced as raw TCP (TLS stays end to end). The name is resolved on the
  host and refused if **any** address is forbidden (ADR-019: loopback,
  private and link-local/metadata ranges, `100.64/10` and the tailnet IPv6
  range, NAT64/6to4/Teredo, the synthetic pool, the host's own addresses).
  Every first flow per destination is audited `egress` with `actingFor:
  "sandbox-of <object>"`; refusals are audited `deny` (the first 32 per run)
  and listed in `twd-hostd`'s `ContainerStatus.Refused`. twd has no tenant
  exit-node or gateway path yet (RFC §4.8), so the path is the host's own
  network (`twd-hostd -container-internet=direct`, the default) or none
  (`off`).
- **Preview URLs** (`exposePort`, `proxyToSandbox`) need public ingress
  (ADR-029): on a container class `exposePort()` throws `not_supported`,
  and a Sandbox SDK preview-proxy request (`x-sandbox-preview-proxy`) gets
  `501 not_supported`.

## No keys in a sandbox

`twd host vmcheck <workload> --container <Class>/<name>` scans the running
container guest's disks (config drive, image, scratch when readable), its
files and a full RAM dump for: `tskey-`, every node/machine private key on
the host, the parent's egress MAC keys (instance root and the object's
derived key), every volume key (DEK) of the tenant's workloads on the host,
the host's own key/token/credential files, and the run's HTTPS-intercept CA
private key. Each needle kind must exist on the host (positive control),
and a per-run canary held in guestd's memory must be found in the dump.

## Images: build, registry, pinning, host pull (ADR-032 D3)

```
developer machine            twd-mgmt (tsnet, WhoIs)                       host (twd-hostd)
twd deploy                    /v2/  OCI registry                            no container engine
  docker buildx (per arch) ─▶   <tenant>/<workload>-<class>  ◀─ pull by digest ─ image.Store.GetPinned
  alias contexts              public/…  (operator bases)                      verify, flatten, mkfs.erofs
  push over your tailnet  ─▶  POST /v1/deployments {containerImages}          cache: <tenant>/ or public/
                              pin every ref → per-arch digest (mirror external refs)
```

- **Build (`twd deploy`, `internal/imagebuild`).** A class whose wrangler
  `image` is a Dockerfile (or build dir), or whose `images.<name>` has a
  `dockerfile`, is built **on the developer's machine** with `docker buildx`,
  once per target arch (`--platform`, default: the arches of the live hosts in
  the tenant's pool that offer containers), into an OCI layout, and pushed to
  `twd.local/<tenantID>/<workload>-<class>[-<name>]` over the CLI's own mgmt
  connection (a linked developer: their own Tailscale connection; WhoIs). The
  result (`…@sha256:…`, an index for several arches) goes to mgmt in
  `DeployRequest.containerImages`; it must be in the deploying tenant's
  namespace. The app's Dockerfile is used as written: the **alias map**
  (`GET /v1/images/aliases`; default `cloudflare/sandbox:0.12.14/1.0.0/1.0.1`
  → twd's arm64 rebuilds on arm64) becomes BuildKit named contexts
  (`--build-context docker.io/cloudflare/sandbox:0.12.14=oci-layout://…`) for
  every form the Dockerfile writes the ref in (FROM, `COPY --from=`, an ARG
  default), fetched from twd's registry (`public/`) or else from the local
  Docker store. (A `docker-image://` context naming a `twd.local` ref makes
  BuildKit try the network first: 100 s of timeouts, measured.)
- **Registry (`internal/registry`, served by twd-mgmt at `/v2/`).** The OCI
  distribution push/pull subset on mgmt's own listeners (tsnet `:80`/`:443`,
  and the local-dev socket), behind the `/v1` WhoIs gate: admins push and pull
  every namespace; hosts only pull, and only `public/…` and the namespaces of
  tenants they hold a deployment of; a host never pushes. One namespace per
  tenant (`<tenantID>/…`; no tenant may be called `public`). Blobs are
  content-addressed files under `<state-dir>/registry/blobs` (`-registry-dir`;
  replicas serving the registry must share it), stored once, but **visible
  only through a repository they were pushed or mirrored into**: the store's
  `registry_links` (SQLite or Postgres, metadata only) say which. Another
  namespace's `HEAD`/`GET` of the same digest is `404 BLOB_UNKNOWN`, a
  cross-namespace mount is refused (a fresh upload instead), and a manifest may
  only reference content already in its own repository. Manifest pushes are
  audited (`image_push`). Routes and roles are documented in
  `api/openapi.yaml` (tag `registry`, excluded from generation).
- **Pinning (deploy).** mgmt resolves every image reference of every class to
  a **linux/arch image manifest digest, once**, for the arches of the pool's
  container hosts, and records the pins in the deployment
  (`ContainerClass.pins`, in `Assignment.containers` and
  `GET /v1/deployments/{id}`); placement is then limited to arches every image
  resolved for (an amd64-only image in a mixed pool lands on an amd64 host).
  `twd.local/<tenant>/…` refs resolve in the tenant's namespace,
  `twd.local/<other>` only ever in `public/`. An **external ref**
  (`docker.io/…`) is mirrored now into `<tenant>/mirror/<registry>/<repo>`
  through **mgmt's own egress** (anonymous bearer tokens for Docker Hub;
  `-no-mirror` refuses external refs; `-registry-insecure` lists plain-HTTP
  upstreams for tests), unless it is a public base. The alias map applies per
  arch. Retagging the upstream after the deploy changes nothing that runs; the
  next deploy picks the new digest. Audited (`image_pin`, with every pin).
- **Public bases.** `twd image public add <ref> [--arch …]` mirrors an
  upstream image into `public/<registry>/<repo>` (or names a `twd.local` ref
  already in `public/`) and marks its per-arch digests public; deploys of every
  tenant then pin those, so hosts cache it once. `twd image push <layout|tar>
  twd.local/<name>:<tag>` is how the operator puts twd's arm64 rebuilds (the
  alias targets) into `public/`. Unmarking (`rm`) leaves pinned deployments as
  they are.
- **Host pull (`hostd/image`).** hostd starts a container only from the pin
  for its own arch, pulled as a host from twd's registry over the API's own
  connection (`mgmtapi.Client.OCI`: never Docker Hub, no container engine),
  every manifest and blob verified against its digest as it arrives. The cache
  scope comes from the pin's repository, not from the tenant: `public/` is
  shared, the tenant's own namespace is its alone, any other namespace is
  refused before anything is fetched; so a cache hit never tells one tenant
  which images another runs. Pinned entries are keyed by manifest digest.
  `-container-image-cache-max-mib` caps the converted cache (least recently
  used first; images of running containers are held). `-container-image-dir`
  (a directory of tarballs) remains for tests and hosts without a registry.
- **Signing** (cosign) is deferred (ADR-032).

## Lifecycle

| event | effect |
|---|---|
| `start()` | returns at once (`running` is true); hostd admits it (`AcquireContainer`: the host's capacity vector, shared with microVM DO instances, else `503 capacity_exhausted`; then mgmt's synchronous tenant reservation, `POST /v1/container-reservations`, else `429 container_quota_exceeded`, or `503 quota_unavailable` when mgmt is unreachable: fail closed), resolves the image (class's pinned refs only), boots |
| entrypoint exits | the VM is killed; `monitor()` resolves on 0, rejects `Container exited with unexpected exit code: N` |
| `destroy()` | SIGTERM to the entrypoint, grace capped (`ContainerGrace`, 10 s), VMM SIGKILL |
| inactivity | no container API call and no open exec/port/stdin stream for the timeout (default 10 min, the `@cloudflare/containers` default; `setInactivityTimeout` ≤ 6 h); a parked `monitor()` is not activity |
| hard max lifetime | `ContainerMaxLife`, default 24 h |
| object evicted / blue-green redeploy | nothing: the next instance reconciles `running` from hostd before any event |
| `twd kill` of the object, workload stopped or fenced (ADR-028) | VMM SIGKILL, no grace |

`running` is synchronous in Cloudflare's API: the polyfill keeps it in the
object's SQLite kv, reconciles it with hostd in `blockConcurrencyWhile` at
construction, updates it from every answer, and keeps a background
`monitor` long-poll (re-armed by each call) to flip it when hostd stops the
container.

## Attribution (who may drive a container)

Every op is scoped to the attested caller's own container key; exec IDs
only work for the container that made them; an object never sees another's
container (not even its existence). Facet mode: the object is the
TailnetApi props, unforgeable. Plain-DO mode: the tenant service claims the
object (as for `ctx.tailnet`, docs/egress-modes.md): code in the workload
could drive another plain object's container of the same workload (same
trust domain), never a facet object's and never another workload's.

## Not in v1 (ADR-032 deferred)

`getTcpPort().connect()`, WebSocket upgrades to container ports,
`snapshotContainer`, directory snapshots, preview URLs (`exposePort`,
`not_supported`), image signing (cosign), registry garbage collection, a
tenant exit-node or gateway path for `enableInternet`.

A host advertises the `containers` tier (and mgmt places container bundles
on it) only when containers can actually run there: a microVM backend whose
helper supports container guests, a guest kernel with erofs, and
`mkfs.erofs`.
