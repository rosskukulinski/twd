// Package cliflow is the M4 end-to-end scenario, driven only through the
// `twd` CLI binary (up → init → deploy → calls from a tsnet test client →
// spawn → kill --cascade → audit → down), shared by the hermetic test
// (cmd/twd, testcontrol + a fake Tailscale API) and E2E-MAC on tailnet 1
// (e2e/mac, `make e2e-mac`).
package cliflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/internal/policy"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
)

// MCPCap is the MCP app capability the templates use.
const MCPCap = "example.com/cap/mcp"

// TestTag tags the test client and the echo service.
const TestTag = "tag:ttest"

// Policy is what the scenario needs in the tenant policy (added to
// tailnet 1 by `make e2e-mac`: validated, If-Match, existing rules kept).
func Policy() policy.Snippet {
	owners := []string{"autogroup:admin", "tag:twd"}
	app := func(tools ...string) map[string]any {
		return map[string]any{MCPCap: []any{map[string]any{"tools": tools}}}
	}
	return policy.Snippet{
		TagOwners: map[string][]string{
			"tag:twd-wl-agent":          owners,
			"tag:twd-wl-mcp":            owners,
			"tag:twd-agent-support":     owners,
			"tag:twd-agent-support-sub": {"autogroup:admin", "tag:twd", "tag:twd-agent-support"},
		},
		Grants: []map[string]any{
			// the test client reaches every twd node of the scenario
			{"src": []string{TestTag}, "dst": []string{"tag:twd-wl-agent", "tag:twd-agent-support", "tag:twd-agent-support-sub", "tag:twd-wl-mcp"}, "ip": []string{"tcp:80"}},
			// … and may call lookup_order (only) on the MCP server
			{"src": []string{TestTag}, "dst": []string{"tag:twd-wl-mcp"}, "ip": []string{"tcp:80"}, "app": app("lookup_order")},
			// agents (and sub-agents) reach the private echo service
			{"src": []string{"tag:twd-wl-agent", "tag:twd-agent-support", "tag:twd-agent-support-sub"}, "dst": []string{TestTag}, "ip": []string{"tcp:80"}},
			// the parent agent's MCP caps (spawn narrows from these)
			{"src": []string{"tag:twd-agent-support"}, "dst": []string{"tag:twd-wl-mcp"}, "ip": []string{"tcp:80"}, "app": app("lookup_order", "refund")},
			{"src": []string{"tag:twd-agent-support-sub"}, "dst": []string{"tag:twd-wl-mcp"}, "ip": []string{"tcp:80"}, "app": app("lookup_order")},
		},
	}
}

// BuildBinaries builds twd, twd-mgmt and twd-hostd into a temp dir.
func BuildBinaries(t testing.TB) string {
	t.Helper()
	root, err := nettest.FindRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cmd := exec.Command("go", "build", "-o", dir+"/", "./cmd/twd", "./cmd/twd-mgmt", "./cmd/twd-hostd")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	// The binaries live outside the repo: point twd at create-twd.
	if os.Getenv("TWD_CREATE_TWD") == "" {
		os.Setenv("TWD_CREATE_TWD", filepath.Join(root, "js", "packages", "create-twd"))
	}
	return dir
}

// Env configures one run.
type Env struct {
	T          testing.TB
	BinDir     string
	StateDir   string
	WorkDir    string
	Tailnet    string
	APIKeyFile string
	ControlURL string
	TSAPIURL   string // "" = production API
	Prefix     string // hostname prefix (twd-e2e-<run>-)
	// NewNode starts a tagged (tag:ttest) tsnet test node named Prefix+role.
	NewNode func(role string) *nettest.Node
	// ClientTagged: test nodes carry tag:ttest (production control). On
	// testcontrol they are untagged users, so the principal is a login.
	ClientTagged bool
	// PolicyGrantsParentRefund: the tenant policy grants the parent agent's
	// tag `refund` (the real test tailnets do; testcontrol's global caps
	// can't grant per tag). Then check (d) also asserts the unnarrowed
	// parent's refund is admitted, proving the child's denial is lim's.
	PolicyGrantsParentRefund bool
	Res                      *Results
	// Exec, if set, runs the CLI elsewhere (e2e-mbp: over ssh on another
	// Mac) with args after the binary (--state-dir first). dur is the CLI's
	// own wall time on that machine.
	Exec func(args []string) (stdout, stderr string, dur time.Duration, err error)
	// PatchFile, if set, edits a file in WorkDir (which is then remote).
	PatchFile func(path string, edit func([]byte) []byte) error
	// Tier, if set, is written into the agent and mcp apps' twd.jsonc
	// ("tier": "microvm" for e2e-linux-vm).
	Tier string
	// OnDeployed, if set, runs right after the agent and mcp deploys.
	OnDeployed func()
	// OnPeers, if set, runs once the client sees the agent and mcp
	// workload nodes (e2e-mbp measures the cross-machine path there).
	OnPeers func(client *nettest.Node, hc *http.Client, agentIP, mcpIP string)
	// UpArgs are extra `twd up` flags (e2e-linux: --service).
	UpArgs []string
	// Persistent: join nodes non-ephemeral (twd up's default; e2e-linux,
	// so they rejoin after a service restart). The caller must then stop
	// with `twd down --purge` (Down doesn't purge).
	Persistent bool
	// Slow scales every wait and latency bound (0 or 1 = as is). e2e-linux
	// sets it for the emulated (qemu TCG) amd64 VM only; the measured
	// numbers are recorded unscaled.
	Slow float64
}

