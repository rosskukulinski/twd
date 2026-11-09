package cliflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/internal/policy"
	"github.com/rosskukulinski/twd/internal/procsb"
	"github.com/rosskukulinski/twd/netd/nettest"
)

// MultiPolicy is what E2E-MULTI needs in each tenant's policy: exactly
// what M4 added to tailnet 1 (Policy: the agent and mcp templates' tags
// owned by tag:twd, the test client → twd nodes, the agents → the tenant's
// own private echo, tag:ttest), so tailnet 1 needs no write and tailnet-2
// mirrors it.
func MultiPolicy() policy.Snippet { return Policy() }

// Tenant is one side of E2E-MULTI.
type Tenant struct {
	Key        string // "A" | "B"
	Tailnet    string
	APIKeyFile string
	ControlURL string
	// NewNode starts a tsnet test node in this tenant's tailnet named
	// <prefix><role> (tag:ttest on production control).
	NewNode func(role string) *nettest.Node
	// Res collects the IDs twd created in this tailnet (leftover checks).
	Res *Results

	TenantID                   string
	client, echo               *nettest.Node
	clientID, clientName       string
	clientIP, echoIP, echoName string
	wlMagic, wlIP              string
	mcpMagic                   string
	agent                      object
	agentIP                    string
	wlObj                      object
	hostDOStorage, hostInstDir string
	hostPID                    int
	sandbox, profile           string
}

// MultiEnv is E2E-MULTI: one twd host (the CLI's supervisor) serving
// tenant A (the operator tailnet, `twd up`) and tenant B (`twd tenant
// add`), with the same app deployed to both.
type MultiEnv struct {
	*Env // CLI runner; Env.Res holds the overall checks and numbers
	A, B *Tenant
	// RequireSandbox fails (d) unless both tenants' workerds run under
	// Seatbelt (macOS).
	RequireSandbox bool
	// GlobalNodeIDs: node IDs are unique across tailnets (production
	// control). Two testcontrols number nodes alike, so (b) then compares
	// node IDs only within a tailnet.
	GlobalNodeIDs bool
}

func (m *MultiEnv) tenants() []*Tenant { return []*Tenant{m.A, m.B} }

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func (m *MultiEnv) other(t *Tenant) *Tenant {
	if t == m.A {
		return m.B
	}
	return m.A
}

func (m *MultiEnv) objectsOf(t *Tenant) []object {
	var ls struct {
		TenantID string   `json:"tenantID"`
		Objects  []object `json:"objects"`
	}
	m.MustJSON(&ls, "ls", "--all", "--tenant", t.Tailnet)
	if ls.TenantID != t.TenantID {
		m.T.Fatalf("ls --tenant %s answered for tenant %s", t.Tailnet, ls.TenantID)
	}
	return ls.Objects
}

