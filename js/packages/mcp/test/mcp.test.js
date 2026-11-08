import { test } from "node:test";
import assert from "node:assert/strict";
import { withTailnet, caller, authorizeTool, budget } from "../dist/index.js";
import { getCaller } from "@twd/tailnet";

const CAP = "acme.com/cap/mcp";
const mkReq = (caps) => {
  const r = new Request("http://payments/mcp", { method: "POST" });
  Object.defineProperty(r, "cf", { value: { objectId: "o", tailscale: { caller: { nodeId: "nA", hostname: "agent", tags: ["tag:agent"], capMap: { [CAP]: caps } } } } });
  return r;
};
const parse = (e) => JSON.parse(e.message);

test("union of grants; max for numeric limits; a grant without the limit is unbounded", () => {
  const who = getCaller(mkReq([
    { tools: ["lookup_order"] },
    { tools: ["refund"], limits: { refund: { maxUsd: 50, perDayUsd: 200 } } },
    { tools: ["refund"], limits: { refund: { maxUsd: 80 } } },
  ]));
  assert.equal(authorizeTool(who, CAP, { tool: "lookup_order" }).tool, "lookup_order");
  const a = authorizeTool(who, CAP, { tool: "refund", usd: 75 });
  assert.equal(a.limits.maxUsd, 80);
  assert.equal(a.limits.perDayUsd, Infinity); // the second refund grant has no perDayUsd
  assert.throws(() => authorizeTool(who, CAP, { tool: "refund", usd: 81 }), (e) => {
    const b = parse(e);
    return e.code === "cap_limit_exceeded" && b.code === "cap_limit_exceeded" && b.limit.maxUsd === 80;
  });
  const who2 = getCaller(mkReq([{ tools: ["refund"], limits: { refund: { maxUsd: 50 } } }, { tools: ["refund"] }]));
  assert.equal(authorizeTool(who2, CAP, { tool: "refund", usd: 1e6 }).limits.maxUsd, Infinity);
});

test("cap_missing is a typed JSON error with what the caller has", () => {
  const who = getCaller(mkReq([{ tools: ["lookup_order"] }]));
  assert.throws(() => authorizeTool(who, CAP, { tool: "refund", usd: 1 }), (e) => {
    const b = parse(e);
    return b.code === "cap_missing" && b.missing.tool === "refund" && b.have.tools[0] === "lookup_order" && b.doc_url.endsWith("#cap_missing") && b.snippet.includes("refund");
  });
  const none = getCaller(mkReq(undefined));
  assert.throws(() => authorizeTool(none, CAP, { tool: "x" }), (e) => parse(e).code === "cap_missing");
  const grouped = getCaller(mkReq([{ tools: ["refund"], groups: { refund: ["group:support"] } }]));
  assert.throws(() => authorizeTool(grouped, CAP, { tool: "refund" }), (e) => /group:support/.test(parse(e).message));
});

test("withTailnet exposes caller() per request and rejects unverified requests", async () => {
  const h = withTailnet(async () => new Response(caller().nodeId), { path: "/mcp" });
  assert.equal(await (await h.fetch(mkReq([]), {}, {})).text(), "nA");
  const bad = await h.fetch(new Request("http://x/mcp"), {}, {});
  assert.equal(bad.status, 401);
  assert.equal(bad.headers.get("twd-error"), "no_identity");
  assert.equal((await h.fetch(new Request("http://x/other"), {}, {})).status, 404);
  assert.throws(() => caller(), (e) => e.code === "no_identity");
});

test("budget reserves against the perDay limit from authorizeTool", async () => {
  const used = new Map();
  const ns = { getByName: (n) => ({ async reserve(a, limit, day) { const k = n + day; const u = used.get(k) ?? 0; if (limit !== null && u + a > limit) return { ok: false, used: u, limit }; used.set(k, u + a); return { ok: true, used: u + a, limit }; }, async used() { return 0; } }) };
  const h = withTailnet(async () => {
    const who = caller();
    authorizeTool(who, CAP, { tool: "refund", usd: 40 });
    const b = budget(ns, who, "refund");
    await b.reserve(40);
    await b.reserve(40);
    try { await b.reserve(40); return new Response("no"); } catch (e) { return new Response(e.message); }
  });
  const r = await h.fetch(mkReq([{ tools: ["refund"], limits: { refund: { maxUsd: 50, perDayUsd: 100 } } }]), {}, {});
  const body = JSON.parse(await r.text());
  assert.equal(body.code, "cap_limit_exceeded");
  assert.equal(body.used, 80);
});

test("mcpFetch routes via ctx.tailnet.fetch with traceparent; typed errors pass through", async () => {
  const { mcpFetch } = await import("../dist/index.js");
  const { TailnetError } = await import("@twd/tailnet");
  const seen = [];
  const ctx = { tailnet: { async fetch(url, init) { seen.push({ url, init }); if (url.includes("denied")) throw new TailnetError("peer_not_visible", "nope"); return new Response("{}"); } } };
  const f = mcpFetch(ctx, { traceId: "0123456789abcdef0123456789abcdef", headers: { "x-a": "1" } });
  const r = await f("http://mcp/mcp", { method: "POST", body: '{"jsonrpc":"2.0"}', headers: { "content-type": "application/json" } });
  assert.equal(r.status, 200);
  assert.equal(seen[0].url, "http://mcp/mcp");
  assert.equal(seen[0].init.method, "POST");
  assert.equal(new TextDecoder().decode(seen[0].init.body), '{"jsonrpc":"2.0"}');
  assert.match(seen[0].init.headers.get("traceparent"), /^00-0123456789abcdef0123456789abcdef-[0-9a-f]{16}-01$/);
  assert.equal(seen[0].init.headers.get("x-a"), "1");
  await assert.rejects(f("http://denied/mcp"), (e) => e.code === "peer_not_visible");
  // redirect: "error" (the @ai-sdk/mcp default) is emulated: workerd rejects it.
  const g = mcpFetch({ tailnet: { async fetch(url, init) { seen.push({ url, init }); return new Response(null, { status: 302, headers: { location: "http://x/" } }); } } });
  await assert.rejects(g("http://mcp/mcp", { redirect: "error" }), /redirect/);
  assert.equal(seen.at(-1).init.redirect, "manual");
  assert.throws(() => mcpFetch({}), (e) => e.code === "no_identity");
});
