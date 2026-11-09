package main

import (
	"context"
	"slices"

	"fmt"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/internal/policy"
	"github.com/rosskukulinski/twd/internal/procsb"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/mgmt"
)

// TenantRef is one tenant this twd serves: the operator tenant (`twd up`)
// or one added with `twd tenant add`. Each has its own tailnet, API key
// file (path only), control URL, netd registry, workerd processes, state
// dir and registration throttle; nothing is shared between them.
type TenantRef struct {
	Tailnet    string `json:"tailnet"`
	TenantID   string `json:"tenantID"`
	MintMode   string `json:"mintMode,omitempty"` // apikey | wif ("" = apikey)
	APIKeyFile string `json:"apiKeyFile,omitempty"`
	ControlURL string `json:"controlURL"`
	AddedAt    string `json:"addedAt,omitempty"`
}

// tenants lists the operator tenant first, then the added ones. Linked
// to a remote mgmt, it is mgmt's tenant list (fetched once per command).
func (s *state) tenants() []TenantRef {
	if s.remote() {
		if s.remoteTenants == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			ts, _ := listAll[mgmt.Tenant](ctx, s.mgmt(), "/v1/tenants", 0)
			s.remoteTenants = []TenantRef{}
			for _, t := range ts {
				s.remoteTenants = append(s.remoteTenants, TenantRef{Tailnet: t.Tailnet, TenantID: t.ID, MintMode: t.MintMode, ControlURL: t.ControlURL})
			}
		}
		return s.remoteTenants
	}
	out := []TenantRef{}
	if s.Cfg.TenantID != "" {
		out = append(out, TenantRef{Tailnet: s.Cfg.Tailnet, TenantID: s.Cfg.TenantID, MintMode: s.Cfg.MintMode, APIKeyFile: s.Cfg.APIKeyFile, ControlURL: s.Cfg.ControlURL})
	}
	return append(out, s.Cfg.Tenants...)
}

// tenant resolves --tenant (a tailnet, a tenant ID or the tailnet's first
// label); "" is the operator tenant.
func (s *state) tenant(ref string) (TenantRef, error) {
	ts := s.tenants()
	if ref == "" {
		if len(ts) == 0 {
			if s.remote() {
				return TenantRef{}, fail("no_tenant", "the linked mgmt "+s.Link.Endpoint+" has no tenant yet").withFix("finish `twd up` on the host")
			}
			return TenantRef{}, fail("not_up", "no tenant yet in "+s.Dir).withFix("twd up")
		}
		return ts[0], nil
	}
	ref = strings.TrimSuffix(ref, ".")
	for _, t := range ts {
		label, _, _ := strings.Cut(t.Tailnet, ".")
		if ref == t.Tailnet || ref == t.TenantID || ref == label {
			return t, nil
		}
	}
	var names []string
	for _, t := range ts {
		names = append(names, t.Tailnet)
	}
	return TenantRef{}, fail("unknown_tenant", fmt.Sprintf("no tenant %q", ref), "tenants: "+strings.Join(names, ", ")).
		withFix("twd tenant ls", "twd tenant add <tailnet> --api-key-file <file>   (on the host)")
}

func tenantFlag(f *flags) *string {
	return f.String("tenant", "", "tenant: its tailnet, tenant ID or tailnet label (default: the `twd up` tenant)")
}

// objectsOf lists a tenant's objects (every page; max > 0 caps the count).
func (s *state) objectsOf(ctx context.Context, tenantID string) ([]mgmt.Object, error) {
	return s.objectsOfN(ctx, tenantID, 0)
}

func (s *state) objectsOfN(ctx context.Context, tenantID string, max int) ([]mgmt.Object, error) {
	return listAll[mgmt.Object](ctx, s.mgmt(), "/v1/objects?tenant="+url.QueryEscape(tenantID), max)
}

