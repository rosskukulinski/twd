package mgmt

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/internal/tsapi"
)

// Service methods added for /v1 (REST semantics: PATCH, DELETE with
// cascades, host join keys, dry-run deploys). The v0-era methods they build
// on live in service.go.

// TenantPatch is the JSON Merge Patch of PATCH /v1/tenants/{id}. A nil
// pointer is "absent"; Clear* report an explicit null.
type TenantPatch struct {
	MintMode               *string
	APIKeyFile             *string
	WIFClientID            *string
	WIFLifecycleClientID   *string
	OAuthClientID          *string
	OAuthClientFile        *string
	RegistrationsPerMinute *int
	MaxObjects             *int
	Pool                   *string
	Tier                   *string
	By                     string
}

// UpdateTenant applies a merge patch. Switching to wif (or changing a WIF
// client ID) drops cached tokens; apikey needs an absolute, existing key
// file.
func (m *Mgmt) UpdateTenant(ctx context.Context, id string, p TenantPatch) (Tenant, error) {
	t, err := m.store.GetTenant(ctx, id)
	if err != nil {
		return Tenant{}, err
	}
	changed := map[string]any{}
	if p.MintMode != nil {
		switch *p.MintMode {
		case MintAPIKey, MintWIF, MintOAuth:
		default:
			return Tenant{}, errf(400, "bad_request", "mintMode must be oauth, apikey or wif")
		}
		t.MintMode = *p.MintMode
		changed["mintMode"] = t.MintMode
	}
	if p.APIKeyFile != nil {
		t.APIKeyFile = *p.APIKeyFile
		changed["apiKeyFile"] = t.APIKeyFile != ""
	}
	if p.WIFClientID != nil {
		t.WIFClientID = *p.WIFClientID
		changed["wifClientID"] = t.WIFClientID
	}
	if p.WIFLifecycleClientID != nil {
		t.WIFLifecycleClientID = *p.WIFLifecycleClientID
		changed["lifecycle"] = t.WIFLifecycleClientID != ""
	}
	if p.OAuthClientID != nil {
		t.OAuthClientID = *p.OAuthClientID
		changed["oauthClientID"] = t.OAuthClientID
	}
	if p.OAuthClientFile != nil {
		t.OAuthClientFile = *p.OAuthClientFile
		changed["oauthClientFile"] = t.OAuthClientFile != ""
	}
	if p.RegistrationsPerMinute != nil {
		if *p.RegistrationsPerMinute <= 0 {
			return Tenant{}, errf(400, "bad_request", "registrationsPerMinute must be positive")
		}
		t.RegistrationsPerMinute = *p.RegistrationsPerMinute
		changed["registrationsPerMinute"] = t.RegistrationsPerMinute
	}
	if p.MaxObjects != nil {
		if *p.MaxObjects <= 0 {
			return Tenant{}, errf(400, "bad_request", "maxObjects must be positive")
		}
		t.MaxObjects = *p.MaxObjects
		changed["maxObjects"] = t.MaxObjects
	}
	if p.Pool != nil {
		t.Pool = *p.Pool
		changed["pool"] = t.Pool
	}
	if p.Tier != nil {
		if *p.Tier != "" && *p.Tier != "process" && *p.Tier != "microvm" {
			return Tenant{}, errf(400, "bad_request", "tier must be process or microvm")
		}
		t.Tier = *p.Tier
		changed["tier"] = t.Tier
	}
	switch t.MintMode {
	case MintAPIKey:
		if t.APIKeyFile == "" {
			return Tenant{}, errf(400, "bad_request", "mintMode apikey needs apiKeyFile")
		}
	case MintWIF:
		if t.WIFClientID == "" {
			return Tenant{}, errf(400, "bad_request", "mintMode wif needs wifClientID")
		}
		if m.broker.Issuer == nil {
			return Tenant{}, errf(400, "bad_request", "mintMode wif needs twd-mgmt to run with an issuer (-issuer-url)")
		}
	case MintOAuth:
		id, err := checkOAuthFile(t.OAuthClientFile, t.OAuthClientID)
		if err != nil {
			return Tenant{}, err
		}
		t.OAuthClientID = id
	}
	t.UpdatedAt = m.now().UnixMilli()
	if err := m.store.UpdateTenant(ctx, t); err != nil {
		return Tenant{}, err
	}
	m.broker.Forget(t.ID)
	m.mu.Lock()
	delete(m.throttles, t.ID) // re-read registrationsPerMinute
	m.mu.Unlock()
	changed["by"] = p.By
	m.audit(ctx, AuditRecord{TenantID: t.ID, Kind: "tenant_update", ActingFor: p.By}, changed)
	return t, nil
}

