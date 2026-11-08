// M5 conformance app: real Cloudflare Agents SDK (`agents`) classes on twd.
// The same Agent body runs in both egress modes (ADR-019 #5):
//   StateAgent  facet mode (default): per-object egress from its own node
//   PlainAgent  plain-DO mode ("egress": "workload"): alarms work
// The Go suite (e2e/sdk) drives it over the tailnet (testcontrol + DERP).
import { Agent, routeAgentRequest, getAgentByName, type Connection, type ConnectionContext, type WSMessage } from "agents";
import { createMcpHandler } from "agents/mcp/server";
import { McpServer } from "@modelcontextprotocol/server";
import { z } from "zod";
import { getCaller, tailnet, isTailnetError } from "@twd/tailnet";
import { withTailnet, caller, authorizeTool } from "@twd/mcp";

interface Env {
  StateAgent: any;
  PlainAgent: any;
  MCP_CAP?: string;
}

interface State {
  count: number;
  last?: string;
}

const errOf = (e: any) => ({ error: String(e?.message ?? e), code: e?.code ?? null, name: e?.name ?? null });
const whoOf = (req: Request) => {
  try {
    const c = getCaller(req);
    return { nodeId: c.nodeId, node: c.node, tags: c.tags };
  } catch (e) {
    return errOf(e);
  }
};

class ConformanceAgent extends Agent<Env, State> {
  initialState: State = { count: 0 };
  boot = crypto.randomUUID().slice(0, 8);

  onStart() {
    this.sql`create table if not exists changes (count integer, source text, at integer)`;
    this.sql`create table if not exists ticks (kind text, payload text, at integer)`;
    this.sql`create table if not exists conns (id text, node text, at integer)`;
  }

  // onStateChanged fires for every setState (server or client). Record it in
  // SQL so the test can see the hook ran.
  onStateChanged(state: State | undefined, source: Connection | "server") {
    this.sql`insert into changes (count, source, at) values (${state?.count ?? -1}, ${source === "server" ? "server" : "client:" + source.id}, ${Date.now()})`;
  }

  // Scheduled callbacks.
  async tick(payload: { kind: string }) {
    this.sql`insert into ticks (kind, payload, at) values (${payload?.kind ?? "?"}, ${JSON.stringify(payload ?? null)}, ${Date.now()})`;
  }

  // ---- WebSockets (hibernatable, via partyserver) ----
  onConnect(conn: Connection, ctx: ConnectionContext) {
    const who = whoOf(ctx.request);
    this.sql`insert into conns (id, node, at) values (${conn.id}, ${(who as any).nodeId ?? "none"}, ${Date.now()})`;
    conn.send(JSON.stringify({ type: "hello", agent: this.name, boot: this.boot, caller: who }));
  }

  onMessage(conn: Connection, msg: WSMessage) {
    const m = String(msg);
    if (m === "inc") {
      this.setState({ count: this.state.count + 1, last: "ws:" + conn.id });
      conn.send(JSON.stringify({ type: "inc", count: this.state.count, boot: this.boot }));
      return;
    }
    if (m === "who") {
      const [row] = this.sql<{ node: string }>`select node from conns where id = ${conn.id}`;
      conn.send(JSON.stringify({ type: "who", node: row?.node ?? null, boot: this.boot, conns: [...this.getConnections()].length }));
      return;
    }
    conn.send(JSON.stringify({ type: "echo", msg: m, boot: this.boot }));
  }

