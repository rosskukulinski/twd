// buildNodeApp bundles a twd Node process tier app (twd.jsonc "runtime":
// "node") into a directory hostd runs with `node dist/server.mjs`:
//
//   dist/server.mjs            the app (twd.jsonc "main"), one ESM bundle for Node
//   .well-known/workflow/v1/   Vercel Workflow bundles (only if workflows/ exists)
//   package.json               {"type":"module","version":…} (see below)
//   twd-bundle.json            {"main":"dist/server.mjs","env":{…deploy vars}}
//
// Vercel Workflow: `"use workflow"`/`"use step"` files under workflows/ are
// compiled by the SDK's StandaloneBuilder (the same builder as `workflow
// build`), and the app bundle gets the SDK's "step" mode SWC transform (as
// the Nitro/Vite/rollup integrations do) so `start(fn)` sees fn.workflowId.
import * as esbuild from "esbuild";
import { builtinModules, createRequire } from "node:module";
import { existsSync, realpathSync, mkdirSync, readFileSync, rmSync, writeFileSync, cpSync } from "node:fs";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { readJSONC } from "./build.mjs";

function resolver(dir) {
  const paths = [path.join(dir, "node_modules"), ...(process.env.TWD_NODE_PATH || "").split(":").filter(Boolean)];
  return {
    paths,
    async load(spec) {
      for (const p of [dir, ...paths]) {
        try {
          const req = createRequire(path.join(p, "noop.js"));
          return await import(pathToFileURL(req.resolve(spec)).href);
        } catch {}
      }
      throw new Error(`cannot resolve ${spec} (install it in the app: npm install ${spec})`);
    },
    pkgVersion(name) {
      for (const p of [path.join(dir, "node_modules"), ...paths]) {
        const f = path.join(p, name, "package.json");
        if (existsSync(f)) return JSON.parse(readFileSync(f, "utf8")).version;
      }
      return undefined;
    },
  };
}

export async function buildNodeApp(dir, outdir, { vars = {}, minify = false } = {}) {
  // Workflow/step IDs are derived from paths relative to the app dir; use the
  // real path so they match what the SDK's own bundler sees (macOS /tmp is a symlink).
  dir = realpathSync(dir);
  const t = readJSONC(path.join(dir, "twd.jsonc"));
  if (t.runtime !== "node") throw new Error('twd.jsonc: not a "runtime": "node" app');
  if (!t.main) throw new Error('twd.jsonc: runtime "node" needs "main" (e.g. "src/server.ts")');
  const r = resolver(dir);
  rmSync(outdir, { recursive: true, force: true });
  mkdirSync(outdir, { recursive: true });
  const hasWorkflows = existsSync(path.join(dir, "workflows"));
  const timings = {};
  let swc;
  if (hasWorkflows) {
    const st = Date.now();
    const b = await r.load("@workflow/builders");
    // The SDK resolves imports from the output location, so it builds into
    // the app's own .well-known/ (as `workflow build` does); flow.mjs is
    // copied out below.
    const wk = path.join(dir, ".well-known", "workflow", "v1");
    const cwd = process.cwd();
    process.chdir(dir); // the SDK derives workflow/step IDs from paths relative to cwd
    try {
      await new b.StandaloneBuilder({
        dirs: ["./workflows"], workingDir: dir, buildTarget: "standalone",
        stepsBundlePath: path.join(wk, "__step_registrations.mjs"),
        workflowsBundlePath: path.join(wk, "flow.mjs"),
        webhookBundlePath: path.join(wk, "webhook.mjs"),
      }).build();
    } finally {
      process.chdir(cwd);
    }
    // flow.mjs is the combined (workflow + step) route bundle; the others are
    // build by-products the app does not load.
    const owk = path.join(outdir, ".well-known", "workflow", "v1");
    mkdirSync(owk, { recursive: true });
    for (const f of ["flow.mjs", "manifest.json"]) cpSync(path.join(wk, f), path.join(owk, f));
    timings.workflowMs = Date.now() - st;
    swc = b;
  }
  const st = Date.now();
  const builtins = new Set(builtinModules);
  const res = await esbuild.build({
    entryPoints: [path.join(dir, t.main)],
    outfile: path.join(outdir, "dist", "server.mjs"),
    absWorkingDir: dir,
    bundle: true,
    platform: "node",
    format: "esm",
    target: "node22",
    minify,
    nodePaths: r.paths,
    legalComments: "none",
    logLevel: "warning",
    metafile: true,
    banner: { js: "import { createRequire as __twdCreateRequire } from 'node:module'; const require = __twdCreateRequire(import.meta.url);" },
    plugins: [{
      name: "twd-node",
      setup(bld) {
        bld.onResolve({ filter: /^[a-z_][a-z0-9_/]*$/ }, (a) => {
          const base = a.path.split("/")[0];
          return builtins.has(base) || builtins.has(a.path) ? { path: "node:" + a.path, external: true } : undefined;
        });
        if (!swc) return;
        // "use step"/"use workflow" in app code (not node_modules): the step-mode
        // transform registers workflow IDs so start() can name them.
        bld.onLoad({ filter: /\.(m?[jt]sx?|c[jt]s)$/ }, async (a) => {
          if (a.path.includes(`${path.sep}node_modules${path.sep}`)) return;
          const src = await readFile(a.path, "utf8");
          const p = swc.detectWorkflowPatterns(src);
          if (!p.hasDirective && !p.hasSerde) return;
          const out = await swc.applySwcTransform(path.relative(dir, a.path), src, "step", a.path, dir);
          return { contents: out.code, loader: "js", resolveDir: path.dirname(a.path) };
        });
      },
    }],
  });
  timings.appMs = Date.now() - st;
  // @workflow/world-local reads <bundle dir>/../package.json for its own
  // version (data-dir migrations); give it one.
  const version = r.pkgVersion("@workflow/world-local") ?? "0.0.0";
  writeFileSync(path.join(outdir, "package.json"), JSON.stringify({ name: t.name ?? path.basename(dir), version, type: "module", private: true }, null, 2));
  writeFileSync(path.join(outdir, "twd-bundle.json"), JSON.stringify({ main: "dist/server.mjs", env: vars }));
  cpSync(path.join(dir, "twd.jsonc"), path.join(outdir, "twd.jsonc"));
  const bytes = Object.values(res.metafile.outputs).reduce((a, o) => a + o.bytes, 0);
  return { outdir, main: "dist/server.mjs", bytes, workflows: hasWorkflows, timings };
}

// CLI: node build-node.mjs <appdir> <outdir> [--var K=V ...]
if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  const dir = path.resolve(process.argv[2] ?? ".");
  const out = path.resolve(process.argv[3] ?? path.join(dir, ".twd-build"));
  const vars = {};
  for (let i = 4; i < process.argv.length; i++) {
    if (process.argv[i] === "--var") {
      const [k, ...v] = process.argv[++i].split("=");
      vars[k] = v.join("=");
    }
  }
  buildNodeApp(dir, out, { vars }).then(
    (r) => console.log(JSON.stringify(r)),
    (e) => { console.error(e.stack || e.message); process.exit(1); },
  );
}
