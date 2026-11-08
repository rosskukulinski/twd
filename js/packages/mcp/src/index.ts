// @twd/mcp: per-request tailnet identity for stateless MCP servers
// (createMcpHandler from agents/mcp/server), per-tool authorization from app
// capabilities, and DO-backed budgets. Needs "nodejs_compat" (AsyncLocalStorage).
import { AsyncLocalStorage } from "node:async_hooks";
import { getCaller, principalKey, TailnetError, isTailnetError } from "@twd/tailnet";
import type { Caller } from "@twd/tailnet";

export type { Caller };
export { getCaller, TailnetError, isTailnetError };

interface Authorized {
  cap: string;
  tool: string;
  /** Max across matching grants per numeric limit key (Infinity = unbounded). */
  limits: Record<string, number>;
}

interface Store {
  caller: Caller;
  request: Request;
  authorized: Record<string, Authorized>;
}

const als = new AsyncLocalStorage<Store>();

type Handler = ((req: Request, env: any, ctx: any) => Promise<Response> | Response) | { fetch(req: Request, env: any, ctx: any): Promise<Response> | Response };

export interface WithTailnetOptions {
  /** Only serve this path (and below); other paths go to fallback or 404. */
  path?: string;
  fallback?: (req: Request, env: any, ctx: any) => Promise<Response> | Response;
}

/**
 * withTailnet resolves getCaller(request) once per request and makes it
 * available to tool handlers via caller() (AsyncLocalStorage). MCP is
 * stateless here: there is no session binding; identity is per request.
 */
export function withTailnet(handler: Handler, opts: WithTailnetOptions = {}) {
  const h = typeof handler === "function" ? handler : (req: Request, env: any, ctx: any) => handler.fetch(req, env, ctx);
  return {
    async fetch(req: Request, env: any, ctx: any): Promise<Response> {
      if (opts.path) {
        const p = new URL(req.url).pathname;
        if (p !== opts.path && !p.startsWith(opts.path.replace(/\/$/, "") + "/")) {
          return opts.fallback ? opts.fallback(req, env, ctx) : new Response("not found", { status: 404 });
        }
      }
      let who: Caller;
      try {
        who = getCaller(req);
      } catch (e) {
        if (isTailnetError(e)) return e.toResponse();
        throw e;
      }
      return als.run({ caller: who, request: req, authorized: {} }, () => h(req, env, ctx));
    },
  };
}

/** caller returns this request's verified caller (inside withTailnet). */
export function caller(): Caller {
  const s = als.getStore();
  if (!s) throw mcpError("no_identity", "caller() called outside withTailnet (or the request has no verified caller)");
  return s.caller;
}

/**
 * mcpError builds a TailnetError whose message is the JSON error body, so an
 * MCP tool result (isError: true, content[0].text = error.message) carries
 * the typed error: {"code":"cap_missing", ...}.
 */
export function mcpError(code: string, detail: string, init: Record<string, unknown> = {}): TailnetError {
  const e = new TailnetError(code, detail, init as any);
  e.message = JSON.stringify(e.toJSON());
  return e;
}

interface Grant {
  tools?: string[];
  limits?: Record<string, Record<string, number>>;
  groups?: Record<string, string[]>;
  [k: string]: unknown;
}

const upperFirst = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

function who2(who: Caller) {
  return `${who.hostname || who.nodeId}${who.tags.length ? " (" + who.tags.join(",") + ")" : ""}`;
}

/**
 * authorizeTool checks that `who` holds app capability `cap` for
 * facts.tool, and that every numeric fact is within the matching limit.
 * Semantics (DX.md journey 3):
 *  - union: the tool is allowed if ANY grant lists it in `tools` (or "*");
 *    a grant without `tools` allows nothing;
 *  - groups[tool] (if present) requires actingFor.groups ∩ groups ≠ ∅;
 *  - numeric limits: fact `usd` is checked against `limits[tool].maxUsd`,
 *    taking the MAX across matching grants; a matching grant without that
 *    limit makes it unbounded.
 * Throws cap_missing / cap_limit_exceeded (message = JSON body).
 */
export function authorizeTool(who: Caller, cap: string, facts: { tool: string; [k: string]: string | number }): Authorized {
  const tool = facts?.tool;
  if (!tool) throw mcpError("bad_request", "authorizeTool: facts.tool is required");
  const grants = who.caps<Grant>(cap).filter((g) => g && typeof g === "object");
  const chain = who.chain.map((h) => h.nodeId);
  const toolOK = (g: Grant) => Array.isArray(g.tools) && (g.tools.includes(tool) || g.tools.includes("*"));
  const groupOK = (g: Grant) => {
    const need = g.groups?.[tool];
    if (!need || need.length === 0) return true;
    const have = who.actingFor?.groups ?? [];
    return need.some((x) => have.includes(x));
  };
  const matching = grants.filter((g) => toolOK(g) && groupOK(g));
  if (matching.length === 0) {
    const have = [...new Set(grants.flatMap((g) => (Array.isArray(g.tools) ? g.tools : [])))];
    const groupBlocked = grants.some(toolOK);
    const src = who.tags.length ? who.tags : [who.user?.login ?? who.nodeId];
    throw mcpError(
      "cap_missing",
      groupBlocked
        ? `tool "${tool}" is granted to ${who2(who)} only when acting for ${grants.filter(toolOK).flatMap((g) => g.groups?.[tool] ?? []).join(", ")}`
        : `tool "${tool}" not granted to ${who2(who)}`,
      {
        missing: { cap, tool },
        have: { cap, tools: have },
        chain,
        snippet: JSON.stringify({ src, dst: ["<this server's tag or svc>"], ip: ["tcp:80"], app: { [cap]: [{ tools: [tool] }] } }),
      },
    );
  }
  // Merge limits for the tool: max across matching grants per key.
  const keys = new Set<string>();
  for (const g of matching) for (const k of Object.keys(g.limits?.[tool] ?? {})) keys.add(k);
  const limits: Record<string, number> = {};
  for (const k of keys) {
    let max = -Infinity;
    for (const g of matching) {
      const v = g.limits?.[tool]?.[k];
      max = typeof v === "number" ? Math.max(max, v) : Infinity;
    }
    limits[k] = max;
  }
  for (const [f, v] of Object.entries(facts)) {
    if (f === "tool" || typeof v !== "number") continue;
    const key = "max" + upperFirst(f);
    const lim = limits[key];
    if (lim !== undefined && v > lim) {
      throw mcpError("cap_limit_exceeded", `${tool} ${f}=${v} exceeds ${key}=${lim} for ${who2(who)} (max across ${matching.length} matching grant(s))`, {
        missing: { cap, tool },
        limit: { [key]: lim },
        chain,
      });
    }
  }
  const out: Authorized = { cap, tool, limits };
  const s = als.getStore();
  if (s) s.authorized[tool] = out;
  return out;
}

