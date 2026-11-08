// enforceEgress (RT-08): raw net/tls/dns/dgram use is routed through the
// egress proxy as CONNECT tunnels or refused, so code that ignores the env
// proxies is still attributed (or fails typed). Cooperative by nature: this
// runs in the app's own process.
import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import net from "node:net";
import dns from "node:dns";
import dgram from "node:dgram";
import { enforceEgress, EgressError } from "../dist/index.js";

// The fake proxy lives in this process too: it must use the unpatched net.connect.
const rawProtoConnect = net.Socket.prototype.connect;
const seen = [];
let proxy, target, proxyPort, targetPort, refuseNext = null;

before(async () => {
  // A plain TCP server standing in for a tailnet peer: echoes upper-case.
  target = net.createServer((c) => c.on("data", (d) => c.write(d.toString().toUpperCase())));
  await new Promise((r) => target.listen(0, "127.0.0.1", r));
  targetPort = target.address().port;
  // A minimal CONNECT proxy like hostd's: records the request, optionally
  // refuses with a typed twd-error, else splices to the target.
  proxy = net.createServer((c) => {
    let buf = "";
    const onData = (d) => {
      buf += d.toString("latin1");
      const i = buf.indexOf("\r\n\r\n");
      if (i < 0) return;
      c.removeListener("data", onData);
      const head = buf.slice(0, i);
      seen.push(head);
      if (refuseNext) {
        c.end(`HTTP/1.1 403 Forbidden\r\ntwd-error: ${refuseNext}\r\ncontent-length: 0\r\n\r\n`);
        refuseNext = null;
        return;
      }
      const up = new net.Socket();
      rawProtoConnect.call(up, targetPort, "127.0.0.1", () => {
        c.write("HTTP/1.1 200 Connection Established\r\n\r\n");
        const rest = buf.slice(i + 4);
        if (rest) up.write(Buffer.from(rest, "latin1"));
        c.pipe(up).pipe(c);
      });
    };
    c.on("data", onData);
  });
  await new Promise((r) => proxy.listen(0, "127.0.0.1", r));
  proxyPort = proxy.address().port;
  process.env.TWD_EGRESS_PROXY = `http://twd:tok@127.0.0.1:${proxyPort}`;
  process.env.PORT = String(targetPort + 1); // "our own" app port (nothing listens; only the allow-list matters)
  enforceEgress();
  enforceEgress(); // idempotent
});

after(() => {
  proxy.close();
  target.close();
});

test("net.connect to a non-local destination becomes a CONNECT tunnel through the proxy", async () => {
  const sock = net.connect({ host: "peer.ts.net", port: 8080 });
  await new Promise((res, rej) => sock.once("connect", res).once("error", rej));
  sock.write("hi");
  const reply = await new Promise((r) => sock.once("data", (d) => r(d.toString())));
  assert.equal(reply, "HI");
  sock.destroy();
  const head = seen.at(-1);
  assert.match(head, /^CONNECT peer\.ts\.net:8080 HTTP\/1\.1/);
  assert.match(head, new RegExp("Proxy-Authorization: Basic " + Buffer.from("twd:tok").toString("base64")));
});

test("the proxy's typed refusal surfaces as the socket error", async () => {
  refuseNext = "forbidden_destination";
  const sock = net.connect(80, "169.254.169.254");
  const err = await new Promise((r) => sock.once("error", r).once("connect", () => r(new Error("connected?!"))));
  assert.ok(err instanceof EgressError, String(err));
  assert.equal(err.code, "forbidden_destination");
});

test("loopback other than the proxy and the app's own port is refused locally", async () => {
  const sock = net.connect(targetPort, "127.0.0.1"); // the target directly: not allowed
  const err = await new Promise((r) => sock.once("error", r).once("connect", () => r(new Error("connected?!"))));
  assert.equal(err.code, "forbidden_destination");
  assert.match(err.message, /loopback/);
  // The proxy itself and the own port stay reachable directly.
  const ok = net.connect(proxyPort, "127.0.0.1");
  await new Promise((res, rej) => ok.once("connect", res).once("error", rej));
  ok.destroy();
});

test("unix sockets other than the app's own are refused", async () => {
  const sock = net.connect("/var/run/docker.sock");
  const err = await new Promise((r) => sock.once("error", r));
  assert.equal(err.code, "forbidden_destination");
});

test("direct DNS and UDP are refused; localhost still resolves", async () => {
  await assert.rejects(dns.promises.lookup("example.com"), /forbidden|not available/);
  await assert.rejects(dns.promises.resolve4("example.com"), /not available/);
  const e = await new Promise((r) => dns.lookup("1.1.1.1.nip.io", (err) => r(err)));
  assert.equal(e.code, "forbidden_destination");
  const l = await new Promise((res, rej) => dns.lookup("localhost", (err, a) => (err ? rej(err) : res(a))));
  assert.ok(l);
  const u = dgram.createSocket("udp4");
  assert.throws(() => u.send(Buffer.from("x"), 53, "1.1.1.1"), /not available/);
});
