package mgmt

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tailscale/peercred"

	"github.com/rosskukulinski/twd/internal/procsb"
)

// Principal is the authenticated caller of an API request.
type Principal struct {
	Kind   string   // "local" (unix socket, same uid), "host" (tag:twd-host), "user" (operator-tailnet human)
	HostID string   // hosts: the registered hostID (tsnet), or Twd-Host-ID (unix)
	NodeID string   // tsnet: caller's stable node ID
	Login  string   // users
	Tags   []string // tsnet: caller's tags
}

type principalKey struct{}

func principalOf(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}

// WithPrincipal attaches a principal (used by the listeners' auth wrappers).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// HostTag is the tag hosts carry on the operator tailnet.
const HostTag = "tag:twd-host"

// Server is the v0 HTTP API.
type Server struct {
	M *Mgmt
	// Admins, if set, restricts operator-tailnet human callers to these logins.
	Admins []string
}

func decodeB64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	e := *asAPIError(err)
	if e.DocURL == "" {
		e.DocURL = "https://twd.dev/errors/" + e.Code
	}
	e.TraceID = r.Header.Get("Twd-Trace-ID")
	if e.TraceID == "" {
		e.TraceID = traceID()
	}
	if e.Status == 0 {
		e.Status = 500
	}
	if e.Code == "identity_pending" {
		w.Header().Set("Retry-After", "1")
	}
	w.Header().Set("Twd-Error", e.Code)
	writeJSON(w, e.Status, e)
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<20))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return errf(400, "bad_request", "invalid JSON: %v", err)
	}
	return nil
}

// role gates.
const (
	roleHost  = 1 << iota // hostd endpoints
	roleAdmin             // CLI endpoints
)

func (s *Server) allowed(p Principal, roles int) bool {
	switch p.Kind {
	case "local":
		return true
	case "host":
		return roles&roleHost != 0
	case "user":
		if roles&roleAdmin == 0 {
			return false
		}
		return len(s.Admins) == 0 || slices.Contains(s.Admins, p.Login)
	}
	return false
}

func (s *Server) h(roles int, fn func(w http.ResponseWriter, r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalOf(r.Context())
		if !s.allowed(p, roles) {
			writeErr(w, r, errf(403, "forbidden", "caller (%s) may not call %s %s", p.Kind, r.Method, r.URL.Path))
			return
		}
		// tsnet hosts may only act as themselves.
		if p.Kind == "host" {
			if id := r.PathValue("id"); id != "" && strings.HasPrefix(r.URL.Path, "/v0/hosts/") && id != p.HostID {
				writeErr(w, r, errf(403, "forbidden", "host %s may not act as host %s", p.HostID, id))
				return
			}
		}
		v, err := fn(w, r)
		if err != nil {
			writeErr(w, r, err)
			return
		}
		if v != nil {
			writeJSON(w, http.StatusOK, v)
		}
	}
}

// hostOf names the calling host: tsnet hosts by WhoIs binding; local
// callers by the Twd-Host-ID header or ?host= ("" if neither).
func hostOf(r *http.Request) string {
	p := principalOf(r.Context())
	if p.Kind == "host" {
		return p.HostID
	}
	if h := r.Header.Get("Twd-Host-ID"); h != "" {
		return h
	}
	return r.URL.Query().Get("host")
}

