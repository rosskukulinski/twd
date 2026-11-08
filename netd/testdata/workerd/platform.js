// PLATFORM CODE (trusted). Ported from the per-object-identity prototype; M3
// replaces it with the generated wrapper. Tenant code never sees these bindings.
import { DurableObject, WorkerEntrypoint } from "cloudflare:workers";
import TENANT_SRC from "tenant.js.txt";

// Entry: netd has stripped client identity headers and set cf (cfBlobHeader) + x-twd-object.
export default {
  async fetch(request, env, ctx) {
    const object = request.cf?.objectId;
    if (!object) return new Response("no objectId in cf (not from netd?)", { status: 400 });
    if (request.headers.get("x-twd-object") !== object) return new Response("routing header mismatch", { status: 400 });
    return ctx.exports.HostDO.get(ctx.exports.HostDO.idFromName(object)).fetch(request);
  },
};

// One HostDO per object. The tenant class runs as a facet in a Dynamic Worker whose
// only network path (globalOutbound) is Egress specialised with this object's ID.
export class HostDO extends DurableObject {
  async fetch(request) {
    const object = request.cf.objectId;
    if (!this.ctx.exports.HostDO.idFromName(object).equals(this.ctx.id)) return new Response("id/name mismatch", { status: 500 });
    if (!this.facet) {
      const worker = this.env.LOADER.get("tenant:" + object, () => ({
        compatibilityDate: "2026-04-01",
        mainModule: "tenant.js",
        modules: { "tenant.js": TENANT_SRC },
        globalOutbound: this.ctx.exports.Egress({ props: { object } }),
        env: {},
      }));
      this.facet = this.ctx.facets.get("app", () => ({ class: worker.getDurableObjectClass("Agent") }));
    }
    return this.facet.fetch(request);
  }
}

// Egress for tenant fetch(). props.object was minted by HostDO; tenant code cannot change it.
export class Egress extends WorkerEntrypoint {
  async fetch(request) {
    const h = new Headers(request.headers);
    for (const k of [...h.keys()]) if (k.startsWith("x-twd") || k === "proxy-authorization") h.delete(k);
    h.set("x-twd-object", this.ctx.props.object);
    // workerd's proxy-style external sends absolute-form without Host; Go's net/http rejects that.
    h.set("host", new URL(request.url).host);
    return this.env.NETD_EGRESS.fetch(new Request(request.url, { method: request.method, headers: h, body: request.body, redirect: "manual" }));
  }
}
