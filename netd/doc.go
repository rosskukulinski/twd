// Package netd is twd's per-tenant tailnet data plane: one tsnet.Server per
// object (Durable Object instance), created asynchronously through a bounded
// join queue, each with its own persistent state directory.
//
// For every object netd provides:
//
//   - ingress: a listener on the object's node. Each request is attributed with
//     WhoIs (LocalClient.OmitAuth = true), every client-supplied identity header
//     is stripped, and the verified caller is stamped into the x-twd-cf blob that
//     workerd's cfBlobHeader turns into request.cf;
//   - egress: an attributed HTTP proxy. A request names its object in
//     x-twd-object (set by the trusted platform Egress entrypoint) and leaves via
//     that object's own node. Per ADR-019, destinations are resolved from the
//     object's netmap only, only tailnet IPs are allowed, the object itself, its
//     same-host siblings, the host, loopback and link-local are refused, and netd
//     never falls back to the host network or host DNS. Refusals are typed JSON
//     errors (peer_not_visible, forbidden_destination);
//   - kill: abort (RST) every in-flight flow, then Close the node, then call an
//     optional device-delete hook (kill tier 1 then tier 2).
//
// Auth keys come from a pluggable KeySource and are only requested for objects
// without saved state; restarts rejoin from the saved node key. Logging goes
// through a slog handler that redacts tskey-* secrets.
package netd
