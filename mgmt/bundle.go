package mgmt

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/tailscale/hujson"
)

// TwdConfig is the part of twd.jsonc mgmt reads.
type TwdConfig struct {
	// Tier is the requested isolation tier ("process", "microvm"; "" = the
	// tenant's default). Placement only sends a workload to hosts that offer
	// its tier.
	Tier string `json:"tier"`
	// Runtime is "" / "workerd" or "node" (Node apps always run in the
	// process tier).
	Runtime string `json:"runtime"`
	Tailnet struct {
		// Tags are the workload entry node's tags (the "_workload" namespace).
		Tags []string `json:"tags"`
		// Connects are declared dependencies (tags or names), preflighted by twd deploy.
		Connects []string `json:"connects"`
		Objects  map[string]struct {
			Tags   []string `json:"tags"`
			Agent  bool     `json:"agent"`
			TTL    string   `json:"ttl"`
			Node   *bool    `json:"node"`   // false: internal class, no tailnet node
			Egress string   `json:"egress"` // "workload": plain-DO mode
			// Fence "never": instances of this class tolerate running on two
			// hosts at once, so mgmt may move it off an unreachable host
			// without --force-takeover (ADR-028 §3).
			Fence string `json:"fence"`
		} `json:"objects"`
		Spawn struct {
			MaxLive  int `json:"maxLive"`
			MaxDepth int `json:"maxDepth"`
		} `json:"spawn"`
	} `json:"tailnet"`
}

// WorkloadNamespace is the object namespace of a workload's entry node
// (its default export); the object name is the workload name.
const WorkloadNamespace = "_workload"

// Classes returns the DO namespaces that get tailnet nodes, sorted by
// class, plus the implicit "_workload" namespace (tailnet.tags) when set.
// Classes with "node": false have no identity and are not namespaces.
func (c *TwdConfig) Classes() []ClassConfig {
	var out []ClassConfig
	for name, o := range c.Tailnet.Objects {
		if o.Node != nil && !*o.Node {
			continue
		}
		out = append(out, ClassConfig{Class: name, Tags: o.Tags, Agent: o.Agent, TTL: o.TTL})
	}
	if len(c.Tailnet.Tags) > 0 {
		out = append(out, ClassConfig{Class: WorkloadNamespace, Tags: c.Tailnet.Tags})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Class < out[j].Class })
	return out
}

// ParseTwdConfig parses twd.jsonc (JSONC/HuJSON).
func ParseTwdConfig(b []byte) (*TwdConfig, error) {
	std, err := hujson.Standardize(b)
	if err != nil {
		return nil, fmt.Errorf("twd.jsonc: %w", err)
	}
	var c TwdConfig
	if err := json.Unmarshal(std, &c); err != nil {
		return nil, fmt.Errorf("twd.jsonc: %w", err)
	}
	return &c, nil
}

// BundleConfig extracts twd.jsonc from a (optionally gzipped) tar bundle,
// preferring the shallowest one.
// BundleStateful reports whether moving this workload between hosts can
// split its Durable Object state (ADR-028 §3): true if any DO class, from
// twd.jsonc tailnet.objects or wrangler.jsonc/.json durable_objects, is not
// marked "fence": "never". It errs on the side of stateful: a wrangler.toml
// (not parsed here) or an unreadable bundle counts as stateful. reason names
// the classes or the uncertainty.
func BundleStateful(bundle []byte) (stateful bool, reason string) {
	files, err := bundleFiles(bundle, "twd.jsonc", "wrangler.jsonc", "wrangler.json", "wrangler.toml")
	if err != nil {
		return true, "bundle unreadable"
	}
	exempt := map[string]bool{}
	classes := map[string]bool{}
	if b, ok := files["twd.jsonc"]; ok {
		if cfg, err := ParseTwdConfig(b); err == nil {
			for name, o := range cfg.Tailnet.Objects {
				classes[name] = true
				if o.Fence == "never" {
					exempt[name] = true
				}
			}
		} else {
			return true, "twd.jsonc unreadable"
		}
	}
	for _, f := range []string{"wrangler.jsonc", "wrangler.json"} {
		b, ok := files[f]
		if !ok {
			continue
		}
		std, err := hujson.Standardize(b)
		var w struct {
			DurableObjects struct {
				Bindings []struct {
					ClassName string `json:"class_name"`
				} `json:"bindings"`
			} `json:"durable_objects"`
		}
		if err != nil || json.Unmarshal(std, &w) != nil {
			return true, f + " unreadable"
		}
		for _, b := range w.DurableObjects.Bindings {
			if b.ClassName != "" {
				classes[b.ClassName] = true
			}
		}
	}
	if _, ok := files["wrangler.toml"]; ok {
		return true, "wrangler.toml (not inspected; assumed to declare Durable Objects)"
	}
	var guarded []string
	for c := range classes {
		if !exempt[c] {
			guarded = append(guarded, c)
		}
	}
	sort.Strings(guarded)
	if len(guarded) == 0 {
		return false, ""
	}
	return true, "Durable Object classes " + strings.Join(guarded, ", ")
}

// bundleFiles returns the shallowest copy of each named file (by base name)
// in a tar or tar.gz bundle.
func bundleFiles(bundle []byte, names ...string) (map[string][]byte, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var r io.Reader = bytes.NewReader(bundle)
	if len(bundle) > 2 && bundle[0] == 0x1f && bundle[1] == 0x8b {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		r = gz
	}
	tr := tar.NewReader(r)
	out, depth := map[string][]byte{}, map[string]int{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("bundle: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		base := path.Base(name)
		if h.Typeflag != tar.TypeReg || !want[base] || strings.Contains(name, "node_modules/") {
			continue
		}
		d := strings.Count(name, "/")
		if old, ok := depth[base]; ok && old <= d {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return nil, err
		}
		out[base], depth[base] = b, d
	}
}

func BundleConfig(bundle []byte) (*TwdConfig, error) {
	var r io.Reader = bytes.NewReader(bundle)
	if len(bundle) > 2 && bundle[0] == 0x1f && bundle[1] == 0x8b {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		r = gz
	}
	tr := tar.NewReader(r)
	var best []byte
	bestDepth := 1 << 30
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("bundle: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if h.Typeflag != tar.TypeReg || path.Base(name) != "twd.jsonc" {
			continue
		}
		if d := strings.Count(name, "/"); d < bestDepth {
			b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
			if err != nil {
				return nil, err
			}
			best, bestDepth = b, d
		}
	}
	if best == nil {
		return nil, errors.New("bundle has no twd.jsonc")
	}
	return ParseTwdConfig(best)
}

// ParseTTL accepts a Go duration string ("10m") or a number of seconds.
func ParseTTL(v json.RawMessage) (time.Duration, error) {
	v = bytes.TrimSpace(v)
	if len(v) == 0 || string(v) == "null" {
		return 0, nil
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		if s == "" {
			return 0, nil
		}
		return time.ParseDuration(s)
	}
	var n float64
	if err := json.Unmarshal(v, &n); err != nil {
		return 0, fmt.Errorf("ttl: want a duration string or seconds")
	}
	return time.Duration(n * float64(time.Second)), nil
}
