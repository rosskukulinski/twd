// Package publicspec is the twd.jsonc "public" schema (ADR-029): which path
// prefixes of a workload are reachable from the public internet over
// Funnel, the ingress limits netd enforces on them, and the per-class
// opt-in for public object creation. It is shared by the bundle parser
// (hostd), mgmt (the deploy gate), netd (route matching and limits) and
// the CLI (deploy preflight), so they can never disagree.
package publicspec

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// Tag is the tag every public node carries, and the only twd tag the
// funnel nodeAttr may target besides tag:twd-mgmt (ADR-029 D6).
const Tag = "tag:twd-public"

// Namespace is the mgmt object namespace of a workload's public node; the
// object name is the workload name (like "_workload").
const Namespace = "_public"

// ReservedPrefix is never public, whatever the routes say: the platform's
// own /_twd/o/<Class>/<name> addressing would bypass the app's routing.
const ReservedPrefix = "/_twd"

// MaxRoutes bounds len(Routes).
const MaxRoutes = 64

// MaxObjectsCeiling is the most objects a class may let public requests
// create (ADR-029 D5).
const MaxObjectsCeiling = 1000

// CreatesPerMinute is the public-create lane: 10% of the ADR-017
// registration planning rate (200/min), per tenant.
const CreatesPerMinute = 20

// Spec is twd.jsonc "public".
type Spec struct {
	// Routes are path patterns: "/x" (exact), "/x/*" ("/x" and everything
	// under it) or "/*" (every path). Required (non-empty) when "public"
	// is present.
	Routes []string `json:"routes"`
	// Upgrade lists the routes (each must also be in Routes) on which
	// WebSocket/SSE-style upgrades are allowed. Default: none.
	Upgrade []string `json:"upgrade,omitempty"`
	// Limits override the defaults, up to the ceilings.
	Limits *Limits `json:"limits,omitempty"`
}

// Limits are netd's public-listener limits for one workload.
type Limits struct {
	RPS            float64 `json:"rps,omitempty"`            // per workload
	Burst          int     `json:"burst,omitempty"`          // per workload
	IPRPS          float64 `json:"ipRps,omitempty"`          // per client IP
	IPBurst        int     `json:"ipBurst,omitempty"`        // per client IP
	MaxBodyBytes   int64   `json:"maxBodyBytes,omitempty"`   // request body
	MaxHeaderBytes int     `json:"maxHeaderBytes,omitempty"` // request headers
	TimeoutSeconds int     `json:"timeoutSeconds,omitempty"` // whole request
	MaxConns       int     `json:"maxConns,omitempty"`       // concurrent connections
}

// Defaults are the limits when twd.jsonc sets none (ADR-029 D5).
var Defaults = Limits{RPS: 50, Burst: 100, IPRPS: 10, IPBurst: 20, MaxBodyBytes: 1 << 20,
	MaxHeaderBytes: 32 << 10, TimeoutSeconds: 30, MaxConns: 256}

// Ceilings are the most a workload may ask for (the operator ceiling).
var Ceilings = Limits{RPS: 1000, Burst: 2000, IPRPS: 200, IPBurst: 400, MaxBodyBytes: 32 << 20,
	MaxHeaderBytes: 64 << 10, TimeoutSeconds: 300, MaxConns: 4096}

// Effective fills unset fields from Defaults.
func (l *Limits) Effective() Limits {
	e := Defaults
	if l == nil {
		return e
	}
	if l.RPS > 0 {
		e.RPS = l.RPS
	}
	if l.Burst > 0 {
		e.Burst = l.Burst
	}
	if l.IPRPS > 0 {
		e.IPRPS = l.IPRPS
	}
	if l.IPBurst > 0 {
		e.IPBurst = l.IPBurst
	}
	if l.MaxBodyBytes > 0 {
		e.MaxBodyBytes = l.MaxBodyBytes
	}
	if l.MaxHeaderBytes > 0 {
		e.MaxHeaderBytes = l.MaxHeaderBytes
	}
	if l.TimeoutSeconds > 0 {
		e.TimeoutSeconds = l.TimeoutSeconds
	}
	if l.MaxConns > 0 {
		e.MaxConns = l.MaxConns
	}
	return e
}

// Timeout is TimeoutSeconds as a duration.
func (l Limits) Timeout() time.Duration { return time.Duration(l.TimeoutSeconds) * time.Second }

// ClassSpec is twd.jsonc tailnet.objects.<Class>.public.
type ClassSpec struct {
	// Create lets a public request create a new object of this class
	// (default false: public requests reach existing objects only).
	Create bool `json:"create,omitempty"`
	// MaxObjects is required with Create: the most objects of this class
	// public requests may create (≤ MaxObjectsCeiling).
	MaxObjects int `json:"maxObjects,omitempty"`
}

