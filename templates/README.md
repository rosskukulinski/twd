# templates

The app templates live in `js/packages/create-twd/templates/` so they ship
with `create-twd` (`npm create twd@latest <dir> -- --template hello|agent|mcp`):

- `hello`: a Worker that returns its verified caller and does a private fetch.
- `agent`: a Cloudflare Agents SDK `Agent` (state, `this.sql`, per-object node
  and egress, `ctx.tailnet.spawn` of a sub-agent).
- `mcp`: a stateless MCP server (`createMcpHandler` from `agents/mcp/server`)
  wrapped in `withTailnet`, with `authorizeTool` and a DO-backed `budget`.

`create-twd build <dir>` bundles an app into `dist/index.js`.
