package fakemgmt

import (
	"context"
	"net/http"
	"runtime"
	"sort"

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/internal/container"
	"github.com/rosskukulinski/twd/internal/registry"
)

// regAuthorize is twd-mgmt's registry rule on the local-dev socket: a
// caller naming a host (Twd-Host-ID, what hostd sends) only pulls, and only
// "public" and the tenants it holds a deployment of; anyone else is the
// local admin.
func (s *Server) regAuthorize(r *http.Request, ns string, push bool) error {
	host := r.Header.Get("Twd-Host-ID")
	if host == "" {
		return nil
	}
	if push {
		return &registry.Denied{Reason: "hosts pull from twd's registry; they never push"}
	}
	if ns == "" || ns == registry.PublicNamespace {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.deployments {
		if d.Host == host && d.tenantID == ns && !d.superseded {
			return nil
		}
	}
	return &registry.Denied{Reason: "host " + host + " holds no deployment of tenant " + ns}
}

// Registry returns the registry (nil without Config.RegistryDir), for tests
// that push images in-process.
func (s *Server) Registry() *registry.Registry { return s.reg }

// badBuilt is a refused containerImages entry (400, like twd-mgmt).
type badBuilt struct{ error }

// pinContainers is twd-mgmt's deploy-time pinning (ADR-032 D3) for one
// host arch. It returns nil classes when there is no registry.
func (s *Server) pinContainers(ctx context.Context, tenant string, app *bundle.App, built map[string]mgmtapi.BuiltImages, arch string) ([]container.Class, error) {
	if s.reg == nil || len(app.Containers) == 0 {
		return nil, nil
	}
	var cls []container.Class
	for _, c := range app.Containers {
		cls = append(cls, c)
	}
	sort.Slice(cls, func(i, j int) bool { return cls[i].Class < cls[j].Class })
	b := map[string]registry.Built{}
	for k, v := range built {
		b[k] = registry.Built{Image: v.Image, Images: v.Images}
	}
	cls, err := registry.ApplyBuilt(tenant, cls, b)
	if err != nil {
		return nil, badBuilt{err}
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	pinned, _, err := s.resolver.Pin(ctx, tenant, cls, []string{arch})
	return pinned, err
}
