// ai-agent: a Vercel AI SDK ToolLoopAgent whose tools are a twd MCP server's
// tools, called over the tailnet. The loop runs inside a Durable Object, one
// per caller, so each agent is its own tailnet node: the MCP server sees
// (and authorizes, per tool) the agent itself. The response streams as an AI
// SDK UI message stream (what `useChat` consumes).
//
//   GET /?prompt=…                 your own agent (keyed by your identity)
//   GET /agents/<name>?prompt=…    a named agent
//   GET /worker?prompt=…           the same loop in the Worker (workload identity)
import { DurableObject } from "cloudflare:workers";
import { ToolLoopAgent, stepCountIs } from "ai";
import { createMCPClient } from "@ai-sdk/mcp";
import { getCaller, principalKey, isTailnetError, type Caller } from "@twd/tailnet";
import { mcpFetch } from "@twd/mcp";
import { model } from "./model";

interface Env {
  AiAgent: any;
  MCP_URL: string;
}

// A typed twd error (peer_not_visible, cap_missing, namespace_conflict…),
// possibly wrapped by the MCP client, becomes its catalog response.
function errorResponse(e: unknown): Response {
  for (let x: any = e, i = 0; x && i < 8; x = x.cause, i++) if (isTailnetError(x)) return x.toResponse();
  throw e;
}

async function run(ctx: any, env: Env, prompt: string, who: Caller): Promise<Response> {
  try {
    return await loop(ctx, env, prompt, who);
  } catch (e) {
    return errorResponse(e);
  }
}

async function loop(ctx: any, env: Env, prompt: string, who: Caller): Promise<Response> {
  // mcpFetch: requests leave via ctx.tailnet.fetch (this node's identity);
  // twd egress refusals throw typed errors; the trace ID is propagated.
  const mcp = await createMCPClient({
    transport: { type: "http", url: env.MCP_URL, fetch: mcpFetch(ctx, { traceId: who.traceId }) },
  });
  try {
    const tools = await mcp.tools();
    const agent = new ToolLoopAgent({
      model: model(),
      instructions: "You are a support agent. Use the tools you are granted.",
      tools,
      stopWhen: stepCountIs(4),
    });
    const result = await agent.stream({ prompt, onFinish: () => mcp.close() });
    return result.toUIMessageStreamResponse();
  } catch (e) {
    await mcp.close();
    throw e;
  }
}

export class AiAgent extends DurableObject<Env> {
  async fetch(req: Request): Promise<Response> {
    const who = getCaller(req);
    const prompt = new URL(req.url).searchParams.get("prompt") ?? "Where is order o1?";
    return run(this.ctx, this.env, prompt, who);
  }
}

export default {
  async fetch(req: Request, env: Env, ctx: any): Promise<Response> {
    const who = getCaller(req);
    const url = new URL(req.url);
    if (url.pathname === "/worker") {
      return run(ctx, env, url.searchParams.get("prompt") ?? "Where is order o1?", who);
    }
    const m = url.pathname.match(/^\/agents\/([^/]+)/);
    const name = m ? decodeURIComponent(m[1]) : principalKey(who);
    return env.AiAgent.getByName(name).fetch(req);
  },
};
