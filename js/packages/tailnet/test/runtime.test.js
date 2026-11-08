import { test } from "node:test";
import assert from "node:assert/strict";
import { TailnetError, isTailnetError, fromResponse, CATALOG, tailnet } from "../dist/index.js";
import { createTailnetContext, wrapHandler, wrapFacetClass } from "../dist/runtime.js";

test("TailnetError wire shape and catalog statuses", async () => {
  const e = new TailnetError("cap_missing", "tool refund not granted", { missing: { cap: "x", tool: "refund" }, snippet: "{}" });
  assert.equal(e.status, 403);
  assert.equal(e.docUrl.endsWith("#cap_missing"), true);
  assert.deepEqual(e.toJSON().missing, { cap: "x", tool: "refund" });
  const r = e.toResponse();
  assert.equal(r.status, 403);
  assert.equal(r.headers.get("twd-error"), "cap_missing");
  const back = await fromResponse(r);
  assert.equal(back.code, "cap_missing");
  assert.equal(back.detail, "tool refund not granted");
  assert.ok(isTailnetError(back));
  assert.equal(await fromResponse(new Response("ok")), null);
  for (const [code, ent] of Object.entries(CATALOG)) assert.ok(ent.status >= 400 && ent.title, code);
  assert.equal(isTailnetError({ name: "TailnetError", code: "x" }), true);
});

test("tailnet(ctx) throws no_identity off-platform", () => {
  assert.throws(() => tailnet({}), (e) => e.code === "no_identity");
});

function fakeTw(log) {
  return {
    async call(op, args) {
      log.push([op, args]);
      if (op === "spawn") return { ok: true, value: { objectID: "o1", status: "pending", hostname: "app-lookup-x" } };
      if (op === "ready") return { ok: true, value: { magicDNS: "app-lookup-x.ts.net" } };
      if (op === "kill") return { ok: false, error: { code: "killed", message: "nope" } };
      return { ok: true, value: {} };
    },
  };
}

test("ctx.tailnet.spawn resolves the binding's class and passes self", async () => {
  const log = [];
  const ns = { getByName: (n) => ({ name: n }) };
  const env = { LOOKUP: ns };
  const tn = createTailnetContext(fakeTw(log), env, { LOOKUP: "LookupAgent" }, { class: "Plain", name: "p1" });
  const sp = await tn.spawn(env.LOOKUP, { name: "x", caps: { a: 1 } });
  assert.equal(sp.objectId, "o1");
  assert.deepEqual(sp.stub, { name: "x" });
  assert.equal(sp.state(), "pending");
  await sp.ready;
  assert.equal(sp.state(), "ready");
  assert.equal(sp.name, "app-lookup-x.ts.net");
  assert.deepEqual(log[0], ["spawn", { class: "LookupAgent", name: "x", caps: { a: 1 }, ttl: undefined, principal: undefined, as: { class: "Plain", name: "p1" } }]);
  await assert.rejects(tn.spawn({}, { name: "y" }), (e) => e.code === "bad_request");
  await assert.rejects(tn.kill(), (e) => isTailnetError(e) && e.code === "killed");
});

test("ctx.tailnet.fetch throws typed errors for twd-error responses", async () => {
  const orig = globalThis.fetch;
  globalThis.fetch = async () => new Response(JSON.stringify({ code: "peer_not_visible", detail: "no peer named db" }), { status: 403, headers: { "twd-error": "peer_not_visible" } });
  try {
    const tn = createTailnetContext(fakeTw([]), {}, {});
    await assert.rejects(tn.fetch("http://db/"), (e) => e.code === "peer_not_visible" && e.status === 403 && /no peer/.test(e.message));
  } finally {
    globalThis.fetch = orig;
  }
});

test("wrapHandler installs ctx.tailnet; wrapFacetClass hides __TWD", async () => {
  const H = wrapHandler({ async fetch(req, env, ctx) { return typeof ctx.tailnet.spawn; } }, {});
  assert.equal(await H.fetch({}, { __TWD: fakeTw([]) }, {}), "function");
  class Base { constructor(ctx, env) { this.ctx = ctx; this.env = env; } }
  const W = wrapFacetClass(Base, "Counter", {});
  assert.equal(W.name, "Counter");
  const w = new W({}, { __TWD: fakeTw([]), X: 1 });
  assert.equal(w.env.__TWD, undefined);
  assert.equal(w.env.X, 1);
  assert.equal(typeof w.ctx.tailnet.fetch, "function");
});
