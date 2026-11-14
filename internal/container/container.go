// Package container holds what mgmt and twd-hostd share about containers
// and code sandboxes on the microVM tier (ADR-032): the container classes a
// bundle declares (wrangler "containers", twd.jsonc "containers"), Cloudflare's
// instance types and the resource vector a running container reserves.
package container

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Resources is what one running container takes: vCPUs (fractional for the
// small instance types; enforced with cpu.max), guest memory and its
// scratch disk.
type Resources struct {
	VCPU    float64 `json:"vcpu"`
	MemMiB  int     `json:"memMiB"`
	DiskMiB int     `json:"diskMiB"`
}

// MicroVCPU is VCPU in millionths (stores and sums use integers; 1/16 is exact).
func (r Resources) MicroVCPU() int64 { return int64(math.Round(r.VCPU * 1e6)) }

// Valid reports whether every dimension is positive and bounded.
func (r Resources) Valid() bool {
	return r.MicroVCPU() > 0 && r.VCPU <= 64 && r.MemMiB > 0 && r.MemMiB <= 1<<20 && r.DiskMiB >= 0 && r.DiskMiB <= 1<<22
}

// Fits reports whether r is within held (a restart may reuse a reservation
// only for the same or a smaller container).
func (r Resources) Fits(held Resources) bool {
	return r.MicroVCPU() <= held.MicroVCPU() && r.MemMiB <= held.MemMiB && r.DiskMiB <= held.DiskMiB
}

// DefaultInstanceType is Cloudflare's default (containers/platform-details).
const DefaultInstanceType = "lite"

// InstanceTypes are Cloudflare's named instance types (disk in MB there;
// MiB here). "dev" and "standard" are the deprecated names.
var InstanceTypes = map[string]Resources{
	"lite":       {VCPU: 0.0625, MemMiB: 256, DiskMiB: 2000},
	"basic":      {VCPU: 0.25, MemMiB: 1024, DiskMiB: 4000},
	"standard-1": {VCPU: 0.5, MemMiB: 4096, DiskMiB: 8000},
	"standard-2": {VCPU: 1, MemMiB: 6144, DiskMiB: 12000},
	"standard-3": {VCPU: 2, MemMiB: 8192, DiskMiB: 16000},
	"standard-4": {VCPU: 4, MemMiB: 12288, DiskMiB: 20000},
	"dev":        {VCPU: 0.0625, MemMiB: 256, DiskMiB: 2000},
	"standard":   {VCPU: 0.5, MemMiB: 4096, DiskMiB: 4000},
}

// Arches a container image (and a host) can be.
const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

// Class is one container class: the DO class whose instances each get one
// container (ADR-032 D2).
type Class struct {
	Class string `json:"class"`
	// Source is the wrangler "image" as written: an image reference or a
	// Dockerfile path / build dir that twd deploy builds.
	Source string `json:"source,omitempty"`
	// Image is the default image reference ctx.container.start() runs: the
	// wrangler image when it is a reference, else the twd.jsonc pin ("" =
	// a Dockerfile twd deploy has not built and pinned yet).
	Image string `json:"image,omitempty"`
	// Images are the named images (Sandbox SDK 1.0 ctx.container images,
	// wrangler "images"), name -> reference ("" = not built yet).
	Images map[string]string `json:"images,omitempty"`
	// InstanceType is a named type ("" = DefaultInstanceType) or "custom".
	InstanceType string     `json:"instanceType,omitempty"`
	Custom       *Resources `json:"custom,omitempty"`
	// MaxInstances bounds the class's running containers per tenant
	// workload (0 = no per-class bound; the tenant quota still applies).
	MaxInstances int `json:"maxInstances,omitempty"`
	// Arch is the image's architecture when the bundle states it ("" = not
	// stated: any host offering containers; pinning at deploy fills it).
	Arch string `json:"arch,omitempty"`
}

// Resources is what one container of this class reserves.
func (c Class) Resources() Resources {
	if c.Custom != nil {
		return *c.Custom
	}
	t := c.InstanceType
	if t == "" {
		t = DefaultInstanceType
	}
	return InstanceTypes[t]
}

// Refs lists every image reference the class may start (pinned ones only),
// sorted.
func (c Class) Refs() []string {
	var out []string
	if c.Image != "" {
		out = append(out, c.Image)
	}
	for _, r := range c.Images {
		if r != "" && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// IsBuildSource reports whether a wrangler image names a Dockerfile (or a
// build dir) rather than a registry reference.
func IsBuildSource(s string) bool {
	return strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || strings.HasPrefix(s, "/") ||
		strings.HasSuffix(strings.ToLower(s), "dockerfile") || s == "."
}

// entry is one "containers" element as written in wrangler.jsonc
// (class_name, image, images, instance_type, max_instances) or twd.jsonc
// (the same keys, plus "arch"; "class" is accepted for class_name, and
// "images" maps a name straight to a pinned reference).
type entry struct {
	ClassName    string          `json:"class_name"`
	Class        string          `json:"class"`
	Image        string          `json:"image"`
	Images       json.RawMessage `json:"images"`
	InstanceType json.RawMessage `json:"instance_type"`
	MaxInstances *int            `json:"max_instances"`
	Arch         string          `json:"arch"`
}

var (
	identRE = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
	refRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]{0,254}$`)
)

// entries reads a "containers" value: an array of entries (Cloudflare's
// shape), or, in twd.jsonc, an object keyed by class ({"<Class>": {"image":
// ref, "images": {name: ref}}}: the pins twd deploy writes).
func entries(src string, b []byte) ([]entry, error) {
	var doc struct {
		Containers json.RawMessage `json:"containers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	raw := bytes.TrimSpace(doc.Containers)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var list []entry
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("%s: containers: %w", src, err)
		}
		return list, nil
	}
	var byClass map[string]entry
	if src == "wrangler" {
		return nil, fmt.Errorf("%s: containers must be an array of {class_name, image, …}", src)
	}
	if err := json.Unmarshal(raw, &byClass); err != nil {
		return nil, fmt.Errorf("%s: containers must be an array, or an object keyed by class: %w", src, err)
	}
	for name, e := range byClass {
		e.ClassName = name
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ClassName < list[j].ClassName })
	return list, nil
}

