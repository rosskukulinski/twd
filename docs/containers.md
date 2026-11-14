# Containers and code sandboxes: `ctx.container` on the microVM tier

twd runs Cloudflare's container surface: `ctx.container` in a Durable
Object, the `Container` class from `@cloudflare/containers`, and the Sandbox
SDK (0.x and 1.0) on top of it. Each container is its own microVM attached to
one object. ADR-032 (design repo) is the decision. This page explains how to
use it first, then how it works.

## Using containers

### Declare the class

Declare containers the way wrangler does: a `containers` entry per DO class,
and the class bound as a Durable Object.

```jsonc
// wrangler.jsonc
{
  "containers": [
    { "class_name": "Sandbox", "image": "./Dockerfile", "instance_type": "lite", "max_instances": 4 }
  ],
  "durable_objects": { "bindings": [{ "name": "Sandbox", "class_name": "Sandbox" }] }
}
```

- `image` is a Dockerfile (or build directory), or a registry ref
  (`docker.io/...`). `twd deploy` builds Dockerfiles on your machine with
  `docker buildx`, pushes them to twd's registry, and pins every image by
  digest (see [Images](#images-build-registry-pinning-host-pull-adr-032-d3)).
  The host never builds and never runs a container engine.
- `instance_type` is one of Cloudflare's types or custom resources (below).
  `max_instances` caps the running containers of that class for the tenant.
- twd.jsonc can add `"containers"` too: the same array form, or
  `{"<Class>": {"image": ref, "images": {name: ref}}}` to pin refs (Sandbox SDK
  1.0 named images). A pin may only name that class's wrangler images.
- A bundle with containers is placed only on a host that offers the
  `containers` tier, for the image's arch (`503 no_host`; with `--host` or a
  sticky lease holder that lacks it, `409 tier_unavailable`). This applies even
  when the parent object itself is process-tier.

### Use it

The app code is Cloudflare's, unchanged. These run unmodified on twd (the v1
acceptance tests):

- **Sandbox SDK 0.x** (`@cloudflare/sandbox` 0.12.x, `examples/minimal`):
  `getSandbox(env.Sandbox, id)`, `sandbox.exec('python3 -c "print(2 + 2)"')`,
  `writeFile` / `readFile`. Its Bun server runs in the image
  (`docker.io/cloudflare/sandbox:0.12.14`) and is reached via `getTcpPort`.
- **Sandbox SDK 1.0** and raw `ctx.container`:
  `ctx.container.start({image, entrypoint: ["sleep", "infinity"], enableInternet: false})`,
  `ctx.container.exec(argv).output()`, and 1.0's `Files` helper, which runs the
  image's `sandbox-shim` through `exec`.

The `ctx.container` methods twd implements:

