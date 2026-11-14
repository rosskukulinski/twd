package mgmt

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/rosskukulinski/twd/internal/publicspec"
)

// Public ingress (ADR-029), mgmt side:
//   - the tenant gate (Tenant.PublicIngress, D1);
//   - a deployment's validated twd.jsonc "public" (Deployment.Public) and
//     the "_public" namespace whose single object is the workload's public
//     node (hostname = the workload name, tags [tag:twd-public]);
//   - the per-workload `twd expose on|off` setting (WorkloadPublic), which
//     outlives deployments;
//   - Assignment.Public, which tells the host what its public node serves.

// PublicNamespace is the object namespace of a workload's public node.
const PublicNamespace = publicspec.Namespace

// PublicAssignment mirrors hostd/mgmtapi.PublicAssignment (the wire
// contract; TestAssignmentPublicWireShape keeps them identical).
type PublicAssignment struct {
	Routes  []string          `json:"routes"`
	Upgrade []string          `json:"upgrade,omitempty"`
	Limits  publicspec.Limits `json:"limits"`
	Tags    []string          `json:"tags"`
	Exposed bool              `json:"exposed"`
}

// DeploymentPublicView is the public part of GET /v1/deployments/{id}.
type DeploymentPublicView struct {
	URL     string   `json:"url"`
	Routes  []string `json:"routes"`
	Exposed bool     `json:"exposed"`
	// Allowed is the tenant gate; false: the routes are declared but not served.
	Allowed bool `json:"allowed"`
}

// publicReservedName refuses workload names that would collide with twd's
// own nodes once published as <workload>.<tailnet>.ts.net.
func publicReservedName(workload string) bool {
	return workload == "twd-mgmt" || strings.HasPrefix(workload, "twd-host")
}

// errPublicNotAllowed is the tenant-gate refusal (ADR-029 D1).
func errPublicNotAllowed(t Tenant, workload string) *Error {
	e := errf(403, "public_ingress_not_allowed",
		"workload %s declares twd.jsonc \"public\" routes, but tenant %s does not allow public ingress; an operator can allow it with: twd tenant set %s --public-ingress allow",
		workload, t.ID, t.ID)
	e.Missing = map[string]any{"publicIngress": PublicAllow}
	return e
}

// checkPublicDeploy validates a deploy's public spec against the tenant
// gate and the reserved names.
func checkPublicDeploy(t Tenant, workload string, pub *DeploymentPublic) error {
	if pub == nil {
		return nil
	}
	if publicReservedName(workload) {
		return errf(400, "bad_request", "workload name %q is reserved for twd's own nodes and cannot be public", workload)
	}
	if !t.PublicAllowed() {
		return errPublicNotAllowed(t, workload)
	}
	return nil
}

// bundlePublic parses and validates the bundle's "public" (nil if none).
func bundlePublic(cfg *TwdConfig) (*DeploymentPublic, error) {
	if cfg == nil {
		return nil, nil
	}
	pub, err := cfg.PublicConfig()
	if err != nil {
		return nil, errf(400, "bad_bundle", "twd.jsonc: %v", err)
	}
	return pub, nil
}

// withPublicNamespace adds the "_public" namespace when pub is set.
func withPublicNamespace(classes []ClassConfig, pub *DeploymentPublic) []ClassConfig {
	if pub == nil || slices.ContainsFunc(classes, func(c ClassConfig) bool { return c.Class == PublicNamespace }) {
		return classes
	}
	return append(classes, ClassConfig{Class: PublicNamespace, Tags: []string{publicspec.Tag}})
}

