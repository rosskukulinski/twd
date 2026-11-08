#!/usr/bin/env node
// create-twd: scaffold a twd app from a template, or bundle one.
//   npm create twd@latest <dir> -- --template hello|agent|mcp
//   create-twd build [dir]        (bundles wrangler.jsonc main → dist/index.js)
//   create-twd build-node [dir] [outdir]  (runtime "node" apps: dist/server.mjs + Workflow bundles)
import { cpSync, existsSync, readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const templates = path.join(here, "..", "templates");
const args = process.argv.slice(2);

if (args[0] === "build") {
  const { buildApp } = await import("../lib/build.mjs");
  const r = await buildApp(path.resolve(args[1] ?? "."));
  console.log(`built ${r.outfile} (${r.bytes} bytes)`);
  process.exit(0);
}

if (args[0] === "build-node") {
  const { buildNodeApp } = await import("../lib/build-node.mjs");
  const dir = path.resolve(args[1] ?? ".");
  const r = await buildNodeApp(dir, path.resolve(args[2] ?? path.join(dir, ".twd-build")));
  console.log(`built ${r.outdir}/${r.main} (${r.bytes} bytes${r.workflows ? ", with Vercel Workflow bundles" : ""})`);
  process.exit(0);
}

const ti = args.indexOf("--template");
const template = ti >= 0 ? args[ti + 1] : "hello";
const dir = args.find((a, i) => !a.startsWith("--") && i !== ti + 1) ?? template;
const available = readdirSync(templates);
if (!available.includes(template)) {
  console.error(`create-twd: unknown template "${template}" (available: ${available.join(", ")})`);
  process.exit(2);
}
if (existsSync(dir) && readdirSync(dir).length) {
  console.error(`create-twd: ${dir} exists and is not empty`);
  process.exit(2);
}
cpSync(path.join(templates, template), dir, { recursive: true });
console.log(`created ${dir} from template ${template}\n  cd ${dir} && npm install && npx twd deploy`);
