// @twd/node: the twd Node process tier SDK.
//
// Ingress: twd-hostd forwards each tailnet request to the app with the
// verified caller (netd WhoIs) in a signed `x-twd-caller` header:
//
//   x-twd-caller: v1.<base64url(JSON payload)>.<base64url(HMAC-SHA256(key, "v1." + payload))>
//
// The key is per process (TWD_CALLER_KEY, base64url, fresh on every start).
// netd strips inbound x-twd-* headers from tailnet clients, so a forged copy
// never reaches the app through the tailnet; a request that reaches the app
// some other way (another local process dialling its port) cannot produce a
// valid signature. Always verify: getCaller() returns null on a missing or
// invalid header, requireCaller() throws.
//
// Egress: outbound HTTP(S) must leave through hostd's attributed egress
// proxy (TWD_EGRESS_PROXY; also exported as HTTP_PROXY/HTTPS_PROXY with
// NODE_USE_ENV_PROXY=1), so the destination sees this deployment's own
// tailnet node and ADR-019 applies (netmap-only names, no loopback, no host,
// no metadata, no internet). In the process tier this is cooperative: a
// malicious app can open raw sockets and bypass env proxies. The microVM tier
// (gVisor gateway) is the enforcement boundary.
import { createHmac, timingSafeEqual } from "node:crypto";
import http from "node:http";
import { Readable } from "node:stream";

export const CALLER_HEADER = "x-twd-caller";
/** Signatures older (or newer) than this are rejected. */
export const DEFAULT_MAX_SKEW_MS = 60_000;

/** The verified caller, as netd's WhoIs saw the transport peer. */
export interface Caller {
  nodeId: string;
  node: string;
  hostname: string;
  addrs: string[];
  tags: string[];
  user?: string;
  capMap: Record<string, unknown[]>;
}

/** The signed payload of x-twd-caller. */
export interface CallerClaims {
  v: 1;
  caller: Caller;
  /** The mgmt object ID of this deployment's node (the callee). */
  objectId: string;
  workload: string;
  traceId?: string;
  /** Issued at, unix ms. */
  iat: number;
}

export class CallerError extends Error {
  code: "no_identity";
  constructor(detail: string) {
    super(detail);
    this.name = "CallerError";
    this.code = "no_identity";
  }
  toResponse(): Response {
    return new Response(JSON.stringify({ code: this.code, message: this.message, doc_url: "https://twd.dev/errors/no_identity" }), {
      status: 401,
      headers: { "content-type": "application/json", "twd-error": this.code },
    });
  }
}

function keyBytes(key?: string | Uint8Array): Buffer {
  const k = key ?? process.env.TWD_CALLER_KEY;
  if (!k) throw new CallerError("TWD_CALLER_KEY is not set (not running under twd-hostd?)");
  return typeof k === "string" ? Buffer.from(k, "base64url") : Buffer.from(k);
}

function sign(key: Buffer, payload: string): string {
  return createHmac("sha256", key).update("v1." + payload).digest("base64url");
}

/** signCaller is what hostd does; exported for tests and local dev. */
export function signCaller(claims: CallerClaims, key?: string | Uint8Array): string {
  const payload = Buffer.from(JSON.stringify(claims)).toString("base64url");
  return `v1.${payload}.${sign(keyBytes(key), payload)}`;
}

export interface VerifyOptions {
  key?: string | Uint8Array;
  maxSkewMs?: number;
  now?: number;
}

/** verifyCaller checks a header value and returns its claims, or throws CallerError. */
export function verifyCaller(header: string | null | undefined, opts: VerifyOptions = {}): CallerClaims {
  if (!header) throw new CallerError("missing " + CALLER_HEADER + " (not called over the tailnet via twd)");
  const parts = header.split(".");
  if (parts.length !== 3 || parts[0] !== "v1") throw new CallerError("malformed " + CALLER_HEADER);
  const [, payload, sig] = parts;
  const want = Buffer.from(sign(keyBytes(opts.key), payload));
  const got = Buffer.from(sig);
  if (want.length !== got.length || !timingSafeEqual(want, got)) throw new CallerError("invalid " + CALLER_HEADER + " signature");
  let claims: CallerClaims;
  try {
    claims = JSON.parse(Buffer.from(payload, "base64url").toString("utf8"));
  } catch {
    throw new CallerError("malformed " + CALLER_HEADER + " payload");
  }
  const skew = Math.abs((opts.now ?? Date.now()) - claims.iat);
  if (!(skew <= (opts.maxSkewMs ?? DEFAULT_MAX_SKEW_MS))) throw new CallerError(CALLER_HEADER + " expired");
  if (claims.v !== 1 || !claims.caller?.nodeId) throw new CallerError("malformed " + CALLER_HEADER + " claims");
  return claims;
}