| Method | On twd |
|---|---|
| `start({env, entrypoint, enableInternet, image, instance, labels})` | ✅ returns at once (no readiness wait); throws if already running. `image` must be one of the class's pinned images |
| `running`, `inspect()` | ✅ |
| `exec(argv, {stdin, stdout, stderr, cwd, env, user, signal, pty})` | ✅ streamed stdio, `kill()`, `exitCode`, `output()`; `pty` + `resize()`. No shell, no timeout, env not inherited except `PATH` (Cloudflare semantics) |
| `getTcpPort(p).fetch()` | ✅ HTTP to a port the container listens on (on `0.0.0.0`) |
| `getTcpPort(p).connect()`, WebSocket upgrades to a container port | ❌ `not_supported` (deferred) |
| `monitor()`, `signal(n)`, `destroy(reason?)` | ✅ |
| `setInactivityTimeout(ms)` | ✅ ≤ 6 h; default 10 min |
| `interceptOutboundHttp(addr, fetcher)`, `interceptAllOutboundHttp(fetcher)`, `interceptOutboundHttps(addr, fetcher)` | ✅ enforced (see [Egress](#egress-what-a-container-can-reach)) |
| `start({containerSnapshot})`, `snapshotContainer()`, `directorySnapshots` | ❌ `not_supported` (v1.1) |
| Sandbox SDK `exposePort()` / preview URLs (`proxyToSandbox`) | ❌ `exposePort()` throws `not_supported`; a preview-proxy request gets `501 not_supported` (needs public ingress, ADR-029) |

### Instance types

Cloudflare's types (checked 2026-11-13), or custom
`{vcpu, memoryMib, diskMb}` up to 4 vCPU, 12 GiB and 20 GB:

| Type | vCPU | Memory | Disk |
|---|---|---|---|
| `lite` | 1/16 | 256 MiB | 2 GB |
| `basic` | 1/4 | 1 GiB | 4 GB |
| `standard-1` | 1/2 | 4 GiB | 8 GB |
| `standard-2` | 1 | 6 GiB | 12 GB |
| `standard-3` | 2 | 8 GiB | 16 GB |
| `standard-4` | 4 | 12 GiB | 20 GB |

Each container is a VM with the type's whole vCPUs rounded up (at least 1)
and its memory as guest RAM. A fractional vCPU is a CPU quota: `cpu.max` on
the VMM's cgroup on Firecracker, `cpu.max` on the guest's container cgroup on
vz. The disk is a fresh sparse scratch disk (the overlay's writable layer),
gone when the VM stops. `pids.max` applies in the guest (and to the VMM on
Firecracker). Container processes hold at most Docker's default capability
set, so root in an image can't lift these limits.

Admission at `start()`, in order: the host's capacity vector (VMs, memory,
vCPU with overcommit, scratch disk; shared with microVM DOs), else
`503 capacity_exhausted`; then a synchronous reservation in mgmt against the
tenant's quotas (`maxContainers` 10, `maxContainerMemMiB` 16384,
`maxContainerVCPU` 8 by default; `twd tenant set --max-containers ...`) and
the class's `max_instances`, else `429 container_quota_exceeded`. If mgmt is
unreachable the start fails closed (`503 quota_unavailable`), except a
restart of a container this host already holds a reservation for. Containers
are not nodes: they don't count against `maxObjects`.

### Egress: what a container can reach

A container has **no tailnet node and no tailnet reach**. Its network ends at
a per-container proxy on the host:

- **By default, nothing.** DNS answers only names the container may reach;
  every other name is NXDOMAIN, every other TCP connection is reset, other
  UDP is dropped. That includes the tailnet, the parent's node, the host,
  link-local/metadata addresses and private ranges.
- **Interceptors are the way to the tailnet.** `interceptOutboundHttp("api.internal", handler)`
  delivers the container's matching requests to the **parent object's**
  handler. The parent decides what to relay, and its own `fetch()` leaves
  with the parent's identity, egress policy and caps. Treat the request as
  untrusted input. `interceptOutboundHttps` works the same over TLS: each
  container gets its own ephemeral CA, installed in the image at
  `/etc/cloudflare/certs/cloudflare-containers-ca.crt` and in its trust
  bundles. Up to 128 interceptors per container.
- **`enableInternet: true`** reaches only the hosts on the workload's
  `tailnet.egress.internet` allowlist in twd.jsonc, never a private, tailnet,
  metadata or host address (even through a DNS name that resolves to one).
  Each new destination is audited as `egress` with `actingFor: "sandbox-of
  <object>"`, and refusals as `deny`. There is no tenant exit node yet, so
  the traffic leaves from the host's own network
  (`twd-hostd -container-internet=direct`, the default; `off` refuses it).
- **Inbound** is only the parent's `getTcpPort(p).fetch()`.

### Lifetime

- `start()` boots the VM; the container runs until its entrypoint exits,
  `destroy()` (SIGTERM, then SIGKILL after at most 10 s), its inactivity
  timeout, or the host's hard max lifetime (24 h by default).
- It belongs to the object, not to the isolate: it survives the object's
  eviction and a blue/green redeploy of the parent (the new code sees
  `running`). It dies with its host; nothing migrates.
- `twd kill <object>`, stopping the workload, and fencing (ADR-028; a
  container inherits its parent class's fence) all SIGKILL the VM at once.
- Disks are ephemeral. There are no snapshots yet.

### Platforms

| Host | Containers | Notes |
|---|---|---|
| Linux arm64, KVM (Firecracker) | ✅ verified (Lima, nested KVM) | the stock `cloudflare/sandbox` images are amd64 only, so twd swaps in its arm64 rebuilds through the alias map; app code is unchanged |
| Linux amd64, KVM (Firecracker) | ⚠ built, not verified | stock images run as published (no alias). The only x86 test host so far is a nested guest whose L2 VMs lose interrupts, so no amd64 VM has booted to ready there |
| macOS, Apple silicon (vz) | ✅ verified | needs `mkfs.erofs` (`brew install erofs-utils`). With `twd-hostd -vm-rosetta` the stock amd64 images run under Rosetta; without it, the arm64 rebuilds |
| no KVM / NAS / process-only hosts | ❌ | they don't offer `containers`; a container bundle is not placed there and `start()` throws `tier_unavailable` (no fallback) |

`twd dev` uses the process tier, so `ctx.container.start()` throws
`tier_unavailable` there.

### Not supported (v1)

- Preview URLs (`exposePort`, `proxyToSandbox`): need public ingress (ADR-029).
- `getTcpPort().connect()`, raw TCP and WebSocket upgrades to container ports.
- `snapshotContainer`, `containerSnapshot`, directory snapshots, backups,
  bucket mounts, persistent sandbox volumes.
- Memory snapshots and fork.
- HTTP/2 and WebSocket upgrades through interceptors (HTTP/1.1 only).
- A container with its own tailnet node (`sandbox.tailnet.tags`), and
  standalone containers not attached to an object.
- Image signing (cosign) and registry garbage collection.
- A tenant exit node or gateway for `enableInternet` (the host's network is
  used).
- Nested virtualization and GPUs (non-goals).
- Workers bindings the Sandbox SDK templates may also use (KV, R2, Workers AI)
  remain unsupported on twd as before (docs/COMPAT.md).

## How it works

```
DO (workerd, any tier) ── ctx.container (polyfill in the generated wrapper)
   └─ platform TailnetApi (attests the object) ── HOSTD binding ── hostd /v1/container…
        └─ container manager, keyed (tenant, workload, class, object)
             └─ hostd/vm BootContainer ── twd-vmhelper ── jailer ── Firecracker   (Linux)
                                       └─ twd-vz (Virtualization.framework)       (macOS)
                  guest: twd-guestd (PID 1) ── overlay(erofs image + scratch) ── ENTRYPOINT/CMD, execs
```

- **One VM per container, one container per object.** On Firecracker its own
  jail uid, network namespace and cgroup (`cpu.max` carries fractional
  vCPUs, `memory.max`, `pids.max`); the parent's DO volume, volume key,
  egress MAC key and tailnet identity are never part of it.
- **Image:** an OCI image becomes a read-only erofs (`hostd/image`; per-tenant
  content-addressed cache, `public` scope for operator-marked bases). Each run
  gets a fresh sparse ext4 scratch: the overlay's upper layer, gone with the
  VM.
- **Guest:** the same guest rootfs and twd-guestd as DO VMs, in container
  mode (`guest.json` "container"); the kernel is `guest/build-kernel.sh`'s
  (erofs) on Linux and `guest/build-kernel-vz.sh`'s on macOS. Every
  container process starts through `twd-guestd ctr-exec`: a re-exec in a new
  mount namespace that `pivot_root`s into the overlay and detaches the old
  root, so no container process sees guestd or its config. It drops every
  capability outside Docker's default set (no `CAP_SYS_ADMIN`, so no cgroup2
  mount; no `CAP_SYS_PTRACE`, so no `/proc/1/root`), then sets the image's
  user.
- **Both backends share the host side** (`hostd/vm/container.go`: exec
  streams, events, ports) and the same NIC: a userspace stack with no gateway
  services whose only DNS server and TCP destinations are the run's sandbox
  egress proxy (`ContainerSpec.Net`).
- **vz differences** (macOS has no jailer, cgroups or `mkfs.ext4`): the
  scratch disk is a sparse file the guest formats
  (`ContainerConfig.ScratchFormat`); fractional vCPUs are `cpu.max` on the
  guest's container cgroup (`ContainerConfig.CPUQuotaUS`); `pids.max` is the
  guest's only. Isolation is the hypervisor plus one VMM process per
  container.
- **Rosetta** (`-vm-rosetta`): guestd registers Rosetta with binfmt_misc
  using the `F` flag, so it works in every container mount namespace. The
  backend reports `amd64` as an emulated arch; hostd then accepts
  linux/amd64 images from `-container-image-dir` and drops its local alias
  map. Images pinned by mgmt follow the host's registered arch (arm64), so
  through the registry a Rosetta Mac still runs the arm64 rebuilds.

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

## The sandbox egress proxy (ADR-032 D4)

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
On vz, Virtualization.framework refuses to save the VM's state (as for DO
guests), so the disks and guest files are scanned but RAM is not; the
report's control then reads `FAILED: guest RAM not scanned` and the command
exits `keys_in_guest`.

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

## Host tiers

A host advertises the `containers` tier (and mgmt places container bundles
on it) only when containers can actually run there: a microVM backend whose
helper (Firecracker) or kernel (vz) supports container guests, a guest
kernel with erofs, and `mkfs.erofs`.