func (m *MultiEnv) waitReady(t *Tenant, ns, name string, timeout time.Duration) object {
	m.T.Helper()
	dl := time.Now().Add(timeout)
	for {
		for _, o := range m.objectsOf(t) {
			if o.Namespace == ns && o.Name == name && o.Status == "ready" && o.MagicDNS != "" {
				return o
			}
		}
		if time.Now().After(dl) {
			m.T.Fatalf("tenant %s: %s/%s not ready after %v", t.Key, ns, name, timeout)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func selfOf(n *nettest.Node) (id, name, ip string) {
	st, err := n.LC.StatusWithoutPeers(context.Background())
	if err != nil || st.Self == nil {
		return "", "", ""
	}
	if len(st.Self.TailscaleIPs) > 0 {
		ip = st.Self.TailscaleIPs[0].String()
	}
	return string(st.Self.ID), strings.TrimSuffix(st.Self.DNSName, "."), ip
}

// Run executes E2E-MULTI checks (a)–(f). The caller cleans up test nodes;
// Down removes twd's own devices in both tailnets.
func (m *MultiEnv) Run() {
	t, res := m.T, m.Res
	m.A.Key, m.B.Key = "A", "B"

	// twd up: the operator tailnet is tenant A.
	upArgs := []string{"up", "--tailnet", m.A.Tailnet, "--api-key-file", m.A.APIKeyFile, "--yes", "--ephemeral",
		"--hostname-prefix", m.Prefix, "--control-url", m.A.ControlURL, "--allow-sibling-egress",
		"--mgmt-hostname", m.Prefix + "mgmt", "--host-id", strings.TrimSuffix(m.Prefix, "-")}
	if m.TSAPIURL != "" {
		upArgs = append(upArgs, "--tsapi-url", m.TSAPIURL)
	}
	var up struct {
		TenantID   string `json:"tenantID"`
		HostID     string `json:"hostID"`
		MgmtNodeID string `json:"mgmtNodeID"`
	}
	d := m.MustJSON(&up, upArgs...)
	m.A.TenantID = up.TenantID
	res.Numbers["up_ready_ms"] = msOf(d)
	m.Env.TrackInfra(up.MgmtNodeID)
	// Cleanup is per tailnet (Tenant.Res): mgmt's and the host's own nodes
	// live on tenant A's tailnet.
	m.A.Res.InfraNodeIDs = append(m.A.Res.InfraNodeIDs, m.Res.InfraNodeIDs...)

	// twd tenant add: tenant B on its own tailnet.
	var ta struct {
		TenantID string             `json:"tenantID"`
		PolicyOK bool               `json:"policyOK"`
		Timings  map[string]float64 `json:"timingsMs"`
	}
	d = m.MustJSON(&ta, "tenant", "add", m.B.Tailnet, "--api-key-file", m.B.APIKeyFile, "--control-url", m.B.ControlURL)
	m.B.TenantID = ta.TenantID
	res.Numbers["tenant_add_ms"] = msOf(d)
	if m.B.TenantID == "" || m.B.TenantID == m.A.TenantID || !ta.PolicyOK {
		t.Fatalf("tenant add: %+v (tenant A %s)", ta, m.A.TenantID)
	}
	// The same key file can't serve a second tailnet.
	if _, se, _, err := m.Twd("tenant", "add", "other.example.ts.net", "--api-key-file", m.A.APIKeyFile); err == nil || !strings.Contains(se, "state_conflict") {
		t.Fatalf("tenant add with tenant A's key file for another tailnet: %v %s", err, se)
	}
	var tl []struct {
		Tailnet  string `json:"tailnet"`
		TenantID string `json:"tenantID"`
		Operator bool   `json:"operator"`
	}
	m.MustJSON(&tl, "tenant", "ls")
	if len(tl) != 2 || !tl[0].Operator || tl[0].TenantID != m.A.TenantID || tl[1].TenantID != m.B.TenantID {
		t.Fatalf("tenant ls: %+v", tl)
	}
	res.pass("tenants", fmt.Sprintf("A %s (%s, operator) · B %s (%s) via `twd tenant add` in %v", m.A.Tailnet, m.A.TenantID, m.B.Tailnet, m.B.TenantID, d.Round(time.Millisecond)))

	// The same agent app (same directory, workload, classes and object
	// names) deployed to both tenants.
	agentDir := filepath.Join(m.WorkDir, "agent")
	m.MustJSON(nil, "init", agentDir, "--template", "agent")
	tj := filepath.Join(agentDir, "twd.jsonc")
	b, _ := os.ReadFile(tj)
	b = bytes.Replace(b, []byte(`"tags": ["tag:twd-wl-agent"],`), []byte(`"tags": ["tag:twd-wl-agent"],
    "connects": ["`+TestTag+`"],`), 1)
	os.WriteFile(tj, b, 0o644)
	for _, tn := range m.tenants() {
		var dr struct {
			TenantID string `json:"tenantID"`
			Endpoint struct {
				MagicDNS string `json:"magicDNS"`
			} `json:"endpoint"`
			Preflight struct {
				OK bool `json:"ok"`
			} `json:"preflight"`
			Timings map[string]float64 `json:"timingsMs"`
		}
		d := m.MustJSON(&dr, "deploy", agentDir, "--tenant", tn.Tailnet)
		if dr.TenantID != tn.TenantID || !dr.Preflight.OK || dr.Endpoint.MagicDNS == "" {
			t.Fatalf("deploy to tenant %s: %+v", tn.Key, dr)
		}
		tn.wlMagic = dr.Endpoint.MagicDNS
		res.Numbers["deploy_"+tn.Key+"_ms"] = msOf(d)
		res.Numbers["deploy_"+tn.Key+"_ready_ms"] = dr.Timings["ready"]
	}
	res.pass("deploy_same_app", fmt.Sprintf("agent → A %s · B %s", m.A.wlMagic, m.B.wlMagic))
	// … and the MCP server, for check (d): per-spawn cap limits.
	mcpDir := filepath.Join(m.WorkDir, "mcp")
	m.MustJSON(nil, "init", mcpDir, "--template", "mcp")
	for _, tn := range m.tenants() {
		var dr struct {
			Endpoint struct {
				MagicDNS string `json:"magicDNS"`
			} `json:"endpoint"`
		}
		m.MustJSON(&dr, "deploy", mcpDir, "--tenant", tn.Tailnet)
		tn.mcpMagic = dr.Endpoint.MagicDNS
	}

	// Test nodes, each in its own tenant's tailnet.
	for _, tn := range m.tenants() {
		k := strings.ToLower(tn.Key)
		tn.client = tn.NewNode(k + "-client")
		tn.echo = tn.NewNode(k + "-echo")
		if err := tn.echo.ServeEcho(); err != nil {
			t.Fatal(err)
		}
		tn.clientID, tn.clientName, tn.clientIP = selfOf(tn.client)
		_, tn.echoName, tn.echoIP = selfOf(tn.echo)
	}

	// (a) Each tenant's client reaches its own agent and sees itself as
	// the verified caller.
	for _, tn := range m.tenants() {
		hc := tn.client.HTTPClient(45 * time.Second)
		tn.wlIP = peerIP(t, tn.client, tn.wlMagic, 90*time.Second)
		r := do(hc, "GET", "http://"+tn.wlIP+"/?agent=a1", nil)
		if r.Code != 200 || get(r.Body, "caller", "nodeId") != tn.clientID || get(r.Body, "agent") != "a1" {
			t.Fatalf("(a) tenant %s via workload: %s", tn.Key, r)
		}
		tn.agent = m.waitReady(tn, "SupportAgent", "a1", 90*time.Second)
		tn.agentIP = peerIP(t, tn.client, tn.agent.MagicDNS, 60*time.Second)
		r = do(hc, "GET", "http://"+tn.agentIP+"/", nil)
		if r.Code != 200 || get(r.Body, "caller", "nodeId") != tn.clientID ||
			(m.ClientTagged && !slices.Contains(anyStrings(get(r.Body, "caller", "tags")), TestTag)) {
			t.Fatalf("(a) tenant %s via its agent's own node: %s", tn.Key, r)
		}
		// … and the agent's egress to its own tenant's echo leaves from its node.
		echoShort, _, _ := strings.Cut(tn.echoName, ".")
		r = do(hc, "GET", "http://"+tn.agentIP+"/?upstream="+url.QueryEscape("http://"+echoShort+"/"), nil)
		eg, _ := get(r.Body, "upstream", "body", "caller", "node").(string)
		if r.Code != 200 || !strings.EqualFold(strings.TrimSuffix(eg, "."), tn.agent.MagicDNS) {
			t.Fatalf("(a) tenant %s agent → own echo: want egress %s, got %q: %s", tn.Key, tn.agent.MagicDNS, eg, r)
		}
		res.pass("a_"+tn.Key+"_own_agent", fmt.Sprintf("caller %s (%s) %v; agent %s egress → own echo as %s",
			tn.clientName, tn.clientID, get(r.Body, "caller", "tags"), tn.agent.MagicDNS, eg))
	}

	// (b) Identical app/object names, distinct identities.
	a, bb := m.A.agent, m.B.agent
	for _, tn := range m.tenants() {
		for _, o := range m.objectsOf(tn) {
			if o.Namespace == "_workload" && o.Name == "agent" {
				tn.wlObj = o
			}
		}
	}
	if a.Namespace != bb.Namespace || a.Name != bb.Name {
		t.Fatalf("(b) not the same names: %s/%s vs %s/%s", a.Namespace, a.Name, bb.Namespace, bb.Name)
	}
	sameNode := m.GlobalNodeIDs && (a.NodeID == bb.NodeID || m.A.wlObj.NodeID == m.B.wlObj.NodeID)
	if a.ID == bb.ID || a.Hostname == bb.Hostname || sameNode || a.MagicDNS == bb.MagicDNS ||
		m.A.wlObj.ID == m.B.wlObj.ID || m.A.wlObj.Hostname == m.B.wlObj.Hostname || a.NodeID == "" || bb.NodeID == "" {
		t.Fatalf("(b) identities collide: A %+v / B %+v; workloads %+v / %+v", a, bb, m.A.wlObj, m.B.wlObj)
	}
	// Distinct workerd processes and state dirs, from the host's view.
	var st struct {
		MgmtSocket string `json:"mgmtSocket"`
		Workloads  []struct {
			TenantID, Workload, InstanceDir, Sandbox, SandboxProfile, DOStorage string
			PID                                                                 int
		} `json:"workloads"`
	}
	m.MustJSON(&st, "status")
	for _, w := range st.Workloads {
		for _, tn := range m.tenants() {
			if w.TenantID == tn.TenantID && w.Workload == "agent" {
				tn.hostDOStorage, tn.hostInstDir, tn.hostPID, tn.sandbox, tn.profile = w.DOStorage, w.InstanceDir, w.PID, w.Sandbox, w.SandboxProfile
			}
		}
	}
	hostData := filepath.Join(m.StateDir, "hostd")
	for _, tn := range m.tenants() {
		td := filepath.Join(hostData, "tenants", tn.TenantID)
		if tn.hostPID == 0 || !strings.HasPrefix(tn.hostDOStorage, td+"/") {
			t.Fatalf("(b) tenant %s workerd/state: pid %d DO storage %s (want under %s)", tn.Key, tn.hostPID, tn.hostDOStorage, td)
		}
		if _, err := os.Stat(filepath.Join(td, "nodes", tn.agent.ID)); err != nil {
			t.Fatalf("(b) tenant %s agent node state not in its tenant dir: %v", tn.Key, err)
		}
		if _, err := os.Stat(filepath.Join(hostData, "tenants", m.other(tn).TenantID, "nodes", tn.agent.ID)); err == nil {
			t.Fatalf("(b) tenant %s's node state is in the other tenant's dir", tn.Key)
		}
	}
	if m.A.hostPID == m.B.hostPID || m.A.hostInstDir == m.B.hostInstDir || m.A.hostDOStorage == m.B.hostDOStorage {
		t.Fatalf("(b) workerd shared: A pid %d %s · B pid %d %s", m.A.hostPID, m.A.hostInstDir, m.B.hostPID, m.B.hostInstDir)
	}
	res.pass("b_distinct_identities", fmt.Sprintf("agent:SupportAgent/a1 → A %s node %s (%s) · B %s node %s (%s); workload nodes %s / %s; workerd pids %d / %d",
		a.ID, a.NodeID, a.MagicDNS, bb.ID, bb.NodeID, bb.MagicDNS, m.A.wlObj.MagicDNS, m.B.wlObj.MagicDNS, m.A.hostPID, m.B.hostPID))

	// (c) Isolation: neither tenant's agent can reach the other tenant's
	// echo or agent, by name or by IP; the refusal is typed.
	for _, src := range m.tenants() {
		dst := m.other(src)
		hc := src.client.HTTPClient(45 * time.Second)
		echoShort, _, _ := strings.Cut(dst.echoName, ".")
		agentShort, _, _ := strings.Cut(dst.agent.MagicDNS, ".")
		targets := []struct{ kind, url, ip string }{
			{"echo name", "http://" + echoShort + "/", ""},
			{"echo FQDN", "http://" + dst.echoName + "/", ""},
			{"echo IP", "http://" + dst.echoIP + "/", dst.echoIP},
			{"agent name", "http://" + agentShort + "/", ""},
			{"agent FQDN", "http://" + dst.agent.MagicDNS + "/", ""},
			{"agent IP", "http://" + dst.agentIP + "/", dst.agentIP},
			{"workload FQDN", "http://" + dst.wlMagic + "/", ""},
			{"workload IP", "http://" + dst.wlIP + "/", dst.wlIP},
		}
		var lines []string
		for _, tg := range targets {
			if tg.ip != "" && (src.client.InNetmap(context.Background(), tg.ip) || tg.ip == src.clientIP) {
				// Two tailnets can number nodes alike (always on testcontrol).
				m.Res.Notes = append(m.Res.Notes, fmt.Sprintf("(c) %s→%s %s %s skipped: the IP is also a node of tenant %s", src.Key, dst.Key, tg.kind, tg.ip, src.Key))
				continue
			}
			r := do(hc, "GET", "http://"+src.agentIP+"/?upstream="+url.QueryEscape(tg.url), nil)
			code := fmt.Sprint(get(r.Body, "upstream", "error"))
			if r.Code != 200 || (code != "peer_not_visible" && code != "forbidden_destination") {
				t.Fatalf("(c) tenant %s agent → tenant %s %s (%s): want peer_not_visible/forbidden_destination, got %s", src.Key, dst.Key, tg.kind, tg.url, r)
			}
			lines = append(lines, fmt.Sprintf("%s %s in %.0f ms", tg.kind, code, msOf(r.Dur)))
		}
		if len(lines) < 5 {
			t.Fatalf("(c) %s→%s: only %d targets checked", src.Key, dst.Key, len(lines))
		}
		res.pass("c_isolation_"+src.Key+"_to_"+dst.Key, strings.Join(lines, "; "))
	}

	// (d) Per-spawn cap limits are enforced at the receiver in each tenant
	// (RT-01): a same-tag child spawned with lim=[lookup_order] gets
	// cap_missing for refund from its tenant's MCP server, while the
	// unnarrowed parent's refund passes the cap check.
	for _, tn := range m.tenants() {
		hc := tn.client.HTTPClient(60 * time.Second)
		mcpIP := peerIP(t, tn.client, tn.mcpMagic, 90*time.Second)
		r := do(hc, "GET", "http://"+tn.agentIP+"/delegate?child=narrow1&server="+url.QueryEscape("http://"+mcpIP+"/mcp"), nil)
		if r.Code != 200 || get(r.Body, "lookup_order", "ok") != true {
			t.Fatalf("(d) tenant %s delegate: %s", tn.Key, r)
		}
		if get(r.Body, "refund", "ok") != false || get(r.Body, "refund", "error") != "cap_missing" {
			t.Fatalf("(d) tenant %s: narrowed child's refund must be cap_missing: %s", tn.Key, r)
		}
		if m.PolicyGrantsParentRefund && (get(r.Body, "parent", "refund", "ok") != true || get(r.Body, "parent", "lookup_order", "ok") != true) {
			t.Fatalf("(d) tenant %s: the unnarrowed parent's refund/lookup_order must be admitted, else the child's denial proves nothing about lim: %s", tn.Key, r)
		}
		res.pass("d_lim_enforced_"+tn.Key, fmt.Sprintf("same-tag child with lim=[lookup_order]: refund cap_missing, lookup_order ok; parent refund %v", get(r.Body, "parent", "refund", "ok")))
	}

	// (d) Tenant A's process can't read tenant B's state dir (and vice versa).
	for _, tn := range m.tenants() {
		td := filepath.Join(hostData, "tenants", tn.TenantID)
		for _, p := range []string{filepath.Dir(td), td, filepath.Join(td, "nodes"), filepath.Join(td, "workloads"), filepath.Join(td, "nodes", tn.agent.ID), tn.hostDOStorage} {
			fi, err := os.Stat(p)
			if err != nil || fi.Mode().Perm() != 0o700 {
				t.Fatalf("(d) tenant %s %s: mode %v %v", tn.Key, p, fi.Mode().Perm(), err)
			}
		}
	}
	if sb := m.A.sandbox; sb == m.B.sandbox && (sb == hostd.SandboxSeatbelt || sb == hostd.SandboxLandlock) {
		mgmtSock := st.MgmtSocket
		for _, src := range m.tenants() {
			dst := m.other(src)
			dstState := filepath.Join(hostData, "tenants", dst.TenantID)
			nodeFile := firstFile(filepath.Join(dstState, "nodes", dst.agent.ID))
			// Run a command under src's live profile (Seatbelt:
			// sandbox-exec; Landlock: hostd's procsb launcher).
			probe := func(args ...string) error {
				cmd, err := hostd.SandboxCommand(src.profile, args...)
				if err != nil {
					t.Fatalf("(d) %v", err)
				}
				return cmd.Run()
			}
			denied := []struct{ what, cmd, path string }{
				{"tenant " + dst.Key + " state dir", "/bin/ls", dstState},
				{"tenant " + dst.Key + " node state", "/bin/cat", nodeFile},
				{"tenant " + dst.Key + " DO storage", "/bin/ls", dst.hostDOStorage},
				{"tenant " + dst.Key + " API key file", "/bin/cat", dst.APIKeyFile},
				{"its own tenant's node keys", "/bin/ls", filepath.Join(hostData, "tenants", src.TenantID, "nodes")},
			}
			var got []string
			for _, dn := range denied {
				if dn.path == "" {
					t.Fatalf("(d) no file to probe for %s", dn.what)
				}
				if err := probe(dn.cmd, dn.path); err == nil {
					t.Fatalf("(d) tenant %s's workerd sandbox can read %s (%s)", src.Key, dn.what, dn.path)
				}
				got = append(got, dn.what+" denied")
			}
			// Normal operation is tsnet only: mgmt has no unix socket. The
			// operator console is mgmt's loopback proxy, whose credential
			// (0600, under mgmt's state) must be out of every sandbox's
			// reach; the host's node state too.
			if _, err := os.Stat(mgmtSock); err == nil {
				t.Fatalf("(d) mgmt serves a unix socket in normal operation (%s)", mgmtSock)
			}
			lb := filepath.Join(m.StateDir, "mgmt", "loopback.json")
			if _, err := os.Stat(lb); err != nil {
				t.Fatalf("(d) control: mgmt's loopback descriptor %s is missing: %v", lb, err)
			}
			if err := probe("/bin/cat", lb); err == nil {
				t.Fatalf("(d) tenant %s's workerd sandbox can read mgmt's loopback credential", src.Key)
			}
			if err := probe("/bin/ls", filepath.Join(m.StateDir, "hostd", "tsnet")); err == nil {
				t.Fatalf("(d) tenant %s's workerd sandbox can list the host node's state", src.Key)
			}
			got = append(got, "no mgmt unix socket; loopback credential and host node state denied")
			if err := probe("/bin/ls", src.hostDOStorage); err != nil {
				t.Fatalf("(d) tenant %s's sandbox can't list its own DO storage: %v", src.Key, err)
			}
			name := "Seatbelt"
			if sb == hostd.SandboxLandlock {
				name = "Landlock"
				got = append(got, landlockProbes(t, src.profile, dst.hostPID, dst.hostInstDir, filepath.Join(filepath.Dir(src.hostInstDir), "hostd.sock"))...)
			}
			res.pass("d_state_isolation_"+src.Key, fmt.Sprintf("tenant dirs 0700; workerd pid %d under %s: %s; own DO storage readable", src.hostPID, name, strings.Join(got, ", ")))
		}
	} else if m.RequireSandbox {
		t.Fatalf("(d) workerds not sandboxed: A %q B %q", m.A.sandbox, m.B.sandbox)
	} else {
		res.pass("d_state_isolation", "tenant dirs 0700 (no process sandbox on this host: same-uid processes are not confined)")
	}

	// Record the IDs twd created in each tailnet (leftover checks).
	for _, tn := range m.tenants() {
		for _, o := range m.objectsOf(tn) {
			id := o.StableNodeID
			if id == "" {
				id = o.NodeID
			}
			if id != "" && !slices.Contains(tn.Res.NodeIDs, id) {
				tn.Res.NodeIDs = append(tn.Res.NodeIDs, id)
			}
		}
	}

	// (f) part 1: cross-tenant operations are refused (before the kill).
	if _, se, _, err := m.Twd("kill", "--tenant", m.B.Tailnet, m.A.agent.ID); err == nil || !strings.Contains(se, "unknown_object") {
		t.Fatalf("kill tenant A's object as tenant B: %v %s", err, se)
	}
	if _, se, _, err := m.Twd("audit", "--tenant", m.B.Tailnet, m.A.agent.ID); err == nil || !strings.Contains(se, "unknown_object") {
		t.Fatalf("audit tenant A's object as tenant B: %v %s", err, se)
	}

	// (e) Killing tenant A's objects doesn't affect tenant B.
	var kr struct {
		Result struct {
			Killed []string `json:"killed"`
			Pushed []string `json:"pushed"`
		} `json:"result"`
	}
	d = m.MustJSON(&kr, "kill", "--tenant", m.A.Tailnet, m.A.agent.MagicDNS, "--cascade", "--reason", "e2e-multi: kill tenant A")
	if len(kr.Result.Killed) < 1 || slices.Contains(kr.Result.Killed, m.B.agent.ID) {
		t.Fatalf("(e) kill: %+v", kr)
	}
	ra := do(m.A.client.HTTPClient(10*time.Second), "GET", "http://"+m.A.agentIP+"/", nil)
	if ra.Err != nil || ra.Code != 410 || get(ra.Body, "code") != "object_killed" {
		t.Fatalf("(e) tenant A's killed agent: %s", ra)
	}
	hcB := m.B.client.HTTPClient(30 * time.Second)
	for i := range 3 {
		r := do(hcB, "GET", "http://"+m.B.agentIP+"/", nil)
		if r.Code != 200 || get(r.Body, "caller", "nodeId") != m.B.clientID {
			t.Fatalf("(e) tenant B's agent after killing tenant A's (try %d): %s", i, r)
		}
		r = do(hcB, "GET", "http://"+m.B.wlIP+"/?agent=a1", nil)
		if r.Code != 200 || get(r.Body, "agent") != "a1" {
			t.Fatalf("(e) tenant B via its workload after killing tenant A's agent: %s", r)
		}
	}
	for _, o := range m.objectsOf(m.B) {
		if o.ID == m.B.agent.ID && o.Status != "ready" {
			t.Fatalf("(e) tenant B's agent is %s after the tenant A kill", o.Status)
		}
	}
	res.Numbers["kill_A_cli_ms"] = msOf(d)
	res.pass("e_kill_A_spares_B", fmt.Sprintf("killed %v in %v; A's agent → 410 object_killed in %v; B's agent and workload still 200 (3×), B object ready",
		kr.Result.Killed, d.Round(time.Millisecond), ra.Dur.Round(time.Microsecond)))

	// (f) twd audit per tenant shows only that tenant's records.
	ids := map[string]map[string]bool{}
	for _, tn := range m.tenants() {
		ids[tn.Key] = map[string]bool{}
		for _, o := range m.objectsOf(tn) {
			ids[tn.Key][o.ID] = true
		}
	}
	for _, tn := range m.tenants() {
		var au struct {
			TenantID string `json:"tenantID"`
			Records  []struct {
				Tenant string          `json:"tenantID"`
				Kind   string          `json:"kind"`
				Object string          `json:"object"`
				Detail json.RawMessage `json:"detail"`
			} `json:"records"`
			Verified bool `json:"verified"`
		}
		m.MustJSON(&au, "audit", "--tenant", tn.Tailnet, "--all")
		kinds := map[string]int{}
		for _, r := range au.Records {
			if r.Tenant != tn.TenantID || ids[m.other(tn).Key][r.Object] {
				t.Fatalf("(f) tenant %s audit has a record of another tenant: %+v", tn.Key, r)
			}
			if r.Object != "" && !ids[tn.Key][r.Object] {
				t.Fatalf("(f) tenant %s audit has a record for unknown object %s", tn.Key, r.Object)
			}
			kinds[r.Kind]++
			var dd struct {
				KeyID string `json:"keyID"`
			}
			if r.Kind == "mint" && json.Unmarshal(r.Detail, &dd) == nil && dd.KeyID != "" && !slices.Contains(tn.Res.KeyIDs, dd.KeyID) {
				tn.Res.KeyIDs = append(tn.Res.KeyIDs, dd.KeyID)
			}
		}
		if !au.Verified || au.TenantID != tn.TenantID || kinds["create"] < 2 || kinds["mint"] < 2 || kinds["ready"] < 2 {
			t.Fatalf("(f) tenant %s audit: verified %v, kinds %v", tn.Key, au.Verified, kinds)
		}
		if tn == m.A && kinds["kill"] < 1 {
			t.Fatalf("(f) tenant A audit lacks the kill: %v", kinds)
		}
		if tn == m.B && kinds["kill"] != 0 {
			t.Fatalf("(f) tenant B audit has kill records: %v", kinds)
		}
		res.pass("f_audit_"+tn.Key, fmt.Sprintf("%d records, all tenant %s, chain verified, kinds %v", len(au.Records), tn.TenantID, kinds))
	}
	if so, _, _, err := m.Twd("audit", "--tenant", m.B.Tailnet, "--all"); err == nil {
		m.T.Logf("twd audit --tenant %s --all:\n%s", m.B.Tailnet, so)
	}
}

func firstFile(dir string) string {
	var out string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && out == "" && d.Type().IsRegular() {
			out = p
		}
		return nil
	})
	return out
}

// Down runs `twd down` (purges both tenants' objects and devices).
func (m *MultiEnv) Down() { m.Env.Down() }

// landlockProbes are the Linux-only (d) checks under a workload's Landlock
// profile: the other tenant's workerd can't be ptraced or read, its egress
// socket and hostd's push socket refuse the peer, and raw TCP fails.
func landlockProbes(t testing.TB, profile string, dstPID int, dstInstDir, hostdSock string) []string {
	t.Helper()
	checks := []struct{ what, op, arg string }{
		{"ptrace of the other tenant's workerd", procsb.ProbePtrace, strconv.Itoa(dstPID)},
		{"its memory", procsb.ProbeVMRead, strconv.Itoa(dstPID)},
		{"its /proc environ", procsb.ProbeRead, "/proc/" + strconv.Itoa(dstPID) + "/environ"},
		{"its egress socket", procsb.ProbeConnectUnix, filepath.Join(dstInstDir, "egress-wl.sock")},
		{"hostd's push socket", procsb.ProbeConnectUnix, hostdSock},
		{"TCP to 1.1.1.1:443", procsb.ProbeConnectTCP, "1.1.1.1:443"},
	}
	var got []string
	for _, c := range checks {
		if c.arg == "" || dstInstDir == "" && c.op == procsb.ProbeConnectUnix && strings.HasSuffix(c.arg, "egress-wl.sock") {
			continue
		}
		cmd, err := procsb.ProbeCommand(profile, c.op, c.arg)
		if err != nil {
			t.Fatalf("(d) %v", err)
		}
		out, _ := cmd.Output()
		if r := strings.TrimSpace(string(out)); r == "ok" {
			t.Fatalf("(d) Landlock profile %s allows %s (%s %s)", profile, c.what, c.op, c.arg)
		} else {
			got = append(got, c.what+" denied ("+r+")")
		}
	}
	return got
}
