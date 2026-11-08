// node-workflow server: plain node:http. Tailnet callers arrive with a signed
// x-twd-caller (verified by @twd/node); the Workflow SDK's own queue calls
// back into /.well-known/workflow/v1/flow over loopback (no caller header).
import http from "node:http";
import path from "node:path";
import { start, getRun, resumeHook, getHookByToken } from "workflow/api";
import { getWorld } from "workflow/runtime";
import { requireCaller, CallerError, enforceEgress } from "@twd/node";

// RT-08: route raw net/tls connects through the egress proxy and refuse
// direct DNS/UDP. Cooperative (see docs/node-tier.md): only a Landlock
// sandbox or a microVM makes the proxy an enforcement boundary.
enforceEgress();
import { supportWorkflow } from "../workflows/support";

// Durable state lives in the deployment's data dir (kept across restarts and redeploys).
process.env.WORKFLOW_LOCAL_DATA_DIR ??= path.join(process.env.TWD_DATA_DIR ?? ".", "workflow-data");
const port = Number(process.env.PORT ?? 3000);
process.env.WORKFLOW_LOCAL_BASE_URL ??= `http://127.0.0.1:${port}`;

const flow = import(new URL("../.well-known/workflow/v1/flow.mjs", import.meta.url).href);

function json(res: http.ServerResponse, status: number, v: unknown) {
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify(v));
}

async function readBody(req: http.IncomingMessage) {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  return Buffer.concat(chunks);
}

function toRequest(req: http.IncomingMessage, body: Buffer) {
  const headers = new Headers();
  for (const [k, v] of Object.entries(req.headers)) if (typeof v === "string") headers.set(k, v);
  const hasBody = req.method !== "GET" && req.method !== "HEAD";
  return new Request(`http://127.0.0.1:${port}${req.url}`, { method: req.method, headers, body: hasBody ? body : undefined });
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url ?? "/", "http://x");
  try {
    const body = await readBody(req);
    if (url.pathname.startsWith("/.well-known/workflow/v1/flow")) {
      const r: Response = await (await flow).POST(toRequest(req, body));
      res.writeHead(r.status, Object.fromEntries(r.headers));
      res.end(Buffer.from(await r.arrayBuffer()));
      return;
    }
    if (url.pathname === "/healthz") return json(res, 200, { ok: true, pid: process.pid });
    // Everything else is a tailnet API: verify the caller.
    const claims = requireCaller(req);
    let m: RegExpMatchArray | null;
    if (req.method === "POST" && url.pathname === "/refunds") {
      const { order, usd, mcp, sleep } = JSON.parse(body.toString() || "{}");
      const run = await start(supportWorkflow, [{ order, usd, mcp, sleep, requestedBy: claims.caller.node }]);
      return json(res, 202, { runId: run.runId, approveToken: `approve:${order}` });
    }
    if ((m = url.pathname.match(/^\/runs\/([^/]+)$/))) {
      const run = getRun(m[1]);
      const status = await run.status;
      return json(res, 200, { runId: m[1], status, result: status === "completed" ? await run.returnValue : undefined });
    }
    if ((m = url.pathname.match(/^\/hooks\/(.+)$/))) {
      const token = decodeURIComponent(m[1]);
      if (req.method === "POST") {
        await resumeHook(token, { approved: true, by: claims.caller.node });
        return json(res, 200, { resumed: token });
      }
      const h = await getHookByToken(token).catch(() => null);
      return json(res, h ? 200 : 404, { token, exists: !!h });
    }
    if (url.pathname === "/whoami") return json(res, 200, claims);
    json(res, 404, { code: "not_found" });
  } catch (e: any) {
    if (e instanceof CallerError) {
      res.writeHead(401, { "content-type": "application/json", "twd-error": e.code });
      return res.end(JSON.stringify({ code: e.code, message: e.message }));
    }
    console.error(e);
    json(res, 500, { code: "internal", message: String(e?.message ?? e) });
  }
});

server.listen(port, "127.0.0.1", async () => {
  // Recover runs that were in flight when the previous process died.
  const world: any = await getWorld();
  await world.start?.();
  console.log(`node-workflow listening on 127.0.0.1:${port} (pid ${process.pid})`);
});
