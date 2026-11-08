import { test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { randomBytes } from "node:crypto";
import { signCaller, verifyCaller, getCaller, requireCaller, fetch, CallerError } from "../dist/index.js";

const key = randomBytes(32).toString("base64url");
const claims = () => ({
  v: 1,
  caller: { nodeId: "nA", node: "a.ts.net", hostname: "a", addrs: ["100.64.0.1"], tags: [], capMap: { "x/cap": [{ ok: 1 }] } },
  objectId: "o1",
  workload: "w",
  iat: Date.now(),
});

test("sign/verify round trip; getCaller on Request and IncomingMessage shapes", () => {
  const h = signCaller(claims(), key);
  assert.equal(verifyCaller(h, { key }).caller.nodeId, "nA");
  assert.equal(getCaller(new Request("http://x/", { headers: { "x-twd-caller": h } }), { key }).nodeId, "nA");
  assert.equal(getCaller({ headers: { "x-twd-caller": h } }, { key }).hostname, "a");
});

test("forged, tampered, wrong-key, expired and missing headers are rejected", () => {
  const h = signCaller(claims(), key);
  const [v, p, s] = h.split(".");
  const forged = JSON.parse(Buffer.from(p, "base64url"));
  forged.caller.nodeId = "nEvil";
  const tampered = `${v}.${Buffer.from(JSON.stringify(forged)).toString("base64url")}.${s}`;
  assert.throws(() => verifyCaller(tampered, { key }), CallerError);
  assert.throws(() => verifyCaller(signCaller(claims(), randomBytes(32)), { key }), /signature/);
  const old = claims();
  old.iat -= 120_000;
  assert.throws(() => verifyCaller(signCaller(old, key), { key }), /expired/);
  assert.throws(() => verifyCaller(undefined, { key }), /missing/);
  assert.throws(() => verifyCaller("v1.e30", { key }), /malformed/);
  assert.equal(getCaller({ headers: { "x-twd-caller": tampered } }, { key }), null);
  assert.equal(getCaller({ headers: { "x-twd-caller": [h, h] } }, { key }), null, "duplicate headers are refused");
  assert.throws(() => requireCaller({ headers: {} }, { key }), (e) => e.toResponse().status === 401);
});

test("fetch goes through the proxy in absolute form with proxy credentials", async () => {
  let seen;
  const srv = http.createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      seen = { method: req.method, url: req.url, auth: req.headers["proxy-authorization"], host: req.headers.host, body };
      res.setHeader("x-r", "1");
      res.end("pong");
    });
  });
  await new Promise((r) => srv.listen(0, "127.0.0.1", r));
  process.env.TWD_EGRESS_PROXY = `http://twd:s3cret@127.0.0.1:${srv.address().port}`;
  const r = await fetch("http://echo.ts.net:8080/p?q=1", { method: "POST", body: "hi" });
  assert.equal(await r.text(), "pong");
  assert.equal(r.headers.get("x-r"), "1");
  assert.deepEqual(seen, { method: "POST", url: "http://echo.ts.net:8080/p?q=1", auth: "Basic " + Buffer.from("twd:s3cret").toString("base64"), host: "echo.ts.net:8080", body: "hi" });
  srv.close();
});
