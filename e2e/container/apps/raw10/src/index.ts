// The Sandbox SDK 1.0 flow on the raw Durable Object container API, as
// Cloudflare documents it (developers.cloudflare.com/sandbox, 1.0): a plain
// DO starts its container with an explicit image and entrypoint, runs
// commands with ctx.container.exec(argv).output(), and moves files with the
// SDK's Files helper (sandbox-shim in the image). twd runs it unmodified:
// ctx.container is twd's polyfill.
import { Files } from "@cloudflare/sandbox";
import { DurableObject } from "cloudflare:workers";

interface Env {
  SANDBOX: DurableObjectNamespace<Sandbox>;
}

// Stand-in for Cloudflare's non-public cloudflare/debian-trixie (guest/images).
const IMAGE = "twd.local/debian-trixie-python:1";

export class Sandbox extends DurableObject<Env> {
  private ensure(): Container {
    const c = this.ctx.container!;
    if (!c.running) c.start({ image: IMAGE, entrypoint: ["sleep", "infinity"], enableInternet: false });
    return c;
  }

  async run(argv: string[]) {
    const c = this.ensure();
    const p = await c.exec(argv, { cwd: "/workspace" });
    const o = await p.output();
    const d = new TextDecoder();
    return { exitCode: o.exitCode, stdout: d.decode(o.stdout), stderr: d.decode(o.stderr) };
  }

  async writeRead(path: string, text: string) {
    const files = new Files(this.ensure());
    await files.writeFile(path, new Blob([text]).stream());
    const r = await files.readFile(path);
    return await r.text();
  }

  async destroy() {
    const c = this.ctx.container!;
    if (c.running) await c.destroy();
  }
}

export default {
  async fetch(req: Request, env: Env): Promise<Response> {
    const u = new URL(req.url);
    const [, op, name = "raw"] = u.pathname.split("/");
    const s = env.SANDBOX.getByName(name);
    if (op === "run") return Response.json(await s.run(["python3", "-c", "print(2 + 2)"]));
    if (op === "file") return Response.json({ content: await s.writeRead("/workspace/hello.txt", "Hello, Sandbox!") });
    if (op === "destroy") {
      await s.destroy();
      return Response.json({ ok: true });
    }
    return new Response("Try /run or /file", { status: 404 });
  },
} satisfies ExportedHandler<Env>;
