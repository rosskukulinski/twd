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
  /**
   * Set when the caller is a twd object with a node certificate (ADR-010):
   * its object ID and the cap limit (`lim`) its spawner set. caps() is
   * already narrowed to CapMap ∩ lim by netd, so a sub-agent spawned with
   * narrower caps than its class tag's grants cannot use the difference.
   */
  twd?: { objectId: string; lim?: Record<string, unknown>; parent?: string; root?: string };
  /**
   * True when netd could not verify the caller's node certificate and
   * withheld its caps (fail closed): caps() is empty whatever the policy says.
   */
  capsWithheld: boolean;
  /** JSON values granted for an app capability (union of all grants, ∩ lim). */
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
  twd?: { objectId: string; lim?: Record<string, unknown>; parent?: string; root?: string };
  capsWithheld?: boolean;
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
    twd: c.twd ? Object.freeze({ objectId: c.twd.objectId, lim: c.twd.lim ? structuredClone(c.twd.lim) : undefined, parent: c.twd.parent, root: c.twd.root }) : undefined,
    capsWithheld: !!c.capsWithheld,
    caps<T = unknown>(name: string): T[] {
      return (capMap[name] ?? []).map((v) => structuredClone(v) as T);
    },
  });
}

/** principalKey is a stable per-human (or per-node) key for budgets and DO names. */
export function principalKey(who: Caller): string {
  return who.actingFor?.login ?? who.user?.login ?? who.nodeId;
}