// tmo scales a wait or a latency bound by e.Slow.
func (e *Env) tmo(d time.Duration) time.Duration {
	if e.Slow > 1 {
		return time.Duration(float64(d) * e.Slow)
	}
	return d
}

// Results are the measured outcome (written as JSON by e2e-mac).
type Results struct {
	Run     string             `json:"run"`
	Tailnet string             `json:"tailnet"`
	Date    string             `json:"date"`
	Checks  map[string]string  `json:"checks"`
	Numbers map[string]float64 `json:"numbers"`
	Notes   []string           `json:"notes,omitempty"`
	Cleanup map[string]any     `json:"cleanup"`
	// IDs this run created via twd (for leftover checks; never printed keys).
	NodeIDs []string `json:"nodeIDs"`
	KeyIDs  []string `json:"keyIDs"`
}

// NewResults returns empty results.
func NewResults(run, tailnet string) *Results {
	return &Results{Run: run, Tailnet: tailnet, Date: time.Now().Format(time.RFC3339), Checks: map[string]string{},
		Numbers: map[string]float64{}, Cleanup: map[string]any{}}
}

func (r *Results) pass(name, detail string) { r.Checks[name] = "PASS " + detail }

// Pass records a passed check (for callers outside cliflow).
func (r *Results) Pass(name, detail string) { r.pass(name, detail) }

// Twd runs the CLI.
func (e *Env) Twd(args ...string) (stdout, stderr string, dur time.Duration, err error) {
	full := append([]string{"--state-dir", e.StateDir}, args...)
	if e.Exec != nil {
		return e.Exec(full)
	}
	cmd := exec.Command(filepath.Join(e.BinDir, "twd"), full...)
	cmd.Dir = e.WorkDir
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	cmd.Env = os.Environ()
	st := time.Now()
	err = cmd.Run()
	dur = time.Since(st)
	return so.String(), se.String(), dur, err
}

