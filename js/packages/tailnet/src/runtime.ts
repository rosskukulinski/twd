// Platform-injected runtime. twd-hostd's generated shims import this module
// (bundled as __twd_rt.js) to install ctx.tailnet on Durable Object state
// and on the Worker ExecutionContext. Tenant code reaches it as
// `this.ctx.tailnet` / `ctx.tailnet` (RFC §6.1); it never imports it.
//
// Every call goes through `tw`, a platform TailnetApi entrypoint stub whose
// props (minted by the platform, read-only here) say which object or
// workload is calling, so tenant code cannot act as another object.

import { TailnetError, fromResponse } from "./errors.js";

/** The platform's TailnetApi stub (RPC). */
export interface TailnetApiStub {
  call(op: string, args: Record<string, unknown>): Promise<{ ok: true; value: any } | { ok: false; error: Record<string, any> }>;
}

export interface SpawnOptions {
  name: string;
  /** Must be ⊆ the parent's effective caps (checked by twd-mgmt). */
  caps?: Record<string, unknown>;
  ttl?: string;
  principal?: "inherit" | "none";
  /** How long `ready` waits for the child's node (default 30 s). */
  readyTimeoutMs?: number;
}

export interface Spawned<T = unknown> {
  stub: T;
  /** Node hostname now; the MagicDNS name once ready. */
  name: string;
  objectId: string;
  state(): "pending" | "ready" | "failed";
  ready: Promise<void>;
}

export interface TailnetContext {
  fetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response>;
  forward(request: Request, to: string | URL, init?: RequestInit): Promise<Response>;
  spawn<T = any>(ns: any, opts: SpawnOptions): Promise<Spawned<T>>;
  kill(opts?: { reason?: string; cascade?: boolean; target?: { class: string; name: string } }): Promise<{ killed: string[] }>;
  audit(records: Array<Record<string, unknown>>): Promise<void>;
  self(): Promise<{ objectId: string; hostname: string; magicDNS?: string; status: string; kind: string }>;
  connect(): never;
}

export type BindingMap = Record<string, string>; // env binding name -> DO class

export interface Self {
  class: string;
  name: string;
}

export function createTailnetContext(tw: TailnetApiStub, env: Record<string, any>, bindings: BindingMap, self?: Self): TailnetContext {
  if (!tw || typeof tw.call !== "function") throw new TailnetError("no_identity", "twd platform binding missing (not running on twd?)");
  const op = async (name: string, args: Record<string, unknown> = {}) => {
    const r = await tw.call(name, self ? { ...args, as: self } : args);
    if (!r || !r.ok) throw TailnetError.from((r as any)?.error);
    return r.value;
  };
  const classOf = (ns: unknown): string => {
    for (const [k, cls] of Object.entries(bindings)) if (env[k] === ns) return cls;
    if (typeof ns === "string" && Object.values(bindings).includes(ns)) return ns;
    throw new TailnetError("bad_request", "spawn: the namespace is not a durable_objects binding of this app");
  };
  const tfetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    let r: Response;
    try {
      r = await fetch(input as any, init);
    } catch (e) {
      throw new TailnetError("dial_failed", String((e as Error)?.message ?? e));
    }
    const err = await fromResponse(r);
    if (err) throw err;
    return r;
  };
  const tn: TailnetContext = {
    fetch: tfetch,
    forward(request: Request, to: string | URL, init?: RequestInit) {
      // MVP: re-issues the request from this node. Signed act-chain
      // assertions (RFC §4.6) are not implemented yet.
      const hasBody = request.method !== "GET" && request.method !== "HEAD";
      return tfetch(new Request(String(to), { method: request.method, headers: request.headers, body: hasBody ? request.body : undefined, ...init }));
    },
    async spawn(ns: any, opts: SpawnOptions) {
      if (!opts || typeof opts.name !== "string" || !opts.name) throw new TailnetError("bad_request", "spawn: opts.name is required");
      const cls = classOf(ns);
      const v = await op("spawn", { class: cls, name: opts.name, caps: opts.caps, ttl: opts.ttl, principal: opts.principal });
      let state: "pending" | "ready" | "failed" = v.status === "ready" ? "ready" : "pending";
      // Await: inside a facet, env.X is an RPC stub and getByName returns an
      // RpcPromise that must not be stored un-awaited.
      const stub = await ns.getByName(opts.name);
      const sp: Spawned<any> = {
        stub,
        name: v.hostname ?? opts.name,
        objectId: v.objectID,
        state: () => state,
        ready: Promise.resolve(),
      };
      sp.ready = op("ready", { class: cls, name: opts.name, timeoutMs: opts.readyTimeoutMs ?? 30000 }).then(
        (r) => {
          state = "ready";
          if (r?.magicDNS) sp.name = r.magicDNS;
        },
        (e) => {
          state = "failed";
          throw e;
        },
      );
      sp.ready.catch(() => {}); // callers may ignore readiness
      return sp;
    },
    kill(opts: { reason?: string; cascade?: boolean; target?: { class: string; name: string } } = {}) {
      return op("kill", { reason: opts.reason ?? "", cascade: !!opts.cascade, target: opts.target });
    },
    async audit(records) {
      await op("audit", { records });
    },
    self() {
      return op("self", {});
    },
    connect(): never {
      throw new TailnetError("plan_feature_missing", "ctx.tailnet.connect() is not implemented yet (raw TCP egress)");
    },
  };
  return Object.freeze(tn);
}