// TenantDeleteResult is DELETE /v1/tenants/{id}.
type TenantDeleteResult struct {
	TenantID           string   `json:"tenantID"`
	Killed             []string `json:"killed"`
	Purged             []string `json:"purged"`
	DeploymentsStopped []string `json:"deploymentsStopped"`
	DevicesDeleting    []string `json:"devicesDeleting,omitempty"`
}

// DeleteTenant removes a tenant. Without force it refuses while any object
// is live (409 tenant_has_live_objects); with force it kills every live
// root (cascade), purges, stops deployments, releases leases and finally
// drops the row and the tenant's custody entries. Audited as tenant_delete.
func (m *Mgmt) DeleteTenant(ctx context.Context, id string, force bool, by string) (TenantDeleteResult, error) {
	res := TenantDeleteResult{TenantID: id, Killed: []string{}, Purged: []string{}, DeploymentsStopped: []string{}}
	t, err := m.store.GetTenant(ctx, id)
	if err != nil {
		return res, err
	}
	live, err := m.store.ListObjects(ctx, ObjectFilter{TenantID: id, Status: []string{StatusPending, StatusMinted, StatusReady}})
	if err != nil {
		return res, err
	}
	if len(live) > 0 && !force {
		e := errf(409, "tenant_has_live_objects", "tenant %s has %d live objects; pass ?force=true to kill and purge them", id, len(live))
		e.Missing = map[string]any{"liveObjects": len(live)}
		return res, e
	}
	for _, o := range live {
		if o.ParentID != "" {
			continue // the root's cascade takes it
		}
		kr, err := m.Kill(ctx, o.ID, KillRequest{Cascade: true, Reason: "tenant deleted", By: by})
		if err != nil {
			return res, err
		}
		res.Killed = append(res.Killed, kr.Killed...)
		res.DevicesDeleting = append(res.DevicesDeleting, kr.DevicesDeleting...)
	}
	// Anything still live (a child whose root was already dead) goes too.
	if rest, err := m.store.ListObjects(ctx, ObjectFilter{TenantID: id, Status: []string{StatusPending, StatusMinted, StatusReady}}); err == nil {
		for _, o := range rest {
			if kr, err := m.Kill(ctx, o.ID, KillRequest{Reason: "tenant deleted", By: by}); err == nil {
				res.Killed = append(res.Killed, kr.Killed...)
			}
		}
	}
	m.FlushDeletes()
	if pr, err := m.Purge(ctx, id); err == nil {
		res.Purged = pr.Purged
	}
	if ds, err := m.store.LatestDeployments(ctx, id); err == nil {
		for _, d := range ds {
			if d.StoppedAt == 0 {
				m.store.StopDeployment(ctx, d.ID, m.now())
				res.DeploymentsStopped = append(res.DeploymentsStopped, d.ID)
			}
			if l, err := m.store.GetLease(ctx, LeaseName(id, d.Workload)); err == nil && l.Holder != "" {
				m.store.ReleaseLease(ctx, l.Name, l.Holder, l.Epoch)
			}
			m.mu.Lock()
			delete(m.depStatus, d.ID)
			m.mu.Unlock()
		}
	}
	if err := m.store.DeleteTenant(ctx, id); err != nil {
		return res, err
	}
	m.broker.Forget(id)
	m.mu.Lock()
	delete(m.throttles, id)
	delete(m.oboKeys, id)
	if q := m.queues[id]; q != nil {
		delete(m.queues, id)
	}
	m.mu.Unlock()
	m.custody.Delete(ctx, oboCustodyName(id))
	m.audit(ctx, AuditRecord{TenantID: id, Kind: "tenant_delete", ActingFor: by}, map[string]any{"tailnet": t.Tailnet, "force": force,
		"killed": len(res.Killed), "purged": len(res.Purged), "deployments": res.DeploymentsStopped, "by": by})
	return res, nil
}

