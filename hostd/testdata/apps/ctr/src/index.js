// ctx.container test app: every op of Cloudflare's container API, driven
// over HTTP (/<f|p>/<object>/<op>?...). Box is a facet-mode class, PBox a
// plain-DO one; Plain has no container.
import { DurableObject } from "cloudflare:workers";

const dec = new TextDecoder();
const J = (v, status = 200) => Response.json(v, { status });

async function handle(self, req) {
  const u = new URL(req.url);
  const op = u.pathname.split("/").pop();
  const q = u.searchParams;
  const c = self.ctx.container;
  const arg = (k, d) => (q.has(k) ? JSON.parse(q.get(k)) : d);
  switch (op) {
    case "has":
      return J({ has: c !== undefined, images: c?.images ?? null });
    case "running":
      return J({ running: c.running });
    case "start": {
      const opts = { env: { GREETING: "hi" } };
      if (q.has("entrypoint")) opts.entrypoint = arg("entrypoint");
      if (q.has("internet")) opts.enableInternet = q.get("internet") === "1";
      if (q.has("image")) opts.image = q.get("image");
      c.start(opts);
      // start() returns at once; running is true right away.
      return J({ running: c.running });
    }
    case "startTwice": {
      c.start();
      try {
        c.start();
        return J({ second: "ok" });
      } catch (e) {
        return J({ second: String(e.message) });
      }
    }
    case "exec": {
      const opts = {};
      for (const k of ["cwd", "stderr", "stdout", "user"]) if (q.has(k)) opts[k] = q.get(k);
      if (q.has("env")) opts.env = arg("env");
      const p = await c.exec(arg("argv"), opts);
      const o = await p.output();
      return J({ pid: p.pid, isPty: p.isPty, exitCode: o.exitCode, stdout: dec.decode(o.stdout), stderr: dec.decode(o.stderr), hasStderr: p.stderr !== null });
    }
    case "stdin": {
      const p = await c.exec(["cat"], { stdin: "pipe" });
      const w = p.stdin.getWriter();
      const body = await req.text();
      for (let i = 0; i < body.length; i += 1000) await w.write(new TextEncoder().encode(body.slice(i, i + 1000)));
      await w.close();
      const o = await p.output();
      return J({ exitCode: o.exitCode, stdout: dec.decode(o.stdout) });
    }
    case "stream": {
      // The first line must arrive while the process still runs.
      const p = await c.exec(["sh", "-c", "echo first; sleep 1; echo second; exit 7"]);
      const rd = p.stdout.getReader();
      const t0 = Date.now();
      const first = await rd.read();
      const tFirst = Date.now() - t0;
      let rest = "";
      for (;;) {
        const { done, value } = await rd.read();
        if (done) break;
        rest += dec.decode(value);
      }
      return J({ first: dec.decode(first.value), tFirst, rest, code: await p.exitCode, tTotal: Date.now() - t0 });
    }
    case "kill": {
      const p = await c.exec(["sleep", "30"]);
      const t0 = Date.now();
      p.kill(9);
      return J({ code: await p.exitCode, ms: Date.now() - t0 });
    }
    case "abort": {
      const ac = new AbortController();
      const p = await c.exec(["sleep", "30"], { signal: ac.signal });
      ac.abort();
      return J({ code: await p.exitCode });
    }
    case "monitor": {
      const t0 = Date.now();
      try {
        await c.monitor();
        return J({ resolved: true, ms: Date.now() - t0, running: c.running });
      } catch (e) {
        return J({ rejected: String(e.message), exitCode: e.exitCode, ms: Date.now() - t0, running: c.running });
      }
    }
    case "signal":
      c.signal(Number(q.get("n") ?? 15));
      return J({ ok: true });
    case "destroy":
      await c.destroy();
      return J({ running: c.running });
    case "idle":
      await c.setInactivityTimeout(Number(q.get("ms")));
      return J({ ok: true });
    case "port": {
      try {
        const r = await c.getTcpPort(Number(q.get("p"))).fetch("http://container/hello?x=1", { headers: { "x-twd-as": "forged", "x-probe": "1" } });
        return J({ status: r.status, body: await r.text() });
      } catch (e) {
        return J({ error: String(e.message) });
      }
    }
    case "portsig": {
      // @cloudflare/containers' health check: a fetch with an AbortSignal.
      const t0 = Date.now();
      try {
        const r = await c.getTcpPort(Number(q.get("p"))).fetch("http://containerstarthealthcheck", { signal: AbortSignal.timeout(3000) });
        return J({ status: r.status, ms: Date.now() - t0 });
      } catch (e) {
        return J({ error: String(e.message), ms: Date.now() - t0 });
      }
    }
    case "inspect":
      return J({ info: await c.inspect() });
    case "intercept":
      await c.interceptOutboundHttp("api.example.com", { fetch: () => new Response("x") });
      return J({ ok: true });
    case "icept": {
      // The credential-proxy pattern: the container's request reaches this
      // handler, which relays to ?relay= under the object's own identity.
      const relay = q.get("relay");
      const name = self.ctx.id.name;
      const handler = {
        async fetch(r) {
          const body = r.method === "GET" || r.method === "HEAD" ? "" : await r.text();
          let echo = null;
          if (relay) {
            const e = await fetch(relay);
            echo = await e.json();
          }
          const seen = {};
          for (const [k, v] of r.headers) if (k.startsWith("x-twd") || k.startsWith("twd-") || k === "x-from-guest") seen[k] = v;
          return J({ fromParent: name, url: r.url, method: r.method, body, headers: seen, echo });
        },
      };
      const kind = q.get("kind") ?? "http";
      if (kind === "all") await c.interceptAllOutboundHttp(handler);
      else if (kind === "https") await c.interceptOutboundHttps(q.get("addr"), handler);
      else await c.interceptOutboundHttp(q.get("addr"), handler);
      return J({ ok: true });
    }
    case "iceptMany": {
      let n = 0;
      try {
        for (let i = 0; i < 200; i++) {
          await c.interceptOutboundHttp("h" + i + ".example.com", { fetch: () => new Response("x") });
          n++;
        }
      } catch (e) {
        return J({ n, error: String(e.message) });
      }
      return J({ n });
    }
    case "expose": {
      // Sandbox SDK 0.x's preview URLs (exposePort; see Base below).
      try {
        const r = await self.exposePort(8080, { hostname: "example.com" });
        return J({ url: r.url });
      } catch (e) {
        return J({ error: String(e.message), code: e.code ?? null });
      }
    }
    case "selfkill":
      await self.ctx.tailnet.kill({ reason: "test kill" });
      return J({ ok: true });
    case "forge": {
      // A plain-DO object reaching for a facet object's container through
      // the raw platform binding (the workload's trust domain may claim
      // plain objects, never facet ones).
      const tw = self.env.__TWD;
      if (!tw) return J({ noBinding: true });
      const r = await tw.ctr(q.get("op") ?? "destroy", { as: { class: q.get("class") ?? "Box", name: q.get("target") } });
      return J(r);
    }
    case "envtwd":
      return J({ hasTWD: "__TWD" in self.env });
  }
  return J({ error: "unknown op " + op }, 404);
}

class Base extends DurableObject {
  // What the Sandbox SDK 0.x's exposePort does: mint a preview URL (twd
  // must refuse it: no public ingress, ADR-029).
  async exposePort(port, opts) {
    return { url: "https://" + port + "-" + this.ctx.id.name + "-tok." + opts.hostname };
  }
  async fetch(req) {
    try {
      return await handle(this, req);
    } catch (e) {
      return J({ error: String(e?.message ?? e), code: e?.code ?? null }, 500);
    }
  }
}

export class Box extends Base {}
export class PBox extends Base {}
export class Plain extends Base {}

export default {
  fetch(req, env) {
    const u = new URL(req.url);
    const [, kind, name] = u.pathname.split("/");
    const ns = kind === "p" ? env.PBOX : kind === "n" ? env.PLAIN : env.BOX;
    return ns.getByName(name).fetch(req);
  },
};