// Parse reads the "containers" of wrangler and twd (plain JSON, comments
// already stripped; either may be nil). A twd.jsonc entry replaces the
// wrangler entry of the same class field by field (the fields it sets), and
// may add a class. The result is sorted by class.
func Parse(wrangler, twd []byte) ([]Class, error) {
	byClass := map[string]*Class{}
	fromWrangler := map[string]bool{} // classes wrangler declares
	for _, src := range []struct {
		name string
		b    []byte
	}{{"wrangler", wrangler}, {"twd.jsonc", twd}} {
		if len(src.b) == 0 {
			continue
		}
		list, err := entries(src.name, src.b)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for i, e := range list {
			name := e.ClassName
			if name == "" {
				name = e.Class
			}
			if !identRE.MatchString(name) {
				return nil, fmt.Errorf("%s: containers[%d]: class_name %q is not a class name", src.name, i, name)
			}
			if seen[name] {
				return nil, fmt.Errorf("%s: two containers entries for %s", src.name, name)
			}
			seen[name] = true
			c := byClass[name]
			if c == nil {
				c = &Class{Class: name}
				byClass[name] = c
				fromWrangler[name] = src.name == "wrangler"
			}
			if e.Image != "" {
				if src.name == "wrangler" {
					c.Source = e.Image
				}
				if !IsBuildSource(e.Image) {
					c.Image = e.Image
				} else if src.name != "wrangler" {
					return nil, fmt.Errorf("%s: containers.%s: image %q must be an image reference (twd.jsonc pins what twd deploy built)", src.name, name, e.Image)
				}
			}
			if err := c.setImages(src.name, e.Images, fromWrangler); err != nil {
				return nil, err
			}
			if e.MaxInstances != nil {
				if *e.MaxInstances < 0 {
					return nil, fmt.Errorf("%s: containers.%s: max_instances must be ≥ 0", src.name, name)
				}
				c.MaxInstances = *e.MaxInstances
			}
			if e.Arch != "" {
				if e.Arch != ArchAMD64 && e.Arch != ArchARM64 {
					return nil, fmt.Errorf("%s: containers.%s: arch %q: want %q or %q", src.name, name, e.Arch, ArchAMD64, ArchARM64)
				}
				c.Arch = e.Arch
			}
			if len(e.InstanceType) > 0 && string(e.InstanceType) != "null" {
				if err := c.setInstanceType(e.InstanceType); err != nil {
					return nil, fmt.Errorf("%s: containers.%s: %w", src.name, name, err)
				}
			}
		}
	}
	out := make([]Class, 0, len(byClass))
	for _, c := range byClass {
		if c.Source == "" && c.Image == "" && len(c.Images) == 0 {
			return nil, fmt.Errorf("containers.%s: image is required (a Dockerfile path or an image reference)", c.Class)
		}
		for _, r := range c.Refs() {
			if !refRE.MatchString(r) {
				return nil, fmt.Errorf("containers.%s: bad image reference %q", c.Class, r)
			}
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Class < out[j].Class })
	return out, nil
}

