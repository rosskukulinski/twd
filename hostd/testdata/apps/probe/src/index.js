// Test fixture for twd-hostd: an ordinary Worker + DOs that expose what the
// platform did (caller, attribution, headers) so the Go tests can check it.
import { DurableObject } from "cloudflare:workers";
import { getCaller, tailnet, isTailnetError } from "@twd/tailnet";

const SEEN = (req) => Object.fromEntries([...req.headers].filter(([k]) => k.startsWith("x-twd") || k.startsWith("cf-") || k === "x-forwarded-for" || k === "tailnet-assertion"));

async function egress(url, init) {
  try {
    const r = await fetch(url, init);
    const h = { "content-type": r.headers.get("content-type") || "text/plain" };
    if (r.headers.get("twd-error")) h["twd-error"] = r.headers.get("twd-error");
    return new Response(await r.text(), { status: r.status, headers: h });
  } catch (e) {
    return Response.json({ tenantCaught: String(e) }, { status: 599 });
  }
}

function stream() {
  const enc = new TextEncoder();
  let n = 0;
  return new Response(new ReadableStream({
    async pull(c) {
      await new Promise((r) => setTimeout(r, 50));
      c.enqueue(enc.encode("tick " + n++ + "\n"));
    },
  }));
}

let boots = 0;

export class Probe extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    boots++;
  }
  async hello(x) {
    return "hello " + x + " from " + this.ctx.id.name;
  }
  async fetch(req) {
    const url = new URL(req.url);
    const q = (k) => url.searchParams.get(k);
    switch (url.pathname) {
      case "/egress":
        return egress(q("url"));
      case "/tegress":
        try {
          const r = await tailnet(this.ctx).fetch(q("url"));
          return new Response(await r.text(), { status: r.status });
        } catch (e) {
          return Response.json({ thrown: isTailnetError(e), code: e.code, message: e.message }, { status: 299 });
        }
      case "/forge-egress": {
        const victim = q("as");
        return egress(q("url"), {
          headers: { "x-twd-object": victim, "x-twd-class": "Probe", "x-twd-name": victim, "x-twd-mac": "00", "x-twd-cf": JSON.stringify({ objectId: victim }), "Proxy-Authorization": victim },
          cf: { objectId: victim },
        });
      }
      case "/stream":
        return stream();
      case "/env":
        return Response.json({ env: Object.keys(this.env).sort(), props: this.ctx.props ?? null, hasTailnet: !!this.ctx.tailnet });
      case "/spawn": {
        const child = await tailnet(this.ctx).spawn(this.env.PROBE, { name: q("name"), caps: q("caps") ? JSON.parse(q("caps")) : undefined, ttl: "10m" });
        const before = child.state();
        try {
          await child.ready;
        } catch (e) {
          return Response.json({ error: e.code, message: e.message, before }, { status: 299 });
        }
        return Response.json({ objectId: child.objectId, name: child.name, before, after: child.state(), reply: await child.stub.hello("parent") });
      }
      case "/kill-self":
        return Response.json(await tailnet(this.ctx).kill({ reason: "self" }));
      case "/sibling":
        return Response.json({ reply: await this.env.PROBE.getByName(q("name")).hello("sibling") });
      case "/ticker":
        return this.env.TICKER.getByName(q("name")).fetch(new Request("http://t/" + (q("op") ?? "")));
    }
    return Response.json({
      kind: "probe",
      name: this.ctx.id.name,
      boots,
      caller: req.cf?.tailscale?.caller ?? null,
      objectId: req.cf?.objectId ?? null,
      sawHeaders: SEEN(req),
    });
  }
}

// Plain-DO mode ("egress": "workload"): alarms work.
export class Ticker extends DurableObject {
  async fetch(req) {
    const url = new URL(req.url);
    if (url.pathname === "/arm") {
      await this.ctx.storage.setAlarm(Date.now() + 100);
      return Response.json({ armed: true });
    }
    if (url.pathname === "/egress") return egress(url.searchParams.get("url"));
    return Response.json({ kind: "ticker", name: this.ctx.id.name, alarms: (await this.ctx.storage.get("alarms")) ?? 0, hasTailnet: !!this.ctx.tailnet, caller: req.cf?.tailscale?.caller ?? null });
  }
  async alarm() {
    await this.ctx.storage.put("alarms", ((await this.ctx.storage.get("alarms")) ?? 0) + 1);
  }
}

export default {
  async fetch(req, env, ctx) {
    const url = new URL(req.url);
    if (url.pathname === "/egress") return egress(url.searchParams.get("url"));
    if (url.pathname.startsWith("/probe/")) {
      const [, , name, ...rest] = url.pathname.split("/");
      const u = new URL(req.url);
      u.pathname = "/" + rest.join("/");
      return env.PROBE.getByName(name).fetch(new Request(u, req));
    }
    if (url.pathname === "/rpc") return Response.json({ reply: await env.PROBE.getByName(url.searchParams.get("name")).hello("worker") });
    return Response.json({ kind: "worker", caller: getCaller(req), sawHeaders: SEEN(req), hasTailnet: !!ctx.tailnet, env: Object.keys(env).sort() });
  },
};
