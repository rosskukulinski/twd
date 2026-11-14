// twd platform wrapper (TRUSTED). Generated per deployment by twd-hostd from
// this template plus twd-config.json / twd-modules.js. Tenant code never
// runs in this isolate and never sees these bindings:
//   LOADER       Dynamic Worker loader (facets)
//   NETD_EGRESS  hostd's attributed egress socket (trusts x-twd-class/name + MAC)
//   EGRESS_WL    hostd's workload egress socket (the tenant service's identity)
//   HOSTD        hostd's local host API socket (ensure/spawn/ready/kill/audit)
//   MAC_KEY      per-process root key; per-object egress MAC keys are derived from it (RT-04)
//   TENANT       the tenant service (the app's default export)
//   NS_<Class>   plain-DO namespaces (classes with "egress": "workload")
//
// Routing comes from request.cf.twd, which only hostd can set (ingress
// socket cfBlobHeader; netd strips client copies).
import { DurableObject, WorkerEntrypoint } from "cloudflare:workers";
import CFG from "twd-config.json";
import MODULES from "twd-modules.js";

const FACET_MODULES = MODULES;

const json = (status, body, extra = {}) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json", "cache-control": "no-store", ...extra } });

const DOC = "https://github.com/rosskukulinski/twd/blob/main/docs/errors.md#";
const STATUS = { killed: 410, no_identity: 401, bad_request: 400, unknown_object: 404, identity_pending: 503, hostd_unavailable: 503 };
function errBody(code, message, extra = {}) {
  return { code, message, doc_url: DOC + code, ...extra };
}
function errResponse(code, message, extra = {}) {
  return json(STATUS[code] ?? 403, errBody(code, message, extra), { "twd-error": code });
}

// principalOf names who a request acts for (audit "actingFor"): the human
// login for an untagged caller, else the calling node's name.
function principalOf(req) {
  const c = req?.cf?.tailscale?.caller;
  if (!c) return "";
  const user = typeof c.user === "string" ? c.user : c.user?.login;
  return user || String(c.node || c.hostname || "").replace(/\.$/, "");
}

// hostd local API. Errors come back as typed bodies.
async function hostd(env, op, body) {
  let r;
  try {
    r = await env.HOSTD.fetch("http://hostd/v1/" + op, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body ?? {}),
    });
  } catch (e) {
    throw Object.assign(new Error("hostd unavailable"), { body: errBody("hostd_unavailable", String(e)) });
  }
  let j;
  try {
    j = await r.json();
  } catch {
    j = errBody("hostd_unavailable", "bad hostd response (" + r.status + ")");
  }
  if (!r.ok) throw Object.assign(new Error(j.message ?? j.code), { body: j });
  return j;
}

const bindingMap = Object.fromEntries(CFG.bindings.map((b) => [b.name, b.class]));

function isFacet(cls) {
  return CFG.classes[cls]?.mode === "facet";
}

function objectStub(env, ctx, cls, name) {
  if (isFacet(cls)) return ctx.exports["Host_" + cls].getByName(name);
  return env["NS_" + cls].getByName(name);
}

async function routeObject(env, ctx, cls, name, req) {
  if (!CFG.classes[cls]) return errResponse("unknown_object", "no Durable Object class " + cls + " in this app");
  if (!name) return errResponse("bad_request", "object name required");
  return objectStub(env, ctx, cls, name).fetch(req);
}

async function admin(req, env, ctx, t) {
  switch (t.op) {
    case "health":
      return json(200, { ok: true, deploymentID: CFG.deploymentID, workload: CFG.workload });
    case "kill": {
      try {
        await objectStub(env, ctx, t.class, t.name).__twd_kill(t.reason || "killed");
      } catch (e) {
        return json(500, errBody("bad_request", "kill: " + String(e)));
      }
      return json(200, { ok: true });
    }
  }
  return errResponse("bad_request", "unknown admin op");
}

