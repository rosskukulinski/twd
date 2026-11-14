package mgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/internal/tssvc"
)

// ServicePort is the only port a workload Service exposes: the workload
// node's attributed HTTP ingress.
const ServicePort = "tcp:80"

// serviceOwnerPrefix marks a Service definition twd manages. The full
// comment is serviceOwner(tenant, workload); a deploy adopts a Service only
// if its comment is exactly that, so one tenant's (or workload's) deploy can
// never take over another's Service name on a shared tailnet.
const serviceOwnerPrefix = "managed by twd: "

func serviceOwner(tenantID, workload string) string {
	return serviceOwnerPrefix + "tenant=" + tenantID + " workload=" + workload
}

// ServiceAPI is the part of the Tailscale API that manages Service
// definitions (*tsapi.Client implements it).
type ServiceAPI interface {
	GetService(ctx context.Context, name string) (tsapi.Service, error)
	PutService(ctx context.Context, s tsapi.Service) (tsapi.Service, error)
}

// ServiceName returns the Tailscale Service the workload's entry node hosts,
// from twd.jsonc tailnet.service: true means "svc:<workload>", a string is
// the name ("svc:" optional), false/absent means none.
func (c *TwdConfig) ServiceName(workload string) (string, error) {
	raw := c.Tailnet.Service
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var on bool
	if json.Unmarshal(raw, &on) == nil {
		if !on {
			return "", nil
		}
		return "svc:" + workload, nil
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return "", fmt.Errorf("twd.jsonc: tailnet.service must be true, false or a Service name (\"svc:<label>\")")
	}
	if name == "" {
		return "", nil
	}
	if !strings.HasPrefix(name, "svc:") {
		name = "svc:" + name
	}
	if err := tssvc.Validate(name); err != nil {
		return "", fmt.Errorf("twd.jsonc: tailnet.service: %v", err)
	}
	return name, nil
}

// applyService resolves twd.jsonc tailnet.service onto the _workload
// namespace. A Service needs the workload node, i.e. tailnet.tags (Service
// hosts must be tagged nodes).
func applyService(cfg *TwdConfig, workload string, classes []ClassConfig) (string, error) {
	if cfg == nil {
		return "", nil
	}
	name, err := cfg.ServiceName(workload)
	if err != nil || name == "" {
		return "", err
	}
	i := slices.IndexFunc(classes, func(c ClassConfig) bool { return c.Class == WorkloadNamespace })
	if i < 0 {
		return "", fmt.Errorf("twd.jsonc: tailnet.service needs tailnet.tags: the Service is hosted by the workload's entry node, which must be a tagged node")
	}
	classes[i].Service = name
	return name, nil
}

// workloadService returns the Service of a deployment's _workload namespace.
func workloadService(classes []ClassConfig) string {
	for _, c := range classes {
		if c.Class == WorkloadNamespace {
			return c.Service
		}
	}
	return ""
}

// ensureService makes sure the tenant tailnet has the Service definition,
// creating it (with ServicePort) if missing. An existing definition is
// adopted only if twd created it for this (tenant, workload); otherwise the
// deploy is refused with service_conflict. The returned definition carries
// the Service's VIPs.
func (m *Mgmt) ensureService(ctx context.Context, t Tenant, workload, name string) (tsapi.Service, error) {
	api, err := m.broker.API(ctx, t, purposePolicy)
	if err != nil {
		return tsapi.Service{}, errf(502, "service_api_failed", "no Tailscale API credential to manage %s: %v", name, err)
	}
	sa, ok := api.(ServiceAPI)
	if !ok {
		return tsapi.Service{}, errf(502, "service_api_failed", "the tenant's API client can't manage Services")
	}
	owner := serviceOwner(t.ID, workload)
	cur, err := sa.GetService(ctx, name)
	switch {
	case err == nil:
		if cur.Comment != owner {
			return tsapi.Service{}, errf(409, "service_conflict",
				"Service %s already exists and is not managed by twd for workload %q of tenant %s (its comment is %q). "+
					"Pick another name in twd.jsonc tailnet.service, delete the Service, or, to hand an existing Service to this workload, set its comment to %q",
				name, workload, t.ID, cur.Comment, owner)
		}
		if slices.Contains(cur.Ports, ServicePort) {
			return cur, nil
		}
		cur.Ports = append(cur.Ports, ServicePort)
	case tsapi.IsNotFound(err):
		// MagicDNS answers a device name before a Service record, so a
		// Service named like an existing device would never be reached by
		// name. Best effort: skipped when the credential can't list devices.
		if dl, ok := api.(interface {
			ListDevices(ctx context.Context) ([]tsapi.Device, error)
		}); ok {
			if devs, err := dl.ListDevices(ctx); err == nil {
				label := tssvc.Label(name)
				for _, d := range devs {
					if first, _, _ := strings.Cut(d.Name, "."); strings.EqualFold(first, label) {
						return tsapi.Service{}, errf(409, "service_conflict",
							"Service %s would share its MagicDNS name with device %s (MagicDNS resolves the device first, so callers would never reach the Service); pick another name in twd.jsonc tailnet.service",
							name, d.Name)
					}
				}
			}
		}
		cur = tsapi.Service{Name: name, Comment: owner, Ports: []string{ServicePort}}
	default:
		return tsapi.Service{}, serviceAPIError(name, "read", err)
	}
	out, err := sa.PutService(ctx, cur)
	if err != nil {
		return tsapi.Service{}, serviceAPIError(name, "create", err)
	}
	m.log.Info("tailscale service ensured", "tenant", t.ID, "workload", workload, "service", name, "addrs", out.Addrs)
	return out, nil
}

func serviceAPIError(name, op string, err error) error {
	if tsapi.IsForbidden(err) || tsapi.IsUnauthorized(err) {
		return errf(502, "service_api_failed",
			"the Tailscale API refused to %s Service %s (%v): an OAuth client needs the Services write scope (Settings › OAuth clients › Services › Write), or create the Service in the admin console",
			op, name, err)
	}
	return errf(502, "service_api_failed", "can't %s Service %s: %v", op, name, err)
}

// serviceAutoApproved reports whether policy autoApprovers.services lets a
// node with one of tags host name ("*" isn't a valid key there, so exact
// names only). Without approval control never routes the Service to the
// workload node, even though it advertises it.
func (p *Policy) serviceAutoApproved(name string, tags []string) bool {
	for _, who := range p.AutoApprovers.Services[name] {
		if slices.Contains(tags, who) {
			return true
		}
	}
	return false
}

// HasServiceGrant reports whether some grant with a network part lets src
// (a tag) reach the Service name.
func (p *Policy) HasServiceGrant(src, name string) bool {
	for _, g := range p.Grants {
		if len(g.IP) > 0 && srcMatches(g.Src, []string{src}) && slices.Contains(g.Dst, name) {
			return true
		}
	}
	return false
}

// servicePreflight adds the Service checks to a preflight result: the
// workload tags must be auto-approved to host it.
func servicePreflight(pol *Policy, res *PreflightResult, name string, wlTags []string) {
	res.Service = name
	if pol.serviceAutoApproved(name, wlTags) {
		return
	}
	res.ServiceUnapproved = true
	tag := "tag:twd"
	if len(wlTags) > 0 {
		tag = wlTags[0]
	}
	res.ServiceSnippet = fmt.Sprintf(`"autoApprovers": { "services": { %q: [%q] } }`, name, tag)
}
