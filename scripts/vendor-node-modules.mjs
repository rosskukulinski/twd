// vendor-node-modules.mjs <repo> <out/node_modules> <pkg>...
// Copies the runtime dependency closure of <pkg>... from the repo's
// node_modules (exactly the versions the tests ran with; no network) into
// out, keeping node's nested layout. Workspace packages (symlinks, e.g.
// @twd/*) are copied as package.json + their "files". devDependencies,
// .bin and native/platform packages (workerd, esbuild) are left out.
import fs from "node:fs";
import path from "node:path";

const [repo, out, ...roots] = process.argv.slice(2);
const nm = path.join(repo, "node_modules");
const skip = (n) => n === "workerd" || n.startsWith("@cloudflare/workerd") || n === "esbuild" || n.startsWith("@esbuild/") || n === "typescript" || n.startsWith("@types/");
const seen = new Map(); // real dir -> rel
let bytes = 0;

function resolve(name, fromDir) {
  for (let d = fromDir; d.startsWith(nm); d = path.dirname(d)) {
    const c = path.join(d, "node_modules", name);
    if (fs.existsSync(path.join(c, "package.json"))) return c;
    if (d === nm) break;
  }
  const c = path.join(nm, name);
  return fs.existsSync(path.join(c, "package.json")) ? c : null;
}

function copyDir(src, dst, filter) {
  fs.mkdirSync(dst, { recursive: true });
  for (const e of fs.readdirSync(src, { withFileTypes: true })) {
    if (e.name === "node_modules" || e.name === ".bin") continue;
    if (filter && !filter(e.name)) continue;
    const s = path.join(src, e.name), d = path.join(dst, e.name);
    const st = fs.statSync(s);
    if (st.isDirectory()) copyDir(s, d);
    else { fs.copyFileSync(s, d); bytes += st.size; }
  }
}

function add(name, fromDir, optional) {
  if (skip(name)) return;
  const dir = resolve(name, fromDir);
  if (!dir) {
    if (!optional) throw new Error(`can't resolve ${name} from ${fromDir}`);
    return;
  }
  const rel = path.relative(nm, dir);
  if (seen.has(rel)) return;
  seen.set(rel, true);
  const real = fs.realpathSync(dir);
  const pj = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8"));
  // Native, platform-specific packages (os/cpu) can't be bundled for workerd.
  if (pj.os || pj.cpu) { seen.set(rel, "native"); return; }
  const dst = path.join(out, rel);
  if (real !== path.resolve(dir) && !real.startsWith(nm)) {
    // workspace package: package.json + "files"
    const files = new Set(["package.json", ...(pj.files ?? [])]);
    copyDir(real, dst, (n) => files.has(n));
  } else {
    copyDir(dir, dst);
  }
  for (const d of Object.keys(pj.dependencies ?? {})) add(d, dir, false);
  for (const d of Object.keys(pj.optionalDependencies ?? {})) add(d, dir, true);
  for (const d of Object.keys(pj.peerDependencies ?? {})) add(d, dir, true);
}

fs.mkdirSync(out, { recursive: true });
for (const r of roots) add(r, nm, false);
console.log(JSON.stringify({ packages: [...seen.values()].filter((v) => v === true).length, skippedNative: [...seen].filter(([, v]) => v === "native").map(([k]) => k), mb: +(bytes / 1048576).toFixed(1) }));
