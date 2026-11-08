// buildApp bundles a twd app (wrangler.jsonc "main") into one ESM module at
// dist/index.js, the way wrangler does for Workers: workerd conditions, Node
// builtins mapped to node:* (needs "nodejs_compat"), cloudflare:* external.
// cloudflare:email is stubbed (email is not supported on twd).
import * as esbuild from "esbuild";
import { builtinModules } from "node:module";
import { readFileSync } from "node:fs";
import path from "node:path";

const builtins = new Set(builtinModules);

export function stripJSONC(s) {
  let out = "", inStr = false, esc = false;
  for (let i = 0; i < s.length; i++) {
    const c = s[i];
    if (inStr) { out += c; if (esc) esc = false; else if (c === "\\") esc = true; else if (c === '"') inStr = false; continue; }
    if (c === '"') { inStr = true; out += c; }
    else if (c === "/" && s[i + 1] === "/") { while (i < s.length && s[i] !== "\n") i++; out += "\n"; }
    else if (c === "/" && s[i + 1] === "*") { i += 2; while (i < s.length && !(s[i] === "*" && s[i + 1] === "/")) i++; i++; }
    else out += c;
  }
  return out.replace(/,(\s*[}\]])/g, "$1");
}

export function readJSONC(file) {
  return JSON.parse(stripJSONC(readFileSync(file, "utf8")));
}

export async function buildApp(dir, { outfile = "dist/index.js", minify = false } = {}) {
  const w = readJSONC(path.join(dir, "wrangler.jsonc"));
  if (!w.main) throw new Error("wrangler.jsonc has no main");
  const r = await esbuild.build({
    entryPoints: [path.join(dir, w.main)],
    outfile: path.resolve(dir, outfile),
    absWorkingDir: dir,
    bundle: true,
    format: "esm",
    platform: "neutral",
    target: "es2022",
    conditions: ["workerd", "worker", "browser"],
    mainFields: ["module", "main"],
    external: ["cloudflare:*", "node:*"],
    minify,
    // Module fallbacks for apps outside the twd repo (twd deploy sets this
    // to twd's own node_modules: @twd/tailnet, @twd/mcp, agents, …).
    nodePaths: (process.env.TWD_NODE_PATH || "").split(":").filter(Boolean),
    legalComments: "none",
    logLevel: "warning",
    metafile: true,
    plugins: [{
      name: "twd",
      setup(b) {
        b.onResolve({ filter: /^cloudflare:email$/ }, () => ({ path: "email", namespace: "twd-stub" }));
        b.onLoad({ filter: /.*/, namespace: "twd-stub" }, () => ({
          contents: "export class EmailMessage { constructor() { throw new Error('cloudflare:email is not supported on twd'); } }",
          loader: "js",
        }));
        b.onResolve({ filter: /^[a-z_][a-z0-9_/]*$/ }, (a) => {
          const base = a.path.split("/")[0];
          return builtins.has(base) || builtins.has(a.path) ? { path: "node:" + a.path, external: true } : undefined;
        });
      },
    }],
  });
  return { outfile: path.resolve(dir, outfile), bytes: Object.values(r.metafile.outputs)[0]?.bytes ?? 0 };
}

// CLI: node build.mjs <appdir> [outfile]
if (import.meta.url === `file://${process.argv[1]}`) {
  const dir = path.resolve(process.argv[2] ?? ".");
  buildApp(dir, { outfile: process.argv[3] ?? "dist/index.js" }).then(
    (r) => console.log(JSON.stringify(r)),
    (e) => { console.error(e.message); process.exit(1); },
  );
}