export default {
  async fetch(req, env, ctx) {
    const t = req.cf?.twd;
    if (!t || typeof t !== "object") return errResponse("no_identity", "request did not come through netd/hostd");
    switch (t.kind) {
      case "admin":
        return admin(req, env, ctx, t);
      case "object":
        return routeObject(env, ctx, t.class, t.name, req);
      case "intercept": {
        // A request an object's container sent to an intercepted
        // destination (hostd's sandbox egress proxy, ADR-032 D4): only the
        // object's own registered handler sees it, in the object.
        if (!CFG.classes[t.class]?.container) return errResponse("unknown_object", "no container class " + t.class + " in this app");
        if (!t.name || typeof t.key !== "string" || typeof t.url !== "string") return errResponse("bad_request", "bad intercept route");
        const hasBody = req.method !== "GET" && req.method !== "HEAD";
        const r = new Request(t.url, { method: req.method, headers: req.headers, body: hasBody ? req.body : undefined, redirect: "manual" });
        return objectStub(env, ctx, t.class, t.name).__twd_intercept(t.key, r);
      }
      case "workload": {
        // /_twd/o/<Class>/<name>/... on the workload node reaches an object
        // before (or instead of) its own node.
        const u = new URL(req.url);
        const m = u.pathname.match(/^\/_twd\/o\/([A-Za-z_$][\w$]*)\/([^/]+)(\/.*)?$/);
        if (m) {
          u.pathname = m[3] ?? "/";
          return routeObject(env, ctx, m[1], decodeURIComponent(m[2]), new Request(u, req));
        }
        return env.TENANT.fetch(req);
      }
    }
    return errResponse("bad_request", "unknown route kind");
  },
};