// publicAssignment is what the host must serve for d (nil: nothing public).
func (m *Mgmt) publicAssignment(ctx context.Context, t Tenant, d Deployment) *PublicAssignment {
	if d.Public == nil || !t.PublicAllowed() {
		return nil
	}
	wp, err := m.store.GetWorkloadPublic(ctx, d.TenantID, d.Workload)
	if err != nil {
		// Fail closed: an unreadable expose setting serves nothing.
		m.log.Warn("public ingress: expose setting unreadable; not exposing", "workload", d.Workload, "err", err)
		wp.Exposed = false
	}
	tags := []string{publicspec.Tag}
	for _, c := range d.Namespaces {
		if c.Class == PublicNamespace && len(c.Tags) > 0 {
			tags = c.Tags
		}
	}
	sp := d.Public.Spec
	return &PublicAssignment{Routes: sp.Routes, Upgrade: sp.Upgrade, Limits: sp.Limits.Effective(), Tags: tags, Exposed: wp.Exposed}
}

// publicView is the deployment view's "public" block.
func (m *Mgmt) publicView(ctx context.Context, d Deployment) *DeploymentPublicView {
	if d.Public == nil {
		return nil
	}
	t, _ := m.store.GetTenant(ctx, d.TenantID)
	wp, _ := m.store.GetWorkloadPublic(ctx, d.TenantID, d.Workload)
	host := d.Workload + "." + t.Tailnet
	if o, err := m.store.GetObjectByName(ctx, d.TenantID, d.Workload, PublicNamespace, d.Workload); err == nil && o.MagicDNS != "" {
		host = strings.TrimSuffix(o.MagicDNS, ".")
	}
	return &DeploymentPublicView{URL: "https://" + host + "/", Routes: d.Public.Spec.Routes, Exposed: wp.Exposed, Allowed: t.PublicAllowed()}
}

// killPublicNode kills a workload's live public node (its public routes
// were removed by a redeploy); the device is deleted like any kill.
func (m *Mgmt) killPublicNode(ctx context.Context, tenantID, workload, reason, by string) {
	o, err := m.store.GetObjectByName(ctx, tenantID, workload, PublicNamespace, workload)
	if err != nil || !o.Live() {
		return
	}
	if _, err := m.Kill(ctx, o.ID, KillRequest{Reason: reason, By: by}); err != nil && !errors.Is(err, ErrNotFound) {
		m.log.Warn("public ingress: kill of the public node failed", "object", o.ID, "err", err)
	}
}

// SetWorkloadExposed is `twd expose on|off` (ADR-029 D1): the setting
// survives redeploys; the lease holder is told at once (push) and opens or
// closes the public node's Funnel listener.
func (m *Mgmt) SetWorkloadExposed(ctx context.Context, tenantID, workload string, exposed bool, by string) (WorkloadPublic, error) {
	if _, err := m.store.GetTenant(ctx, tenantID); err != nil {
		return WorkloadPublic{}, lookupErr(err, errf(404, "unknown_tenant", "tenant %q not found", tenantID))
	}
	if !validName(workload) {
		return WorkloadPublic{}, errf(400, "bad_request", "workload must be a DNS label ([a-z0-9-], ≤63)")
	}
	ds, err := m.store.LatestDeployments(ctx, tenantID)
	if err != nil {
		return WorkloadPublic{}, err
	}
	var dep *Deployment
	for i := range ds {
		if ds[i].Workload == workload {
			dep = &ds[i]
		}
	}
	if dep == nil {
		return WorkloadPublic{}, errf(404, "unknown_workload", "tenant %s has no deployment of workload %q", tenantID, workload)
	}
	wp := WorkloadPublic{TenantID: tenantID, Workload: workload, Exposed: exposed, UpdatedAt: m.now().UnixMilli(), UpdatedBy: by}
	kind := "expose_off"
	if exposed {
		kind = "expose_on"
	}
	actx, ai := m.intend(ctx, AuditRecord{TenantID: tenantID, Kind: kind, ActingFor: by},
		map[string]any{"workload": workload, "exposed": exposed, "by": by, "public": dep.Public != nil})
	if err := m.store.SetWorkloadPublic(actx, wp); err != nil { // the expose record commits with it (H22)
		return WorkloadPublic{}, err
	}
	m.recordIntent(ctx, ai, nil)
	if l, err := m.store.GetLease(ctx, LeaseName(tenantID, workload)); err == nil && l.Holder != "" {
		go m.push(l.Holder, "/v1/push/assignments", map[string]any{})
	}
	return wp, nil
}