// Handler returns the API mux.
func (s *Server) Handler() http.Handler {
	m := s.M
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })

	// Tenants.
	mux.HandleFunc("POST /v0/tenants", s.h(roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req TenantRequest
		if err := readJSON(r, &req); err != nil {
			return nil, err
		}
		t, err := m.CreateTenant(r.Context(), req)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"tenantID": t.ID}
		if info := m.IssuerInfo(t); info != nil && t.MintMode == MintWIF {
			out["wif"] = info
		}
		return out, nil
	}))
	mux.HandleFunc("GET /v0/tenants", s.h(roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		ts, err := m.store.ListTenants(r.Context())
		if ts == nil {
			ts = []Tenant{}
		}
		return ts, err
	}))
	mux.HandleFunc("GET /v0/tenants/{id}", s.h(roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		t, err := m.store.GetTenant(r.Context(), r.PathValue("id"))
		if err != nil {
			return nil, err
		}
		pub, err := m.OBOPublicKey(r.Context(), t.ID)
		if err != nil {
			return nil, err
		}
		return struct {
			Tenant
			WIF           map[string]any `json:"wif,omitempty"`
			MintQueue     int            `json:"mintQueue"`
			EffectiveRate float64        `json:"effectiveRegistrationsPerMinute"`
			// OBOPublicKey (RT-01) verifies this tenant's node certificates
			// (GET /v0/certs/{stableNodeID}); hosts pin it per tenant.
			OBOPublicKey string `json:"oboPublicKey"`
		}{t, m.IssuerInfo(t), m.QueueDepth(t.ID), m.throttle(t).Rate(), pub}, nil
	}))

	// Node certificates (ADR-010, RT-01): the receiving host fetches the
	// sender's certificate by its WhoIs stable node ID and narrows the
	// sender's CapMap by cert.lim.
	mux.HandleFunc("GET /v0/certs/{nodeID}", s.h(roleHost|roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		w.Header().Set("Cache-Control", "no-store")
		return m.NodeCert(r.Context(), r.URL.Query().Get("tenant"), r.PathValue("nodeID"))
	}))

	// Hosts.
	mux.HandleFunc("POST /v0/hosts/register", s.h(roleHost, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req RegisterRequest
		if err := readJSON(r, &req); err != nil {
			return nil, err
		}
		p := principalOf(r.Context())
		if p.Kind == "host" && p.HostID != "" && p.HostID != req.HostID {
			return nil, errf(403, "host_identity_mismatch", "this node is registered as host %s", p.HostID)
		}
		if err := m.RegisterHost(r.Context(), req, p.NodeID); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	}))
	mux.HandleFunc("POST /v0/hosts/{id}/heartbeat", s.h(roleHost, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req struct {
			LiveNodes   int                `json:"liveNodes"`
			Leases      []HeartbeatLease   `json:"leases"`
			Deployments []DeploymentStatus `json:"deployments"`
		}
		if err := readJSON(r, &req); err != nil {
			return nil, err
		}
		return m.Heartbeat(r.Context(), r.PathValue("id"), req.LiveNodes, req.Leases, req.Deployments...)
	}))
	mux.HandleFunc("GET /v0/hosts/{id}/assignments", s.h(roleHost, func(w http.ResponseWriter, r *http.Request) (any, error) {
		return m.Assignments(r.Context(), r.PathValue("id"))
	}))
	mux.HandleFunc("GET /v0/hosts", s.h(roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		hs, err := m.store.ListHosts(r.Context())
		if hs == nil {
			hs = []Host{}
		}
		return hs, err
	}))

	// Deployments.
	mux.HandleFunc("POST /v0/deployments", s.h(roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req DeployRequest
		if err := readJSON(r, &req); err != nil {
			return nil, err
		}
		return m.Deploy(r.Context(), req)
	}))
	mux.HandleFunc("GET /v0/deployments/{id}", s.h(roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		return m.GetDeployment(r.Context(), r.PathValue("id"))
	}))
	mux.HandleFunc("GET /v0/deployments", s.h(roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		ds, err := m.store.LatestDeployments(r.Context(), r.URL.Query().Get("tenant"))
		if err != nil {
			return nil, err
		}
		out := []DeploymentView{}
		for _, d := range ds {
			if v, err := m.GetDeployment(r.Context(), d.ID); err == nil {
				out = append(out, v)
			}
		}
		return out, nil
	}))
	mux.HandleFunc("POST /v0/tenants/{id}/preflight", s.h(roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req PreflightRequest
		if err := readJSON(r, &req); err != nil {
			return nil, err
		}
		return m.Preflight(r.Context(), r.PathValue("id"), req)
	}))
	mux.HandleFunc("GET /v0/status", s.h(roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{"ok": true, "pendingDeviceDeletes": m.PendingDeletes()}, nil
	}))
	mux.HandleFunc("POST /v0/tenants/{id}/purge", s.h(roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		return m.Purge(r.Context(), r.PathValue("id"))
	}))
	mux.HandleFunc("POST /v0/flush", s.h(roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		n := m.PendingDeletes()
		m.FlushDeletes()
		return map[string]any{"ok": true, "flushed": n}, nil
	}))
	mux.HandleFunc("GET /v0/deployments/{id}/bundle", s.h(roleHost|roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		d, err := m.store.GetDeployment(r.Context(), r.PathValue("id"))
		if err != nil {
			return nil, err
		}
		if p := principalOf(r.Context()); p.Kind == "host" && d.HostID != p.HostID {
			return nil, errf(403, "forbidden", "deployment %s is not assigned to host %s", d.ID, p.HostID)
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Write(d.Bundle)
		return nil, nil
	}))

	// Objects.
	mux.HandleFunc("POST /v0/objects", s.h(roleHost|roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req CreateObjectRequest
		if err := readJSON(r, &req); err != nil {
			return nil, err
		}
		if req.TraceID == "" {
			req.TraceID = r.Header.Get("Twd-Trace-ID")
		}
		return m.CreateObject(r.Context(), req)
	}))
	mux.HandleFunc("GET /v0/objects", s.h(roleHost|roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		return m.ListObjects(r.Context(), r.URL.Query().Get("tenant"), r.URL.Query().Get("workload"), r.URL.Query().Get("namespace"))
	}))
	mux.HandleFunc("GET /v0/objects/{id}", s.h(roleHost|roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		return m.GetObject(r.Context(), r.PathValue("id"))
	}))
	mux.HandleFunc("GET /v0/objects/{id}/credential", s.h(roleHost, func(w http.ResponseWriter, r *http.Request) (any, error) {
		epoch, err := strconv.ParseInt(r.URL.Query().Get("epoch"), 10, 64)
		if err != nil || epoch <= 0 {
			return nil, errf(400, "bad_request", "epoch query parameter required")
		}
		wait := 10 * time.Second
		if v := r.URL.Query().Get("wait"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 30 {
				wait = time.Duration(n) * time.Second
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		return m.Credential(r.Context(), r.PathValue("id"), hostOf(r), epoch, wait)
	}))
	mux.HandleFunc("GET /v0/volumes/key", s.h(roleHost, func(w http.ResponseWriter, r *http.Request) (any, error) {
		q := r.URL.Query()
		epoch, err := strconv.ParseInt(q.Get("epoch"), 10, 64)
		if err != nil || epoch <= 0 {
			return nil, errf(400, "bad_request", "epoch query parameter required")
		}
		w.Header().Set("Cache-Control", "no-store")
		key, err := m.VolumeKey(r.Context(), q.Get("tenant"), q.Get("workload"), hostOf(r), epoch)
		if err != nil {
			return nil, err
		}
		return VolumeKeyResponse{Key: key}, nil
	}))
	mux.HandleFunc("POST /v0/objects/{id}/ready", s.h(roleHost, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req ReadyRequest
		if err := readJSON(r, &req); err != nil {
			return nil, err
		}
		return m.Ready(r.Context(), r.PathValue("id"), hostOf(r), req)
	}))
	mux.HandleFunc("POST /v0/objects/{id}/remint", s.h(roleHost, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req RemintRequest
		if err := readJSON(r, &req); err != nil {
			return nil, err
		}
		return m.Remint(r.Context(), r.PathValue("id"), hostOf(r), req)
	}))
	mux.HandleFunc("POST /v0/objects/{id}/kill", s.h(roleHost|roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req KillRequest
		if err := readJSON(r, &req); err != nil {
			return nil, err
		}
		p := principalOf(r.Context())
		switch p.Kind {
		case "user":
			req.By = p.Login
		case "host":
			req.By = "host:" + p.HostID
			// Hosts may only kill objects in namespaces they hold.
			o, err := m.store.GetObject(r.Context(), r.PathValue("id"))
			if err != nil {
				return nil, err
			}
			if l, err := m.store.GetLease(r.Context(), o.Lease); err != nil || l.Holder != p.HostID {
				return nil, errf(403, "forbidden", "host %s does not hold %s", p.HostID, o.Lease)
			}
		default:
			req.By = r.Header.Get("Twd-Actor")
			if req.By == "" {
				req.By = "local"
			}
		}
		return m.Kill(r.Context(), r.PathValue("id"), req)
	}))

	// Audit.
	mux.HandleFunc("POST /v0/audit", s.h(roleHost, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req struct {
			Records []json.RawMessage `json:"records"`
		}
		if err := readJSON(r, &req); err != nil {
			return nil, err
		}
		host := hostOf(r)
		if host == "" {
			return nil, errf(400, "bad_request", "audit records need a host (Twd-Host-ID header on the unix socket)")
		}
		n, st, err := m.IngestAudit(r.Context(), host, req.Records)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "accepted": n, "chain": st}, nil
	}))
	mux.HandleFunc("GET /v0/audit", s.h(roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		recs, chains, err := m.QueryAudit(r.Context(), AuditFilter{ObjectID: q.Get("object"), ActingFor: q.Get("human"),
			TraceID: q.Get("trace"), TenantID: q.Get("tenant"), Chain: q.Get("chain"), Limit: limit})
		if err != nil {
			return nil, err
		}
		ok := true
		for _, c := range chains {
			ok = ok && c.OK
		}
		return map[string]any{"records": recs, "chains": chains, "verified": ok}, nil
	}))
	mux.HandleFunc("GET /v0/audit/verify", s.h(roleAdmin, func(w http.ResponseWriter, r *http.Request) (any, error) {
		chains, ok, err := m.VerifyAll(r.Context())
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": ok, "chains": chains}, nil
	}))
	return mux
}