// HostJoin is POST /v1/hosts: the record plus a single-use join key for
// the host's tag:twd-host node.
type HostJoin struct {
	Host       Host     `json:"host"`
	AuthKey    string   `json:"authKey"`
	ExpiresAt  int64    `json:"expiresAt"`
	Tags       []string `json:"tags"`
	Hostname   string   `json:"hostname"`
	ControlURL string   `json:"controlURL,omitempty"`
	Tailnet    string   `json:"tailnet,omitempty"`
}

// String never prints the key.
func (HostJoin) String() string { return "mgmt.HostJoin{[REDACTED]}" }

// HostCreate is the POST /v1/hosts body.
type HostCreate struct {
	HostID   string
	TenantID string
	Pool     string
	Tiers    []string
	By       string
}

// HostJoinKeyExpiry is how long a host join key lives.
const HostJoinKeyExpiry = 10 * time.Minute

// HostNodeName is the tsnet hostname of a host's own node.
func HostNodeName(hostID string) string { return "twd-host-" + hostID }

// operatorTenant finds the tenant whose tailnet is mgmt's own (the one
// whose credential mints host and mgmt keys). With an explicit ID it is
// looked up directly.
func (m *Mgmt) operatorTenant(ctx context.Context, id string) (Tenant, error) {
	if id != "" {
		t, err := m.store.GetTenant(ctx, id)
		if err != nil {
			return t, errf(404, "unknown_tenant", "tenant %q not found", id)
		}
		return t, nil
	}
	ts, err := m.store.ListTenants(ctx, Page{})
	if err != nil {
		return Tenant{}, err
	}
	for _, t := range ts {
		if m.cfg.OperatorTailnet != "" && strings.EqualFold(t.Tailnet, m.cfg.OperatorTailnet) {
			return t, nil
		}
	}
	if m.cfg.OperatorTailnet == "" && len(ts) == 1 {
		return ts[0], nil
	}
	return Tenant{}, errf(404, "unknown_tenant", "no tenant for the operator tailnet %q (pass tenantID)", m.cfg.OperatorTailnet)
}

// CreateHost records a host (status pending until it registers) and mints
// a single-use, pre-authorized tag:twd-host key for its node. Re-running it
// for a pending host mints a fresh key; a bound host is 409 exists.
func (m *Mgmt) CreateHost(ctx context.Context, r HostCreate) (HostJoin, bool, error) {
	if !validHostID(r.HostID) {
		return HostJoin{}, false, errf(400, "bad_request", "hostID must be one safe path element (letters, digits, . _ -; ≤63)")
	}
	t, err := m.operatorTenant(ctx, r.TenantID)
	if err != nil {
		return HostJoin{}, false, err
	}
	old, gerr := m.store.GetHost(ctx, r.HostID)
	created := errors.Is(gerr, ErrNotFound)
	if gerr == nil && (old.NodeID != "" || !old.Pending()) {
		return HostJoin{}, false, errf(409, "exists", "host %s is already registered (node %s); delete it first", r.HostID, old.NodeID)
	}
	api, err := m.broker.API(ctx, t, purposeMint)
	if err != nil {
		return HostJoin{}, false, errf(502, "bootstrap_failed", "no mint credential for tenant %s: %v", t.ID, err)
	}
	tags := []string{HostTag}
	k, err := api.CreateKey(ctx, tsapi.KeyOptions{Tags: tags, Preauthorized: true, ExpirySeconds: int(HostJoinKeyExpiry / time.Second),
		Description: "twd host " + r.HostID})
	if err != nil {
		if tsapi.IsTagsNotPermitted(err) {
			e := errf(403, "policy_missing_tag_owners", "the control plane refused to mint a %s key with tenant %s's %s credential: %v", HostTag, t.ID, t.MintMode, err)
			e.Snippet = TagOwnersSnippet()
			return HostJoin{}, false, e
		}
		return HostJoin{}, false, errf(502, "bootstrap_failed", "mint host join key: %v", err)
	}
	now := m.now().UnixMilli()
	tiers := r.Tiers
	if len(tiers) == 0 {
		tiers = []string{"process"}
	}
	h := Host{ID: r.HostID, Tiers: tiers, Pool: r.Pool, CreatedAt: old.CreatedAt, UpdatedAt: now}
	if h.CreatedAt == 0 {
		h.CreatedAt = now
	}
	if err := m.store.UpsertHost(ctx, h); err != nil {
		api.DeleteKey(ctx, k.ID)
		return HostJoin{}, false, err
	}
	m.audit(ctx, AuditRecord{TenantID: t.ID, Kind: "host_add", ActingFor: r.By}, map[string]any{"host": r.HostID, "keyID": k.ID,
		"tags": tags, "mode": t.MintMode, "by": r.By, "new": created})
	return HostJoin{Host: h, AuthKey: k.Secret, ExpiresAt: m.now().Add(HostJoinKeyExpiry).UnixMilli(), Tags: tags,
		Hostname: HostNodeName(r.HostID), ControlURL: m.cfg.OperatorControlURL, Tailnet: t.Tailnet}, created, nil
}