// Host_<Class>: one platform DO per tenant object (facet mode). The tenant
// class runs as a facet in a Dynamic Worker whose ONLY network path
// (globalOutbound) is Egress specialised with this object's class+name.
// Any RPC method is forwarded to the facet via a Proxy.
function makeHost(cls) {
  const H = class extends DurableObject {
    constructor(ctx, env) {
      super(ctx, env);
      this.__f = null;
      this.__ensured = null;
      this.__actingFor = ""; // own property: the Proxy would answer unknown names with an RPC forwarder
      this.__killed = ctx.storage.kv.get("__twd_killed") ?? null;
      const self = this;
      return new Proxy(this, {
        get(t, p, r) {
          if (typeof p === "symbol" || p in t) return Reflect.get(t, p, r);
          if (p === "then") return undefined;
          return async (...args) => {
            if (self.__killed) throw new Error("[killed] object " + cls + "/" + self.ctx.id.name + " was killed: " + self.__killed);
            const f = self.__facet(true);
            return f[p](...args);
          };
        },
      });
    }
    __ctxEnsure() {
      const p = this.__ensure().catch(() => {});
      try {
        this.ctx.waitUntil(p);
      } catch {
        /* ignore */
      }
    }
    __ensure() {
      if (!this.__ensured) {
        this.__ensured = hostd(this.env, "ensure", { as: { class: cls, name: this.ctx.id.name }, wake: true, actingFor: this.__actingFor || undefined }).catch((e) => {
          this.__ensured = null;
          throw e;
        });
      }
      return this.__ensured;
    }
    // __facet(rpc): an RPC call (e.g. the Agents SDK's setName from
    // getAgentByName) usually precedes the first fetch, which knows the
    // caller; give it a moment so the object is created acting for that
    // caller (audit), then ensure anyway.
    __facet(rpc) {
      if (!this.__f) {
        const name = this.ctx.id.name;
        if (!name) throw new Error("[bad_request] twd objects must be addressed by name (getByName/idFromName); newUniqueId() is not supported yet");
        const p = rpc
          ? new Promise((res) => setTimeout(res, 250)).then(() => this.__ensure()).catch(() => {})
          : this.__ensure().catch(() => {});
        try {
          this.ctx.waitUntil(p);
        } catch {
          /* ignore */
        }
        const exp = this.ctx.exports;
        const w = this.env.LOADER.get(CFG.deploymentID + ":" + cls + ":" + name, () => {
          const env = { ...CFG.vars };
          for (const b of CFG.bindings) env[b.name] = exp.NsProxy({ props: { class: b.class } });
          env.__TWD = exp.TailnetApi({ props: { kind: "object", class: cls, name } });
          return {
            compatibilityDate: CFG.compatDate,
            compatibilityFlags: CFG.compatFlags,
            mainModule: "__twd_facet.js",
            modules: FACET_MODULES,
            globalOutbound: exp.Egress({ props: { class: cls, name } }),
            env,
          };
        });
        this.__f = this.ctx.facets.get("app", () => ({ class: w.getDurableObjectClass(cls) }));
      }
      return this.__f;
    }
    async fetch(req) {
      if (this.__killed) return errResponse("killed", "object " + cls + "/" + this.ctx.id.name + " was killed: " + this.__killed);
      // Internal fetches (e.g. partyserver's set-name) carry no caller.
      const who = principalOf(req);
      if (who && !this.__actingFor) {
        this.__actingFor = who;
        if (this.__ensured) {
          // Created before its first caller-bearing request (RPC first):
          // tell hostd whom it acts for (audit "principal").
          const p = hostd(this.env, "ensure", { as: { class: cls, name: this.ctx.id.name }, actingFor: who }).catch(() => {});
          try {
            this.ctx.waitUntil(p);
          } catch {
            /* ignore */
          }
        }
      }
      if (who && this.__f && !this.__ensured) this.__ctxEnsure();
      return this.__facet(!who).fetch(req);
    }
    // Facet alarm delegation (facets cannot set alarms in workerd): the
    // facet's ctx.storage.{set,get,delete}Alarm land here via TailnetApi
    // "alarm", and alarm() runs the facet's alarm handler.
    async __twd_alarm_op(action, time) {
      if (this.__killed) throw new Error("[killed] object " + cls + "/" + this.ctx.id.name + " was killed: " + this.__killed);
      if (action === "set") {
        await this.ctx.storage.setAlarm(time);
        return null;
      }
      if (action === "delete") {
        await this.ctx.storage.deleteAlarm();
        return null;
      }
      return await this.ctx.storage.getAlarm();
    }
    // A request the object's container sent to an intercepted destination.
    async __twd_intercept(key, req) {
      if (this.__killed) return errResponse("killed", "object " + cls + "/" + this.ctx.id.name + " was killed: " + this.__killed);
      return this.__facet(true).__twd_intercept(key, req);
    }
    async alarm(info) {
      if (this.__killed) return;
      // AlarmInvocationInfo is not serializable; pass its fields.
      await this.__facet(true).__twd_alarm({ retryCount: info?.retryCount ?? 0, isRetry: !!info?.isRetry });
    }
    async __twd_kill(reason) {
      this.__killed = reason || "killed";
      this.ctx.storage.kv.put("__twd_killed", this.__killed);
      try {
        await this.ctx.storage.deleteAlarm();
      } catch {
        /* ignore */
      }
      try {
        this.ctx.facets.abort("app", new Error("killed"));
      } catch {
        /* not running */
      }
      this.__f = null;
      return true;
    }
  };
  Object.defineProperty(H, "name", { value: "Host_" + cls });
  return H;
}

//@@HOST_CLASSES@@

// NsProxy stands in for a DO namespace binding inside a facet's env:
// env.X.getByName(name) returns the real stub (RPC promise pipelining makes
// env.X.getByName(n).method() work).
export class NsProxy extends WorkerEntrypoint {
  getByName(name) {
    return objectStub(this.env, this.ctx, this.ctx.props.class, String(name));
  }
}

