package mgmt

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/rosskukulinski/twd/internal/container"
	"github.com/rosskukulinski/twd/internal/oci"
	"github.com/rosskukulinski/twd/internal/registry"
)

// The OCI registry (ADR-032 D3). twd-mgmt serves the distribution API at
// /v2/ on the same listeners as /v1 (tsnet :80/:443, and the local-dev
// socket), behind the same WhoIs gate:
//
//   - admins (AdminCap, -admins, the local operator) push and pull every
//     namespace: a tenant's ("<tenantID>/…") and the operator's "public/…";
//   - hosts (tag:twd-host) pull only: "public/…" and the namespaces of
//     tenants they hold a deployment of. A host never pushes;
//   - everyone else was already refused by TailnetHandler (403 not_admin).
//
// Blobs are content-addressed files under Config.RegistryDir (shared by
// every replica that serves the registry: put it on shared storage, or run
// the registry on one replica); the store holds which repository sees
// which content (registry_links), tags and the public base list.
// Deploy resolves every image reference to a digest once (Resolver),
// mirroring external images into the tenant's namespace through mgmt's own
// egress, and records the pins in the deployment (container.Class.Pins).

// PublicImage is one operator-marked public base: a mirrored upstream
// image (or a pushed alias target) that every tenant's deployment may pin
// and every host caches once (hostd's "public" cache scope).
type PublicImage struct {
	ID         string `json:"id"`
	Ref        string `json:"ref"`        // normalised upstream ref (docker.io/cloudflare/sandbox:0.12.14) or twd.local/…
	Arch       string `json:"arch"`       // amd64 | arm64
	Repository string `json:"repository"` // public/…
	Digest     string `json:"digest"`     // the linux/arch image manifest
	// SourceDigest is the upstream top-level digest at add time (an
	// index or the manifest itself).
	SourceDigest string `json:"sourceDigest"`
	CreatedBy    string `json:"createdBy,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
}

func (m *Mgmt) initRegistry() error {
	dir := m.cfg.RegistryDir
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o700); err != nil {
		return fmt.Errorf("registry dir: %w", err)
	}
	m.reg = &registry.Registry{Dir: dir, Meta: m.store, Authorize: m.registryAuthorize, Pushed: m.imagePushed}
	aliases := m.cfg.ImageAliases
	if aliases == nil {
		aliases = registry.DefaultAliases
	}
	m.resolver = &registry.Resolver{Reg: m.reg, Aliases: aliases, Public: publicLookup{m}}
	if !m.cfg.NoMirror {
		up := m.cfg.RegistryUpstream
		if up == nil {
			up = func(host string) *oci.Client {
				return &oci.Client{Base: oci.RegistryBase(host, slices.Contains(m.cfg.RegistryInsecure, host)), HTTP: oci.DefaultHTTP}
			}
		}
		m.resolver.Upstream = up
	}
	return nil
}

// Registry returns the OCI registry handler (nil without RegistryDir).
func (m *Mgmt) Registry() http.Handler {
	if m.reg == nil {
		return nil
	}
	return m.reg
}

// registryAuthorize is the registry's access rule (see the top of this file).
func (m *Mgmt) registryAuthorize(r *http.Request, ns string, push bool) error {
	p := principalOf(r.Context())
	host := ""
	switch p.Kind {
	case "host":
		host = p.HostID
	case "local":
		host = r.Header.Get("Twd-Host-ID") // hostd on the local-dev socket: host rules
	case "user":
		// TailnetHandler admits a user principal only as an admin.
	default:
		return &registry.Denied{Reason: "unauthenticated"}
	}
	if host != "" {
		if push {
			return &registry.Denied{Reason: "hosts pull from twd's registry; they never push (caller " + p.Kind + " host " + host + ")"}
		}
		if ns == "" || ns == registry.PublicNamespace {
			return nil
		}
		if !m.hostServesTenant(r.Context(), host, ns) {
			return &registry.Denied{Reason: fmt.Sprintf("host %s holds no deployment of tenant %q, so it may not pull its images", host, ns)}
		}
		return nil
	}
	if ns == "" || ns == registry.PublicNamespace {
		return nil
	}
	if _, err := m.store.GetTenant(r.Context(), ns); err != nil {
		return &registry.Denied{Reason: fmt.Sprintf("namespace %q is not a tenant (repositories are <tenantID>/<name>, or public/<name> for operator base images)", ns)}
	}
	return nil
}

// hostServesTenant: host holds a live (not stopped) deployment of tenant.
func (m *Mgmt) hostServesTenant(ctx context.Context, host, tenant string) bool {
	ds, err := m.store.Assignments(ctx, host)
	if err != nil {
		return false
	}
	for _, d := range ds {
		if d.TenantID == tenant && d.StoppedAt == 0 {
			return true
		}
	}
	return false
}

// imagePushed audits a manifest push (image_push).
func (m *Mgmt) imagePushed(r *http.Request, repo, reference, digest, mediaType string) {
	p := principalOf(r.Context())
	ns := registry.Namespace(repo)
	tenant := ""
	if ns != registry.PublicNamespace {
		tenant = ns
	}
	m.audit(r.Context(), AuditRecord{TenantID: tenant, Kind: "image_push", ActingFor: p.Actor(r)},
		map[string]any{"repository": repo, "reference": reference, "digest": digest, "mediaType": mediaType, "by": p.Actor(r)})
}

type publicLookup struct{ m *Mgmt }

// LookupPublic implements registry.PublicImages.
func (pl publicLookup) LookupPublic(ctx context.Context, ref, arch string) (string, string, bool, error) {
	ps, err := pl.m.store.ListPublicImages(ctx)
	if err != nil {
		return "", "", false, err
	}
	n := oci.Normalize(ref)
	for _, p := range ps {
		if p.Ref == n && p.Arch == arch {
			return p.Repository, p.Digest, true, nil
		}
	}
	return "", "", false, nil
}

// AddPublicImage marks ref (an upstream image, mirrored now into
// public/<host>/<repository>, or a twd.local ref already pushed to the
// public namespace) as a public base for each arch (empty: every linux
// arch it has). Re-adding refreshes the pinned digest.
func (m *Mgmt) AddPublicImage(ctx context.Context, ref string, arches []string, by string) ([]PublicImage, error) {
	if m.reg == nil {
		return nil, errf(409, "registry_unavailable", "this twd-mgmt has no registry (-registry-dir)")
	}
	r, err := oci.ParseRef(ref)
	if err != nil {
		return nil, errf(400, "bad_request", "%v", err)
	}
	for _, a := range arches {
		if a != container.ArchAMD64 && a != container.ArchARM64 {
			return nil, errf(400, "bad_request", "arch %q: want amd64 or arm64", a)
		}
	}
	var src oci.Source
	var srcRepo, repo string
	if r.Registry == registry.LocalRegistry {
		src, srcRepo = m.reg, registry.PublicNamespace+"/"+strings.TrimPrefix(r.Repository, registry.PublicNamespace+"/")
		repo = srcRepo
	} else {
		if m.resolver.Upstream == nil {
			return nil, errf(409, "mirror_disabled", "this twd-mgmt does not mirror external images (-no-mirror)")
		}
		src, srcRepo = m.resolver.Upstream(r.Registry), r.Repository
		repo = registry.PublicNamespace + "/" + strings.NewReplacer(":", "-").Replace(r.Registry) + "/" + r.Repository
	}
	cp, err := oci.Copy(ctx, src, srcRepo, r.Reference(), m.reg, repo, arches, "")
	if err != nil {
		return nil, errf(502, "image_unavailable", "%s: %v", r, err)
	}
	var out []PublicImage
	archs := make([]string, 0, len(cp.Platforms))
	for a := range cp.Platforms {
		archs = append(archs, a)
	}
	sort.Strings(archs)
	for _, a := range archs {
		if a != container.ArchAMD64 && a != container.ArchARM64 {
			continue
		}
		p := PublicImage{ID: newID("pi_"), Ref: r.String(), Arch: a, Repository: repo, Digest: cp.Platforms[a], SourceDigest: cp.Digest,
			CreatedBy: by, CreatedAt: m.now().UnixMilli()}
		if err := m.store.PutPublicImage(ctx, p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errf(400, "bad_request", "%s has no linux/amd64 or linux/arm64 image", r)
	}
	m.audit(ctx, AuditRecord{Kind: "image_public", ActingFor: by}, map[string]any{"ref": r.String(), "repository": repo, "platforms": cp.Platforms, "source": cp.Digest, "by": by})
	return out, nil
}

// RemovePublicImage unmarks one public base (deployments already pinned
// to it keep pulling it; new deploys mirror into the tenant instead).
func (m *Mgmt) RemovePublicImage(ctx context.Context, id, by string) error {
	if err := m.store.DeletePublicImage(ctx, id); err != nil {
		return lookupErr(err, errf(404, "not_found", "public image %q not found", id))
	}
	m.audit(ctx, AuditRecord{Kind: "image_public", ActingFor: by}, map[string]any{"removed": id, "by": by})
	return nil
}

// ListPublicImages lists the public bases.
func (m *Mgmt) ListPublicImages(ctx context.Context) ([]PublicImage, error) {
	ps, err := m.store.ListPublicImages(ctx)
	if ps == nil {
		ps = []PublicImage{}
	}
	return ps, err
}

// ImageAliases is the alias map deploys apply (twd deploy applies the same
// map as BuildKit named contexts when it builds a Dockerfile).
func (m *Mgmt) ImageAliases() []registry.Alias {
	if m.resolver == nil {
		a := m.cfg.ImageAliases
		if a == nil {
			a = registry.DefaultAliases
		}
		return a
	}
	return m.resolver.Aliases
}

// containerArches are the arches deploys pin images for: those of the
// live hosts in t's pool that offer containers, narrowed to want ("" =
// any).
func (m *Mgmt) containerArches(ctx context.Context, t Tenant, want string) ([]string, error) {
	hosts, err := m.store.ListHosts(ctx, Page{})
	if err != nil {
		return nil, err
	}
	now := m.now()
	var out []string
	for _, h := range hosts {
		if !h.Live(now) || t.Pool != "" && h.Pool != t.Pool || !slices.Contains(h.Tiers, CapContainers) || h.Arch == "" {
			continue
		}
		if want != "" && h.Arch != want || slices.Contains(out, h.Arch) {
			continue
		}
		out = append(out, h.Arch)
	}
	sort.Strings(out)
	return out, nil
}

// pinContainers resolves a deploy's container images (see Resolver.Pin)
// for the arches of the tenant's container hosts. It returns the classes
// with pins and the arches every image resolved for (placement is then
// limited to hosts of those arches). Without a registry it changes
// nothing.
func (m *Mgmt) pinContainers(ctx context.Context, t Tenant, ctrs []container.Class, wantArch string, by string) ([]container.Class, []string, error) {
	if m.reg == nil || len(ctrs) == 0 {
		return ctrs, nil, nil
	}
	arches, err := m.containerArches(ctx, t, wantArch)
	if err != nil {
		return nil, nil, err
	}
	if len(arches) == 0 {
		return ctrs, nil, nil // pickHost explains: no live host offers containers
	}
	pinned, ok, err := m.resolver.Pin(ctx, t.ID, ctrs, arches)
	if err != nil {
		return nil, nil, errf(502, "image_unavailable", "%v", err)
	}
	var pins []map[string]string
	for _, c := range pinned {
		for _, p := range c.Pins {
			pins = append(pins, map[string]string{"class": c.Class, "ref": p.Ref, "arch": p.Arch, "repository": p.Repository, "digest": p.Digest, "from": p.From})
		}
	}
	m.audit(ctx, AuditRecord{TenantID: t.ID, Kind: "image_pin", ActingFor: by}, map[string]any{"pins": pins, "arches": ok})
	return pinned, ok, nil
}
