// Calling a twd MCP server over the tailnet. The request leaves through
// hostd's egress proxy (@twd/node fetch), so the MCP server's WhoIs sees this
// deployment's own node and authorizes the tool by that node's app caps.
import { fetch as twdFetch } from "@twd/node";

export type ToolResult = { tool: string; isError: boolean; text: string; code?: string };

export async function callMcpTool(url: string, tool: string, args: Record<string, unknown>): Promise<ToolResult> {
  "use step";
  const r = await twdFetch(url, {
    method: "POST",
    headers: { "content-type": "application/json", accept: "application/json, text/event-stream", "mcp-protocol-version": "2025-06-18" },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: tool, arguments: args } }),
  });
  let body = await r.text();
  if (!r.ok) return { tool, isError: true, text: body, code: r.headers.get("twd-error") ?? `http_${r.status}` };
  for (const line of body.split("\n")) if (line.startsWith("data: ")) body = line.slice(6);
  const msg = JSON.parse(body);
  const text: string = msg.result?.content?.[0]?.text ?? msg.error?.message ?? "";
  let code: string | undefined;
  try { code = JSON.parse(text).code; } catch {}
  return { tool, isError: !!(msg.result?.isError || msg.error), text, code };
}
