// A scripted stand-in for an LLM (AI SDK `MockLanguageModelV4` from `ai/test`)
// so this template runs with no provider keys. Step 1 calls the MCP tools it
// was given; step 2 answers from their results.
//
// Swap in a real provider for production, e.g. an OpenAI-compatible gateway
// on your tailnet, called from this agent's own node:
//   import { createOpenAICompatible } from "@ai-sdk/openai-compatible";
//   const llm = createOpenAICompatible({ name: "gw", baseURL: "http://llm-gw/v1", fetch: ctx.tailnet.fetch });
//   export const model = () => llm("my-model");
import { MockLanguageModelV4 } from "ai/test";
import { simulateReadableStream } from "ai";

const usage = {
  inputTokens: { total: 10, noCache: 10, cacheRead: 0, cacheWrite: 0 },
  outputTokens: { total: 10, text: 10, reasoning: 0 },
};

/** The tool calls the scripted model makes, in order (only those the agent has). */
const PLAN: Array<{ tool: string; input: Record<string, unknown> }> = [
  { tool: "whoami", input: {} },
  { tool: "lookup_order", input: { order: "o1" } },
  { tool: "refund", input: { order: "o1", usd: 10 } },
];

export function model() {
  return new MockLanguageModelV4({
    modelId: "twd-scripted",
    doStream: async (opts: any) => {
      const prompt: any[] = opts.prompt ?? [];
      const results = prompt.filter((m) => m.role === "tool").flatMap((m) => m.content);
      const tools = new Set((opts.tools ?? []).map((t: any) => t.name));
      let chunks: any[];
      if (results.length === 0) {
        chunks = PLAN.filter((p) => tools.has(p.tool)).map((p, i) => ({
          type: "tool-call",
          toolCallId: `call-${i}`,
          toolName: p.tool,
          input: JSON.stringify(p.input),
        }));
        chunks.push({ type: "finish", finishReason: { unified: "tool-calls", raw: "tool_calls" }, usage });
      } else {
        const lines = results.map((r: any) => `${r.toolName}: ${JSON.stringify(r.output?.value ?? r.output)}`);
        const text = ["Tool results:", ...lines].join("\n");
        // Several deltas, so the response visibly streams.
        const deltas = text.match(/[\s\S]{1,40}/g) ?? [text];
        chunks = [
          { type: "text-start", id: "t0" },
          ...deltas.map((d) => ({ type: "text-delta", id: "t0", delta: d })),
          { type: "text-end", id: "t0" },
          { type: "finish", finishReason: { unified: "stop", raw: "stop" }, usage },
        ];
      }
      return { stream: simulateReadableStream({ chunks, chunkDelayInMs: 5 }) };
    },
  });
}
