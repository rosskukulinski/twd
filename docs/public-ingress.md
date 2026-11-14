# Public ingress (Funnel)

Everything twd runs is tailnet-only by default: callers are identified by
their WireGuard identity (`getCaller`). Some apps need a public surface
too: a webhook receiver (GitHub, Stripe, Slack), an OAuth callback, a
public page. twd can serve **chosen path prefixes** of a workload to the
public internet over [Tailscale Funnel](https://tailscale.com/kb/1223/funnel).
The design is ADR-029.

## How it works

- **One public node per public workload.** It is an extra node named after
  the workload, so the URL is `https://<workload>.<tailnet>.ts.net/`. It
  carries `tag:twd-public`, needs no grants, and listens **only** on
  Funnel (`:443`, TLS terminated on the node).
- **Every public request goes to the workload's Worker** (its default
  export), never straight to a Durable Object. Object nodes are never
  public. Your routing (`routeAgentRequest`, `/agents/<class>/<name>`)
  decides which object a request reaches.
- **The public node is netd's own listener.** There is no extra gateway
  hop, so `twd expose off` and `twd kill` act on it directly.

## Opting in

In `twd.jsonc`:

```jsonc
{
  "public": {
    // path patterns: "/x" (exact), "/x/*" (/x and below), "/*" (everything)
    "routes": ["/webhooks/*", "/oauth/callback"],
    // routes that may upgrade to WebSocket/SSE (default: none)
    "upgrade": [],
    // optional: override the defaults (up to the ceilings below)
    "limits": { "rps": 50, "ipRps": 10, "maxBodyBytes": 1048576 }
  },
  "tailnet": {
    "objects": {
      // optional: let public requests create objects of this class
      "Room": { "public": { "create": true, "maxObjects": 100 } }
    }
  }
}
```

- **No `"public": true` shorthand.** If you mean every path, write `"/*"`.
- **Never public:** anything under `/_twd` (the platform's own object
  addressing), paths with dot segments or doubled slashes, and encoded
  slashes or backslashes.
- **Unlisted paths** get `404 not_public` from netd and never reach your
  code.

### The tenant gate

Routes only take effect if the tenant allows public ingress:

```sh
twd tenant set <tenant> --public-ingress allow   # or deny
```

- **Single-tenant (`twd up`):** the gate is set to `allow` the first time,
  and an explicit `deny` is kept on later runs.
- **Gate is `deny`:** a deploy with `"public"` is refused with
  `public_ingress_not_allowed`. Switching the gate to `deny` later closes
  every public listener of the tenant.

### Tailnet policy

`twd deploy` checks this and prints it if it's missing:

```jsonc
"tagOwners": { "tag:twd-public": ["autogroup:admin", "tag:twd"] },
"nodeAttrs": [ { "target": ["tag:twd-public"], "attr": ["funnel"] } ]
```

- **HTTPS:** your tailnet needs HTTPS certificates (admin console › DNS ›
  HTTPS Certificates).
- **No new OAuth scopes:** `auth_keys` with tag `tag:twd` already lets twd
  mint `tag:twd-public` nodes.
- **twd never adds `funnel` to any other tag**, apart from `tag:twd-issuer`,
  the WIF issuer's own node. mgmt's node (`tag:twd-mgmt`) is never
  Funnel-capable (`internal/policy.Check` refuses it).

### Deploying

```sh
twd deploy                      # prints the public URL
twd deploy --wait-public 10m    # …and waits until it answers from the internet
```

- **The first fetch of a new name is slow:** the first certificate for a
  new name takes about 1–2 minutes, and public DNS can take up to
  10 minutes. Later deploys reuse the node and its certificate.
- **Name taken:** if another device already has the name, MagicDNS would
  append `-1`. twd refuses to serve that name (`public_name_taken`).

## What your code sees

```ts
import { getCaller, getRequester } from "@twd/tailnet";

export default {
  async fetch(req, env, ctx) {
    const who = getRequester(req);
    if (who.kind === "public") {
      // who.ip, who.proto ("https"), who.host — no identity
    } else {
      // who.kind === "tailnet": the full Caller (nodeId, tags, user, caps())
    }
  },
};
```

- **`getCaller(req)` stays tailnet-only.** On a public request it throws
  `TailnetError("public_request")`, so code that authorizes on the caller
  fails closed instead of treating "no identity" as a principal.
