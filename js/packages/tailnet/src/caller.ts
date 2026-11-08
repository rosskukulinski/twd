import { TailnetError } from "./errors.js";

export interface Caller {
  /** Stable node ID; the primary key to authorize on. */
  nodeId: string;
  tags: readonly string[];
  /** Display only; never authorize on it. */
  hostname: string;
  /** MagicDNS FQDN. */
  node: string;
  addrs: readonly string[];
  /** Untagged (human) callers only. */
  user?: { login: string };
  actingFor?: { sub: string; login?: string; groups: string[]; verified: "twd-assertion" | "control" };
  /** Nearest hop first. */
  chain: ReadonlyArray<{ nodeId: string; tags: readonly string[] }>;
  /** The object (node) the request was delivered to. */
  objectId: string;
  traceId: string;
  /** JSON values granted for an app capability (union of all grants). */
  caps<T = unknown>(name: string): T[];
}

interface WireCaller {
  nodeId?: string;
  node?: string;
  hostname?: string;
  addrs?: string[];
  tags?: string[];
  user?: string;
  capMap?: Record<string, unknown[]>;
}

/**
 * getCaller returns the verified tailnet caller of a request delivered by
 * netd (request.cf.tailscale). Throws TailnetError("no_identity") otherwise.
 * request.cf survives entry Worker → DO stub → facet, so pass the original
 * request along (or its cf) when you forward it in-process.
 */
export function getCaller(req: Request | { cf?: any } | null | undefined): Caller {
  const cf = (req as any)?.cf;
  const c: WireCaller | undefined = cf?.tailscale?.caller;
  if (!c || !c.nodeId) throw new TailnetError("no_identity", "request has no verified tailnet caller (not delivered by netd?)");
  const capMap = c.capMap ?? {};
  const tags = Object.freeze([...(c.tags ?? [])]);
  return Object.freeze({
    nodeId: c.nodeId,
    tags,
    hostname: c.hostname ?? "",
    node: c.node ?? "",
    addrs: Object.freeze([...(c.addrs ?? [])]),
    user: c.user ? Object.freeze({ login: c.user }) : undefined,
    actingFor: undefined,
    objectId: cf.objectId ?? "",
    chain: Object.freeze([Object.freeze({ nodeId: c.nodeId, tags })]),
    traceId: cf.traceId ?? "",
    caps<T = unknown>(name: string): T[] {
      return (capMap[name] ?? []).map((v) => structuredClone(v) as T);
    },
  });
}

/** principalKey is a stable per-human (or per-node) key for budgets and DO names. */
export function principalKey(who: Caller): string {
  return who.actingFor?.login ?? who.user?.login ?? who.nodeId;
}
