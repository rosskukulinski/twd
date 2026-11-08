// @twd/tailnet: portable access to the verified caller that netd stamps into
// request.cf (RFC §4.5, §6.1). Tenant code should use getCaller(), never read
// request.cf.tailscale directly.

export class TailnetError extends Error {
  constructor(code, message, extra = {}) {
    super(message ?? code);
    this.name = "TailnetError";
    this.code = code;
    Object.assign(this, extra);
    this.docUrl ??= "https://github.com/rosskukulinski/twd/blob/main/docs/errors.md#" + code;
  }
}

/**
 * getCaller returns the verified tailnet caller of a request delivered by netd.
 * Authorize on nodeId and tags; hostname is display-only.
 * @param {Request} req
 */
export function getCaller(req) {
  const c = req?.cf?.tailscale?.caller;
  if (!c || !c.nodeId) throw new TailnetError("no_identity", "request has no verified tailnet caller (not delivered by netd?)");
  const capMap = c.capMap ?? {};
  return Object.freeze({
    nodeId: c.nodeId,
    tags: Object.freeze([...(c.tags ?? [])]),
    hostname: c.hostname ?? "",
    node: c.node ?? "",
    addrs: Object.freeze([...(c.addrs ?? [])]),
    user: c.user ? Object.freeze({ login: c.user }) : undefined,
    objectId: req.cf.objectId,
    chain: Object.freeze([Object.freeze({ nodeId: c.nodeId, tags: Object.freeze([...(c.tags ?? [])]) })]),
    traceId: req.cf.traceId ?? "",
    /** caps returns the JSON values granted for an app capability (union of grants). */
    caps(name) {
      return (capMap[name] ?? []).map((v) => structuredClone(v));
    },
  });
}
