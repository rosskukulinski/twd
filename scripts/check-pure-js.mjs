// check-pure-js.mjs <node_modules>: the vendored tree is shared by every
// release target, so it must hold no platform-specific code. Fails on
//   - native addons (*.node) or any ELF / Mach-O / PE / WASI-less binary,
//   - packages that declare "os" or "cpu" (npm platform packages),
//   - binding.gyp (a node-gyp native build).
// Install scripts are listed but allowed: vendoring copies files and never
// runs them. Prints a one-line summary on success, the offenders on failure.
import fs from "node:fs";
import path from "node:path";

const root = process.argv[2];
const bad = [];
const scripts = [];
let files = 0, pkgs = 0;
const magic = (f) => {
  const fd = fs.openSync(f, "r");
  const b = Buffer.alloc(4);
  const n = fs.readSync(fd, b, 0, 4, 0);
  fs.closeSync(fd);
  if (n < 4) return null;
  if (b.readUInt32BE(0) === 0x7f454c46) return "ELF";
  const le = b.readUInt32LE(0);
  if (le === 0xfeedfacf || le === 0xfeedface || b.readUInt32BE(0) === 0xcafebabe) return "Mach-O";
  if (b[0] === 0x4d && b[1] === 0x5a) return "PE";
  return null;
};
function walk(d) {
  for (const e of fs.readdirSync(d, { withFileTypes: true })) {
    const p = path.join(d, e.name);
    const rel = path.relative(root, p);
    if (e.isSymbolicLink()) continue;
    if (e.isDirectory()) { walk(p); continue; }
    files++;
    if (e.name.endsWith(".node")) bad.push(`${rel}: native addon`);
    else if (e.name === "binding.gyp") bad.push(`${rel}: node-gyp build`);
    else { const m = magic(p); if (m) bad.push(`${rel}: ${m} binary`); }
    if (e.name === "package.json") {
      let j;
      try { j = JSON.parse(fs.readFileSync(p, "utf8")); } catch { continue; }
      if (!j.name) continue;
      pkgs++;
      if (j.os || j.cpu) bad.push(`${rel}: platform package (os=${j.os} cpu=${j.cpu})`);
      const s = j.scripts || {};
      for (const k of ["preinstall", "install", "postinstall"]) if (s[k]) scripts.push(`${j.name} ${k}`);
    }
  }
}
walk(root);
if (bad.length) { console.log(bad.join("\n")); process.exit(1); }
console.log(`${pkgs} packages, ${files} files: no .node, ELF, Mach-O or PE files, no os/cpu packages; install scripts never run: ${scripts.join(", ") || "none"}`);
