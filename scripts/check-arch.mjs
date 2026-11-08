// check-arch.mjs <os> <arch> <file>...: every file must be an executable for
// <os>-<arch> (ELF for linux, Mach-O for darwin; arm64 or amd64). Reads the
// headers only. Exits 1 and names the mismatches otherwise.
import fs from "node:fs";

const [os, arch, ...files] = process.argv.slice(2);
const want = `${os}-${arch}`;

function kind(f) {
  const fd = fs.openSync(f, "r");
  const b = Buffer.alloc(20);
  fs.readSync(fd, b, 0, 20, 0);
  fs.closeSync(fd);
  if (b.readUInt32BE(0) === 0x7f454c46) {
    const m = b[5] === 1 ? b.readUInt16LE(18) : b.readUInt16BE(18);
    return "linux-" + ({ 183: "arm64", 62: "amd64" }[m] ?? `elf-machine-${m}`);
  }
  if (b.readUInt32LE(0) === 0xfeedfacf) {
    const cpu = b.readUInt32LE(4);
    return "darwin-" + ({ 0x0100000c: "arm64", 0x01000007: "amd64" }[cpu] ?? `macho-cpu-${cpu.toString(16)}`);
  }
  return "not-an-executable";
}

let bad = 0;
for (const f of files) {
  const k = kind(f);
  if (k !== want) {
    console.error(`check-arch: ${f} is ${k}, want ${want}`);
    bad++;
  }
}
if (bad) process.exit(1);
console.log(`release: ${files.length} binaries are ${want}`);
