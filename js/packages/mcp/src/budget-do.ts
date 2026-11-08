// TwdBudget: the Durable Object behind @twd/mcp budget(). Declare it in
// wrangler.jsonc durable_objects and mark it `"node": false` in twd.jsonc
// (it is internal state; it needs no tailnet node of its own).
// @ts-ignore: provided by workerd
import { DurableObject } from "cloudflare:workers";

export class TwdBudget extends (DurableObject as any) {
  declare ctx: any;
  async reserve(amount: number, limit: number | null, day: string) {
    if (typeof amount !== "number" || !Number.isFinite(amount) || amount < 0) throw new Error("budget: amount must be a non-negative number");
    const k = "used:" + day;
    const used: number = this.ctx.storage.kv.get(k) ?? 0;
    if (limit !== null && used + amount > limit) return { ok: false, used, limit };
    this.ctx.storage.kv.put(k, used + amount);
    return { ok: true, used: used + amount, limit };
  }
  async used(day: string) {
    return this.ctx.storage.kv.get("used:" + day) ?? 0;
  }
}
