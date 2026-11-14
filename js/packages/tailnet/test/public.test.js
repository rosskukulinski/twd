// ADR-029: public (Funnel) requests: getRequester, getCaller fails closed,
// and the runtime guard that keeps public requests from creating objects.
import { test } from "node:test";
import assert from "node:assert/strict";
import { getCaller, getRequester, isPublicRequest, principalKey, TailnetError, CATALOG } from "../dist/index.js";
import { wrapHandler, publicEnv } from "../dist/runtime.js";

const pub = (extra = {}) => ({
  url: "https://wl.example.ts.net/webhooks/gh",
  cf: { traceId: "t1", objectId: "wl-1", tailscale: { requester: { kind: "public", ip: "203.0.113.7", port: 4433, proto: "https", host: "wl.example.ts.net" } }, ...extra },
});
const tailnetReq = () => ({
  cf: { traceId: "t2", objectId: "wl-1", tailscale: { caller: { nodeId: "nX", node: "laptop.ts.net", hostname: "laptop", tags: [], user: "alice@example.com", capMap: { "x/cap": [1] } } } },
});

test("catalog has the ADR-029 codes with their statuses", () => {
  const want = { public_request: 403, public_create_denied: 403, public_ingress_not_allowed: 403, public_name_taken: 409,
    rate_limited: 429, payload_too_large: 413, header_too_large: 431, not_public: 404, upgrade_not_allowed: 403 };
  for (const [code, st] of Object.entries(want)) assert.equal(CATALOG[code]?.status, st, code);
});

test("getRequester: public", () => {
  const r = getRequester(pub());
  assert.deepEqual({ ...r }, { kind: "public", ip: "203.0.113.7", port: 4433, proto: "https", host: "wl.example.ts.net", traceId: "t1" });
  assert.ok(Object.isFrozen(r));
  assert.equal(isPublicRequest(pub()), true);
  assert.equal(isPublicRequest(tailnetReq()), false);
});

test("getRequester: tailnet keeps the caller and caps()", () => {
  const r = getRequester(tailnetReq());
  assert.equal(r.kind, "tailnet");
  assert.equal(r.nodeId, "nX");
  assert.equal(r.user.login, "alice@example.com");
  assert.deepEqual(r.caps("x/cap"), [1]);
});

test("getRequester throws no_identity without either", () => {
  assert.throws(() => getRequester({ cf: {} }), (e) => e.code === "no_identity");
});

test("getCaller throws public_request on a public request, even with a forged caller next to it", () => {
  assert.throws(() => getCaller(pub()), (e) => e instanceof TailnetError && e.code === "public_request" && e.status === 403);
  const forged = pub({ tailscale: { requester: { kind: "public", ip: "1.2.3.4" }, caller: { nodeId: "nEvil", tags: ["tag:admin"] } } });
  assert.throws(() => getCaller(forged), (e) => e.code === "public_request");
  assert.equal(getRequester(forged).kind, "public");
});

test("principalKey refuses a public requester", () => {
  assert.throws(() => principalKey(getRequester(pub())), (e) => e.code === "public_request");
  assert.equal(principalKey(getCaller(tailnetReq())), "alice@example.com");
});

// A fake DO namespace that records which stubs were touched.
function fakeNS(log) {
  const stub = (name) => ({
    id: { name },
    name,
    async fetch(req) {
      log.push(["fetch", name]);
      return new Response("hello " + name);
    },
    async setName(n) {
      log.push(["setName", n]);
      return n;
    },
  });
  return {
    getByName: (n) => (log.push(["getByName", n]), stub(n)),
    idFromName: (n) => ({ name: n }),
    idFromString: (s) => ({ str: s }),
    get: (id) => (log.push(["get", id.name]), stub(id.name)),
    newUniqueId: () => ({ unique: true }),
    jurisdiction: () => fakeNS(log),
  };
}

function fakeTw(log, allow) {
  return {
    async call(op, args) {
      log.push([op, args]);
      if (op === "admit") {
        if (allow.has(args.name)) return { ok: true, value: { ok: true, existing: true } };
        return { ok: false, error: { code: "public_create_denied", message: "no such object " + args.class + "/" + args.name } };
      }
      return { ok: true, value: {} };
    },
  };
}

