// M5 conformance app: Vercel's MCP server adapter (`mcp-handler`, MCP SDK v2)
// hosted on twd and wrapped with @twd/mcp withTailnet: per-request verified
// caller, per-tool authorization from the tailnet app capability. No OAuth.
import { createMcpHandler } from "mcp-handler";
import { z } from "zod";
import { withTailnet, caller, authorizeTool } from "@twd/mcp";

interface Env {
  MCP_CAP?: string;
}
let CAP = "example.com/cap/mcp";

const handler = createMcpHandler(
  (server) => {
    server.registerTool("whoami", { description: "Your verified tailnet identity", inputSchema: z.object({}) }, async () => {
      const who = caller();
      return { content: [{ type: "text", text: JSON.stringify({ nodeId: who.nodeId, node: who.node, traceId: who.traceId }) }] };
    });
    server.registerTool("lookup_order", { description: "Look up an order", inputSchema: z.object({ order: z.string() }) }, async ({ order }) => {
      authorizeTool(caller(), CAP, { tool: "lookup_order" });
      return { content: [{ type: "text", text: JSON.stringify({ order, status: "shipped", server: "mcp-handler" }) }] };
    });
    server.registerTool("refund", { description: "Refund an order", inputSchema: z.object({ order: z.string(), usd: z.number() }) }, async ({ order, usd }) => {
      authorizeTool(caller(), CAP, { tool: "refund", usd });
      return { content: [{ type: "text", text: JSON.stringify({ refunded: order, usd }) }] };
    });
  },
  { serverInfo: { name: "vercel-mcp", version: "1.0.0" } },
);

// mcp-handler does not inspect the path; mount it at /mcp.
const mcp = withTailnet(handler, { path: "/mcp" });

export default {
  fetch(req: Request, env: Env, ctx: any) {
    CAP = env.MCP_CAP || CAP;
    return mcp.fetch(req, env, ctx);
  },
};