  // ---- HTTP ----
  async onRequest(req: Request): Promise<Response> {
    const url = new URL(req.url);
    const op = url.pathname.split("/").filter(Boolean).pop() ?? "";
    const q = (k: string) => url.searchParams.get(k);
    switch (op) {
      case "state": {
        if (q("inc")) this.setState({ count: this.state.count + Number(q("inc")), last: "http" });
        const changes = this.sql<{ count: number; source: string }>`select count, source from changes order by at`;
        return Response.json({ agent: this.name, boot: this.boot, state: this.state, changes, caller: whoOf(req) });
      }
      case "sql": {
        this.sql`create table if not exists kv (k text primary key, v text)`;
        if (q("k")) this.sql`insert or replace into kv (k, v) values (${q("k")}, ${q("v") ?? ""})`;
        const rows = this.sql<{ k: string; v: string }>`select k, v from kv order by k`;
        return Response.json({ rows });
      }
      case "schedule": {
        // schedule(delaySeconds) and scheduleEvery(seconds) — both alarm-backed.
        const out: Record<string, unknown> = { mode: this.constructor.name };
        try {
          const s = await this.schedule(1, "tick", { kind: "once" });
          out.once = { id: s.id, type: s.type };
        } catch (e) {
          out.once = errOf(e);
        }
        try {
          const s = await this.scheduleEvery(1, "tick", { kind: "every" });
          out.every = { id: s.id, type: s.type };
        } catch (e) {
          out.every = errOf(e);
        }
        try {
          out.alarm = await this.ctx.storage.getAlarm();
        } catch (e) {
          out.alarm = errOf(e);
        }
        return Response.json(out);
      }
      case "ticks": {
        const rows = this.sql<{ kind: string; n: number }>`select kind, count(*) as n from ticks group by kind order by kind`;
        let schedules: unknown = null;
        try {
          schedules = (await this.listSchedules()).map((s: any) => ({ type: s.type, callback: s.callback }));
        } catch (e) {
          schedules = errOf(e);
        }
        if (q("cancel")) {
          for (const s of (await this.listSchedules().catch(() => [])) as any[]) await this.cancelSchedule(s.id);
        }
        return Response.json({ ticks: Object.fromEntries(rows.map((r) => [r.kind, r.n])), schedules });
      }
      case "mcp-client": {
        // The Agent as an MCP client (addMcpServer) to a twd MCP server over
        // the tailnet. The server sees THIS object's node as the caller.
        const target = q("url");
        if (!target) return Response.json({ error: "url required" }, { status: 400 });
        const out: Record<string, unknown> = {};
        try {
          const r = await this.addMcpServer("payments", target, { transport: { type: "streamable-http" } });
          out.add = { state: r.state };
          const tools = this.mcp.listTools().map((t: any) => t.name).sort();
          out.tools = tools;
          const call = async (name: string, args: Record<string, unknown>) => {
            try {
              const res: any = await this.mcp.callTool({ serverId: r.id, name, arguments: args });
              return { isError: !!res.isError, text: res.content?.[0]?.text };
            } catch (e) {
              return errOf(e);
            }
          };
          out.lookup = await call("lookup_order", { order: "o1" });
          out.refund = await call("refund", { order: "o1", usd: 10 });
          out.whoami = await call("whoami", {});
          await this.removeMcpServer(r.id);
        } catch (e) {
          out.error = errOf(e);
        }
        return Response.json(out);
      }
      case "egress": {
        // Plain fetch() from the agent (no twd API): attributed per egress mode.
        try {
          const r = await fetch(q("url")!);
          return Response.json({ status: r.status, body: await r.json() });
        } catch (e) {
          return Response.json(errOf(e), { status: 502 });
        }
      }
      case "tegress": {
        try {
          const r = await tailnet(this.ctx).fetch(q("url")!);
          return Response.json({ status: r.status, body: await r.json() });
        } catch (e) {
          return Response.json({ thrown: true, typed: isTailnetError(e), ...errOf(e) }, { status: 299 });
        }
      }
      case "probe": {
        // Cloudflare-only bindings and APIs: documents what is (not) there.
        const env = this.env as any;
        const out: Record<string, unknown> = {
          AI: typeof env.AI,
          KV: typeof env.KV,
          WORKFLOW: typeof env.WORKFLOW,
          BROWSER: typeof env.BROWSER,
          LOADER: typeof env.LOADER,
          exports: typeof (this.ctx as any).exports,
          facets: typeof (this.ctx as any).facets,
        };
        try {
          const { EmailMessage } = await import("cloudflare:email");
          new (EmailMessage as any)("a@example.com", "b@example.com", "raw");
          out.email = "constructed";
        } catch (e) {
          out.email = errOf(e);
        }
        try {
          await (this as any).subAgent?.(PlainAgent, "sub-1");
          out.subAgent = "ok";
        } catch (e) {
          out.subAgent = errOf(e);
        }
        return Response.json(out);
      }
    }
    return Response.json({ agent: this.name, boot: this.boot, caller: whoOf(req), state: this.state });
  }
}

export class StateAgent extends ConformanceAgent {}
export class PlainAgent extends ConformanceAgent {}

// A stateless MCP server in the same app: createMcpHandler + withTailnet.
let ENV: Env;
const mcpServer = () => {
  const cap = ENV?.MCP_CAP || "example.com/cap/mcp";
  const s = new McpServer({ name: "cfagents", version: "1.0.0" });
  s.registerTool("whoami", { description: "The verified caller of this request", inputSchema: {} }, async () => {
    const who = caller();
    return { content: [{ type: "text", text: JSON.stringify({ nodeId: who.nodeId, node: who.node }) }] };
  });
  s.registerTool("lookup_order", { description: "Look up an order", inputSchema: { order: z.string() } }, async ({ order }) => {
    authorizeTool(caller(), cap, { tool: "lookup_order" });
    return { content: [{ type: "text", text: JSON.stringify({ order, status: "shipped" }) }] };
  });
  s.registerTool("refund", { description: "Refund an order", inputSchema: { order: z.string(), usd: z.number() } }, async ({ order, usd }) => {
    authorizeTool(caller(), cap, { tool: "refund", usd });
    return { content: [{ type: "text", text: JSON.stringify({ refunded: order, usd }) }] };
  });
  return s;
};
const mcp = withTailnet(createMcpHandler(mcpServer, { route: "/mcp" }), { path: "/mcp" });

export default {
  async fetch(req: Request, env: Env, ctx: any): Promise<Response> {
    ENV = env;
    const url = new URL(req.url);
    if (url.pathname === "/mcp" || url.pathname.startsWith("/mcp/")) return mcp.fetch(req, env, ctx);
    // /agents/<kebab-class>/<name>/... → the Agent (HTTP and WebSocket).
    const routed = await routeAgentRequest(req, env);
    if (routed) return routed;
    if (url.pathname === "/by-name") {
      const a = await getAgentByName(env.StateAgent, url.searchParams.get("name") ?? "x");
      return a.fetch(req);
    }
    return Response.json({ worker: true, caller: whoOf(req) });
  },
};
