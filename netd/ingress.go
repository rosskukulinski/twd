package netd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/internal/caps"
	"tailscale.com/client/tailscale/apitype"
)

// Caller is the verified identity netd stamps into request.cf.tailscale.caller.
// Authorize on NodeID and Tags; Hostname and Node are for display.
type Caller struct {
	NodeID   string                       `json:"nodeId"`   // stable node ID
	Node     string                       `json:"node"`     // MagicDNS FQDN
	Hostname string                       `json:"hostname"` // first label of Node
	Addrs    []string                     `json:"addrs"`
	Tags     []string                     `json:"tags"`
	User     string                       `json:"user,omitempty"` // login, untagged (human) callers only
	CapMap   map[string][]json.RawMessage `json:"capMap"`
	// Object is set when the caller is a twd object with a node certificate
	// (ADR-010): CapMap has already been narrowed to CapMap ∩ Object.Lim.
	Object *CallerObject `json:"twd,omitempty"`
	// CapsWithheld is true when a certificate lookup failed and netd failed
	// closed: CapMap is empty regardless of the policy (RT-01).
	CapsWithheld bool `json:"capsWithheld,omitempty"`
}

// CallerObject is what the sender's node certificate says about it.
type CallerObject struct {
	ObjectID string          `json:"objectId"`
	Lim      json.RawMessage `json:"lim,omitempty"`
	ParentID string          `json:"parent,omitempty"`
	RootID   string          `json:"root,omitempty"`
}

// CertSource resolves a caller's node certificate (RT-01). Lookup returns
// the verified certificate of the twd object whose node is stableNodeID,
// ErrNoCert if that node is not a twd object of this tenant (the WhoIs
// CapMap then applies unchanged), or any other error, on which netd FAILS
// CLOSED: the caller's caps are withheld.
type CertSource interface {
	Lookup(ctx context.Context, stableNodeID string) (*CallerObject, error)
}

// ErrNoCert: the peer is not a twd object (a human, a plain tailnet node,
// an MCP server...). Its policy caps apply as WhoIs reports them.
var ErrNoCert = errors.New("netd: no node certificate for this peer")

// CertLookupTimeout bounds one certificate lookup on the ingress path.
const CertLookupTimeout = 3 * time.Second

// applyCert narrows c by the sender's certificate (if any). It never lets a
// lookup failure widen anything: on error the caps are emptied.
func (r *Registry) applyCert(ctx context.Context, c *Caller) {
	src := r.cfg.CallerCerts
	if src == nil || c.NodeID == "" {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, CertLookupTimeout)
	defer cancel()
	co, err := src.Lookup(lctx, c.NodeID)
	switch {
	case errors.Is(err, ErrNoCert):
		return
	case err != nil:
		r.log.Warn("node certificate lookup failed; withholding the caller's caps (fail closed)", "caller", c.NodeID, "err", RedactError(err))
		c.CapMap = map[string][]json.RawMessage{}
		c.CapsWithheld = true
		return
	}
	c.Object = co
	c.CapMap = caps.Intersect(c.CapMap, co.Lim)
}

// CFBlob is the JSON netd puts in HeaderCF; workerd exposes it as request.cf.
type CFBlob struct {
	Tailscale struct {
		Caller Caller `json:"caller"`
	} `json:"tailscale"`
	ObjectID string `json:"objectId"`
}

// CallerFromWhoIs converts a WhoIs response.
func CallerFromWhoIs(w *apitype.WhoIsResponse) Caller {
	c := Caller{CapMap: map[string][]json.RawMessage{}, Tags: []string{}, Addrs: []string{}}
	if w.Node != nil {
		c.NodeID = string(w.Node.StableID)
		c.Node = strings.TrimSuffix(w.Node.Name, ".")
		c.Hostname, _, _ = strings.Cut(c.Node, ".")
		c.Tags = append(c.Tags, w.Node.Tags...)
		for _, a := range w.Node.Addresses {
			c.Addrs = append(c.Addrs, a.Addr().String())
		}
		if len(w.Node.Tags) == 0 && w.UserProfile != nil {
			c.User = w.UserProfile.LoginName
		}
	}
	for k, vs := range w.CapMap {
		out := make([]json.RawMessage, 0, len(vs))
		for _, v := range vs {
			if json.Valid([]byte(v)) {
				out = append(out, json.RawMessage(v))
			} else {
				q, _ := json.Marshal(string(v))
				out = append(out, q)
			}
		}
		c.CapMap[string(k)] = out
	}
	return c
}

// isIdentityHeader reports whether a client-supplied header could be used to
// claim an identity or an object, and so must never reach workerd.
func isIdentityHeader(k string) bool {
	k = strings.ToLower(k)
	switch {
	case strings.HasPrefix(k, "x-twd"), strings.HasPrefix(k, "twd-"), strings.HasPrefix(k, "cf-"),
		strings.HasPrefix(k, "x-forwarded-"), strings.HasPrefix(k, "x-poi"):
		return true
	}
	switch k {
	case "tailnet-assertion", "forwarded", "x-real-ip", "true-client-ip", "proxy-authorization",
		"tailscale-user-login", "tailscale-user-name", "tailscale-user-profile-pic", "tailscale-app-capabilities":
		return true
	}
	return false
}

func stripIdentityHeaders(h http.Header) {
	for k := range h {
		if isIdentityHeader(k) {
			delete(h, k)
		}
	}
}

func (o *Object) serveIngress(w http.ResponseWriter, req *http.Request) {
	o.mu.Lock()
	lc, state := o.lc, o.state
	o.mu.Unlock()
	if state != StateReady {
		WriteError(w, newErr(CodeKilled, "object "+o.id+" is "+string(state)))
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	who, err := lc.WhoIs(ctx, req.RemoteAddr)
	cancel()
	if err != nil {
		WriteError(w, newErr(CodeNoIdentity, "WhoIs failed for the transport peer"))
		return
	}
	stripIdentityHeaders(req.Header)
	var blob CFBlob
	blob.Tailscale.Caller = CallerFromWhoIs(who)
	o.reg.applyCert(req.Context(), &blob.Tailscale.Caller)
	blob.ObjectID = o.id
	b, _ := json.Marshal(blob)
	req.Header.Set(HeaderCF, string(b))
	req.Header.Set(HeaderObject, o.id)
	up := o.reg.cfg.Upstream
	if up == nil {
		WriteError(w, newErr(CodeUpstreamUnavailable, "no upstream configured"))
		return
	}
	up.ServeHTTP(w, req)
}

// UnixUpstream returns an ingress Upstream that reverse-proxies to an HTTP
// server on a unix socket (the workerd ingress socket, configured with
// cfBlobHeader = HeaderCF). The original Host header is preserved.
func UnixUpstream(path string) http.Handler {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     60 * time.Second,
	}
	target := &url.URL{Scheme: "http", Host: "workerd"}
	return &httputil.ReverseProxy{
		Transport:     tr,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0), // errors are reported via ErrorHandler
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			KeepForwarded(pr)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			WriteError(w, newErr(CodeUpstreamUnavailable, "upstream: "+err.Error()))
		},
	}
}

// KeepForwarded copies X-Forwarded-For/-Proto/-Host from the inbound to the
// outbound request of a Rewrite-style httputil.ReverseProxy (which drops
// them by default). Safe only behind netd: netd strips every client copy
// on both listeners and sets them itself for public requests (ADR-029).
func KeepForwarded(pr *httputil.ProxyRequest) {
	for _, k := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host"} {
		if v := pr.In.Header.Values(k); len(v) > 0 {
			pr.Out.Header[k] = append([]string(nil), v...)
		}
	}
}