func cmdTenant(args []string) error {
	// Global flags (--state-dir X, --json) may precede the subcommand.
	sub := -1
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			sub = i
			break
		}
		if (a == "--state-dir" || a == "-state-dir") && i+1 < len(args) {
			i++
		}
	}
	if sub < 0 {
		return fail("bad_request", "twd tenant needs a subcommand: add | ls | set | rm", "usage: twd "+usageOf("tenant"))
	}
	rest := append(append([]string{}, args[:sub]...), args[sub+1:]...)
	switch args[sub] {
	case "add":
		return cmdTenantAdd(rest)
	case "ls", "list":
		return cmdTenantLs(rest)
	case "set":
		return cmdTenantSet(rest)
	case "rm", "remove", "delete":
		return cmdTenantRm(rest)
	}
	return fail("bad_request", fmt.Sprintf("unknown subcommand %q", args[sub]), "usage: twd "+usageOf("tenant"))
}

type tenantAddResult struct {
	Tailnet    string             `json:"tailnet"`
	TenantID   string             `json:"tenantID"`
	ControlURL string             `json:"controlURL"`
	Existing   bool               `json:"existing"`
	PolicyOK   bool               `json:"policyOK"`
	PolicyDiff string             `json:"policySnippet,omitempty"`
	Applied    bool               `json:"policyApplied"`
	Timings    map[string]float64 `json:"timingsMs"`
}

