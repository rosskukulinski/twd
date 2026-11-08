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
// no metadata, no internet). enforceEgress() (called by
// installGlobalDispatcher) additionally routes raw `net`/`tls` connects
// through the proxy as CONNECT tunnels and refuses direct DNS/UDP, so code
// that ignores the env proxies is still attributed or fails. All of this is
// COOPERATIVE: it runs inside the app's own process, and hostile code can
// undo it. Only an OS sandbox that confines the process's network (Landlock
// ABI ≥ 4 on Linux) or a microVM makes the proxy an enforcement boundary;
// see docs/node-tier.md.
import { createHmac, timingSafeEqual } from "node:crypto";
import dgram from "node:dgram";
import dns from "node:dns";
import http from "node:http";
import net from "node:net";
import { Readable } from "node:stream";
import tls from "node:tls";

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
  /** Per-request nonce; a header seen twice within the skew window is refused (RT-05). */
  nonce?: string;
  /** The calling twd object's ID, when the caller is a twd object with a node certificate (RT-01). */
  callerObjectId?: string;
  /** The caller's spawn-time cap limit (its node certificate's lim); caller.capMap is already narrowed by it. */
  lim?: Record<string, unknown>;
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
  const now = opts.now ?? Date.now();
  const maxSkew = opts.maxSkewMs ?? DEFAULT_MAX_SKEW_MS;
  const skew = Math.abs(now - claims.iat);
  if (!(skew <= maxSkew)) throw new CallerError(CALLER_HEADER + " expired");
  if (claims.v !== 1 || !claims.caller?.nodeId) throw new CallerError("malformed " + CALLER_HEADER + " claims");
  if (claims.nonce !== undefined && !rememberNonce(sig, now, maxSkew)) throw new CallerError(CALLER_HEADER + " replayed");
  return claims;
}