function define(ctx: any, tn: TailnetContext) {
  try {
    Object.defineProperty(ctx, "tailnet", { value: tn, enumerable: false, configurable: true });
  } catch {
    // a frozen ctx: leave ctx.tailnet absent rather than fail the request
  }
}

/** wrapFacetClass wraps a tenant DO class run as a facet (per-object egress). */
export function wrapFacetClass(C: any, name: string, bindings: BindingMap) {
  if (typeof C !== "function") throw new Error(`twd: durable_objects class ${name} is not exported by the main module`);
  const W = {
    [name]: class extends C {
      constructor(ctx: any, env: any) {
        const tw = env?.__TWD;
        const e = { ...env };
        delete e.__TWD;
        define(ctx, createTailnetContext(tw, e, bindings));
        super(ctx, e);
      }
    },
  }[name];
  return W;
}

/**
 * wrapPlainClass wraps a tenant DO class run as a plain DO (egress:
 * "workload"): alarms work, egress leaves as the workload node. The wrapper
 * tells hostd the object exists (so its node comes up) and enforces kill.
 */
export function wrapPlainClass(C: any, name: string, bindings: BindingMap, opts: { node: boolean }) {
  if (typeof C !== "function") throw new Error(`twd: durable_objects class ${name} is not exported by the main module`);
  const proto = C.prototype;
  const killedResponse = (reason: string) =>
    new TailnetError("killed", `object ${name} was killed: ${reason}`).toResponse();
  const W: any = {
    [name]: class extends C {
      constructor(ctx: any, env: any) {
        const tw = env?.__TWD;
        const oname = ctx?.id?.name;
        if (tw && oname) define(ctx, createTailnetContext(tw, env, bindings, { class: name, name: oname }));
        super(ctx, env);
        if (tw && oname && opts.node) {
          const p = tw.call("ensure", { as: { class: name, name: oname } }).catch(() => {});
          try {
            ctx.waitUntil?.(p);
          } catch {
            /* ignore */
          }
        }
      }
      async __twd_kill(reason: string) {
        (this as any).ctx.storage.kv.put("__twd_killed", reason || "killed");
        return true;
      }
    },
  }[name];
  if (typeof proto.fetch === "function") {
    W.prototype.fetch = async function (this: any, req: Request) {
      const k = this.ctx.storage.kv.get("__twd_killed");
      if (k) return killedResponse(k);
      return proto.fetch.call(this, req);
    };
  }
  return W;
}

/** wrapHandler installs ctx.tailnet (workload identity) on the default export. */
export function wrapHandler(H: any, bindings: BindingMap) {
  if (!H) {
    return {
      fetch: () => new TailnetError("unknown_object", "this app has no default export (Worker); address objects via their own nodes or /_twd/o/<Class>/<name>/").toResponse(),
    };
  }
  if (typeof H === "function") {
    // A WorkerEntrypoint class.
    return {
      [H.name || "Default"]: class extends H {
        constructor(ctx: any, env: any) {
          define(ctx, createTailnetContext(env?.__TWD, env, bindings));
          super(ctx, env);
        }
      },
    }[H.name || "Default"];
  }
  const out: Record<string, unknown> = { ...H };
  for (const k of Object.keys(H)) {
    if (typeof H[k] !== "function") continue;
    out[k] = function (this: unknown, a: unknown, env: any, ctx: any) {
      if (ctx && env?.__TWD && !("tailnet" in ctx)) define(ctx, createTailnetContext(env.__TWD, env, bindings));
      return H[k].call(H, a, env, ctx);
    };
  }
  return out;
}