// GetWorkloadPublic returns a workload's expose setting.
func (m *Mgmt) GetWorkloadPublic(ctx context.Context, tenantID, workload string) (WorkloadPublic, error) {
	if _, err := m.store.GetTenant(ctx, tenantID); err != nil {
		return WorkloadPublic{}, lookupErr(err, errf(404, "unknown_tenant", "tenant %q not found", tenantID))
	}
	return m.store.GetWorkloadPublic(ctx, tenantID, workload)
}

// PublicFunnelSnippet is the policy a public workload needs (ADR-029 D6);
// kept in step with internal/policy.PublicSnippet.
const PublicFunnelSnippet = `"tagOwners": { "tag:twd-public": ["autogroup:admin", "tag:twd"] },
"nodeAttrs": [ { "target": ["tag:twd-public"], "attr": ["funnel"] } ]`

// ActingForPublic is CreateObjectRequest.ActingFor for an object a public
// (Funnel) request created (ADR-029 D5); mgmt records origin "public".
const ActingForPublic = "public"

// findPublicClass is the latest deployment of the workload that declares
// public routes, and its "_public" namespace. A gate that is not allow
// refuses (403) so a host can't mint a public node the operator disabled.
func (m *Mgmt) findPublicClass(ctx context.Context, t Tenant, workload string) (Deployment, ClassConfig, error) {
	ds, err := m.store.LatestDeployments(ctx, t.ID)
	if err != nil {
		return Deployment{}, ClassConfig{}, err
	}
	for _, d := range ds {
		if d.Workload != workload || d.Public == nil {
			continue
		}
		if !t.PublicAllowed() {
			return Deployment{}, ClassConfig{}, errPublicNotAllowed(t, workload)
		}
		for _, c := range d.Namespaces {
			if c.Class == PublicNamespace {
				return d, c, nil
			}
		}
	}
	return Deployment{}, ClassConfig{}, ErrNotFound
}

// purgeDeadPublicNode forgets a killed public node of the workload so a
// deploy that declares public routes again can recreate it (same
// deterministic ID and hostname). Its pending device delete runs first.
func (m *Mgmt) purgeDeadPublicNode(ctx context.Context, tenantID, workload string) {
	o, err := m.store.GetObjectByName(ctx, tenantID, workload, PublicNamespace, workload)
	if err != nil || o.Live() {
		return
	}
	m.runDeleteForce(o.ID, true)
	l := m.tenantLock(tenantID)
	l.Lock()
	err = m.store.DeleteDeadObject(ctx, o.ID)
	l.Unlock()
	if err != nil {
		m.log.Warn("public ingress: purge of the old public node failed", "object", o.ID, "err", err)
		return
	}
	m.mu.Lock()
	delete(m.waiters, o.ID)
	m.mu.Unlock()
	m.audit(ctx, AuditRecord{TenantID: tenantID, ObjectID: o.ID, Kind: "purge"},
		map[string]any{"workload": workload, "namespace": PublicNamespace, "name": workload, "status": o.Status, "reason": "public routes declared again"})
}

// validPublicIngress checks a Tenant.PublicIngress value.
func validPublicIngress(v string) error {
	switch v {
	case "", PublicAllow, PublicDeny:
		return nil
	}
	return errf(400, "bad_request", "publicIngress must be %q or %q", PublicAllow, PublicDeny)
}

// pushTenantHosts tells every host holding one of the tenant's workload
// leases to re-sync its assignments (the public gate changed).
func (m *Mgmt) pushTenantHosts(ctx context.Context, tenantID string) {
	ds, err := m.store.LatestDeployments(ctx, tenantID)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, d := range ds {
		l, err := m.store.GetLease(ctx, LeaseName(tenantID, d.Workload))
		if err != nil || l.Holder == "" || seen[l.Holder] {
			continue
		}
		seen[l.Holder] = true
		go m.push(l.Holder, "/v1/push/assignments", map[string]any{})
	}
}

// PublicTag is the tag every public node carries (ADR-029 D6).
const PublicTag = publicspec.Tag
