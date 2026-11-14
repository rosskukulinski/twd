package netd

// Public ingress (ADR-029): a node created with ObjectOptions.Public serves
// the public internet over Funnel instead of the tailnet.
//
// CONTRACT (implemented in this file):
//
//   - The node never listens on the tailnet (no IngressPort listener, no
//     WhoIs). Its only listener comes from Config.PublicListen (default:
//     srv.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())), opened when
//     the node is ready AND the options say Enabled. Connections are
//     *ipn.FunnelConn (possibly wrapped in *tls.Conn); a connection that
//     is not a FunnelConn is closed at once (fail closed: nothing on this
//     listener is ever treated as a tailnet caller).
//   - Per request, in this order:
//     1. limits: MaxConns (excess connections are closed), MaxHeaderBytes
//     (431), per-workload token bucket RPS/Burst and per-IP bucket
//     IPRPS/IPBurst keyed by FunnelConn.Src's address (429 with
//     Retry-After, code rate_limited), MaxBodyBytes (413
//     payload_too_large, by Content-Length and by a capped reader),
//     Timeout (the request context; 504 on expiry if nothing was written);
//     2. route: publicspec.CanonicalPath (400 bad_request on error), then
//     publicspec.MatchAny(Routes) (404 not_public, never forwarded);
//     upgrades (Connection: upgrade / Upgrade header) only on a route in
//     Upgrade (403 upgrade_not_allowed);
//     3. headers: stripIdentityHeaders (as the tailnet path), then set
//     CF-Connecting-IP and X-Forwarded-For to the FunnelConn.Src address
//     (single value) and X-Forwarded-Proto: https;
//     4. cf blob (HeaderCF): {"tailscale": {"requester": PublicRequester},
//     "objectId": DeliverTo} — no "caller" key at all; HeaderObject =
//     DeliverTo (the workload node's netd ID, so hostd routes the request
//     to the workload's default export);
//     5. Config.Upstream.ServeHTTP.
//   - Error bodies use WriteError (catalog shape, twd-error header).
//   - After each request that reached step 5 (or was refused at step 2),
//     Config.OnPublicRequest (if set) is called with a PublicRequestRecord.
//     Requests refused at step 1 are NOT reported one by one: they are
//     counted and Config.OnPublicRejected (if set) is called at most once
//     per PublicRejectWindow per node with the counts (when non-zero).
//   - SetPublicEnabled(id, false) closes the listener and aborts every
//     public flow (RST, like Kill tier 1) and returns within a second;
//     SetPublicEnabled(id, true) reopens it. The node stays joined.
//   - Kill of a public node behaves like any object (tombstone etc.).

import (
	"net/netip"
	"time"

	"github.com/rosskukulinski/twd/internal/publicspec"
)

// PublicOptions make a node a public (Funnel) node.
type PublicOptions struct {
	// Routes and Upgrade are publicspec patterns (already validated).
	Routes  []string
	Upgrade []string
	// Limits are the effective limits (publicspec.Limits.Effective()).
	Limits publicspec.Limits
	// DeliverTo is the netd object ID stamped as HeaderObject/objectId:
	// the workload entry node this public node fronts.
	DeliverTo string
	// Enabled opens the listener once the node is ready (`twd expose`).
	Enabled bool
}

// PublicRequester is cf.tailscale.requester for a public request.
type PublicRequester struct {
	Kind  string `json:"kind"` // always "public"
	IP    string `json:"ip"`
	Port  int    `json:"port,omitempty"`
	Proto string `json:"proto"` // "https"
	Host  string `json:"host"`  // the request's Host
}

// PublicRequestRecord is one public request, for audit (D7).
type PublicRequestRecord struct {
	ObjectID string // the public node's netd ID
	Time     time.Time
	Method   string
	Path     string // canonical path, never the query string
	Route    string // matched pattern ("" when refused as not_public)
	Status   int
	Code     string // twd-error code when netd refused it
	ClientIP netip.Addr
	BytesIn  int64
	BytesOut int64
	Duration time.Duration
}

// PublicRejectSummary aggregates requests refused by the limits.
type PublicRejectSummary struct {
	ObjectID       string
	From, To       time.Time
	RateLimited    int // 429
	TooLarge       int // 413 + 431
	ConnsRefused   int // over MaxConns
	TopIPs         []IPCount
	UniqueIPApprox int
}

// IPCount is one client IP and its count.
type IPCount struct {
	IP    netip.Addr `json:"ip"`
	Count int        `json:"count"`
}

// PublicRejectWindow is how often rejected requests are summarized.
const PublicRejectWindow = time.Minute

// SetPublicEnabled opens or closes a public node's Funnel listener
// (`twd expose on|off`). Closing aborts the node's public flows.
func (r *Registry) SetPublicEnabled(id string, on bool) error {
	return errNotImplemented
}

var errNotImplemented = newErr(CodeUpstreamUnavailable, "public ingress: not implemented")