func validHostID(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || (i > 0 && (r == '-' || r == '_' || r == '.'))
		if !ok {
			return false
		}
	}
	return true
}

// HostDeleteResult is DELETE /v1/hosts/{id}.
type HostDeleteResult struct {
	HostID         string   `json:"hostID"`
	Killed         []string `json:"killed"`
	LeasesReleased []string `json:"leasesReleased"`
	DevicesLeft    []string `json:"devicesLeft,omitempty"`
}

// HostLeases lists the namespace leases a host holds.
func (m *Mgmt) HostLeases(ctx context.Context, hostID string) ([]Lease, error) {
	ds, err := m.store.Assignments(ctx, hostID)
	if err != nil {
		return nil, err
	}
	var out []Lease
	for _, d := range ds {
		if l, err := m.store.GetLease(ctx, LeaseName(d.TenantID, d.Workload)); err == nil && l.Holder == hostID {
			out = append(out, l)
		}
	}
	return out, nil
}

// DeleteHost drains (with drain) and removes a host: its objects are
// killed, its leases released, its record dropped. Without drain it refuses
// while leases are held (409 host_holds_leases). The host's own tsnet
// device is left for the operator (DevicesLeft) unless mgmt can reach the
// operator tenant's lifecycle credential.
func (m *Mgmt) DeleteHost(ctx context.Context, id string, drain bool, by string) (HostDeleteResult, error) {
	res := HostDeleteResult{HostID: id, Killed: []string{}, LeasesReleased: []string{}}
	h, err := m.store.GetHost(ctx, id)
	if err != nil {
		return res, err
	}
	leases, err := m.HostLeases(ctx, id)
	if err != nil {
		return res, err
	}
	if len(leases) > 0 && !drain {
		names := make([]string, 0, len(leases))
		for _, l := range leases {
			names = append(names, l.Name)
		}
		e := errf(409, "host_holds_leases", "host %s holds %d leases; pass ?drain=true", id, len(leases))
		e.Missing = map[string]any{"leases": names}
		return res, e
	}
	for _, l := range leases {
		tenantID, workload, _ := strings.Cut(l.Name, "/")
		objs, _ := m.store.ListObjects(ctx, ObjectFilter{TenantID: tenantID, Workload: workload, Status: []string{StatusPending, StatusMinted, StatusReady}})
		for _, o := range objs {
			if o.ParentID != "" {
				continue
			}
			if kr, err := m.Kill(ctx, o.ID, KillRequest{Cascade: true, Reason: "host " + id + " removed", By: by}); err == nil {
				res.Killed = append(res.Killed, kr.Killed...)
			}
		}
		if ds, err := m.store.Assignments(ctx, id); err == nil {
			for _, d := range ds {
				if d.TenantID == tenantID && d.Workload == workload {
					m.setDepStatus(d.ID, DeploymentStatus{Status: "draining"})
				}
			}
		}
		if err := m.store.ReleaseLease(ctx, l.Name, l.Holder, l.Epoch); err == nil {
			res.LeasesReleased = append(res.LeasesReleased, l.Name)
		}
	}
	if err := m.store.DeleteHost(ctx, id); err != nil {
		return res, err
	}
	m.mu.Lock()
	delete(m.pushURLs, id)
	m.mu.Unlock()
	if h.NodeID != "" {
		deleted := false
		if t, err := m.operatorTenant(ctx, ""); err == nil {
			if api, err := m.broker.API(ctx, t, purposeLifecycle); err == nil && api.DeleteDevice(ctx, h.NodeID) == nil {
				deleted = true
			}
		}
		if !deleted {
			res.DevicesLeft = append(res.DevicesLeft, h.NodeID)
		}
	}
	m.audit(ctx, AuditRecord{Kind: "host_delete", ActingFor: by}, map[string]any{"host": id, "node": h.NodeID, "drain": drain,
		"killed": len(res.Killed), "leases": res.LeasesReleased, "devicesLeft": res.DevicesLeft, "by": by})
	return res, nil
}