export interface BudgetOptions {
  /** Daily limit; default: the first perDay* limit from authorizeTool for this tool. */
  limit?: number;
  /** Override the principal key (default: actingFor/user login, else nodeId). */
  key?: string;
}

interface BudgetStub {
  reserve(amount: number, limit: number | null, day: string): Promise<{ ok: boolean; used: number; limit: number | null }>;
  used(day: string): Promise<number>;
}

/**
 * budget returns a per-(tool, principal) daily counter backed by the
 * TwdBudget Durable Object (export it from your Worker:
 * `export { TwdBudget } from "@twd/mcp/budget-do"`). reserve() is serialised
 * by the DO and throws cap_limit_exceeded past the limit.
 */
export function budget(ns: { getByName(name: string): unknown }, who: Caller, tool: string, opts: BudgetOptions = {}) {
  const s = als.getStore();
  let limit = opts.limit;
  let limitKey = "limit";
  if (limit === undefined) {
    const lim = s?.authorized[tool]?.limits ?? {};
    const k = Object.keys(lim).find((x) => x.startsWith("perDay"));
    if (k && Number.isFinite(lim[k])) {
      limit = lim[k];
      limitKey = k;
    }
  }
  const pk = opts.key ?? principalKey(who);
  const stub = ns.getByName(`${tool}:${pk}`) as BudgetStub;
  const day = () => new Date().toISOString().slice(0, 10);
  return {
    async reserve(amount: number) {
      const r = await stub.reserve(amount, limit ?? null, day());
      if (!r.ok) {
        throw mcpError("cap_limit_exceeded", `${tool}: daily budget ${limitKey}=${r.limit} exceeded (used ${r.used}, requested ${amount}) for ${pk}`, {
          limit: { [limitKey]: r.limit },
          used: r.used,
        });
      }
      return r;
    },
    used() {
      return stub.used(day());
    },
  };
}

export interface McpFetchOptions {
  /** Extra headers on every MCP request. */
  headers?: Record<string, string>;
  /** Trace ID to propagate as W3C `traceparent` (default: the current withTailnet caller's traceId, if any). */
  traceId?: string;
}

/**
 * mcpFetch returns a `fetch` for MCP clients running on twd (for example
 * `createMCPClient({ transport: { type: "http", url, fetch: mcpFetch(ctx) } })`
 * from `@ai-sdk/mcp`). Requests leave through `ctx.tailnet.fetch`, so they
 * carry the calling object's (or workload's) own node identity, and a twd
 * egress refusal (`peer_not_visible`, `forbidden_destination`, `acl_denied`,
 * `identity_pending`…) throws a typed TailnetError instead of surfacing as an
 * opaque "HTTP 403" from the MCP SDK. The trace ID is propagated as
 * `traceparent` so the server's audit can join the hop.
 *
 * Not implemented yet (RFC §4.6): signed on-behalf-of (`actingFor`)
 * assertions. The MCP server sees this node as the caller; it cannot yet
 * verify which human the call is for.
 */
export function mcpFetch(ctx: { tailnet?: { fetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> } } | null | undefined, opts: McpFetchOptions = {}) {
  const tn = ctx?.tailnet;
  if (!tn) throw new TailnetError("no_identity", "mcpFetch: ctx.tailnet is not available (not running on twd?)");
  return async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    // workerd rejects redirect: "error" (the @ai-sdk/mcp default); emulate it
    // with "manual" plus a check.
    const wantErr = init?.redirect === "error";
    const req = new Request(input as any, init ? { ...init, redirect: wantErr ? "manual" : init.redirect } : undefined);
    const h = new Headers(req.headers);
    for (const [k, v] of Object.entries(opts.headers ?? {})) h.set(k, v);
    const trace = opts.traceId ?? als.getStore()?.caller.traceId;
    if (trace && /^[0-9a-f]{32}$/.test(trace) && !h.has("traceparent")) {
      const span = [...crypto.getRandomValues(new Uint8Array(8))].map((b) => b.toString(16).padStart(2, "0")).join("");
      h.set("traceparent", `00-${trace}-${span}-01`);
    }
    const hasBody = req.method !== "GET" && req.method !== "HEAD";
    const r = await tn.fetch(req.url, { method: req.method, headers: h, body: hasBody ? await req.arrayBuffer() : undefined, signal: init?.signal ?? undefined, redirect: req.redirect });
    if (wantErr && r.status >= 300 && r.status < 400) throw new TypeError(`mcpFetch: redirect (${r.status}) refused (redirect: "error")`);
    return r;
  };
}