// setImages takes wrangler's {name: {dockerfile | image}} or twd.jsonc's
// {name: ref} pins. A twd.jsonc pin of a class wrangler declares must name
// one of its wrangler images (a typo is an error, not a new image).
func (c *Class) setImages(src string, raw json.RawMessage, fromWrangler map[string]bool) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if c.Images == nil {
		c.Images = map[string]string{}
	}
	if src == "wrangler" {
		var ims map[string]struct {
			Dockerfile string `json:"dockerfile"`
			Image      string `json:"image"`
		}
		if err := json.Unmarshal(raw, &ims); err != nil {
			return fmt.Errorf("%s: containers.%s.images: %w", src, c.Class, err)
		}
		for n, im := range ims {
			if !identRE.MatchString(n) {
				return fmt.Errorf("%s: containers.%s: bad image name %q", src, c.Class, n)
			}
			c.Images[n] = ""
			if im.Image != "" && !IsBuildSource(im.Image) {
				c.Images[n] = im.Image
			}
		}
		return nil
	}
	var pins map[string]string
	if err := json.Unmarshal(raw, &pins); err != nil {
		return fmt.Errorf("%s: containers.%s.images: want {name: image reference}: %w", src, c.Class, err)
	}
	for n, r := range pins {
		if _, ok := c.Images[n]; !ok && fromWrangler[c.Class] {
			return fmt.Errorf("%s: containers.%s.images.%s is not in wrangler's images", src, c.Class, n)
		}
		if !identRE.MatchString(n) {
			return fmt.Errorf("%s: containers.%s: bad image name %q", src, c.Class, n)
		}
		c.Images[n] = r
	}
	return nil
}

// setInstanceType takes a named type ("basic") or Cloudflare's custom form
// {"vcpu": 1, "memory_mib": 2048, "disk_mb": 8000}.
func (c *Class) setInstanceType(raw json.RawMessage) error {
	var name string
	if json.Unmarshal(raw, &name) == nil {
		if _, ok := InstanceTypes[name]; !ok {
			return fmt.Errorf("instance_type %q: want one of lite, basic, standard-1 … standard-4, or {vcpu, memory_mib, disk_mb}", name)
		}
		c.InstanceType, c.Custom = name, nil
		return nil
	}
	var cu struct {
		VCPU      float64 `json:"vcpu"`
		MemoryMiB int     `json:"memory_mib"`
		DiskMB    int     `json:"disk_mb"`
	}
	if err := json.Unmarshal(raw, &cu); err != nil {
		return fmt.Errorf("instance_type: %w", err)
	}
	r := Resources{VCPU: cu.VCPU, MemMiB: cu.MemoryMiB, DiskMiB: cu.DiskMB}
	if !r.Valid() {
		return fmt.Errorf("instance_type %s: vcpu and memory_mib must be positive (disk_mb ≥ 0)", raw)
	}
	c.InstanceType, c.Custom = "custom", &r
	return nil
}

// Arch is the one image architecture the classes require ("" = none
// stated), or an error if they state different ones (a container is
// co-located with its parent DO, so one host must run them all).
func Arch(classes []Class) (string, error) {
	arch := ""
	for _, c := range classes {
		if c.Arch == "" {
			continue
		}
		if arch != "" && c.Arch != arch {
			return "", fmt.Errorf("containers need both %s and %s images; a workload's containers run on its one host", arch, c.Arch)
		}
		arch = c.Arch
	}
	return arch, nil
}