// ---- listeners ----

// ListenUnix listens on a unix socket (0600 in a 0700 dir) and only accepts
// connections whose peer uid is ours.
func ListenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return nil, err
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	// Sandboxed workloads (Linux: procsb) are refused too, even with our
	// uid: on kernels whose Landlock can't refuse unix-socket connects
	// (ABI < 9), this is what keeps tenant code off the admin API.
	return procsb.Guard(&uidListener{Listener: ln, uid: strconv.Itoa(os.Getuid())}, nil), nil
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

type uidListener struct {
	net.Listener
	uid string
}

func (l *uidListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		cr, err := peercred.Get(c)
		if err == nil {
			if uid, ok := cr.UserID(); ok && uid == l.uid {
				return c, nil
			}
		}
		c.Close() // wrong uid or no creds: refuse
	}
}

// HostTokenHeader carries the per-host bearer token on the unix socket
// (RT-03). Same-uid peercred alone admits nobody: every request, host or
// admin, must present the token mgmt minted into the host's token file.
const HostTokenHeader = "Twd-Host-Token"

// LocalHandler wraps h for the unix socket: a request becomes the "local"
// principal only if it carries HostTokenHeader equal to token (constant
// time). An empty token fails closed: nothing is admitted.
func LocalHandler(h http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(HostTokenHeader)
		if token == "" {
			writeErr(w, r, errf(503, "host_token_unconfigured", "twd-mgmt has no host token configured (-host-token-file); the unix socket admits nobody"))
			return
		}
		if got == "" {
			writeErr(w, r, errf(401, "host_token_required", "missing %s (twd-mgmt's unix socket needs the per-host token from the token file)", HostTokenHeader))
			return
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeErr(w, r, errf(403, "host_token_invalid", "%s does not match this twd-mgmt's token", HostTokenHeader))
			return
		}
		h.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{Kind: "local"})))
	})
}