const enc = new TextEncoder();
const hex = (u8) => [...u8].map((b) => b.toString(16).padStart(2, "0")).join("");
async function hmacKey(secret) {
  return crypto.subtle.importKey("raw", enc.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
}
let rootKey = null;
const objectKeys = new Map(); // "class\nname" -> CryptoKey
// objectKey derives this object's egress key from the instance root key
// (hostd gen.ObjectMACKey): hex(HMAC(root, "twd-egress-object-key-v1\n" +
// class + "\n" + name)). hostd verifies a (class, name) MAC only under that
// object's key.
async function objectKey(env, cls, name) {
  const k = cls + "\n" + name;
  let ck = objectKeys.get(k);
  if (!ck) {
    rootKey ??= await hmacKey(env.MAC_KEY);
    const derived = hex(new Uint8Array(await crypto.subtle.sign("HMAC", rootKey, enc.encode("twd-egress-object-key-v1\n" + k))));
    ck = await hmacKey(derived);
    objectKeys.set(k, ck);
  }
  return ck;
}
async function mac(env, cls, name, s) {
  const sig = new Uint8Array(await crypto.subtle.sign("HMAC", await objectKey(env, cls, name), enc.encode(s)));
  return hex(sig);
}

// Egress for a facet's fetch(). props were minted by Host_<Class>; tenant code
// cannot change them. hostd verifies the MAC, maps class+name to the object
// and its node, and netd dials from that node.
export class Egress extends WorkerEntrypoint {
  async fetch(request) {
    const { class: cls, name } = this.ctx.props;
    const h = new Headers(request.headers);
    for (const k of [...h.keys()]) if (k.startsWith("x-twd") || k.startsWith("twd-") || k === "proxy-authorization") h.delete(k);
    const ts = String(Date.now());
    const nonce = crypto.randomUUID();
    h.set("x-twd-class", cls);
    h.set("x-twd-name", encodeURIComponent(name));
    h.set("x-twd-ts", ts);
    h.set("x-twd-nonce", nonce);
    h.set("x-twd-mac", await mac(this.env, cls, name, [cls, name, request.method, request.url, ts, nonce].join("\n")));
    // workerd's proxy-style external sends absolute-form without Host; Go's net/http rejects that.
    h.set("host", new URL(request.url).host);
    return this.env.NETD_EGRESS.fetch(new Request(request.url, { method: request.method, headers: h, body: request.body, redirect: "manual" }));
  }
}

// WorkloadEgress is the tenant service's globalOutbound: the Worker and
// plain-DO objects egress as the workload node.
export class WorkloadEgress extends WorkerEntrypoint {
  async fetch(request) {
    const h = new Headers(request.headers);
    for (const k of [...h.keys()]) if (k.startsWith("x-twd") || k.startsWith("twd-") || k === "proxy-authorization") h.delete(k);
    h.set("host", new URL(request.url).host);
    return this.env.EGRESS_WL.fetch(new Request(request.url, { method: request.method, headers: h, body: request.body, redirect: "manual" }));
  }
}

// attestedAs is who a TailnetApi call acts for: the facet object named by
// props (unforgeable), or a plain-DO object the tenant service claims
// (args.as; never a facet object).
function attestedAs(props, args) {
  if (props.kind === "object") return { class: props.class, name: props.name };
  if (args?.as && CFG.classes[args.as.class] && !isFacet(args.as.class)) return { class: String(args.as.class), name: String(args.as.name) };
  return null;
}

const ctrErr = (message) => ({ ok: false, error: errBody("bad_request", message) });

// containerAs: the calling object, which must have a container (wrangler
// "containers"); container ops only ever act on that object's own one.
function containerAs(props, args) {
  const as = attestedAs(props, args);
  if (!as || !as.name) return null;
  if (!CFG.classes[as.class]?.container) return null;
  return as;
}

// TailnetApi backs ctx.tailnet and ctx.container. props: {kind:"object",
// class, name} for a facet (unforgeable), or {kind:"workload"} for the
// tenant service, which may act as a plain-DO object it hosts (args.as) but
// never as a facet object.
export class TailnetApi extends WorkerEntrypoint {
  // ctx.container ops (JSON): status, start, destroy, signal, monitor,
  // setInactivityTimeout, inspect, intercept, exec-signal, exec-resize.
  async ctr(op, args = {}) {
    const as = containerAs(this.ctx.props ?? {}, args);
    if (!as) return ctrErr("ctx.container is only for the calling object's own container");
    const { as: _ignored, ...rest } = args;
    try {
      return { ok: true, value: await hostd(this.env, "container", { ...rest, op: String(op), as }) };
    } catch (e) {
      return { ok: false, error: e.body ?? errBody("hostd_unavailable", String(e)) };
    }
  }
  // exec: a streamed Response (stdio frames), headers x-twd-exec(-pid).
  async ctrExec(args = {}) {
    const as = containerAs(this.ctx.props ?? {}, args);
    if (!as) return errResponse("bad_request", "ctx.container is only for the calling object's own container");
    return this.env.HOSTD.fetch("http://hostd/v1/container/exec", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ as, spec: args.spec ?? {} }),
    });
  }
  // stdin: body streams into the exec's stdin, then it is half-closed.
  async ctrStdin(args = {}, body) {
    const as = containerAs(this.ctx.props ?? {}, args);
    if (!as) return ctrErr("ctx.container is only for the calling object's own container");
    const r = await this.env.HOSTD.fetch("http://hostd/v1/container/stdin?exec=" + encodeURIComponent(String(args.exec ?? "")), {
      method: "POST",
      headers: { "x-twd-as": JSON.stringify(as) },
      body,
    });
    return { ok: r.ok, status: r.status };
  }
  // getTcpPort(port).fetch(request)
  async ctrFetch(args = {}, request) {
    const as = containerAs(this.ctx.props ?? {}, args);
    if (!as) return errResponse("bad_request", "ctx.container is only for the calling object's own container");
    const port = Number(args.port);
    if (!Number.isInteger(port) || port < 1 || port > 65535) return errResponse("bad_request", "bad port");
    const u = new URL(request.url);
    const h = new Headers(request.headers);
    for (const k of [...h.keys()]) if (k.startsWith("x-twd") || k.startsWith("twd-")) h.delete(k);
    h.set("x-twd-as", JSON.stringify(as));
    h.set("x-twd-host", u.host);
    const hasBody = request.method !== "GET" && request.method !== "HEAD";
    return this.env.HOSTD.fetch("http://hostd/v1/container/port/" + port + u.pathname + u.search, {
      method: request.method,
      headers: h,
      body: hasBody ? request.body : undefined,
      redirect: "manual",
    });
  }
  async call(op, args = {}) {
    const p = this.ctx.props ?? {};
    const as = attestedAs(p, args);
    if (p.kind !== "object" && args.as && !as) {
      return { ok: false, error: errBody("bad_request", "the workload may only act as its plain-DO objects") };
    }
    if (op === "alarm") {
      // Only a facet object may manage its own (delegated) alarm.
      if (p.kind !== "object" || !isFacet(p.class)) return { ok: false, error: errBody("bad_request", "alarm delegation is for facet objects only") };
      const act = args.action;
      if (!["set", "get", "delete"].includes(act)) return { ok: false, error: errBody("bad_request", "bad alarm action") };
      if (act === "set" && !Number.isFinite(args.time)) return { ok: false, error: errBody("bad_request", "bad alarm time") };
      try {
        return { ok: true, value: await this.ctx.exports["Host_" + p.class].getByName(p.name).__twd_alarm_op(act, args.time) };
      } catch (e) {
        const m = String(e?.message ?? e);
        return { ok: false, error: errBody(m.includes("[killed]") ? "killed" : "bad_request", m) };
      }
    }
    if (!["ensure", "spawn", "ready", "kill", "audit", "self"].includes(op)) return { ok: false, error: errBody("bad_request", "unknown op " + op) };
    if (op === "ensure" && !as) return { ok: false, error: errBody("bad_request", "ensure needs an object") };
    try {
      return { ok: true, value: await hostd(this.env, op, { ...args, as }) };
    } catch (e) {
      return { ok: false, error: e.body ?? errBody("hostd_unavailable", String(e)) };
    }
  }
}
