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
	Tailnet struct {
		Objects map[string]struct {
			Tags  []string `json:"tags"`
			Agent bool     `json:"agent"`
			TTL   string   `json:"ttl"`
		} `json:"objects"`
		Spawn struct {
			MaxLive  int `json:"maxLive"`
			MaxDepth int `json:"maxDepth"`
		} `json:"spawn"`
	} `json:"tailnet"`
}

// Classes returns the DO namespaces, sorted by class.
func (c *TwdConfig) Classes() []ClassConfig {
	var out []ClassConfig
	for name, o := range c.Tailnet.Objects {
		out = append(out, ClassConfig{Class: name, Tags: o.Tags, Agent: o.Agent, TTL: o.TTL})
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
