// A durable support workflow: an AI SDK WorkflowAgent (mock LLM) uses twd MCP
// tools over the tailnet, then the run sleeps and waits for a human approval
// hook. Kill the node process at any point: twd-hostd restarts it, the local
// World re-enqueues the run, and it resumes from its event log.
import { sleep, createHook } from "workflow";
import { WorkflowAgent } from "@ai-sdk/workflow";
import { z } from "zod";
import { MockLLM } from "./mock-llm";
import { callMcpTool, type ToolResult } from "./mcp";

export type SupportInput = { order: string; usd: number; mcp: string; requestedBy: string; sleep?: string };

async function now(label: string) {
  "use step";
  return { label, at: Date.now(), pid: process.pid };
}

export async function supportWorkflow(input: SupportInput) {
  "use workflow";
  const calls: ToolResult[] = [];
  const agent = new WorkflowAgent({
    model: new MockLLM([
      { tool: "lookup_order", input: { order: input.order } },
      { tool: "refund", input: { order: input.order, usd: input.usd } },
      { text: `triaged ${input.order}` },
    ]),
    system: "You are a support agent. Look up the order, then try the refund.",
    tools: {
      lookup_order: {
        description: "Look up an order",
        inputSchema: z.object({ order: z.string() }),
        execute: async (a: { order: string }) => {
          const r = await callMcpTool(input.mcp, "lookup_order", a);
          calls.push(r);
          return r;
        },
      },
      refund: {
        description: "Refund an order",
        inputSchema: z.object({ order: z.string(), usd: z.number() }),
        execute: async (a: { order: string; usd: number }) => {
          const r = await callMcpTool(input.mcp, "refund", a);
          calls.push(r);
          return r;
        },
      },
    },
  });
  const triage = await agent.generate({ prompt: `Customer ${input.requestedBy} wants a $${input.usd} refund for ${input.order}.` });
  const t0 = await now("triaged");
  await sleep((input.sleep ?? "3s") as any);
  const t1 = await now("slept");
  const hook = createHook<{ approved: boolean; by: string }>({ token: `approve:${input.order}` });
  const approval = await hook;
  const t2 = await now("approved");
  return { order: input.order, triage: triage.text, calls, approval, marks: [t0, t1, t2] };
}
