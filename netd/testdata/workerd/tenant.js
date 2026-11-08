// TENANT CODE: an ordinary Durable Object. No platform imports; it reads the
// caller from request.cf and uses plain global fetch().
import { DurableObject } from "cloudflare:workers";

export class Agent extends DurableObject {
  async fetch(request) {
    const url = new URL(request.url);
    if (url.pathname === "/egress") {
      try {
        const r = await fetch(url.searchParams.get("url"));
        const h = { "content-type": r.headers.get("content-type") || "text/plain" };
        if (r.headers.get("twd-error")) h["twd-error"] = r.headers.get("twd-error");
        return new Response(await r.text(), { status: r.status, headers: h });
      } catch (e) {
        return Response.json({ tenantCaught: String(e) }, { status: 599 });
      }
    }
    if (url.pathname === "/forge-egress") {
      const victim = url.searchParams.get("as");
      const r = await fetch(url.searchParams.get("url"), {
        headers: { "x-twd-object": victim, "x-twd-cf": JSON.stringify({ objectId: victim }), "Proxy-Authorization": victim },
        cf: { objectId: victim },
      });
      return new Response(await r.text(), { status: r.status, headers: { "content-type": "application/json" } });
    }
    if (url.pathname === "/stream") {
      // Streams until the client goes away (kill test through the full chain).
      const enc = new TextEncoder();
      let n = 0;
      const stream = new ReadableStream({
        async pull(c) {
          await new Promise((r) => setTimeout(r, 50));
          c.enqueue(enc.encode("tick " + n++ + "\n"));
        },
      });
      return new Response(stream);
    }
    return Response.json({
      caller: request.cf?.tailscale?.caller ?? null,
      objectId: request.cf?.objectId ?? null,
      sawHeaders: Object.fromEntries([...request.headers].filter(([k]) => k.startsWith("x-twd") || k.startsWith("cf-") || k === "x-forwarded-for")),
    });
  }
}
