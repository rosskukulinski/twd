# templates

The app templates live in `js/packages/create-twd/templates/` so they ship
with `create-twd` (`npm create twd@latest <dir> -- --template hello|agent|mcp|ai-agent`):

- `hello`: a Worker that returns its verified caller and does a private fetch.
- `agent`: a Cloudflare Agents SDK `Agent` (state, `this.sql`, per-object node
  and egress, `routeAgentRequest` with WebSockets and `getCaller` in
  `onConnect`, `schedule()`, `ctx.tailnet.spawn` of a sub-agent).
- `mcp`: a stateless MCP server (`createMcpHandler` from `agents/mcp/server`)
  wrapped in `withTailnet`, with `authorizeTool`, a DO-backed `budget` and a
  `whoami` tool.
- `ai-agent`: a Vercel AI SDK `ToolLoopAgent` in a Durable Object (one agent,
  one node per caller) whose tools are a twd MCP server's tools over the
  tailnet (`@ai-sdk/mcp` + `@twd/mcp` `mcpFetch`); it streams a UI message
  response. It ships with a scripted mock model (`src/model.ts`, no keys);
  swap in a real provider.

The verified SDK surface and its gaps are in `docs/COMPAT.md`.

`create-twd build <dir>` bundles an app into `dist/index.js`.