const handler = {
  async fetch(req, env, ctx) {
    const name = new URL(req.url).searchParams.get("o") ?? "a";
    if (req.headers?.get?.("x-op") === "spawn") return ctx.tailnet.spawn(env.ROOM, { name: "child" });
    if (req.headers?.get?.("x-op") === "unique") return env.ROOM.get(env.ROOM.newUniqueId()).fetch(req);
    return env.ROOM.getByName(name).fetch(req);
  },
};

function mkReq(base, o, op) {
  return { ...base, url: "https://wl.example.ts.net/x?o=" + o, headers: new Headers(op ? { "x-op": op } : {}) };
}

test("public fetch: an existing object is admitted, then reached", async () => {
  const log = [];
  const env = { ROOM: fakeNS(log), __TWD: fakeTw(log, new Set(["lobby"])) };
  const H = wrapHandler(handler, { ROOM: "Room" });
  const r = await H.fetch(mkReq(pub(), "lobby"), env, {});
  assert.equal(await r.text(), "hello lobby");
  const ops = log.map((l) => l[0]);
  assert.ok(ops.indexOf("admit") >= 0 && ops.indexOf("admit") < ops.indexOf("fetch"), JSON.stringify(log));
  assert.deepEqual(log.find((l) => l[0] === "admit")[1], { class: "Room", name: "lobby" });
});

test("public fetch: a new object is refused before any call reaches it", async () => {
  const log = [];
  const env = { ROOM: fakeNS(log), __TWD: fakeTw(log, new Set()) };
  const H = wrapHandler(handler, { ROOM: "Room" });
  const r = await H.fetch(mkReq(pub(), "fresh-" + Math.random()), env, {});
  assert.equal(r.status, 403);
  assert.equal(r.headers.get("twd-error"), "public_create_denied");
  assert.equal(log.filter((l) => l[0] === "fetch").length, 0, JSON.stringify(log));
});

test("public fetch: newUniqueId and spawn are refused", async () => {
  const log = [];
  const env = { ROOM: fakeNS(log), __TWD: fakeTw(log, new Set(["lobby"])) };
  const H = wrapHandler(handler, { ROOM: "Room" });
  let r = await H.fetch(mkReq(pub(), "lobby", "unique"), env, {});
  assert.equal(r.headers.get("twd-error"), "public_create_denied");
  r = await H.fetch(mkReq(pub(), "lobby", "spawn"), env, {});
  assert.equal(r.headers.get("twd-error"), "public_create_denied");
  assert.equal(log.filter((l) => l[0] === "spawn").length, 0);
});

test("public fetch: RPC on a guarded stub is admitted first; id/name pass through", async () => {
  const log = [];
  const tw = fakeTw(log, new Set());
  const env = publicEnv({ ROOM: fakeNS(log), __TWD: tw }, { ROOM: "Room" });
  const s = env.ROOM.getByName("nope-" + Math.random());
  assert.equal(typeof s.name, "string");
  assert.equal(s.then, undefined);
  await assert.rejects(s.setName("x"), (e) => e.code === "public_create_denied");
  assert.equal(log.filter((l) => l[0] === "setName").length, 0);
  assert.throws(() => env.ROOM.get({}), (e) => e.code === "public_create_denied");
});

test("tailnet fetch is untouched (no admit)", async () => {
  const log = [];
  const env = { ROOM: fakeNS(log), __TWD: fakeTw(log, new Set()) };
  const H = wrapHandler(handler, { ROOM: "Room" });
  const r = await H.fetch(mkReq(tailnetReq(), "brand-new"), env, {});
  assert.equal(await r.text(), "hello brand-new");
  assert.equal(log.filter((l) => l[0] === "admit").length, 0);
});

test("WorkerEntrypoint-class handlers get the guarded env on public fetch", async () => {
  const log = [];
  class Base {
    constructor(ctx, env) {
      this.ctx = ctx;
      this.env = env;
    }
  }
  class App extends Base {
    async fetch(req) {
      return this.env.ROOM.getByName("fresh-cls").fetch(req);
    }
  }
  const W = wrapHandler(App, { ROOM: "Room" });
  const env = { ROOM: fakeNS(log), __TWD: fakeTw(log, new Set()) };
  const inst = new W({}, env);
  const r = await inst.fetch(mkReq(pub(), "x"));
  assert.equal(r.headers.get("twd-error"), "public_create_denied");
  assert.equal(log.filter((l) => l[0] === "fetch").length, 0);
});