// Undeploy (DELETE /v1/deployments/{id}) kills the deployment's objects,
// releases the workload lease and marks it stopped. Only the latest
// deployment of a workload can be undeployed.
func (m *Mgmt) Undeploy(ctx context.Context, id, reason, by string) (KillResult, error) {
	res := KillResult{Killed: []string{}, DevicesDeleted: []string{}}
	d, err := m.store.GetDeployment(ctx, id)
	if err != nil {
		return res, err
	}
	if latest, err := m.store.LatestDeployments(ctx, d.TenantID); err == nil {
		for _, l := range latest {
			if l.Workload == d.Workload && l.ID != d.ID {
				return res, errf(409, "superseded", "%s was superseded by %s; undeploy the latest deployment", d.ID, l.ID)
			}
		}
	}
	if d.StoppedAt != 0 {
		return res, nil // idempotent
	}
	if reason == "" {
		reason = "undeployed"
	}
	objs, err := m.store.ListObjects(ctx, ObjectFilter{TenantID: d.TenantID, Workload: d.Workload, Status: []string{StatusPending, StatusMinted, StatusReady}})
	if err != nil {
		return res, err
	}
	for _, o := range objs {
		if o.ParentID != "" {
			continue
		}
		kr, err := m.Kill(ctx, o.ID, KillRequest{Cascade: true, Reason: reason, By: by})
		if err != nil {
			continue
		}
		res.Killed = append(res.Killed, kr.Killed...)
		res.Pushed = append(res.Pushed, kr.Pushed...)
		res.DevicesDeleted = append(res.DevicesDeleted, kr.DevicesDeleted...)
		res.DevicesDeleting = append(res.DevicesDeleting, kr.DevicesDeleting...)
	}
	if l, err := m.store.GetLease(ctx, LeaseName(d.TenantID, d.Workload)); err == nil && l.Holder != "" {
		m.store.ReleaseLease(ctx, l.Name, l.Holder, l.Epoch)
	}
	if err := m.store.StopDeployment(ctx, id, m.now()); err != nil {
		return res, err
	}
	m.setDepStatus(d.ID, DeploymentStatus{Status: "stopped"})
	go m.push(d.HostID, "/v1/push/assignments", map[string]any{})
	m.audit(ctx, AuditRecord{TenantID: d.TenantID, Kind: "undeploy", ActingFor: by}, map[string]any{"deploymentID": d.ID,
		"workload": d.Workload, "host": d.HostID, "killed": len(res.Killed), "reason": reason, "by": by})
	return res, nil
}

