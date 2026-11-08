// mcp: a stateless MCP server (createMcpHandler from agents/mcp/server) with
// per-caller authorization from the tailnet app capability. No OAuth, no
// sessions: WireGuard + WhoIs authenticate every request.
import { createMcpHandler } from "agents/mcp/server";
import { McpServer } from "@modelcontextprotocol/server";
import { z } from "zod";
import { withTailnet, caller, authorizeTool, budget } from "@twd/mcp";
export { TwdBudget } from "@twd/mcp/budget-do";

interface Env {
  BUDGET: any;
  MCP_CAP?: string;
}

const ORDERS: Record<string, { total: number; status: string }> = {
  o1: { total: 42, status: "shipped" },
  o2: { total: 75, status: "processing" },
};

let ENV: Env;

const server = () => {
  const cap = ENV?.MCP_CAP || "example.com/cap/mcp";
  const s = new McpServer({ name: "payments", version: "1.0.0" });
  s.registerTool("whoami", { description: "Your verified tailnet identity, as this server sees it", inputSchema: {} }, async () => {
    const who = caller();
    return { content: [{ type: "text", text: JSON.stringify({ nodeId: who.nodeId, node: who.node, tags: who.tags }) }] };
  });
  s.registerTool("lookup_order", { description: "Look up an order", inputSchema: { order: z.string() } }, async ({ order }) => {
    authorizeTool(caller(), cap, { tool: "lookup_order" });
    return { content: [{ type: "text", text: JSON.stringify({ order, ...(ORDERS[order] ?? { status: "unknown" }) }) }] };
  });
  s.registerTool("refund", { description: "Refund an order", inputSchema: { order: z.string(), usd: z.number() } }, async ({ order, usd }) => {
    const who = caller(); // this request's verified caller
    authorizeTool(who, cap, { tool: "refund", usd }); // tool granted? usd ≤ maxUsd?
    await budget(ENV.BUDGET, who, "refund").reserve(usd); // perDayUsd, serialised in a DO
    return { content: [{ type: "text", text: JSON.stringify({ refunded: order, usd, by: who.nodeId }) }] };
  });
  return s;
};

const mcp = withTailnet(createMcpHandler(server, { route: "/mcp" }), { path: "/mcp" });

export default {
  fetch(req: Request, env: Env, ctx: any) {
    ENV = env;
    return mcp.fetch(req, env, ctx);
  },
};