type AnyRequest = Request | http.IncomingMessage | { headers: Record<string, string | string[] | undefined> | Headers };

function headerOf(req: AnyRequest): string | undefined {
  const h: any = (req as any).headers;
  if (!h) return undefined;
  if (typeof h.get === "function") return h.get(CALLER_HEADER) ?? undefined;
  const v = h[CALLER_HEADER];
  return Array.isArray(v) ? (v.length === 1 ? v[0] : undefined) : v;
}

/** getCaller returns the verified caller of a request, or null. */
export function getCaller(req: AnyRequest, opts?: VerifyOptions): Caller | null {
  try {
    return verifyCaller(headerOf(req), opts).caller;
  } catch {
    return null;
  }
}

/** requireCaller returns the verified claims or throws CallerError (401). */
export function requireCaller(req: AnyRequest, opts?: VerifyOptions): CallerClaims {
  return verifyCaller(headerOf(req), opts);
}

/** capValues returns the caller's values for an app capability. */
export function capValues<T = unknown>(c: Caller | null, cap: string): T[] {
  return ((c?.capMap?.[cap] ?? []) as T[]);
}

// ---- egress ----

/** proxyUrl is the attributed egress proxy (with its per-process credentials). */
export function proxyUrl(): string {
  const p = process.env.TWD_EGRESS_PROXY;
  if (!p) throw new Error("TWD_EGRESS_PROXY is not set (not running under twd-hostd?)");
  return p;
}

function proxyParts() {
  const u = new URL(proxyUrl());
  const auth = u.username ? "Basic " + Buffer.from(decodeURIComponent(u.username) + ":" + decodeURIComponent(u.password)).toString("base64") : undefined;
  return { host: u.hostname, port: Number(u.port || 80), auth, uri: `${u.protocol}//${u.host}` };
}

/**
 * fetch sends a request through the egress proxy in absolute form (no
 * CONNECT): for https:// URLs the proxy (netd) originates TLS from this
 * deployment's node. Same signature as the global fetch.
 */
export async function fetch(input: string | URL | Request, init?: RequestInit): Promise<Response> {
  const req = new Request(input, init);
  const p = proxyParts();
  const body = req.body ? Buffer.from(await req.arrayBuffer()) : undefined;
  const headers: Record<string, string> = {};
  req.headers.forEach((v, k) => (headers[k] = v));
  headers["host"] = new URL(req.url).host;
  if (p.auth) headers["proxy-authorization"] = p.auth;
  if (body) headers["content-length"] = String(body.length);
  return new Promise((resolve, reject) => {
    const r = http.request({ host: p.host, port: p.port, method: req.method, path: req.url, headers, signal: req.signal }, (res) => {
      const h = new Headers();
      for (const [k, v] of Object.entries(res.headers)) {
        if (Array.isArray(v)) v.forEach((x) => h.append(k, x));
        else if (v != null) h.set(k, v);
      }
      const nobody = req.method === "HEAD" || res.statusCode === 204 || res.statusCode === 304;
      resolve(new Response(nobody ? null : (Readable.toWeb(res) as any), { status: res.statusCode, statusText: res.statusMessage, headers: h }));
    });
    r.on("error", reject);
    r.end(body);
  });
}

/**
 * dispatcher returns an undici ProxyAgent for the egress proxy (needs the
 * `undici` package): `undici.fetch(url, { dispatcher: await dispatcher() })`
 * or `setGlobalDispatcher(await dispatcher())`. http:// targets are
 * forwarded in absolute form; https:// targets tunnel via CONNECT.
 */
export async function dispatcher(): Promise<any> {
  const { ProxyAgent } = await import("undici" as string);
  const p = proxyParts();
  return new ProxyAgent({ uri: p.uri, token: p.auth, proxyTunnel: false });
}
