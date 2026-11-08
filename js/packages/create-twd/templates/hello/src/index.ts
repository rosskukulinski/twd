// hello: a Worker that returns its verified tailnet caller and does a
// private fetch. Global fetch() leaves through this Worker's own tailnet
// node (tag:twd-wl-hello); names resolve from the netmap only.
import { getCaller, isTailnetError } from "@twd/tailnet";

interface Env {
  UPSTREAM?: string; // e.g. "http://inventory-db/items"
}

export default {
  async fetch(req: Request, env: Env): Promise<Response> {
    const who = getCaller(req);
    let upstream: unknown = null;
    if (env.UPSTREAM) {
      const r = await fetch(env.UPSTREAM);
      const text = await r.text();
      try {
        upstream = { status: r.status, error: r.headers.get("twd-error"), body: JSON.parse(text) };
      } catch {
        upstream = { status: r.status, error: r.headers.get("twd-error"), body: text };
      }
    }
    return Response.json({
      hello: who.user?.login ?? who.hostname,
      caller: { nodeId: who.nodeId, node: who.node, tags: who.tags, user: who.user ?? null, addrs: who.addrs },
      upstream,
    });
  },
} satisfies { fetch(req: Request, env: Env): Promise<Response> };

export { isTailnetError };