// Replay cache (RT-05): a signature is accepted once within the skew window.
const seenNonces = new Map<string, number>();
let lastSweep = 0;
function rememberNonce(sig: string, now: number, maxSkew: number): boolean {
  if (now - lastSweep > maxSkew || seenNonces.size > 100_000) {
    for (const [k, exp] of seenNonces) if (exp <= now) seenNonces.delete(k);
    lastSweep = now;
  }
  if (seenNonces.has(sig)) return false;
  seenNonces.set(sig, now + maxSkew);
  return true;
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

// A plain agent: Node's global agent may itself apply HTTP_PROXY
// (NODE_USE_ENV_PROXY), which would proxy our request to the proxy.
const directAgent = new http.Agent({ keepAlive: true });

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
    const r = http.request({ agent: directAgent, host: p.host, port: p.port, method: req.method, path: req.url, headers, signal: req.signal }, (res) => {
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
 * dispatcher returns an undici ProxyAgent for the egress proxy:
 * `undici.fetch(url, { dispatcher: await dispatcher(undici) })`. Pass your
 * `undici` module (import * as undici from "undici"); without it, undici is
 * imported at run time from node_modules. http:// targets are forwarded in
 * absolute form; https:// targets tunnel via CONNECT.
 */
export async function dispatcher(undici?: any): Promise<any> {
  if (!undici) {
    const spec = "undici"; // optional peer: not resolved at bundle time
    undici = await import(spec);
  }
  const p = proxyParts();
  return new undici.ProxyAgent({ uri: p.uri, token: p.auth, proxyTunnel: false });
}

/**
 * installGlobalDispatcher routes the global fetch() (Node's built-in and
 * undici's) through the egress proxy, and calls enforceEgress(). Call it
 * after importing `undici`: loading the undici package installs a plain
 * Agent as the global dispatcher, which silently overrides
 * NODE_USE_ENV_PROXY for every later global fetch() in the process
 * (measured on Node 25 + undici 8).
 */
export async function installGlobalDispatcher(undici?: any): Promise<void> {
  if (!undici) {
    const spec = "undici";
    undici = await import(spec);
  }
  undici.setGlobalDispatcher(await dispatcher(undici));
  enforceEgress();
}

// ---- egress enforcement (cooperative; RT-08) ----

export class EgressError extends Error {
  code: "forbidden_destination";
  constructor(detail: string) {
    super(detail);
    this.name = "EgressError";
    this.code = "forbidden_destination";
    (this as any).errno = -13; // EACCES-like for code that inspects errno
    (this as any).syscall = "connect";
  }
}

export interface EnforceEgressOptions {
  /** Extra "host:port" destinations allowed directly (default: the proxy and this app's own listener). */
  allow?: string[];
  /** Also refuse direct DNS (dns.lookup/resolve*) and UDP sockets (default true). */
  dns?: boolean;
}

let enforced = false;

function isLoopbackHost(h: string): boolean {
  return h === "localhost" || h === "127.0.0.1" || h === "::1" || h === "[::1]" || h === "0.0.0.0" || h === "::" || /^127\./.test(h);
}

/**
 * enforceEgress patches `net`, `tls`, `dns` and `dgram` so that code which
 * bypasses the env proxies is still attributed: a TCP connect to anything
 * but the egress proxy or this app's own listener becomes a CONNECT tunnel
 * through the proxy (netd then applies ADR-019 from the deployment's node
 * and refuses loopback/LAN/metadata/internet), direct DNS resolution fails
 * with forbidden_destination (names resolve at the proxy, from the
 * netmap), and UDP sockets fail. Idempotent.
 *
 * This is in-process and therefore cooperative: hostile code can restore
 * the originals. It closes accidental and dependency-level bypasses, not a
 * determined attacker; see docs/node-tier.md for what does.
 */
export function enforceEgress(opts: EnforceEgressOptions = {}): void {
  if (enforced) return;
  enforced = true;
  const p = proxyParts();
  const allow = new Set<string>([`${p.host}:${p.port}`, `127.0.0.1:${p.port}`, `localhost:${p.port}`]);
  const own = process.env.PORT;
  if (own) for (const h of ["127.0.0.1", "localhost", "::1", "0.0.0.0"]) allow.add(`${h}:${own}`);
  for (const a of opts.allow ?? []) allow.add(a);
  const ownSock = process.env.TWD_SOCKET;

  const origConnect = net.Socket.prototype.connect;
  const tunnel = (sock: net.Socket, host: string, port: number, cb?: () => void) => {
    // CONNECT host:port through the proxy on this very socket, then hand it
    // back to the caller as a connected socket. While the handshake runs,
    // the socket's own connect/ready/data events are intercepted so the
    // caller only sees a (fake) "connect" once the tunnel is established.
    const hp = net.isIPv6(host) ? `[${host}]:${port}` : `${host}:${port}`;
    const realEmit = sock.emit as (this: net.Socket, ev: string, ...args: any[]) => boolean;
    let buf = "";
    let handshaking = true;
    let wasFlowing = false;
    const finish = (err?: Error, rest?: Buffer) => {
      handshaking = false;
      (sock as any).emit = realEmit;
      if (err) {
        sock.destroy(err);
        return;
      }
      if (!wasFlowing && sock.listenerCount("data") === 0) {
        // Back to "never flowed": a later on("data") resumes as usual (an
        // explicit pause() would make it stay paused).
        sock.pause();
        (sock as any)._readableState.flowing = null;
      }
      if (rest && rest.length) sock.unshift(rest);
      sock.emit("connect");
      sock.emit("ready");
      cb?.();
    };
    (sock as any).emit = function (this: net.Socket, ev: string, ...args: any[]) {
      if (!handshaking) return realEmit.call(this, ev, ...args);
      switch (ev) {
        case "connect":
          this.write(`CONNECT ${hp} HTTP/1.1\r\nHost: ${hp}\r\n${p.auth ? "Proxy-Authorization: " + p.auth + "\r\n" : ""}\r\n`);
          // net.Socket deferred its first read to its own once("connect")
          // listener, which we just swallowed (the stream already counts
          // itself as reading): start the handle read directly.
          (this as any)._read(0);
          // The response must reach our "data" interception even if the
          // caller has not attached a data listener yet (stream not
          // flowing); finish() pauses again in that case.
          wasFlowing = this.readableFlowing === true;
          this.resume();
          return true;
        case "ready":
          return true;
        case "data": {
          buf += (args[0] as Buffer).toString("latin1");
          const i = buf.indexOf("\r\n\r\n");
          if (i < 0) return true;
          const head = buf.slice(0, i);
          const m = /^HTTP\/1\.[01] (\d{3})/.exec(head);
          if (!m || m[1] !== "200") {
            const te = /twd-error:\s*([a-z_]+)/i.exec(head);
            const err = new EgressError(`egress to ${hp} refused by the twd egress proxy (${te ? te[1] : "HTTP " + (m ? m[1] : "?")})`);
            if (te) err.code = te[1] as any;
            finish(err);
            return true;
          }
          finish(undefined, Buffer.from(buf.slice(i + 4), "latin1"));
          return true;
        }
        case "end":
        case "close":
          if (handshaking) {
            handshaking = false;
            (sock as any).emit = realEmit;
            realEmit.call(this, "error", new EgressError(`egress proxy closed the connection before answering CONNECT ${hp}`));
          }
          return realEmit.call(this, ev, ...args);
      }
      return realEmit.call(this, ev, ...args);
    };
    origConnect.call(sock, { host: p.host, port: p.port } as any);
  };
  (net.Socket.prototype as any).connect = function (this: net.Socket, ...args: any[]) {
    const n = normalizeConnectArgs(args);
    if (!n) return origConnect.apply(this, args as any);
    const { options, cb } = n;
    if (options.path) {
      if (ownSock && options.path === ownSock) return origConnect.apply(this, args as any);
      process.nextTick(() => this.destroy(new EgressError(`unix socket ${options.path} is not reachable from a twd Node app (only this app's own socket is)`)));
      return this;
    }
    const host = String(options.host ?? "localhost");
    const port = Number(options.port);
    if (allow.has(`${host}:${port}`)) return origConnect.apply(this, args as any);
    if (isLoopbackHost(host)) {
      process.nextTick(() => this.destroy(new EgressError(`loopback ${host}:${port} is not reachable from a twd Node app except its own port and the egress proxy`)));
      return this;
    }
    (this as any)._twdTunnel = `${host}:${port}`;
    tunnel(this, host, port, cb);
    return this;
  };
  (net as any).connect = (net as any).createConnection = function (...args: any[]) {
    const s = new net.Socket(typeof args[0] === "object" && args[0] && !Array.isArray(args[0]) ? args[0] : undefined);
    return (s as any).connect(...args);
  };
  // tls.connect builds its own TCP socket via net.connect unless given one:
  // our patched net.connect tunnels it, so TLS runs end-to-end through the
  // tunnel. servername must then come from the target, not the proxy.
  const origTLSConnect = tls.connect;
  (tls as any).connect = function (...args: any[]) {
    const n = normalizeConnectArgs(args);
    if (n && !n.options.socket && !n.options.path) {
      const host = String(n.options.host ?? "localhost");
      const port = Number(n.options.port ?? 443);
      if (!allow.has(`${host}:${port}`)) {
        const raw = new net.Socket();
        (raw as any).connect({ host, port });
        const o = { ...n.options, socket: raw, servername: n.options.servername ?? (net.isIP(host) ? undefined : host) };
        delete (o as any).host;
        delete (o as any).port;
        return (origTLSConnect as any).call(tls, o, n.cb);
      }
    }
    return origTLSConnect.apply(tls, args as any);
  };
  if (opts.dns !== false) {
    const refuse = (what: string) => new EgressError(`${what} is not available in a twd Node app: names resolve at the egress proxy (netmap only)`);
    const origLookup = dns.lookup;
    (dns as any).lookup = function (hostname: string, ...rest: any[]) {
      const cb = rest[rest.length - 1];
      if (isLoopbackHost(hostname) || hostname === p.host) return (origLookup as any).call(dns, hostname, ...rest);
      if (typeof cb === "function") process.nextTick(() => cb(refuse(`dns.lookup(${hostname})`)));
      return undefined;
    };
    (dns.promises as any).lookup = async (hostname: string, ...rest: any[]) => {
      if (isLoopbackHost(hostname) || hostname === p.host) return (origLookup as any).__promisified?.(hostname, ...rest) ?? new Promise((res, rej) => (origLookup as any).call(dns, hostname, ...rest.filter((x: any) => typeof x !== "function"), (e: any, a: any, f: any) => (e ? rej(e) : res({ address: a, family: f }))));
      throw refuse(`dns.promises.lookup(${hostname})`);
    };
    for (const k of Object.keys(dns)) {
      if (/^resolve/.test(k) || k === "reverse") {
        (dns as any)[k] = (name: string, ...rest: any[]) => {
          const cb = rest[rest.length - 1];
          if (typeof cb === "function") process.nextTick(() => cb(refuse(`dns.${k}(${name})`)));
        };
        if ((dns.promises as any)[k]) (dns.promises as any)[k] = async (name: string) => { throw refuse(`dns.promises.${k}(${name})`); };
      }
    }
    const origCreateSocket = dgram.createSocket;
    (dgram as any).createSocket = function (...args: any[]) {
      const s = (origCreateSocket as any).apply(dgram, args);
      const deny = (fn: string) => () => { throw refuse(`dgram.Socket.${fn}()`); };
      s.send = deny("send");
      s.connect = deny("connect");
      s.bind = deny("bind");
      return s;
    };
  }
}

function normalizeConnectArgs(args: any[]): { options: any; cb?: () => void } | null {
  let options: any;
  let cb: any;
  if (args.length === 0) return null;
  if (typeof args[0] === "object" && args[0] !== null && !Array.isArray(args[0])) {
    options = args[0];
    cb = typeof args[1] === "function" ? args[1] : undefined;
  } else if (typeof args[0] === "string" && (args.length === 1 || typeof args[1] === "function" || typeof args[1] !== "string") && isNaN(Number(args[0]))) {
    options = { path: args[0] };
    cb = typeof args[1] === "function" ? args[1] : undefined;
  } else {
    options = { port: Number(args[0]), host: typeof args[1] === "string" ? args[1] : "localhost" };
    cb = typeof args[args.length - 1] === "function" ? args[args.length - 1] : undefined;
  }
  return { options, cb };
}
