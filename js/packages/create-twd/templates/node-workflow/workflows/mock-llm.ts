// A scripted LanguageModelV4 (no LLM, no keys): turn N of the conversation
// returns script[N] — a tool call or the final text. It crosses workflow step
// boundaries, so it implements the Workflow SDK's class serialization.
import type { LanguageModelV4, LanguageModelV4CallOptions, LanguageModelV4StreamPart } from "@ai-sdk/provider";

export type Turn = { tool: string; input: Record<string, unknown> } | { text: string };

const usage = (n: number) => ({
  inputTokens: { total: n, noCache: n, cacheRead: 0, cacheWrite: 0 },
  outputTokens: { total: 8, text: 8, reasoning: 0 },
});

export class MockLLM implements LanguageModelV4 {
  readonly specificationVersion = "v4" as const;
  readonly provider = "twd-mock";
  readonly modelId: string;
  readonly supportedUrls = {};
  constructor(readonly script: Turn[], id = "scripted") {
    this.modelId = id;
  }

  static [Symbol.for("workflow-serialize")](m: MockLLM) {
    return { script: m.script, id: m.modelId };
  }
  static [Symbol.for("workflow-deserialize")](d: { script: Turn[]; id: string }) {
    return new MockLLM(d.script, d.id);
  }

  private turn(opts: LanguageModelV4CallOptions): Turn {
    // Each tool round trip adds one tool message to the prompt.
    const done = opts.prompt.filter((m) => m.role === "tool").length;
    return this.script[Math.min(done, this.script.length - 1)];
  }

  async doGenerate(opts: LanguageModelV4CallOptions): Promise<any> {
    const t = this.turn(opts);
    const content = "tool" in t
      ? [{ type: "tool-call", toolCallId: `call-${t.tool}-${opts.prompt.length}`, toolName: t.tool, input: JSON.stringify(t.input) }]
      : [{ type: "text", text: t.text }];
    return { content, finishReason: { unified: "tool" in t ? "tool-calls" : "stop", raw: undefined }, usage: usage(opts.prompt.length), warnings: [] };
  }

  async doStream(opts: LanguageModelV4CallOptions): Promise<any> {
    const t = this.turn(opts);
    const parts: LanguageModelV4StreamPart[] = [{ type: "stream-start", warnings: [] }];
    if ("tool" in t) {
      parts.push({ type: "tool-call", toolCallId: `call-${t.tool}-${opts.prompt.length}`, toolName: t.tool, input: JSON.stringify(t.input) });
    } else {
      parts.push({ type: "text-start", id: "t" }, { type: "text-delta", id: "t", delta: t.text }, { type: "text-end", id: "t" });
    }
    parts.push({ type: "finish", finishReason: { unified: "tool" in t ? "tool-calls" : "stop", raw: undefined }, usage: usage(opts.prompt.length) } as any);
    return { stream: new ReadableStream({ start(c) { parts.forEach((p) => c.enqueue(p)); c.close(); } }) };
  }
}
