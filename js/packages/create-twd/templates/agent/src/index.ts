// agent: a Cloudflare Agents SDK Agent. Each agent instance (one per
// caller by default) is its own Durable Object AND its own tailnet node;
// its fetch() leaves from that node. State (setState) and SQL (this.sql)
// are the Agent's own SQLite storage.
//
//   GET /?agent=<name>                 HTTP via getAgentByName (default: one per caller)
//   GET|WS /agents/support-agent/<n>   routeAgentRequest (HTTP and WebSocket)
//   GET .../remind?in=5&note=…         schedule() a reminder (alarm-backed)
import { Agent, getAgentByName, routeAgentRequest, type Connection, type ConnectionContext } from "agents";
import { getCaller, tailnet, principalKey } from "@twd/tailnet";

interface Env {
  SupportAgent: DurableObjectNamespace;
  LookupAgent: DurableObjectNamespace;
  UPSTREAM?: string;
}
type DurableObjectNamespace = any;

interface State {
  visits: number;
}

export class SupportAgent extends Agent<Env, State> {
  initialState: State = { visits: 0 };

  // WebSocket clients (hibernatable): the upgrade request carries the
  // verified caller too.
  onConnect(conn: Connection, ctx: ConnectionContext) {
    const who = getCaller(ctx.request);
    conn.send(JSON.stringify({ type: "hello", agent: this.name, caller: { nodeId: who.nodeId, node: who.node } }));
  }

  // A scheduled callback (see the /remind route).
  async remind(p: { note: string }) {
    this.sql`create table if not exists reminders (note text, at integer)`;
    this.sql`insert into reminders (note, at) values (${p.note}, ${Date.now()})`;
  }

  async onRequest(req: Request): Promise<Response> {
    const who = getCaller(req);
    const url = new URL(req.url);
    this.sql`create table if not exists calls (node text, path text, at integer)`;
    this.sql`insert into calls (node, path, at) values (${who.nodeId}, ${url.pathname}, ${Date.now()})`;
    this.setState({ visits: this.state.visits + 1 });
    const [{ n }] = this.sql<{ n: number }>`select count(*) as n from calls`;

    if (url.pathname.endsWith("/remind")) {
      const secs = Number(url.searchParams.get("in") ?? "0");
      if (secs > 0) await this.schedule(secs, "remind", { note: url.searchParams.get("note") ?? "ping" });
      this.sql`create table if not exists reminders (note text, at integer)`;
      return Response.json({ agent: this.name, fired: this.sql`select note from reminders order by at`.map((r: any) => r.note) });
    }

    if (url.pathname.endsWith("/spawn")) {
      // A sub-agent with its own node and narrower caps.
      const child = await tailnet(this.ctx).spawn(this.env.LookupAgent, {
        name: url.searchParams.get("child") ?? `lookup-${crypto.randomUUID().slice(0, 8)}`,
        caps: { "example.com/cap/mcp": { tools: ["lookup_order"] } },
        ttl: "10m",
      });
      await child.ready;
      const r = await child.stub.fetch(new Request("http://child/", { headers: { "x-parent": this.name } }));
      return Response.json({ agent: this.name, child: { name: child.name, objectId: child.objectId, reply: await r.json() } });
    }

    let upstream: unknown = null;
    const target = url.searchParams.get("upstream") ?? this.env.UPSTREAM;
    if (target) {
      try {
        const r = await tailnet(this.ctx).fetch(target);
        upstream = { status: r.status, body: await r.json() };
      } catch (e: any) {
        upstream = { error: e.code ?? String(e), message: e.message };
      }
    }
    return Response.json({
      agent: this.name,
      caller: { nodeId: who.nodeId, node: who.node, tags: who.tags, user: who.user ?? null },
      state: this.state,
      calls: n,
      upstream,
    });
  }
}

export class LookupAgent extends Agent<Env, { lookups: number }> {
  initialState = { lookups: 0 };
  async onRequest(req: Request): Promise<Response> {
    this.setState({ lookups: this.state.lookups + 1 });
    return Response.json({ lookup: this.name, parent: req.headers.get("x-parent"), lookups: this.state.lookups });
  }
}

export default {
  async fetch(req: Request, env: Env): Promise<Response> {
    const routed = await routeAgentRequest(req, env);
    if (routed) return routed;
    const who = getCaller(req);
    const name = new URL(req.url).searchParams.get("agent") ?? principalKey(who);
    const agent = await getAgentByName(env.SupportAgent, name);
    return agent.fetch(req);
  },
};
