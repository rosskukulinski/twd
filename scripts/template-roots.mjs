// template-roots.mjs [templates dir]: the npm packages the release vendors
// into share/twd/node_modules, one per line: every dependency a workerd
// template declares (so those templates init + deploy offline), and, for
// Node-tier templates (twd.jsonc "runtime": "node"), only the unpublished
// workspace packages (@twd/*). A Node-tier app needs Node.js and its own
// `npm install` to build anyway (the Workflow builder resolves from the app
// dir), and its third-party deps are not in the root workspace.
import fs from "node:fs";
import path from "node:path";

const dir = process.argv[2] ?? "js/packages/create-twd/templates";
const roots = new Set();
for (const t of fs.readdirSync(dir)) {
  let pj;
  try { pj = JSON.parse(fs.readFileSync(path.join(dir, t, "package.json"), "utf8")); } catch { continue; }
  let jsonc = "";
  try { jsonc = fs.readFileSync(path.join(dir, t, "twd.jsonc"), "utf8"); } catch {}
  const nodeTier = /"runtime"\s*:\s*"node"/.test(jsonc);
  for (const k of Object.keys(pj.dependencies ?? {})) {
    if (!nodeTier || k.startsWith("@twd/")) roots.add(k);
  }
}
console.log([...roots].sort().join("\n"));