// cmdTenantAdd is the CLI equivalent of `twd connect` in apikey mode: it
// checks the key can read the tailnet's policy, registers the tenant with
// mgmt (its own throttle and mint queue) and records it in twd.json. The
// key file path is stored, never the key.
func cmdTenantAdd(args []string) error {
	f := newFlags("tenant add")
	keyFile := f.String("api-key-file", "", "file with TS_API_KEY=… for this tailnet (mint mode \"apikey\"; read at use, never copied)")
	apply := f.Bool("policy-apply", false, "apply the base policy snippet (validated first, written with If-Match); default: print it")
	control := f.String("control-url", prodControl, "coordination server for this tailnet's nodes")
	rpm := f.Int("registrations-per-minute", 0, "this tenant's node registration throttle (default 200/min)")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fail("bad_request", "twd tenant add needs one tailnet", "usage: twd "+usageOf("tenant"))
	}
	tailnet := strings.TrimSuffix(pos[0], ".")
	if *keyFile == "" {
		return fail("bad_request", "twd tenant add needs --api-key-file (mint mode \"apikey\")",
			"WIF trust credentials (no stored secret) need `twd connect`, which is not built yet").
			withFix("twd tenant add " + tailnet + " --api-key-file <file with TS_API_KEY=…>")
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	// A second tenant needs a process sandbox (or the explicit opt-out):
	// without one, every tenant's code shares one uid.
	if sb, _ := hostd.ResolveSandbox(hostd.SandboxAuto); sb == hostd.SandboxNone && !s.Cfg.InsecureSharedHost {
		why := "no process sandbox on " + runtime.GOOS
		if runtime.GOOS == "linux" {
			why = "Landlock is unavailable on this kernel"
		}
		return fail("sandbox_unavailable", "this host has no process sandbox ("+why+"), so its process tier must not serve more than one tenant",
			"twd doctor explains the sandbox check; tenants of a shared host need Seatbelt (macOS) or Landlock (Linux)").
			withFix("use a kernel with Landlock, deploy with tier \"microvm\", or (tenants NOT isolated) twd down && twd up --insecure-shared-host")
	}
	if s.remote() {
		return fail("remote_unsupported", "twd tenant add runs on the host that runs twd up (this machine is linked to "+s.Link.Endpoint+")")
	}
	if err := s.requireUp(); err != nil {
		return err
	}
	// Defence in depth (Linux): sockets this user can reach that would let
	// a process act as someone else. Container engines are root-equivalent
	// for twd itself: refuse. tailscaled's socket matters only if some
	// workload could reach it: every workload under Landlock+seccomp is
	// denied AF_UNIX, except Node apps listening on a unix socket.
	if hs := procsb.ScanHostSockets(); !hs.Empty() && !s.Cfg.InsecureSharedHost {
		fix := "run twd as a dedicated user that can't reach those sockets and isn't in the docker/podman/lxd groups, or (tenants NOT isolated) twd down && twd up --insecure-shared-host"
		if len(hs.Hard) > 0 {
			return fail("sandbox_unavailable", "this user can reach "+strings.Join(hs.Hard, ", ")+": root-equivalent for twd-hostd itself, so this host must not serve more than one tenant",
				"twd doctor lists them under host_sockets").withFix(fix)
		}
		if why := tailscaledExposed(s, hs.Tailscaled); why != "" {
			return fail("sandbox_unavailable", "this user can reach "+hs.Tailscaled+" (this host's tailnet identity) and "+why+", so this host must not serve more than one tenant",
				"twd doctor lists it under host_sockets").withFix(fix + "; or enable Landlock and use TCP listen mode for Node apps")
		}
	}
	t0 := time.Now()
	res := tenantAddResult{Tailnet: tailnet, ControlURL: *control, Timings: map[string]float64{}}
	absKey, _ := filepath.Abs(*keyFile)
	apiKey, err := tsapi.LoadEnvFile(absKey, "TS_API_KEY")
	if err != nil {
		return fail("bad_request", "can't read TS_API_KEY from "+absKey, err.Error())
	}
	if st, err := os.Stat(absKey); err == nil && st.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "warning: %s is readable by other users (mode %v); chmod 600 it\n", absKey, st.Mode().Perm())
	}
	for _, t := range s.tenants() {
		if t.APIKeyFile == absKey && t.Tailnet != tailnet {
			return fail("state_conflict", "that API key file already serves tenant "+t.Tailnet,
				"a key belongs to one tailnet; each tenant needs its own").withFix("twd tenant add " + tailnet + " --api-key-file <this tailnet's key file>")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	api := &tsapi.Client{Tailnet: tailnet, APIKey: apiKey, BaseURL: s.Cfg.TSAPIURL}
	pol, _, err := api.GetPolicyHuJSON(ctx)
	if err != nil {
		return fail("bootstrap_failed", "the Tailscale API rejected the key for "+tailnet,
			"step:  GET /api/v2/tailnet/"+tailnet+"/acl → "+err.Error()).
			withFix("check the tailnet name and that the key in " + absKey + " is valid and can read the policy")
	}
	res.Timings["api_check"] = ms(time.Since(t0))

	tid := tenantIDFor(tailnet)
	var existing mgmt.Tenant
	if err := s.mgmt().Do(ctx, "GET", "/v1/tenants/"+tid, nil, &existing); err == nil {
		if existing.Tailnet != tailnet {
			return fail("state_conflict", fmt.Sprintf("tenant ID %s is taken by tailnet %s", tid, existing.Tailnet))
		}
		res.Existing = true
	} else {
		body := map[string]any{"tenantID": tid, "tailnet": tailnet, "controlURL": *control, "mintMode": "apikey", "apiKeyFile": absKey}
		if *rpm > 0 {
			body["registrationsPerMinute"] = *rpm
		}
		var tr struct {
			TenantID string `json:"tenantID"`
		}
		if err := s.mgmt().Do(ctx, "POST", "/v1/tenants", body, &tr); err != nil {
			return err
		}
		tid = tr.TenantID
	}
	res.TenantID = tid
	if tid != s.Cfg.TenantID {
		found := false
		for i, t := range s.Cfg.Tenants {
			if t.TenantID == tid {
				s.Cfg.Tenants[i].APIKeyFile, s.Cfg.Tenants[i].ControlURL, found = absKey, *control, true
			}
		}
		if !found {
			s.Cfg.Tenants = append(s.Cfg.Tenants, TenantRef{Tailnet: tailnet, TenantID: tid, APIKeyFile: absKey, ControlURL: *control,
				AddedAt: time.Now().UTC().Format(time.RFC3339)})
		}
		if err := s.save(); err != nil {
			return err
		}
	}
	res.Timings["tenant_ready"] = ms(time.Since(t0))

	miss, err := policy.Missing(pol, baseSnippet())
	if err != nil {
		return err
	}
	res.PolicyOK = miss.Empty()
	if !res.PolicyOK {
		res.PolicyDiff = miss.HuJSON()
		if *apply {
			ar, err := policy.Apply(ctx, api, baseSnippet())
			if err != nil {
				return fail("policy_apply_failed", "could not apply the policy snippet", err.Error()).withFix("add it in the admin console:", res.PolicyDiff)
			}
			res.Applied, res.PolicyOK = ar.Changed, true
		}
	}
	res.Timings["total"] = ms(time.Since(t0))
	emit(f, res, func() {
		verb := "added"
		if res.Existing {
			verb = "already served"
		}
		fmt.Printf("✓ tenant   %s %s (tenant %s, API key, mint mode apikey) · control %s\n", tailnet, verb, tid, *control)
		fmt.Printf("  isolated: its own netd registry, workerd processes, state dir (0700) and registration throttle\n")
		switch {
		case res.Applied:
			fmt.Printf("✓ policy   applied tag:twd tagOwners (validated, If-Match)\n")
		case res.PolicyOK:
			fmt.Printf("✓ policy   tag:twd tagOwners present\n")
		default:
			fmt.Printf("› policy   not applied (re-run with --policy-apply, or add in the admin console):\n")
			for _, l := range strings.Split(strings.TrimSpace(res.PolicyDiff), "\n") {
				fmt.Printf("    %s\n", l)
			}
		}
		fmt.Printf("deploy to it with `twd deploy --tenant %s`\n", tailnet)
	})
	return nil
}

type tenantLsEntry struct {
	TenantRef
	Operator bool           `json:"operator"`
	Objects  map[string]int `json:"objectsByStatus"`
	Rate     float64        `json:"registrationsPerMinute"`
	Queue    int            `json:"mintQueue"`
}

func cmdTenantLs(args []string) error {
	f := newFlags("tenant ls")
	if _, err := f.parse(args); err != nil {
		return err
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	if err := s.requireUp(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out := []tenantLsEntry{}
	for i, t := range s.tenants() {
		e := tenantLsEntry{TenantRef: t, Operator: i == 0, Objects: map[string]int{}}
		var tn struct {
			Q    int     `json:"mintQueue"`
			Rate float64 `json:"effectiveRegistrationsPerMinute"`
		}
		s.mgmt().Do(ctx, "GET", "/v1/tenants/"+t.TenantID, nil, &tn)
		e.Rate, e.Queue = tn.Rate, tn.Q
		if objs, err := s.objectsOf(ctx, t.TenantID); err == nil {
			for _, o := range objs {
				e.Objects[o.Status]++
			}
		}
		out = append(out, e)
	}
	emit(f, out, func() {
		fmt.Printf("  %-24s %-18s %-9s %-14s %s\n", "TAILNET", "TENANT", "ROLE", "THROTTLE", "OBJECTS")
		for _, e := range out {
			role := "tenant"
			if e.Operator {
				role = "operator"
			}
			fmt.Printf("  %-24s %-18s %-9s %-14s %v\n", e.Tailnet, e.TenantID, role, fmt.Sprintf("%.0f/min", e.Rate), e.Objects)
		}
	})
	return nil
}

// tailscaledExposed says why a workload on this host could reach the system
// tailscaled's socket ("" if none can): the sandbox isn't Landlock+seccomp,
// or a running Node app listens on a unix socket. It asks twd-hostd.
func tailscaledExposed(s *state, sock string) string {
	if sock == "" {
		return ""
	}
	if sb, _ := hostd.ResolveSandbox(hostd.SandboxAuto); sb != hostd.SandboxLandlock {
		return "the process sandbox (" + sb + ") doesn't deny unix sockets"
	}
	if s == nil || !s.supervisorAlive() {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var hv hostd.StatusView
	if err := s.hostd().Do(ctx, "GET", "/v1/status", nil, &hv); err != nil {
		return "twd-hostd is not answering, so running workloads can't be checked"
	}
	if !strings.HasPrefix(hv.Sandbox, hostd.SandboxLandlock) {
		return "twd-hostd runs with sandbox " + hv.Sandbox
	}
	for _, w := range hv.Workloads {
		if w.Node != nil && w.Node.Listen == "unix" {
			return "Node app " + w.TenantID + "/" + w.Workload + " listens on a unix socket (that mode keeps AF_UNIX sockets)"
		}
	}
	return ""
}

// cmdTenantSet is `twd tenant set <tenant> [--max-objects N] [--registrations-per-minute N]
// [--pool P] [--tier process|microvm] [--wif-client-id ID] [--wif-lifecycle-client-id ID|-]`:
// a JSON Merge Patch (PATCH /v1/tenants/{id}); "-" clears a field.
func cmdTenantSet(args []string) error {
	f := newFlags("tenant set")
	maxObj := f.Int("max-objects", 0, "object quota")
	rpm := f.Int("registrations-per-minute", 0, "node registration throttle")
	pool := f.String("pool", "", "host pool the tenant's deployments are placed on (\"-\" = any host)")
	tier := f.String("tier", "", "default isolation tier: process | microvm (\"-\" = unset)")
	wifID := f.String("wif-client-id", "", "switch to mint mode wif with this mint trust credential")
	wifLife := f.String("wif-lifecycle-client-id", "", "lifecycle trust credential (\"-\" = clear)")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fail("bad_request", "twd tenant set needs one tenant", "usage: twd "+usageOf("tenant"))
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	if err := s.requireUp(); err != nil {
		return err
	}
	tn, err := s.tenant(pos[0])
	if err != nil {
		return err
	}
	patch := map[string]any{}
	val := func(v string) any {
		if v == "-" {
			return nil
		}
		return v
	}
	if *maxObj > 0 {
		patch["maxObjects"] = *maxObj
	}
	if *rpm > 0 {
		patch["registrationsPerMinute"] = *rpm
	}
	if *pool != "" {
		patch["pool"] = val(*pool)
	}
	if *tier != "" {
		patch["tier"] = val(*tier)
	}
	if *wifID != "" {
		patch["mintMode"], patch["wifClientID"] = "wif", *wifID
	}
	if *wifLife != "" {
		patch["wifLifecycleClientID"] = val(*wifLife)
	}
	if len(patch) == 0 {
		return fail("bad_request", "nothing to set", "usage: twd "+usageOf("tenant"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var t mgmt.Tenant
	if err := s.mgmt().DoHeaders(ctx, "PATCH", "/v1/tenants/"+tn.TenantID, patch, &t,
		map[string]string{"Content-Type": "application/merge-patch+json", "Twd-Actor": actor()}); err != nil {
		return err
	}
	emit(f, t, func() {
		fmt.Printf("✓ tenant   %s (%s) updated · mint mode %s · %d objects max · %d registrations/min · pool %q · tier %q\n",
			t.Tailnet, t.ID, t.MintMode, t.MaxObjects, t.RegistrationsPerMinute, t.Pool, t.Tier)
	})
	return nil
}

// cmdTenantRm is `twd tenant rm <tenant> [--force]` (DELETE /v1/tenants/{id}):
// refused while objects are live unless --force kills and purges them.
func cmdTenantRm(args []string) error {
	f := newFlags("tenant rm")
	force := f.Bool("force", false, "kill every live object (cascade) and purge before deleting")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fail("bad_request", "twd tenant rm needs one tenant", "usage: twd "+usageOf("tenant"))
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	if err := s.requireUp(); err != nil {
		return err
	}
	tn, err := s.tenant(pos[0])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	q := ""
	if *force {
		q = "?force=true"
	}
	var res struct {
		TenantID           string   `json:"tenantID"`
		Killed             []string `json:"killed"`
		Purged             []string `json:"purged"`
		DeploymentsStopped []string `json:"deploymentsStopped"`
	}
	if err := s.mgmt().DoHeaders(ctx, "DELETE", "/v1/tenants/"+tn.TenantID+q, nil, &res, map[string]string{"Twd-Actor": actor()}); err != nil {
		if me, ok := mgmtapi.AsError(err); ok && me.Code == "tenant_has_live_objects" {
			return fail("tenant_has_live_objects", me.Message).withFix("twd tenant rm " + pos[0] + " --force")
		}
		return err
	}
	// Forget it locally too.
	if !s.remote() {
		if s.Cfg.TenantID == tn.TenantID {
			s.Cfg.TenantID = ""
		}
		s.Cfg.Tenants = slices.DeleteFunc(s.Cfg.Tenants, func(t TenantRef) bool { return t.TenantID == tn.TenantID })
		s.save()
	}
	res.TenantID = tn.TenantID
	emit(f, res, func() {
		fmt.Printf("✓ tenant   %s (%s) removed · %d objects killed · %d forgotten · %d deployments stopped\n", tn.Tailnet, tn.TenantID, len(res.Killed), len(res.Purged), len(res.DeploymentsStopped))
	})
	return nil
}
