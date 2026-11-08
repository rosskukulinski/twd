import http from "node:http";
import fs from "node:fs";
import net from "node:net";
import { getCaller, requireCaller, CallerError, fetch as twdFetch, dispatcher, installGlobalDispatcher, enforceEgress } from "@twd/node";
// The /net sandbox probe must use the UNPATCHED connect (enforceEgress
// tunnels raw connects through the proxy; the probe measures the OS
// sandbox). net.connect() itself goes through Socket.prototype.connect, so
// keep the raw prototype method.
const rawProtoConnect = net.Socket.prototype.connect;
const rawConnect = (opts) => rawProtoConnect.call(new net.Socket(), opts);
enforceEgress();

const port = Number(process.env.PORT);
const json = (res, code, v) => { res.writeHead(code, { "content-type": "application/json" }); res.end(JSON.stringify(v)); };

async function egress(mode, url) {
  let r;
  if (mode === "sdk") r = await twdFetch(url);
  else if (mode === "global") r = await fetch(url); // NODE_USE_ENV_PROXY + HTTPS_PROXY/HTTP_PROXY (CONNECT)
  else if (mode === "http") {
    r = await new Promise((resolve, reject) => http.get(url, (res) => { let b = ""; res.on("data", (c) => (b += c)); res.on("end", () => resolve({ status: res.statusCode, headers: new Headers(Object.entries(res.headers).filter(([, v]) => typeof v === "string")), text: async () => b })); }).on("error", reject));
  } else if (mode === "connect") {
    // A raw CONNECT tunnel through the proxy (what env proxies do for https).
    const pu = new URL(process.env.TWD_EGRESS_PROXY), t = new URL(url);
    const auth = "Basic " + Buffer.from(decodeURIComponent(pu.username) + ":" + decodeURIComponent(pu.password)).toString("base64");
    const status = await new Promise((resolve, reject) => {
      const q = http.request({ host: pu.hostname, port: pu.port, method: "CONNECT", path: `${t.hostname.replace(/^\[|\]$/g, "")}:${t.port || 80}`.replace(/^([^:]*:[^:]*:.*):(\d+)$/, "[$1]:$2"), agent: false, headers: { "proxy-authorization": auth } });
      q.on("connect", (res, sock) => { sock.destroy(); resolve({ code: res.statusCode, err: res.headers["twd-error"] }); });
      q.on("error", reject);
      q.end();
    });
    return { status: status.code, twdError: status.err ?? null, body: "" };
  } else if (mode === "undici") {
    const undici = await import("undici");
    r = await undici.fetch(url, { dispatcher: await dispatcher(undici) });
    // Importing undici replaced the global dispatcher (so global fetch()
    // would now bypass the env proxy); put the egress proxy back.
    await installGlobalDispatcher(undici);
  } else throw new Error("mode");
  return { status: r.status, twdError: r.headers.get("twd-error"), body: await r.text() };
}

http.createServer(async (req, res) => {
  const u = new URL(req.url, "http://x");
  try {
    switch (u.pathname) {
      case "/": return json(res, 200, { ok: true });
      case "/whoami": {
        const claims = requireCaller(req);
        return json(res, 200, { claims, greeting: process.env.PROBE_GREETING });
      }
      case "/maybe": return json(res, 200, { caller: getCaller(req) });
      case "/egress": return json(res, 200, await egress(u.searchParams.get("mode") ?? "sdk", u.searchParams.get("url")));
      case "/proc": return json(res, 200, { pid: process.pid, rss: process.memoryUsage().rss, uptimeMs: Math.round(process.uptime() * 1000),
        env: Object.keys(process.env).filter((k) => k.startsWith("TWD_") || k.endsWith("PROXY") || k.endsWith("proxy")).sort() });
      case "/fs": {
        // Sandbox probe: only the outcome (and a length), never file contents.
        const p = u.searchParams.get("path"), op = u.searchParams.get("op") ?? "read";
        try {
          if (op === "write") { fs.writeFileSync(p, "probe"); return json(res, 200, { ok: true }); }
          if (op === "list") return json(res, 200, { ok: true, n: fs.readdirSync(p).length });
          return json(res, 200, { ok: true, n: fs.readFileSync(p).length });
        } catch (e) { return json(res, 200, { ok: false, code: e.code ?? String(e) }); }
      }
      case "/net": {
        // Sandbox probe: raw TCP (host+port) or unix-socket (path) connect,
        // bypassing the egress proxy. Only the outcome is returned.
        const p = u.searchParams.get("path");
        const opts = p ? { path: p } : { host: u.searchParams.get("host"), port: Number(u.searchParams.get("port")) };
        const r = await new Promise((resolve) => {
          const s = rawConnect(opts);
          const t = setTimeout(() => { s.destroy(); resolve({ ok: false, code: "TIMEOUT" }); }, 3000);
          s.on("connect", () => { clearTimeout(t); s.destroy(); resolve({ ok: true }); });
          s.on("error", (e) => { clearTimeout(t); resolve({ ok: false, code: e.code ?? String(e) }); });
        });
        return json(res, 200, r);
      }
      case "/log": console.log("probe-log-line", u.searchParams.get("m")); return json(res, 200, { ok: true });
      case "/crash": json(res, 200, { bye: process.pid }); setTimeout(() => process.exit(3), 10); return;
      case "/stream": {
        res.writeHead(200, { "content-type": "text/plain" });
        const t = setInterval(() => res.write("tick\n"), 50);
        req.on("close", () => clearInterval(t));
        return;
      }
    }
    json(res, 404, {});
  } catch (e) {
    if (e instanceof CallerError) return json(res, 401, { code: e.code, message: e.message });
    json(res, 500, { error: String(e?.cause?.message ?? e?.message ?? e) });
  }
}).listen(...(process.env.TWD_SOCKET ? [process.env.TWD_SOCKET] : [port, "127.0.0.1"]));