// PreflightBundle is POST /v1/deployments?dryRun=true: the deploy body,
// checked against the policy, nothing written.
func (m *Mgmt) PreflightBundle(ctx context.Context, r DeployRequest) (PreflightResult, error) {
	var res PreflightResult
	if _, err := m.store.GetTenant(ctx, r.TenantID); err != nil {
		return res, errf(404, "unknown_tenant", "tenant %q not found", r.TenantID)
	}
	if !validName(r.Workload) {
		return res, errf(400, "bad_request", "workload must be a DNS label ([a-z0-9-], ≤63)")
	}
	classes := r.Namespaces
	var connects []string
	if len(r.Bundle) > 0 {
		cfg, err := BundleConfig(r.Bundle)
		if err != nil && len(classes) == 0 {
			return res, errf(400, "bad_bundle", "%v", err)
		}
		if err == nil {
			if len(classes) == 0 {
				classes = cfg.Classes()
			}
			connects = cfg.Tailnet.Connects
		}
	}
	if len(classes) == 0 {
		return res, errf(400, "bad_bundle", "no namespaces: the bundle's twd.jsonc declares neither tailnet.tags (the workload node) nor tailnet.objects")
	}
	var src []string
	for _, c := range classes {
		for _, t := range c.Tags {
			if !slices.Contains(src, t) {
				src = append(src, t)
			}
		}
	}
	res, err := m.Preflight(ctx, r.TenantID, PreflightRequest{SrcTags: src, Connects: connects})
	if err != nil {
		return res, err
	}
	res.SrcTags = src
	m.audit(ctx, AuditRecord{TenantID: r.TenantID, Kind: "preflight", ActingFor: r.By}, map[string]any{"workload": r.Workload,
		"ok": res.OK, "missing": len(res.Missing), "missingTagOwners": res.MissingTagOwners, "by": r.By})
	return res, nil
}

// SetObjectTTL (admin PATCH {ttl}) sets or clears an object's TTL; the
// reaper applies it from createdAt.
func (m *Mgmt) SetObjectTTL(ctx context.Context, id string, ttl *int64, by string) (Object, error) {
	o, err := m.store.UpdateObject(ctx, id, nil, func(o *Object) error {
		if !o.Live() {
			return errf(410, "killed", "object %s is %s", o.ID, o.Status)
		}
		if ttl == nil {
			o.TTLSeconds = 0
		} else {
			o.TTLSeconds = *ttl
		}
		o.UpdatedAt = m.now().UnixMilli()
		return nil
	})
	if err != nil {
		return o, err
	}
	m.audit(ctx, AuditRecord{TenantID: o.TenantID, ObjectID: o.ID, Kind: "object_update", ActingFor: by}, map[string]any{"ttl": o.TTLSeconds, "by": by})
	return o, nil
}

// StatusCounts is GET /v1/status.
type StatusCounts struct {
	Hosts, LiveHosts int
	Tenants          int
	Objects          map[string]int
	PendingDeletes   int
	Clock            ClockStatus
}

// Status gathers counters.
func (m *Mgmt) Status(ctx context.Context) (StatusCounts, error) {
	var st StatusCounts
	hosts, err := m.store.ListHosts(ctx, Page{})
	if err != nil {
		return st, err
	}
	now := m.now()
	st.Hosts = len(hosts)
	for _, h := range hosts {
		if h.Live(now) {
			st.LiveHosts++
		}
	}
	ts, err := m.store.ListTenants(ctx, Page{})
	if err != nil {
		return st, err
	}
	st.Tenants = len(ts)
	if st.Objects, err = m.store.CountObjects(ctx); err != nil {
		return st, err
	}
	st.PendingDeletes = m.PendingDeletes()
	st.Clock = m.clockStatus()
	return st, nil
}

// DeploymentStatusOf returns the host-reported status of a deployment,
// "stopped" once undeployed, "superseded" once a newer deploy exists.
func (m *Mgmt) deploymentStatus(ctx context.Context, d Deployment) (string, string) {
	if d.StoppedAt != 0 {
		return "stopped", ""
	}
	m.mu.Lock()
	st := m.depStatus[d.ID]
	m.mu.Unlock()
	if ds, err := m.store.LatestDeployments(ctx, d.TenantID); err == nil {
		for _, l := range ds {
			if l.Workload == d.Workload && l.ID != d.ID {
				return "superseded", st.Error
			}
		}
	}
	if st.Status == "" {
		return "assigned", ""
	}
	return st.Status, st.Error
}

var _ = fmt.Sprintf

// TagOwnersSnippet is the tagOwners twd's own nodes need: tag:twd (the
// mint credential's tag) must own the mgmt and host tags so an OAuth
// client or trust credential tagged tag:twd can mint their keys.
func TagOwnersSnippet() string {
	return `"tagOwners": { "tag:twd": ["autogroup:admin"], "` + MgmtTag + `": ["autogroup:admin", "tag:twd"], "` + HostTag + `": ["autogroup:admin", "tag:twd"] }`
}
