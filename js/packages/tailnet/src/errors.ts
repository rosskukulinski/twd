// The typed error catalog (DX.md "Error catalog", RFC §6.1). Every denial
// has the same shape: from fetch() an HTTP status with a `twd-error: <code>`
// header and a JSON body {code, message|detail, missing?, snippet?, doc_url,
// trace_id?}; from ctx.tailnet.* and the @twd/mcp helpers a thrown
// TailnetError.

export type TailnetErrorCode =
  | "peer_not_visible"
  | "forbidden_destination"
  | "acl_denied"
  | "cap_missing"
  | "cap_limit_exceeded"
  | "delegation_invalid"
  | "delegation_exceeds_parent"
  | "spawn_budget_exceeded"
  | "bootstrap_failed"
  | "identity_pending"
  | "derp_unreachable"
  | "plan_feature_missing"
  | "killed"
  | "object_killed"
  | "no_identity"
  | "lease_lost"
  | "namespace_conflict"
  | "bad_request"
  | "unknown_object"
  | "dial_failed"
  | "upstream_unavailable"
  | "hostd_unavailable";

export interface CatalogEntry {
  /** HTTP status used when the error is rendered as a response. */
  status: number;
  /** One-line meaning. */
  title: string;
}

export const DOC_BASE = "https://github.com/rosskukulinski/twd/blob/main/docs/errors.md#";

export const CATALOG: Readonly<Record<TailnetErrorCode, CatalogEntry>> = Object.freeze({
  peer_not_visible: { status: 403, title: "the destination is not in this node's netmap (a grant is missing, or the name does not exist)" },
  forbidden_destination: { status: 403, title: "the destination class is refused by netd (loopback, self, sibling, host, non-tailnet)" },
  acl_denied: { status: 403, title: "the peer is visible but the port or service is not granted" },
  cap_missing: { status: 403, title: "the caller lacks the app capability (or the tool) required" },
  cap_limit_exceeded: { status: 403, title: "within the granted tool, but above a numeric limit or budget" },
  delegation_invalid: { status: 403, title: "the on-behalf-of assertion failed verification" },
  delegation_exceeds_parent: { status: 403, title: "spawn asked for caps the parent does not have" },
  spawn_budget_exceeded: { status: 429, title: "too many live descendants or too deep a spawn chain" },
  bootstrap_failed: { status: 502, title: "the object's node could not be created (mint, token exchange or join failed)" },
  identity_pending: { status: 503, title: "the object's node is not ready yet" },
  derp_unreachable: { status: 502, title: "no direct path and no reachable DERP relay" },
  plan_feature_missing: { status: 501, title: "the tailnet's plan or control server lacks a feature twd can use" },
  killed: { status: 410, title: "the object was killed" },
  object_killed: { status: 410, title: "a new request reached a killed object (its node's tombstone answered)" },
  no_identity: { status: 401, title: "the request carries no verified tailnet caller" },
  lease_lost: { status: 409, title: "this host no longer holds the namespace lease" },
  namespace_conflict: { status: 409, title: "another workload of this tenant already owns this DO class + object name (class names must be unique per tenant)" },
  bad_request: { status: 400, title: "the request is malformed" },
  unknown_object: { status: 404, title: "no such object" },
  dial_failed: { status: 502, title: "the tailnet dial failed" },
  upstream_unavailable: { status: 503, title: "the platform upstream is unavailable" },
  hostd_unavailable: { status: 503, title: "the host agent is unavailable" },
});

export interface Missing {
  src?: string[];
  dst?: string[];
  ip?: string[];
  cap?: string;
  tool?: string;
  [k: string]: unknown;
}

export interface TailnetErrorInit {
  missing?: Missing;
  snippet?: string;
  docUrl?: string;
  traceId?: string;
  status?: number;
  [k: string]: unknown;
}

const BRAND = Symbol.for("twd.TailnetError");

export class TailnetError extends Error {
  readonly code: TailnetErrorCode | string;
  missing?: Missing;
  snippet?: string;
  docUrl: string;
  traceId: string;
  status: number;
  /** Human-readable detail (message may be JSON; see @twd/mcp). */
  detail: string;
  [k: string]: unknown;

  constructor(code: TailnetErrorCode | string, message?: string, init: TailnetErrorInit = {}) {
    super(message ?? (CATALOG as Record<string, CatalogEntry>)[code]?.title ?? code);
    this.name = "TailnetError";
    this.code = code;
    this.detail = message ?? this.message;
    const { missing, snippet, docUrl, traceId, status, ...rest } = init;
    Object.assign(this, rest);
    this.missing = missing;
    this.snippet = snippet;
    this.docUrl = docUrl ?? DOC_BASE + code;
    this.traceId = traceId ?? "";
    this.status = status ?? (CATALOG as Record<string, CatalogEntry>)[code]?.status ?? 500;
    Object.defineProperty(this, BRAND, { value: true });
  }

  /** The wire body (DX.md shape). */
  toJSON(): Record<string, unknown> {
    const o: Record<string, unknown> = { code: this.code, message: this.detail, doc_url: this.docUrl };
    if (this.missing) o.missing = this.missing;
    if (this.snippet) o.snippet = this.snippet;
    if (this.traceId) o.trace_id = this.traceId;
    for (const k of ["have", "chain", "limit", "used", "object", "destination"]) if (this[k] !== undefined) o[k] = this[k];
    return o;
  }

  /** Render as an HTTP response with the twd-error header. */
  toResponse(): Response {
    return new Response(JSON.stringify(this.toJSON()), {
      status: this.status,
      headers: { "content-type": "application/json", "twd-error": String(this.code), "cache-control": "no-store" },
    });
  }

  /** Build from a wire body ({code, message|detail, ...}). */
  static from(body: Record<string, any> | null | undefined, status?: number): TailnetError {
    const b = body ?? {};
    const code = typeof b.code === "string" && b.code ? b.code : "upstream_unavailable";
    const { code: _c, message, detail, doc_url, trace_id, missing, snippet, ...rest } = b;
    return new TailnetError(code, detail ?? message ?? code, { ...rest, missing, snippet, docUrl: doc_url, traceId: trace_id, status });
  }
}

/** isTailnetError works across bundle copies (no instanceof). */
export function isTailnetError(e: unknown): e is TailnetError {
  return !!e && typeof e === "object" && ((e as any)[BRAND] === true || (e as any).name === "TailnetError");
}

/**
 * fromResponse returns a TailnetError if r is a typed twd/netd denial
 * (it has a twd-error header), else null. The body is read from a clone.
 */
export async function fromResponse(r: Response): Promise<TailnetError | null> {
  const code = r.headers.get("twd-error");
  if (!code) return null;
  let body: Record<string, any> = { code };
  try {
    body = { code, ...(await r.clone().json()) };
  } catch {
    // non-JSON body: keep the header code
  }
  return TailnetError.from(body, r.status);
}