// LoadOrCreateHostToken reads the per-host token file (0600; 64 hex chars),
// creating it (and its 0700 directory) if missing. With rotate, a fresh
// token replaces any existing one. The token is never logged.
func LoadOrCreateHostToken(path string, rotate bool) (string, error) {
	if !rotate {
		if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) == 64 {
			if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
				return "", fmt.Errorf("host token %s must be mode 0600 (is %v)", path, st.Mode().Perm())
			}
			return strings.TrimSpace(string(b)), nil
		}
	}
	tok := NewHostToken()
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tok, nil
}

// NewHostToken returns a fresh 256-bit token as 64 hex characters.
func NewHostToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ReadHostToken reads a token file written by LoadOrCreateHostToken ("" if absent).
func ReadHostToken(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// WhoIsFunc resolves a tailnet caller (tsnet LocalClient.WhoIs).
type WhoIsFunc func(ctx context.Context, remoteAddr string) (nodeID string, tags []string, login string, err error)

// TailnetHandler authenticates operator-tailnet callers by WhoIs: tagged
// tag:twd-host nodes are hosts (bound to the hostID they registered with),
// untagged nodes are human users, other tagged nodes are refused.
func (s *Server) TailnetHandler(whois WhoIsFunc, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeID, tags, login, err := whois(r.Context(), r.RemoteAddr)
		if err != nil {
			writeErr(w, r, errf(401, "no_identity", "WhoIs failed for %s", r.RemoteAddr))
			return
		}
		p := Principal{NodeID: nodeID, Tags: tags}
		switch {
		case slices.Contains(tags, HostTag):
			p.Kind = "host"
			hosts, _ := s.M.store.ListHosts(r.Context())
			for _, hst := range hosts {
				if hst.NodeID == nodeID {
					p.HostID = hst.ID
				}
			}
			if p.HostID == "" && r.URL.Path != "/v0/hosts/register" {
				writeErr(w, r, errf(403, "host_not_registered", "node %s must POST /v0/hosts/register first", nodeID))
				return
			}
		case len(tags) == 0 && login != "":
			p.Kind, p.Login = "user", login
		default:
			writeErr(w, r, errf(403, "forbidden", "tagged node %v is not a twd host", tags))
			return
		}
		h.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}
