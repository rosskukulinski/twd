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
new listener on the host; the stack has **no handlers** for a container
VM, so every flow the guest starts (gateway platform ports, the host, the
internet) is refused: deny-all egress until the phase-B egress proxy. A
server must listen on `0.0.0.0` (as on Cloudflare). Not listening →
Cloudflare's error string `the container is not listening`, which
`@cloudflare/containers` relies on.

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
`snapshotContainer`, directory snapshots, preview URLs (`exposePort`), egress
enforcement (`enableInternet`, `intercept*` are recorded; the guest has no
egress at all), the registry pull (images come from `image.DirSource`).

A host advertises the `containers` tier (and mgmt places container bundles
on it) only when containers can actually run there: a microVM backend whose
helper supports container guests, a guest kernel with erofs, and
`mkfs.erofs`.
