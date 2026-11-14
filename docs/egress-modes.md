# Per-object egress: facet mode vs plain-DO mode

Each Durable Object class picks how its outbound `fetch()` is attributed, in `twd.jsonc`:

```jsonc
{ "tailnet": { "objects": {
    "SupportAgent": { "tags": ["tag:twd-agent-support"] },                     // facet mode (default)
    "Reminder":     { "tags": ["tag:twd-reminder"], "egress": "workload" },    // plain-DO mode
    "TwdBudget":    { "egress": "workload", "node": false }                     // internal: no node at all
} } }
```

| | **facet** (default, `"egress": "object"`) | **plain-DO** (`"egress": "workload"`) |
|---|---|---|
| How it runs | The platform's `Host_<Class>` DO hosts your class as a facet in a Dynamic Worker | Your class is an ordinary DO in the tenant service |
| Egress identity | **The object's own node.** `globalOutbound` is the platform `Egress` specialised with this object's class+name (props tenant code can't change), and hostd verifies a MAC | **The workload node**, shared by every object of the workload |
| Ingress identity | Own node (`getCaller()` per request) | Own node (`getCaller()` per request) |
| Alarms (`setAlarm`, Agents SDK `schedule()`) | ❌ Facets have no alarm scheduler (workerd today) | ✅ |
| `ctx.tailnet.spawn/kill` | Attested by the platform (unforgeable `as`) | Claimed by the tenant-side wrapper. Within one workload, code could claim to be another plain object of the same workload. Same trust domain (RFC §4.8 residual risk) |
| Kill | Enforced in platform code (the `Host_` flag, plus the facet aborted) | Enforced by the tenant-side wrapper's `fetch`; the node is killed by netd either way |
| `ctx.container` (classes with wrangler `containers`, docs/containers.md) | ✅ Only the object's own container (platform-attested) | ✅ Its own container; like `spawn/kill`, claimed by the tenant-side wrapper, so code in the workload could drive another plain object's container of the same workload, never a facet object's |
| A container's intercepted requests (`interceptOutboundHttp(s)`, docs/containers.md) | Run in the object; the handler's `fetch()` leaves as **the object's node** | Run in the object; the handler's `fetch()` leaves as **the workload node** |
| Env bindings to other DOs | Proxies: `env.X.getByName(name)` only. **Await** it, or call a method on it at once; don't store the un-awaited result. `idFromName`/`get(id)` are not available inside facets | Real namespaces |
| `newUniqueId()` objects | Not supported (objects must have names; a name is the node's identity) | Work, but get no node (no `ctx.id.name`) |

Switching a class between modes changes its storage namespace (`Host_<Class>` vs `<Class>`), so existing objects of that class start empty. Pick the mode before you store data. The fix for both columns is upstream ask WD-3 (per-actor `globalOutbound`), which removes the facet requirement and gives alarms *and* per-object egress.

**Hibernation (both modes).** workerd evicts idle objects after about 10 s, which drops in-memory state. The object's node stays online in netd, and the next request wakes the object (measured in `TestHostd/HibernationKeepsNode`).