// Validate checks a class's public settings.
func (c *ClassSpec) Validate(class string) error {
	if c == nil {
		return nil
	}
	if !c.Create {
		if c.MaxObjects != 0 {
			return fmt.Errorf("tailnet.objects.%s.public.maxObjects needs \"create\": true", class)
		}
		return nil
	}
	if c.MaxObjects <= 0 || c.MaxObjects > MaxObjectsCeiling {
		return fmt.Errorf("tailnet.objects.%s.public: \"create\": true needs \"maxObjects\" between 1 and %d (got %d)", class, MaxObjectsCeiling, c.MaxObjects)
	}
	return nil
}

// Validate checks the spec and normalizes Routes/Upgrade (sorted, deduped).
func (s *Spec) Validate() error {
	if s == nil {
		return nil
	}
	if len(s.Routes) == 0 {
		return errors.New("public.routes: at least one route is required (use \"/*\" for every path)")
	}
	if len(s.Routes) > MaxRoutes {
		return fmt.Errorf("public.routes: at most %d routes", MaxRoutes)
	}
	seen := map[string]bool{}
	for _, r := range s.Routes {
		if err := validPattern(r); err != nil {
			return fmt.Errorf("public.routes: %w", err)
		}
		seen[r] = true
	}
	for _, u := range s.Upgrade {
		if !seen[u] {
			return fmt.Errorf("public.upgrade: %q is not one of public.routes", u)
		}
	}
	s.Routes, s.Upgrade = dedupe(s.Routes), dedupe(s.Upgrade)
	if l := s.Limits; l != nil {
		if err := l.validate(); err != nil {
			return fmt.Errorf("public.limits: %w", err)
		}
	}
	return nil
}

func (l *Limits) validate() error {
	c := Ceilings
	check := func(name string, v, max float64) error {
		if v < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
		if v > max {
			return fmt.Errorf("%s %v is above the ceiling %v", name, v, max)
		}
		return nil
	}
	for _, e := range []error{
		check("rps", l.RPS, c.RPS), check("burst", float64(l.Burst), float64(c.Burst)),
		check("ipRps", l.IPRPS, c.IPRPS), check("ipBurst", float64(l.IPBurst), float64(c.IPBurst)),
		check("maxBodyBytes", float64(l.MaxBodyBytes), float64(c.MaxBodyBytes)),
		check("maxHeaderBytes", float64(l.MaxHeaderBytes), float64(c.MaxHeaderBytes)),
		check("timeoutSeconds", float64(l.TimeoutSeconds), float64(c.TimeoutSeconds)),
		check("maxConns", float64(l.MaxConns), float64(c.MaxConns)),
	} {
		if e != nil {
			return e
		}
	}
	return nil
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return in
	}
	m := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func validPattern(p string) error {
	if p == "/*" {
		return nil
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("%q must start with /", p)
	}
	if strings.ContainsAny(p, "?#%\\ \t") {
		return fmt.Errorf("%q: no query, fragment, escapes or spaces", p)
	}
	base := strings.TrimSuffix(p, "/*")
	if strings.Contains(base, "*") {
		return fmt.Errorf("%q: \"*\" is only allowed as a trailing \"/*\"", p)
	}
	if base == "" || path.Clean(base) != base {
		return fmt.Errorf("%q is not a clean path (no //, ., .. or trailing /)", p)
	}
	if base == ReservedPrefix || strings.HasPrefix(base, ReservedPrefix+"/") {
		return fmt.Errorf("%q: %s is reserved for the platform and is never public", p, ReservedPrefix)
	}
	return nil
}

// Match reports whether a request path matches pattern. p must already be
// canonical (see CanonicalPath).
func Match(pattern, p string) bool {
	if pattern == "/*" {
		return true
	}
	if base, ok := strings.CutSuffix(pattern, "/*"); ok {
		return p == base || strings.HasPrefix(p, base+"/")
	}
	return p == pattern
}

// MatchAny returns the first pattern in pats matching p.
func MatchAny(pats []string, p string) (string, bool) {
	for _, pat := range pats {
		if Match(pat, p) {
			return pat, true
		}
	}
	return "", false
}

// CanonicalPath returns the request path netd matches routes against, or
// an error for a path that could be read differently by netd and by the
// app (dot segments, doubled slashes, encoded slashes or backslashes), or
// that is under ReservedPrefix. A trailing slash is kept.
func CanonicalPath(u *url.URL) (string, error) {
	p := u.Path
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		return "", errors.New("path must start with /")
	}
	raw := strings.ToLower(u.RawPath)
	if strings.Contains(raw, "%2f") || strings.Contains(raw, "%5c") || strings.Contains(p, "\\") {
		return "", errors.New("encoded slashes and backslashes are refused")
	}
	trimmed := p
	if len(p) > 1 {
		trimmed = strings.TrimSuffix(p, "/")
	}
	if path.Clean(trimmed) != trimmed {
		return "", errors.New("non-canonical path (dot segments or doubled slashes)")
	}
	if trimmed == ReservedPrefix || strings.HasPrefix(trimmed, ReservedPrefix+"/") {
		return "", errors.New(ReservedPrefix + " is never public")
	}
	return p, nil
}
