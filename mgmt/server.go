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

	"github.com/tailscale/peercred"

	"github.com/rosskukulinski/twd/internal/procsb"
)

// Principal is the authenticated caller of an API request.
type Principal struct {
	Kind   string   // "local" (unix socket + host token), "host" (tag:twd-host), "user" (tailnet admin: autogroup:admin / twd.dev/cap/admin)
	HostID string   // hosts: the registered hostID (tsnet), or Twd-Host-ID (unix)
	NodeID string   // tsnet: caller's stable node ID
	Login  string   // users: the WhoIs login (tagged admin nodes: their first tag)
	Tags   []string // tsnet: caller's tags
	// Caps are the app capabilities the caller holds toward mgmt (WhoIs
	// CapMap): twd.dev/cap/admin makes a tailnet caller an admin.
	Caps []string
}

// Actor names the principal for audit records (deployedBy, kill by):
// the WhoIs login (or tag + node) over the tailnet, the Twd-Actor header
// on the unix socket.
func (p Principal) Actor(r *http.Request) string {
	switch p.Kind {
	case "user":
		if p.NodeID != "" && len(p.Tags) > 0 {
			return p.Login + " (" + p.NodeID + ")"
		}
		return p.Login
	case "host":
		return "host:" + p.HostID
	}
	if a := r.Header.Get("Twd-Actor"); a != "" {
		return a
	}
	return "local"
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

// MgmtTag is mgmt's own tag; the admin grant targets it.
const MgmtTag = "tag:twd-mgmt"

// AdminCap is the app capability that makes a tailnet caller a twd admin.
// It is granted to tag:twd-mgmt; autogroup:admin members get it from the
// base snippet, and any group, user or tag may be granted it too.
const AdminCap = "twd.dev/cap/admin"

// AdminGrantSnippet is the policy grant that admits a source to mgmt's
// API (network path and the admin cap). src is "autogroup:admin" in the
// base snippet.
func AdminGrantSnippet(src string) string {
	return `{"src": ["` + src + `"], "dst": ["` + MgmtTag + `"], "ip": ["tcp:443", "tcp:80"], "app": {"` + AdminCap + `": [{}]}}`
}

// Server is the /v1 HTTP API (api/openapi.yaml; handlers in v1.go).
type Server struct {
	M *Mgmt
	// Admins, if set, are tailnet logins admitted as admins even without
	// AdminCap (an operator override; the cap is the normal path).
	Admins []string
	// Self describes mgmt's own tailnet node (GET /v1/whoami).
	Self SelfInfo
	// Listeners names what mgmt is serving on (GET /v1/status), e.g.
	// "tsnet:80", "tsnet:443", "funnel:443", "unix:/path" (local-dev).
	Listeners []string
	// Version is reported by GET /v1/status.
	Version string
}

// SelfInfo is mgmt's own identity, reported to callers so `twd link` can
// learn the endpoint and tailnet.
type SelfInfo struct {
	DNSName string `json:"dnsName,omitempty"` // twd-mgmt.<tailnet>.ts.net
	Tailnet string `json:"tailnet,omitempty"` // MagicDNS suffix
	HTTPS   bool   `json:"https"`             // the API is served on :443 with the node's cert
	Issuer  string `json:"issuer,omitempty"`  // WIF issuer base URL (Funnel)
	// Transport is how this request reached mgmt: "tsnet" (normal
	// operation) or "local-dev" (the unix socket behind --local-dev).
	Transport string `json:"transport,omitempty"`
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
		return slices.Contains(p.Caps, AdminCap) || slices.Contains(s.Admins, p.Login)
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

// WhoAmI is GET /v1/whoami.
type WhoAmI struct {
	Kind   string   `json:"kind"`
	Login  string   `json:"login,omitempty"`
	NodeID string   `json:"nodeID,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	HostID string   `json:"hostID,omitempty"`
	Admin  bool     `json:"admin"`
	Actor  string   `json:"actor"`
	Mgmt   SelfInfo `json:"mgmt"`
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

// WhoIsInfo is what WhoIs tells mgmt about a tailnet caller.
type WhoIsInfo struct {
	NodeID string   // stable node ID
	Name   string   // MagicDNS name
	Tags   []string // tagged nodes
	Login  string   // UserProfile.LoginName (tagged nodes: "tagged-devices")
	Caps   []string // app capabilities the caller holds toward mgmt (CapMap keys)
}

// WhoIsFunc resolves a tailnet caller (tsnet LocalClient.WhoIs).
type WhoIsFunc func(ctx context.Context, remoteAddr string) (WhoIsInfo, error)

// TailnetHandler authenticates tailnet callers by WhoIs (ADR-026):
//   - tag:twd-host nodes are hosts, bound to the hostID they registered with;
//   - callers holding AdminCap (granted to tag:twd-mgmt; autogroup:admin
//     members get it from the base snippet) or listed in Admins are admins,
//     whether users or tagged nodes;
//   - everyone else is refused with 403 not_admin and the grant to add.
func (s *Server) TailnetHandler(whois WhoIsFunc, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wi, err := whois(r.Context(), r.RemoteAddr)
		if err != nil {
			writeErr(w, r, errf(401, "no_identity", "WhoIs failed for %s", r.RemoteAddr))
			return
		}
		p := Principal{NodeID: wi.NodeID, Tags: wi.Tags, Caps: wi.Caps}
		switch {
		case slices.Contains(wi.Tags, HostTag):
			p.Kind = "host"
			hosts, _ := s.M.store.ListHosts(r.Context(), Page{})
			for _, hst := range hosts {
				if hst.NodeID == wi.NodeID {
					p.HostID = hst.ID
				}
			}
			if p.HostID == "" && !(r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/hosts/") && strings.Count(r.URL.Path, "/") == 3) {
				writeErr(w, r, errf(403, "host_not_registered", "node %s (%s) has not registered; PUT /v1/hosts/{id} first", wi.NodeID, HostTag))
				return
			}
		default:
			p.Kind = "user"
			p.Login = wi.Login
			if len(wi.Tags) > 0 {
				p.Login = wi.Tags[0]
			}
			if !s.allowed(p, roleAdmin) {
				who := wi.Login
				src := wi.Login
				if len(wi.Tags) > 0 {
					who = "node " + strings.TrimSuffix(wi.Name, ".") + " " + fmt.Sprint(wi.Tags)
					src = wi.Tags[0]
				}
				e := errf(403, "not_admin", "%s is not a twd admin: no %s capability from a grant to %s (and not in -admins)", who, AdminCap, MgmtTag)
				e.Snippet = AdminGrantSnippet(src)
				if src == "" {
					e.Snippet = AdminGrantSnippet("autogroup:admin")
				}
				e.Missing = map[string]any{"src": src, "dst": MgmtTag, "cap": AdminCap}
				writeErr(w, r, e)
				return
			}
		}
		h.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}
