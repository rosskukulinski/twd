# SDK compatibility on twd (process tier)

What runs in-isolate on twd, as **measured** by the hermetic conformance suite
`make test-sdk` (`e2e/sdk`): real SDK code on twd-hostd + the generated
platform + stock workerd, driven over testcontrol + local DERP by a tsnet
client, on macOS (arm64) and Linux (Lima `twd-dev`, arm64). No real tailnet,
no LLM keys.

Pinned versions (root `package.json`, exact):

| Package | Version | Note |
|---|---|---|
| workerd | 1.20261108.1 | platform runtime |
| `agents` (Cloudflare Agents SDK) | 0.33.0 | npm `latest` on 2026-11-08; runs on the pinned workerd unmodified |
| `@modelcontextprotocol/server` / `client` | 2.4.0 | MCP SDK v2 (agents' peers; `createMcpHandler`, `addMcpServer`) |
| `@modelcontextprotocol/sdk` | 1.32.2 | v1, only for agents' deprecated `McpAgent` (not used) |
| `ai` (Vercel AI SDK) | 7.0.165 | `ToolLoopAgent`, `ai/test` `MockLanguageModelV4` |
| `@ai-sdk/provider` / `provider-utils` | 4.0.37 / 5.0.76 | |
| `@ai-sdk/mcp` | 2.0.93 | HTTP transport only |
| `mcp-handler` (Vercel) | 2.3.1 | MCP SDK v2, 2026-07-28 spec |
| `zod` | 4.6.7 | |

Egress modes (ADR-019 #5, `docs/egress-modes.md`): **facet** (default; the
object's `fetch()` leaves from its own node) and **plain-DO**
(`"egress": "workload"`; egress leaves as the workload node). Every Agent
check below runs in both.

## Cloudflare Agents SDK (`agents` 0.33.0)

| Feature | Facet | Plain-DO | Test (`e2e/sdk`) | Notes |
|---|---|---|---|---|
| `routeAgentRequest` (`/agents/<kebab-class>/<name>`) | ✅ | ✅ | `RouteAgentRequest` | unknown namespace → 400 (SDK behaviour) |
| `getAgentByName` | ✅ | ✅ | `RouteAgentRequest` | |
| `getCaller` in `onRequest` | ✅ | ✅ | `RouteAgentRequest`, `SetState…` | via the workload node and via the object's own node |
| `getCaller` in `onConnect` (`ctx.request`) | ✅ | ✅ | `WebSocketsAndHibernation` | the upgrade request keeps `cf` |
| `setState` / `onStateChanged` | ✅ | ✅ | `SetStateOnStateChangedSQL` | `initialState`'s first persist also fires `onStateChanged` (SDK behaviour) |
| `this.sql` | ✅ | ✅ | `SetStateOnStateChangedSQL` | |
| `schedule(delay)` / `scheduleEvery` / `listSchedules` / `cancelSchedule` | ✅ (shim) | ✅ | `Schedule`, `TestAgentTemplate` | workerd facets cannot set alarms; twd delegates the facet's `ctx.storage.{set,get,delete}Alarm` to its Host DO, whose `alarm()` runs the facet's. Once + 2× every fired in ~1.9 s in both modes |
| WebSockets (partyserver), state-sync broadcast (`cf_agent_state`) | ✅ | ✅ | `WebSocketsAndHibernation` | through netd ingress + hostd (`httputil.ReverseProxy` upgrade) |
| WebSocket **hibernation** | ✅ | ✅ | `WebSocketsAndHibernation` | 14 s idle: the DO was evicted (new boot ID), sockets stayed open, connection state (attachments, SQL) intact |
| Plain `fetch()` egress attribution | own node | workload node | `EgressMode` | |
| `createMcpHandler` (`agents/mcp/server`) + `@twd/mcp` `withTailnet` | n/a (Worker) | | `CreateMcpHandlerWithTailnet` | per-request caller; `lookup_order` allowed, `refund` → `isError` with `cap_missing` JSON |
| Agent as MCP client: `addMcpServer(name, url)` + `this.mcp.callTool` | ✅ | ✅ | `AgentAsMCPClient` | to a twd MCP server on **another host** over the tailnet. The server's per-request caller is the agent's own node (facet) or the workload node (plain-DO). Allow and deny both checked |
| `addMcpServer` to an MCP server on the **same host** | ❌ | ❌ | `SameHostMCPRefused` | netd refuses sibling hairpins (`forbidden_destination`, RFC §4.5); the SDK surfaces it as "HTTP 403" (no fetch hook to type it). Same-host object→object calls over the tailnet are not built |
| `subAgent()` / sub-agent facets | ❌ | ? | `UnsupportedBindings` | inside a facet the Dynamic Worker's `ctx.exports` has no tenant classes ("Sub-agent class … not found in worker exports"). Use `ctx.tailnet.spawn` (own node, narrower caps) |
| `env.AI` (Workers AI) | ❌ | ❌ | `UnsupportedBindings` | no binding. Use an AI SDK provider pointed at a tailnet LLM gateway (`ai-agent` template, `src/model.ts`) |
| Workers KV binding (incl. `OAUTH_KV` for workers-oauth-provider) | ❌ | ❌ | `UnsupportedBindings` | no KV binding (DO `ctx.storage.kv` works). For tailnet callers use `getCaller` instead of OAuth |
| Workflows (`AgentWorkflow`, `runWorkflow`, `waitForApproval`) | ❌ | ❌ | `UnsupportedBindings` | no Workflows binding/engine (ADR-022: v1) |
| Email (`cloudflare:email`, `routeAgentEmail`) | ❌ | ❌ | `UnsupportedBindings` | the bundler stubs `cloudflare:email`; constructing `EmailMessage` throws "not supported on twd" |
| Browser Rendering (`env.BROWSER`, `agents/browser`) | ❌ | ❌ | `UnsupportedBindings` | |
| Codemode / dynamic agents (`env.LOADER` for tenants) | ❌ | ❌ | `UnsupportedBindings` | Worker Loader is platform-only |
| `harness/container/*` | ❌ | ❌ | — | needs a container/process (`node:child_process`, `fs`, `net`); ADR-022 Node process tier |
| `McpAgent` | not tested | | — | deprecated upstream; use `createMcpHandler` (ADR-022) |
| `@callable` RPC, `useAgent`, `AIChatAgent` | not tested | | — | same DO/WebSocket primitives as above (assumption) |

## Vercel AI SDK (`ai` 7.0.165) and `mcp-handler` 2.3.1

| Feature | Status | Test | Notes |
|---|---|---|---|
| `ToolLoopAgent.stream()` in a Durable Object (facet) | ✅ | `ToolLoopAgent/*` | `MockLanguageModelV4` (scripted: tool calls, then text). One agent = one node: the MCP server saw `…-aiagent-<name>` as the caller |
| `ToolLoopAgent.stream()` in the Worker | ✅ | `ToolLoopAgent/*` | caller = the workload node |
| `toUIMessageStreamResponse()` (SSE, what `useChat` reads) | ✅ | `ToolLoopAgent/*` | 36–38 events; first event 50–400 ms after the request on this Mac |
| `@ai-sdk/mcp` `createMCPClient` (HTTP transport) → twd MCP server over the tailnet | ✅ with `mcpFetch` | `ToolLoopAgent/*` | `tools()` from both `mcp-handler` and CF `createMcpHandler`; allowed tool output and denied tool (`cap_missing`) reach the model |
| `@ai-sdk/mcp` with the default `fetch` | ❌ | — | its default `redirect: "error"` throws in workerd ("Invalid redirect value"). `@twd/mcp` `mcpFetch(ctx)` emulates it (manual + check) |
| `@ai-sdk/mcp` stdio transport | ❌ | — | `child_process` (Node process tier) |
| `mcp-handler` `createMcpHandler` + `withTailnet` | ✅ | `McpHandlerWithTailnet` | Web-standard handler mounted at `/mcp`; per-request caller, allow/deny, `whoami` |
| Typed egress refusals through the MCP client | ✅ | `EgressRefusalIsTyped` | `mcpFetch` throws `TailnetError`; the template renders it (`403 peer_not_visible`) instead of a 500 |
| `traceparent` propagation | ✅ | `TraceparentPropagated` | `mcpFetch` sends `00-<caller traceId>-<span>-01`; hostd assigns each hop its own trace ID (the header is not trusted) |
| On-behalf-of (`actingFor`) assertions on MCP calls | ❌ | — | RFC §4.6 not implemented: the server sees the agent's node, not the human it acts for |
| `WorkflowAgent` / Workflow SDK | ❌ | — | `node:vm` (ADR-022: Node process tier, branch m9) |
| `HarnessAgent`, Sandbox, hosted Vercel services | ❌ | — | client-only or process tier |

## twd behaviour found by this suite

- **Facet alarms** (fixed): workerd throws "Facets currently cannot set
  alarms", and the Agents SDK's lifecycle re-arms on every wake, so calling
  `schedule()` in facet mode wedged the agent (every later request 500).
  twd now delegates facet alarms to the Host DO (`hostd/gen/js/platform.js`
  `__twd_alarm_op`/`alarm`, `@twd/tailnet` runtime `installAlarmShim`).
- **Cross-workload object collision** (fixed in M7): M5 found that mgmt
  keyed objects by `(tenant, namespace = class, name)`, so two workloads of
  one tenant with the same DO class and object name got the **same** object
  (the second app's egress left from the first app's node), and hostd
  failed closed with `409 namespace_conflict`. mgmt now scopes identity by
  `(tenant, workload, class, name)`: the two get distinct objects, nodes,
  hostnames and state dirs (`SameClassAcrossWorkloadsDistinctIdentities`).
  hostd keeps `namespace_conflict` as a defensive assertion only.
- **Same-host MCP**: an agent and the MCP server it calls must be on
  different hosts (or the call must go through the platform, not built).