// MustJSON runs the CLI with --json and decodes stdout.
func (e *Env) MustJSON(out any, args ...string) time.Duration {
	e.T.Helper()
	so, se, d, err := e.Twd(append(args, "--json")...)
	if err != nil {
		e.T.Fatalf("twd %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, so, netd.Redact(se))
	}
	if out != nil {
		if err := json.Unmarshal([]byte(so), out); err != nil {
			e.T.Fatalf("twd %s: bad JSON: %v\n%s", strings.Join(args, " "), err, so)
		}
	}
	return d
}

func msOf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func get(m any, path ...string) any {
	for _, p := range path {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[p]
	}
	return m
}

type object struct {
	ID           string          `json:"objectID"`
	Namespace    string          `json:"namespace"`
	Name         string          `json:"objectName"`
	Hostname     string          `json:"hostname"`
	Tags         []string        `json:"tags"`
	Caps         json.RawMessage `json:"caps"`
	Parent       string          `json:"parent"`
	Status       string          `json:"status"`
	NodeID       string          `json:"nodeID"`
	StableNodeID string          `json:"stableNodeID"`
	MagicDNS     string          `json:"magicDNS"`
}

func (e *Env) objects() []object {
	var ls struct {
		Objects []object `json:"objects"`
	}
	e.MustJSON(&ls, "ls", "--all")
	return ls.Objects
}

func (e *Env) findObject(ns, name string) (object, bool) {
	for _, o := range e.objects() {
		if o.Namespace == ns && o.Name == name {
			return o, true
		}
	}
	return object{}, false
}

func (e *Env) waitObjectReady(ns, name string, timeout time.Duration) object {
	e.T.Helper()
	dl := time.Now().Add(timeout)
	for {
		if o, ok := e.findObject(ns, name); ok && o.Status == "ready" && o.MagicDNS != "" {
			return o
		}
		if time.Now().After(dl) {
			o, _ := e.findObject(ns, name)
			e.T.Fatalf("object %s/%s not ready after %v: %+v", ns, name, timeout, o)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// peerIP waits until magicDNS is in n's netmap and returns its IPv4.
func peerIP(t testing.TB, n *nettest.Node, magicDNS string, timeout time.Duration) string {
	t.Helper()
	want := strings.TrimSuffix(magicDNS, ".") + "."
	dl := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st, err := n.LC.Status(ctx)
		cancel()
		if err == nil {
			for _, p := range st.Peer {
				if strings.EqualFold(p.DNSName, want) && len(p.TailscaleIPs) > 0 {
					return p.TailscaleIPs[0].String()
				}
			}
		}
		if time.Now().After(dl) {
			t.Fatalf("%s not in %s's netmap after %v", magicDNS, n.Hostname, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type httpRes struct {
	Code int
	Hdr  http.Header
	Body map[string]any
	Raw  string
	Dur  time.Duration
	Err  error
}

func do(hc *http.Client, method, u string, body []byte, hdr ...string) httpRes {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, u, rd)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	st := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return httpRes{Err: err, Dur: time.Since(st)}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	r := httpRes{Code: resp.StatusCode, Hdr: resp.Header, Raw: string(b), Dur: time.Since(st)}
	json.Unmarshal(b, &r.Body)
	return r
}

func (r httpRes) String() string {
	if r.Err != nil {
		return "error " + r.Err.Error()
	}
	s := r.Raw
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	return fmt.Sprintf("%d %s", r.Code, s)
}

func mcpCall(hc *http.Client, ip, tool string, args map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	r := do(hc, "POST", "http://"+ip+"/mcp", body, "content-type", "application/json",
		"accept", "application/json, text/event-stream", "mcp-protocol-version", "2025-06-18")
	if r.Err != nil || r.Code != 200 {
		return nil, fmt.Errorf("%s: %s", tool, r)
	}
	b := []byte(r.Raw)
	for _, line := range strings.Split(r.Raw, "\n") {
		if strings.HasPrefix(line, "data: ") {
			b = []byte(strings.TrimPrefix(line, "data: "))
		}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %s", tool, b)
	}
	return m, nil
}

// upArgs is the `twd up` command line of this run.
func (e *Env) upArgs() []string {
	// --allow-sibling-egress: the agent and the MCP server share this host;
	// check (d) needs the spawned child to reach the server over the tailnet.
	upArgs := []string{"up", "--tailnet", e.Tailnet, "--api-key-file", e.APIKeyFile, "--yes",
		"--hostname-prefix", e.Prefix, "--control-url", e.ControlURL, "--allow-sibling-egress"}
	if !e.Persistent {
		upArgs = append(upArgs, "--ephemeral")
	}
	if e.TSAPIURL != "" {
		upArgs = append(upArgs, "--tsapi-url", e.TSAPIURL)
	}
	return append(upArgs, e.UpArgs...)
}

// RedeployAfterPurge is the user flow `twd down --purge && twd up && twd
// deploy` on the SAME state dir, after Run and a purging Down: the agent
// workload must deploy and its node be ready again (it was refused with
// `bootstrap_failed: the workload node for agent is killed` before the
// killed objects were forgotten by purge). It ends with another purging
// `twd down`; the new node IDs are tracked for the leftover check.
func (e *Env) RedeployAfterPurge() {
	t, res := e.T, e.Res
	t.Helper()
	var up struct {
		TenantID string `json:"tenantID"`
	}
	e.MustJSON(&up, e.upArgs()...)
	var dep struct {
		Status   string `json:"status"`
		Endpoint struct {
			Node     string `json:"node"`
			MagicDNS string `json:"magicDNS"`
			Status   string `json:"status"`
		} `json:"endpoint"`
		Timings map[string]float64 `json:"timingsMs"`
	}
	d := e.MustJSON(&dep, "deploy", filepath.Join(e.WorkDir, "agent"))
	if dep.Status != "active" || dep.Endpoint.Status != "ready" || dep.Endpoint.MagicDNS == "" {
		t.Fatalf("redeploy after purge: %+v", dep)
	}
	e.TrackObjectNodes()
	res.Numbers["redeploy_after_purge_ready_ms"] = dep.Timings["ready"]
	res.pass("redeploy_after_purge", fmt.Sprintf("agent %s ready again on the same state dir in %v", dep.Endpoint.MagicDNS, d.Round(time.Millisecond)))
	var dr struct {
		Killed []string `json:"killed"`
		Purged int      `json:"objectsPurged"`
	}
	e.MustJSON(&dr, "down", "--purge")
	if len(dr.Killed) < 1 || dr.Purged < 1 {
		t.Fatalf("down --purge after redeploy: %+v", dr)
	}
	res.pass("redeploy_after_purge_down", fmt.Sprintf("%d objects killed, %d forgotten", len(dr.Killed), dr.Purged))
}

// Object is a row of `twd ls --all` (exported for harnesses that drive extra
// phases, e.g. e2e-linux's ephemeral restart).
type Object = object

// WaitObjectReady waits until ns/name is ready with a MagicDNS name.
func (e *Env) WaitObjectReady(ns, name string, timeout time.Duration) Object {
	e.T.Helper()
	return e.waitObjectReady(ns, name, timeout)
}

// FindObject returns ns/name from `twd ls --all`.
func (e *Env) FindObject(ns, name string) (Object, bool) { return e.findObject(ns, name) }

// PeerIPs returns every IPv4 the client's netmap has for magicDNS (a device
// that was re-minted may briefly coexist with its predecessor's entry).
func PeerIPs(n *nettest.Node, magicDNS string) []string {
	want := strings.TrimSuffix(magicDNS, ".") + "."
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := n.LC.Status(ctx)
	if err != nil {
		return nil
	}
	var ips []string
	for _, p := range st.Peer {
		if strings.EqualFold(p.DNSName, want) && len(p.TailscaleIPs) > 0 {
			ips = append(ips, p.TailscaleIPs[0].String())
		}
	}
	return ips
}

// PeerIP waits until magicDNS is in n's netmap and returns its IPv4.
func PeerIP(t testing.TB, n *nettest.Node, magicDNS string, timeout time.Duration) string {
	t.Helper()
	return peerIP(t, n, magicDNS, timeout)
}

// TrackObjectNodes records every object node ID twd reports (live or
// killed) in Res.NodeIDs for the leftover check.
func (e *Env) TrackObjectNodes() {
	for _, o := range e.objects() {
		for _, id := range []string{o.StableNodeID, o.NodeID} {
			if id != "" && !slices.Contains(e.Res.NodeIDs, id) {
				e.Res.NodeIDs = append(e.Res.NodeIDs, id)
			}
		}
	}
}

// Run executes the scenario. Cleanup of test-created nodes is the caller's;
// `twd down` (in Down) removes twd's own devices.
func (e *Env) Run() {
	t, res := e.T, e.Res
	upArgs := e.upArgs()
	// Interactive mode must refuse (it would use tailscaled).
	if _, se, _, err := e.Twd("up", "--yes"); err == nil || !strings.Contains(se, "tailnet_required") {
		t.Fatalf("twd up without --tailnet should fail with tailnet_required: %v %s", err, se)
	}
	res.pass("up_interactive_refused", "tailnet_required")

	var up struct {
		TenantID string             `json:"tenantID"`
		HostID   string             `json:"hostID"`
		PolicyOK bool               `json:"policyOK"`
		Timings  map[string]float64 `json:"timingsMs"`
	}
	d := e.MustJSON(&up, upArgs...)
	res.Numbers["up_ready_ms"] = msOf(d)
	res.Numbers["up_mgmt_ready_ms"] = up.Timings["mgmt_ready"]
	res.Numbers["up_host_ready_ms"] = up.Timings["host_ready"]
	res.pass("up", fmt.Sprintf("tenant %s host %s in %v", up.TenantID, up.HostID, d.Round(time.Millisecond)))
	var st struct {
		Healthy bool `json:"healthy"`
	}
	e.MustJSON(&st, "status")
	if !st.Healthy {
		t.Fatal("twd status: not healthy")
	}
	res.pass("status", "healthy")

	// Apps from the templates.
	agentDir, mcpDir, helloDir := filepath.Join(e.WorkDir, "agent"), filepath.Join(e.WorkDir, "mcp"), filepath.Join(e.WorkDir, "hello")
	for dir, tmpl := range map[string]string{agentDir: "agent", mcpDir: "mcp", helloDir: "hello"} {
		e.MustJSON(nil, "init", dir, "--template", tmpl)
	}
	// Declare the agent's dependency on the echo service (preflighted).
	tj := filepath.Join(agentDir, "twd.jsonc")
	addConnects := func(b []byte) []byte {
		return bytes.Replace(b, []byte(`"tags": ["tag:twd-wl-agent"],`), []byte(`"tags": ["tag:twd-wl-agent"],
    "connects": ["`+TestTag+`"],`), 1)
	}
	patch := func(path string, edit func([]byte) []byte) {
		if e.PatchFile != nil {
			if err := e.PatchFile(path, edit); err != nil {
				t.Fatalf("patch %s: %v", path, err)
			}
		} else {
			b, _ := os.ReadFile(path)
			os.WriteFile(path, edit(b), 0o644)
		}
	}
	patch(tj, addConnects)
	if e.Tier != "" {
		for _, d := range []string{agentDir, mcpDir} {
			patch(filepath.Join(d, "twd.jsonc"), func(b []byte) []byte {
				return bytes.Replace(b, []byte("{"), []byte("{\n  \"tier\": \""+e.Tier+"\","), 1)
			})
		}
	}

	// Preflight refuses hello (tailnet.connects tag:db has no grant).
	_, se, _, err := e.Twd("deploy", helloDir)
	if err == nil || !strings.Contains(se, "acl_denied") || !strings.Contains(se, `"dst": ["tag:db"]`) {
		t.Fatalf("deploy hello should fail preflight with a snippet: %v\n%s", err, se)
	}
	res.pass("preflight_missing_grant", "acl_denied with HuJSON snippet")

	type depRes struct {
		DeploymentID string `json:"deploymentID"`
		Endpoint     struct {
			Node     string `json:"node"`
			MagicDNS string `json:"magicDNS"`
		} `json:"endpoint"`
		Preflight struct {
			OK bool `json:"ok"`
		} `json:"preflight"`
		Timings map[string]float64 `json:"timingsMs"`
	}
	var agent, mcp depRes
	d = e.MustJSON(&agent, "deploy", agentDir)
	if !agent.Preflight.OK {
		t.Fatal("agent preflight not ok")
	}
	res.Numbers["deploy_agent_cli_ms"] = msOf(d)
	res.Numbers["deploy_agent_ready_ms"] = agent.Timings["ready"]
	res.Numbers["deploy_agent_active_ms"] = agent.Timings["active"]
	d = e.MustJSON(&mcp, "deploy", mcpDir)
	res.Numbers["deploy_mcp_cli_ms"] = msOf(d)
	res.Numbers["deploy_mcp_ready_ms"] = mcp.Timings["ready"]
	res.pass("deploy", fmt.Sprintf("agent %s, mcp %s", agent.Endpoint.MagicDNS, mcp.Endpoint.MagicDNS))
	if e.OnDeployed != nil {
		e.OnDeployed()
	}

	client := e.NewNode("client")
	echo := e.NewNode("echo")
	if err := echo.ServeEcho(); err != nil {
		t.Fatal(err)
	}
	cst, _ := client.LC.StatusWithoutPeers(context.Background())
	clientID, clientName := string(cst.Self.ID), strings.TrimSuffix(cst.Self.DNSName, ".")
	est, _ := echo.LC.StatusWithoutPeers(context.Background())
	echoName := strings.TrimSuffix(est.Self.DNSName, ".")
	hc := client.HTTPClient(e.tmo(45 * time.Second))

	agentIP := peerIP(t, client, agent.Endpoint.MagicDNS, e.tmo(60*time.Second))
	mcpIP := peerIP(t, client, mcp.Endpoint.MagicDNS, e.tmo(60*time.Second))
	if e.OnPeers != nil {
		e.OnPeers(client, hc, agentIP, mcpIP)
	}

	// (a) verified caller, via the workload node to a fresh agent object.
	first := time.Now()
	r := do(hc, "GET", "http://"+agentIP+"/?agent=a1", nil)
	if r.Code != 200 || get(r.Body, "caller", "nodeId") != clientID || get(r.Body, "agent") != "a1" {
		t.Fatalf("(a) agent via workload: %s", r)
	}
	res.Numbers["agent_first_request_ms"] = msOf(r.Dur)
	r0 := r
	ao := e.waitObjectReady("SupportAgent", "a1", e.tmo(90*time.Second))
	res.Numbers["first_object_node_ready_ms"] = msOf(time.Since(first))
	aoIP := peerIP(t, client, ao.MagicDNS, e.tmo(60*time.Second))
	r = do(hc, "GET", "http://"+aoIP+"/", nil)
	if r.Code != 200 || get(r.Body, "caller", "nodeId") != clientID || (e.ClientTagged && !slices.Contains(anyStrings(get(r.Body, "caller", "tags")), TestTag)) {
		t.Fatalf("(a) agent via its own node: %s", r)
	}
	res.pass("a_verified_caller", fmt.Sprintf("nodeId %s tags %v (workload and the object's own node %s)", clientID, get(r.Body, "caller", "tags"), ao.MagicDNS))

	// (b) the agent's fetch to the private echo leaves from its own node.
	echoShort, _, _ := strings.Cut(echoName, ".")
	r = do(hc, "GET", "http://"+aoIP+"/?upstream=http://"+echoShort+"/", nil)
	egressNode, _ := get(r.Body, "upstream", "body", "caller", "node").(string)
	if r.Code != 200 || !strings.EqualFold(strings.TrimSuffix(egressNode, "."), ao.MagicDNS) {
		t.Fatalf("(b) agent egress: want %s, got %q: %s", ao.MagicDNS, egressNode, r)
	}
	res.Numbers["agent_egress_ms"] = msOf(r.Dur)
	res.pass("b_egress_own_node", "echo WhoIs = "+egressNode)

	// (c) MCP: allowed tool, and cap_missing for one the caller lacks.
	ok, err := mcpCall(hc, mcpIP, "lookup_order", map[string]any{"order": "o1"})
	if err != nil || get(ok, "result", "isError") == true {
		t.Fatalf("(c) lookup_order: %v %v", err, ok)
	}
	deny, err := mcpCall(hc, mcpIP, "refund", map[string]any{"order": "o1", "usd": 10})
	if err != nil || get(deny, "result", "isError") != true {
		t.Fatalf("(c) refund should be denied: %v %v", err, deny)
	}
	var te map[string]any
	if c, _ := get(deny, "result", "content").([]any); len(c) > 0 {
		json.Unmarshal([]byte(fmt.Sprint(get(c[0], "text"))), &te)
	}
	if te["code"] != "cap_missing" || get(te, "missing", "tool") != "refund" {
		t.Fatalf("(c) refund error: %v", deny)
	}
	res.pass("c_mcp_caps", "lookup_order allowed; refund → cap_missing")

	// (d) spawn: a sub-agent with its own node and narrower caps.
	r = do(hc, "GET", "http://"+aoIP+"/spawn?child=c1", nil)
	if r.Code != 200 || get(r.Body, "child", "reply", "lookup") != "c1" || get(r.Body, "child", "reply", "parent") != "a1" {
		t.Fatalf("(d) spawn: %s", r)
	}
	res.Numbers["spawn_ready_ms"] = msOf(r.Dur)
	co := e.waitObjectReady("LookupAgent", "c1", e.tmo(60*time.Second))
	var caps map[string]any
	json.Unmarshal(co.Caps, &caps)
	if co.Parent != ao.ID || co.NodeID == "" || co.NodeID == ao.NodeID || co.MagicDNS == ao.MagicDNS ||
		!slices.Contains(co.Tags, "tag:twd-agent-support-sub") || fmt.Sprint(get(caps, MCPCap, "tools")) != "[lookup_order]" {
		t.Fatalf("(d) child object: %+v (parent %s)", co, ao.ID)
	}
	coIP := peerIP(t, client, co.MagicDNS, e.tmo(60*time.Second))
	if r := do(hc, "GET", "http://"+coIP+"/", nil); r.Code != 200 || get(r.Body, "lookup") != "c1" {
		t.Fatalf("(d) child via its own node: %s", r)
	}
	// (d) continued: the per-spawn cap limit is ENFORCED at the receiver
	// (RT-01). A sub-agent of the parent's own class carries the parent's
	// tag, which the tenant policy grants both lookup_order and refund; it
	// is spawned with lim = [lookup_order]. The MCP server must refuse its
	// refund with cap_missing (the WhoIs capMap ∩ the child's node
	// certificate lim) while the unnarrowed parent's refund is admitted by
	// the cap check, and the child's lookup_order still works.
	r = do(hc, "GET", "http://"+aoIP+"/delegate?child=narrow1&server="+url.QueryEscape("http://"+mcpIP+"/mcp"), nil)
	if r.Code != 200 {
		t.Fatalf("(d) delegate: %s", r)
	}
	res.Numbers["delegate_ms"] = msOf(r.Dur)
	if get(r.Body, "lookup_order", "ok") != true {
		t.Fatalf("(d) narrowed child should still be allowed lookup_order: %s", r)
	}
	if get(r.Body, "refund", "ok") != false || get(r.Body, "refund", "error") != "cap_missing" {
		t.Fatalf("(d) narrowed child's refund must be cap_missing (lim not enforced?): %s", r)
	}
	if e.PolicyGrantsParentRefund {
		// The parent (same tag, no lim) must be ADMITTED: the child's denial
		// then comes from its certificate's lim, not from the tenant policy.
		if get(r.Body, "parent", "refund", "ok") != true || get(r.Body, "parent", "lookup_order", "ok") != true {
			t.Fatalf("(d) the unnarrowed parent's refund/lookup_order must be admitted, else the child's denial proves nothing about lim: %s", r)
		}
	} else {
		res.Notes = append(res.Notes, "(d) hermetic policy grants nobody refund: the parent's refund is cap_missing as well; the lim-vs-policy contrast is asserted on the real tailnets")
	}
	no := e.waitObjectReady("SupportAgent", "narrow1", 60*time.Second)
	var ncaps map[string]any
	json.Unmarshal(no.Caps, &ncaps)
	if no.Parent != ao.ID || !slices.Contains(no.Tags, "tag:twd-agent-support") || fmt.Sprint(get(ncaps, MCPCap, "tools")) != "[lookup_order]" {
		t.Fatalf("(d) narrowed child object: %+v", no)
	}
	res.pass("d_spawn", fmt.Sprintf("child %s node %s tags %v caps %s; lim enforced: same-tag child %s (tag %v, lim %s) → refund cap_missing, lookup_order ok; parent refund ok=%v",
		co.ID, co.MagicDNS, co.Tags, co.Caps, no.ID, no.Tags, no.Caps, get(r.Body, "parent", "refund", "ok")))

	// Record the IDs twd created (leftover checks after down).
	for _, o := range e.objects() {
		id := o.StableNodeID
		if id == "" {
			id = o.NodeID
		}
		if id != "" && !slices.Contains(res.NodeIDs, id) {
			res.NodeIDs = append(res.NodeIDs, id)
		}
	}

	// (e) kill --cascade: the CLI returns in < 1 s; later calls fail fast.
	var kr struct {
		Result struct {
			Killed          []string `json:"killed"`
			Pushed          []string `json:"pushed"`
			PushMS          float64  `json:"pushMs"`
			DevicesDeleting []string `json:"devicesDeleting"`
		} `json:"result"`
		Chain   string             `json:"chain"`
		Timings map[string]float64 `json:"timingsMs"`
	}
	d = e.MustJSON(&kr, "kill", ao.MagicDNS, "--cascade", "--reason", "e2e: prompt injection")
	res.Numbers["kill_cli_ms"] = msOf(d)
	res.Numbers["kill_api_ms"] = kr.Timings["kill_api"]
	res.Numbers["kill_push_ms"] = kr.Result.PushMS
	// The cascade covers the agent, its LookupAgent child and the narrowed
	// SupportAgent child from check (d).
	noIP := peerIP(t, client, no.MagicDNS, e.tmo(60*time.Second))
	if d > e.tmo(time.Second) || len(kr.Result.Killed) != 3 || len(kr.Result.Pushed) != 3 {
		t.Fatalf("(e) kill: %v %+v", d, kr)
	}
	var worst time.Duration
	for _, tgt := range []struct{ name, ip string }{{"agent", aoIP}, {"sub-agent", coIP}, {"narrowed sub-agent", noIP}} {
		r := do(client.HTTPClient(e.tmo(10*time.Second)), "GET", "http://"+tgt.ip+"/", nil)
		if r.Err != nil || r.Code != 410 || get(r.Body, "code") != "object_killed" || r.Dur > e.tmo(time.Second) {
			t.Fatalf("(e) new request to killed %s: %s after %v", tgt.name, r, r.Dur)
		}
		worst = max(worst, r.Dur)
	}
	r = do(hc, "GET", "http://"+agentIP+"/?agent=a1", nil)
	if r.Err != nil || r.Code != 410 || r.Dur > e.tmo(time.Second) {
		t.Fatalf("(e) via workload after kill: %s after %v", r, r.Dur)
	}
	res.Numbers["killed_new_request_ms"] = msOf(worst)
	res.Numbers["killed_via_workload_ms"] = msOf(r.Dur)
	res.pass("e_kill_cascade", fmt.Sprintf("cli %v, push %.1f ms, new requests → 410 object_killed in ≤%v, via workload 410 %v",
		d.Round(time.Millisecond), kr.Result.PushMS, worst.Round(time.Millisecond), get(r.Body, "code")))

	// (f) audit: human/caller → agent → sub-agent.
	var au struct {
		Chain   string `json:"chain"`
		Records []struct {
			Kind      string `json:"kind"`
			Object    string `json:"object"`
			ActingFor string `json:"actingFor"`
			Text      string `json:"text"`
		} `json:"records"`
		Verified bool `json:"verified"`
	}
	e.MustJSON(&au, "audit", co.MagicDNS)
	principal := clientName
	if !e.ClientTagged {
		principal, _ = get(r0.Body, "caller", "user", "login").(string)
	}
	wantChain := principal + " → " + strings.Split(ao.MagicDNS, ".")[0] + " → " + strings.Split(co.MagicDNS, ".")[0]
	if au.Chain != wantChain || !au.Verified {
		so, _, _, _ := e.Twd("audit", ao.ID, "--json")
		t.Logf("agent audit: %s", so)
		t.Fatalf("(f) audit chain %q (want %q), verified %v", au.Chain, wantChain, au.Verified)
	}
	kinds := map[string]bool{}
	for _, rr := range au.Records {
		kinds[rr.Kind] = true
	}
	for _, k := range []string{"spawn", "mint", "ready", "kill"} {
		if !kinds[k] {
			t.Fatalf("(f) audit of the sub-agent lacks %q records: %+v", k, au.Records)
		}
	}
	var aa struct {
		Records []struct {
			Kind   string `json:"kind"`
			Object string `json:"object"`
		} `json:"records"`
		Verified bool `json:"verified"`
	}
	e.MustJSON(&aa, "audit", ao.MagicDNS)
	kills := 0
	for _, rr := range aa.Records {
		if rr.Kind == "kill" {
			kills++
		}
	}
	if !aa.Verified || kills < 2 {
		t.Fatalf("(f) agent audit (with descendants): verified %v, %d kill records", aa.Verified, kills)
	}
	var hu struct {
		Records []any `json:"records"`
	}
	e.MustJSON(&hu, "audit", "--human", principal)
	if len(hu.Records) < 2 {
		t.Fatalf("(f) audit --human %s: %d records", principal, len(hu.Records))
	}
	so, _, _, _ := e.Twd("audit", co.MagicDNS)
	t.Logf("twd audit %s:\n%s", co.MagicDNS, so)
	res.pass("f_audit_chain", fmt.Sprintf("%q, %d records for the sub-agent, chain verified", au.Chain, len(au.Records)))

	// Mint key IDs (for the leftover check after down).
	for _, o := range e.objects() {
		var oa struct {
			Records []struct {
				Kind   string          `json:"kind"`
				Detail json.RawMessage `json:"detail"`
			} `json:"records"`
		}
		e.MustJSON(&oa, "audit", o.ID)
		for _, rr := range oa.Records {
			var dd struct {
				KeyID string `json:"keyID"`
			}
			if rr.Kind == "mint" && json.Unmarshal(rr.Detail, &dd) == nil && dd.KeyID != "" && !slices.Contains(res.KeyIDs, dd.KeyID) {
				res.KeyIDs = append(res.KeyIDs, dd.KeyID)
			}
		}
	}

	// Smoke: ls, logs, status.
	if so, se, _, err := e.Twd("logs", "agent", "-n", "5"); err != nil {
		t.Fatalf("twd logs agent: %v %s", err, se)
	} else {
		_ = so
	}
	if _, se, _, err := e.Twd("ls"); err != nil {
		t.Fatalf("twd ls: %v %s", err, se)
	}
	_ = mcp
}

func anyStrings(v any) []string {
	var out []string
	if a, ok := v.([]any); ok {
		for _, x := range a {
			out = append(out, fmt.Sprint(x))
		}
	}
	return out
}

// Down runs `twd down` and records its duration.
func (e *Env) Down() {
	var dr struct {
		Killed  []string           `json:"killed"`
		Flushed int                `json:"devicesDeleted"`
		Timings map[string]float64 `json:"timingsMs"`
	}
	d := e.MustJSON(&dr, "down")
	e.Res.Numbers["down_ms"] = msOf(d)
	e.Res.pass("down", fmt.Sprintf("%d objects purged, %d devices deleted, %v", len(dr.Killed), dr.Flushed, d.Round(time.Millisecond)))
}

// Leftovers checks (by ID only) which devices and keys twd created in this
// run still exist after `twd down`. A key that is consumed (invalid) or
// revoked doesn't count.
func Leftovers(ctx context.Context, api *tsapi.Client, res *Results) (devices, keys []string) {
	for _, id := range res.NodeIDs {
		if _, err := api.GetDevice(ctx, id); err == nil || !tsapi.IsNotFound(err) {
			devices = append(devices, id)
		}
	}
	for _, id := range res.KeyIDs {
		k, err := api.GetKey(ctx, id)
		if err == nil && !k.Invalid && k.Revoked == "" {
			keys = append(keys, id)
		} else if err != nil && !tsapi.IsNotFound(err) {
			keys = append(keys, id+" ("+netd.Redact(err.Error())+")")
		}
	}
	return devices, keys
}

// Objects lists every object (twd ls --all).
func (e *Env) Objects() []Object { return e.objects() }

// RunPrefixes names one real-tailnet run's nodes: the test client + echo
// get ttest-<label>-<run>- and every node twd creates gets twd-<label>-<run>-.
// label defaults to def; TWD_E2E_PREFIX=ttest-<label>- overrides it so
// concurrent runs (and the final-verification runs, ttest-fv-) clean up
// only their own IDs.
func RunPrefixes(def, run string) (testPrefix, twdPrefix, label string) {
	label = def
	if p := os.Getenv("TWD_E2E_PREFIX"); p != "" {
		l, ok := strings.CutPrefix(p, "ttest-")
		if !ok || l == "" {
			panic("TWD_E2E_PREFIX " + p + " must be ttest-<label>-")
		}
		label = strings.TrimSuffix(l, "-")
	}
	return "ttest-" + label + "-" + run + "-", "twd-" + label + "-" + run + "-", label
}
