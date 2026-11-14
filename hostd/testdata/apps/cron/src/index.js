// Test fixture for cron triggers: scheduled() records each run in a
// Durable Object; GET /runs returns them.
import { DurableObject } from "cloudflare:workers";

export class Log extends DurableObject {
  async add(run) {
    const runs = (await this.ctx.storage.get("runs")) ?? [];
    runs.push(run);
    await this.ctx.storage.put("runs", runs);
  }
  async list() {
    return (await this.ctx.storage.get("runs")) ?? [];
  }
}

export default {
  async fetch(req, env) {
    const url = new URL(req.url);
    if (url.pathname === "/runs") return Response.json(await env.LOG.getByName("log").list());
    return Response.json({ kind: "cron-fixture" });
  },
  async scheduled(controller, env, ctx) {
    const run = { cron: controller.cron, scheduledTime: controller.scheduledTime, type: controller.type ?? null, hasTailnet: !!ctx.tailnet, at: Date.now() };
    if (controller.cron === "0 0 1 1 *") throw new Error("boom from scheduled");
    if (controller.cron === "0 0 2 1 *") {
      await env.LOG.getByName("log").add({ ...run, phase: "start" });
      await new Promise((r) => setTimeout(r, Number(env.SLEEP_MS ?? 1500)));
      run.phase = "end";
    }
    if (controller.cron === "0 0 3 1 *") {
      try {
        const r = await fetch(env.ECHO_URL ?? "http://echo/from-cron");
        run.egress = { status: r.status, body: await r.text() };
      } catch (e) {
        run.egress = { error: String(e) };
      }
    }
    await env.LOG.getByName("log").add(run);
  },
};