- **Public kind can't be forged.** netd sets it from the Funnel connection
  itself, never from a header. Client copies of every identity header are
  stripped: `Tailscale-*`, `X-Forwarded-*`, `CF-*`, `x-twd-*`,
  `Forwarded`, `X-Real-IP`.
- **Client IP:** netd sets `CF-Connecting-IP` and `X-Forwarded-For` (one
  value) to the client address Funnel reports, and `X-Forwarded-Proto:
  https`. There is no geo data (`request.cf.country` is absent).

## Objects and spawn amplification

In twd every new Durable Object name becomes a tailnet node, so public
traffic must not be able to mint nodes.

- **New objects are refused.** A public request can reach objects that
  already exist (created by tailnet traffic). Reaching a name that doesn't
  exist fails with `403 public_create_denied`, before the object is
  created.
- **No spawning.** `ctx.tailnet.spawn()` and `kill()` are refused in a
  public request, and so is `newUniqueId()`.
- **Opt-in creation.** A class may set `"public": {"create": true,
  "maxObjects": N}`.
  - **Caps:** N is required and at most 1000. Creations are also limited to
    20 per minute per tenant.
  - **Audit:** such objects are recorded with `actingFor: "public"`.

Known limits of the guard (v1):

- **Bypass:** it guards the `env` your handler is given. Code that reaches
  the raw bindings some other way (`import { env } from
  "cloudflare:workers"`, or an `env` saved in a global from an earlier
  request) bypasses it.
  - **Facet classes:** a `fetch()` through the bypass is still refused by
    the platform.
  - **Plain-DO and `"node": false` classes:** they rely on the guard alone.
- **Chained creation:** an object a public request may reach can still
  create other objects in its own code. That is your app's logic.
- **`"node": false` classes:** they have no registry, so they are
  reachable from public requests only with `"public": {"create": true}`.

## Limits

netd enforces these on the public listener. Each is overridable in
`public.limits`, up to a ceiling.

| Limit | Default | Ceiling | Over the limit |
|---|---|---|---|
| `rps` / `burst` (per workload) | 50 / 100 | 1000 / 2000 | `429 rate_limited` + `Retry-After` |
| `ipRps` / `ipBurst` (per client IP) | 10 / 20 | 200 / 400 | `429 rate_limited` + `Retry-After` |
| `maxBodyBytes` | 1 MiB | 32 MiB | `413 payload_too_large` |
| `maxHeaderBytes` | 32 KiB | 64 KiB | `431 header_too_large` |
| `timeoutSeconds` | 30 | 300 | `504 request_timeout` |
| `maxConns` | 256 | 4096 | connection closed |

Upgrades on a route not listed in `upgrade` get `403 upgrade_not_allowed`.
Funnel has its own bandwidth limits, which Tailscale doesn't publish.

## Exposing and unexposing

```sh
twd expose off <workload>    # close the public listener now; survives redeploys
twd expose on  <workload>
```

- **`off`:** closes the Funnel listener and resets in-flight public
  connections. The node stays joined, so its certificate stays warm.
  Measured on a real tailnet, see STATUS.md.
- **Removing `"public"` from `twd.jsonc` and redeploying** deletes the
  public node.

## Audit

- **Per request:** every public request is a `public_request` record in
  the host audit chain (`twd audit <public-object-id>`). It holds the
  method, the path **without the query string**, the matched route, the
  status, the client IP, bytes and duration.
- **Limit refusals:** requests the limits refused (429/413/431, and
  connections over `maxConns`) are rolled up in one `public_rejected`
  summary per minute, so a refused flood can't grow the chain.
- **Other records:** `public_object_created`, and `expose_on`/`expose_off`.
  The operator's change is recorded on mgmt's chain and the listener
  change on the host's.

## Checking it

`twd doctor` shows:

- `https_certs`;
- `public_ingress` (the tenant gate);
- per public workload, `funnel_attr:<workload>` (needs a readable
  policy);
- per public workload, `public_probe:<workload>`: public DNS plus a TLS
  handshake from this machine, with the certificate's expiry.

## Not in v1

- **Signing in tailnet users on public routes (tsidp).** tsidp refuses
  `/authorize` over Funnel and refuses tagged nodes. A later ADR covers it.
- **Others:** custom domains (Funnel serves `*.ts.net` only), a
  bring-your-own front door (Caddy, Cloudflare Tunnel), GeoIP, the Node
  process tier (`"runtime": "node"`), and inbound email or cron triggers.
  For email, use a provider's inbound webhook pointed at a public route.
