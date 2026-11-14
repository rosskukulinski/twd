package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rosskukulinski/twd/mgmt"
	"golang.org/x/net/proxy"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/hostd/mgmtapi"
)

// Config is <state>/twd.json, written by `twd up`.
type Config struct {
	Tailnet  string `json:"tailnet"`
	TenantID string `json:"tenantID,omitempty"`
	// MintMode is "oauth" (the default since M12: a Tailscale OAuth client
	// whose secret stays in a 0600 file; no browser, no Funnel), "wif"
	// (keyless browser flow: the OIDC issuer on its own node over Funnel + a trust
	// credential) or "apikey" (--api-key-file). Empty means apikey (state
	// written before M10).
	MintMode   string `json:"mintMode,omitempty"`
	APIKeyFile string `json:"apiKeyFile,omitempty"` // apikey mode: path only; the key itself is never copied
	// oauth mode: the client file's absolute path (read at use by the CLI
	// and twd-mgmt; never copied) and the client's public ID.
	OAuthClientFile string `json:"oauthClientFile,omitempty"`
	OAuthClientID   string `json:"oauthClientID,omitempty"`
	// Keyless mode: mgmt's own tailnet node (tag:twd-mgmt), joined by
	// interactive login in `twd up`; the API is served on it (WhoIs auth)
	// (never Funnel: the WIF issuer has its own node, Issuer* below).
	MgmtHostname string `json:"mgmtHostname,omitempty"` // tsnet hostname (default twd-mgmt)
	MgmtDNSName  string `json:"mgmtDNSName,omitempty"`  // twd-mgmt.<tailnet>.ts.net
	MgmtNodeID   string `json:"mgmtNodeID,omitempty"`   // stable node ID
	MgmtIP       string `json:"mgmtIP,omitempty"`       // tailnet IPv4
	HTTPS        bool   `json:"https,omitempty"`        // the tailnet has HTTPS certificates (Funnel + twd link need it)
	IssuerURL    string `json:"issuerURL,omitempty"`    // https://<issuerDNSName> (Funnel, wif only)
	// The WIF issuer's own node (tag:twd-issuer, wif only): the only node
	// twd runs for mgmt that is Funnel-capable; mgmt's node never is.
	IssuerHostname string `json:"issuerHostname,omitempty"`
	IssuerDNSName  string `json:"issuerDNSName,omitempty"`
	IssuerNodeID   string `json:"issuerNodeID,omitempty"`
	// WIF trust credential client IDs (public identifiers, not secrets).
	WIFClientID          string `json:"wifClientID,omitempty"`
	WIFLifecycleClientID string `json:"wifLifecycleClientID,omitempty"`
	ControlURL           string `json:"controlURL"`
	TSAPIURL             string `json:"tsapiURL,omitempty"`
	// LocalDev (twd up --local-dev): mgmt also serves a unix socket with
	// the per-host token and hostd talks to it there (hermetic tests, twd
	// dev). Normal operation is tsnet only: the CLI reaches mgmt through
	// mgmt's node loopback proxy, hostd through its own tag:twd-host node.
	LocalDev bool `json:"localDev,omitempty"`
	// PushPort is the port on hostd's node where mgmt pushes kills.
	PushPort       int    `json:"pushPort,omitempty"`
	HostID         string `json:"hostID"`
	HostnamePrefix string `json:"hostnamePrefix,omitempty"`
	Ephemeral      bool   `json:"ephemeral,omitempty"`
	// InsecureSharedHost (twd up --insecure-shared-host): without a process
	// sandbox, still serve process-tier workloads of several tenants
	// (incl. Node-tier apps whose egress is then unenforced, RT-08).
	InsecureSharedHost bool `json:"insecureSharedHost,omitempty"`
	// AllowSiblingEgress: objects may reach other objects on this host over
	// the tailnet (single-host tests/demos).
	AllowSiblingEgress bool   `json:"allowSiblingEgress,omitempty"`
	Workerd            string `json:"workerd"`
	Heartbeat          string `json:"heartbeat"`
	KillTombstone      string `json:"killTombstone"`
	DeviceDeleteDelay  string `json:"deviceDeleteDelay"`
	CreatedAt          string `json:"createdAt"`
	// Tenants are the tenants added with `twd tenant add` (the operator
	// tenant above is the default for every command).
	Tenants []TenantRef `json:"tenants,omitempty"`
}

type state struct {
	Dir string
	Cfg Config
	// Link is <state>/link.json (`twd link`): this machine drives a remote
	// mgmt over the tailnet instead of a local `twd up` stack.
	Link *Link

	remoteTenants []TenantRef
}

// keyless reports whether this state runs in keyless (WIF) mint mode.
func (c Config) keyless() bool { return c.MintMode == "wif" }

// oauth reports whether this state mints through an OAuth client (M12).
func (c Config) oauth() bool { return c.MintMode == "oauth" }

// modeName is the mint mode's short name ("apikey" for pre-M10 state).
func (c Config) modeName() string {
	if c.MintMode == "" {
		return "apikey"
	}
	return c.MintMode
}

// mintMode names the mode for humans.
func (c Config) mintMode() string {
	switch {
	case c.keyless():
		return "wif (keyless)"
	case c.oauth():
		return "oauth"
	}
	return "apikey"
}

func stateDirFrom(flagVal string) (string, error) {
	d := flagVal
	if d == "" {
		d = os.Getenv("TWD_STATE_DIR")
	}
	if d == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		d = filepath.Join(home, ".twd")
	}
	return filepath.Abs(d)
}

func openState(flagVal string) (*state, error) {
	d, err := stateDirFrom(flagVal)
	if err != nil {
		return nil, err
	}
	s := &state{Dir: d}
	if l, err := loadLink(s.linkPath()); err == nil {
		s.Link = l
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	b, err := os.ReadFile(s.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	return s, json.Unmarshal(b, &s.Cfg)
}

func (s *state) configured() bool { return s.Cfg.Tailnet != "" }

// remote reports whether commands go to a linked mgmt over the tailnet:
// link.json exists and this machine has no local `twd up` state.
func (s *state) remote() bool { return s.Link != nil && !s.configured() }

func (s *state) requireUp() error {
	if s.remote() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := s.whoami(ctx); err != nil {
			return err
		}
		return nil
	}
	if !s.configured() {
		return fail("not_up", "twd is not set up in "+s.Dir).withFix("twd up   (on the host; keyless)", "twd link <twd-mgmt.<tailnet>.ts.net>   (on a developer machine)")
	}
	if !s.supervisorAlive() {
		return fail("not_running", "twd is set up in "+s.Dir+" but not running").withFix("twd up")
	}
	return nil
}

func (s *state) configPath() string { return filepath.Join(s.Dir, "twd.json") }
func (s *state) linkPath() string   { return filepath.Join(s.Dir, "link.json") }
func (s *state) mgmtTsnetDir() string {
	return filepath.Join(s.Dir, "mgmt", "tsnet") // twd-mgmt -state-dir <state>/mgmt, tsnet under it
}

// issuerTsnetDir is the WIF issuer node's state (wif mode), next to mgmt's.
func (s *state) issuerTsnetDir() string { return filepath.Join(s.Dir, "mgmt", "issuer-tsnet") }

func (s *state) save() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s.Cfg, "", "  ")
	return os.WriteFile(s.configPath(), b, 0o600)
}

// runDir holds unix sockets. macOS limits socket paths to 104 bytes, and
// hostd nests instance sockets two levels below, so long state dirs (test
// temp dirs) get a short, deterministic /tmp dir instead.
func (s *state) runDir() string {
	d := filepath.Join(s.Dir, "run")
	if len(d) <= 48 {
		return d
	}
	h := sha256.Sum256([]byte(s.Dir))
	return filepath.Join("/tmp", "twd-"+hex.EncodeToString(h[:4]))
}

func (s *state) mgmtSock() string       { return filepath.Join(s.runDir(), "mgmt.sock") }
func (s *state) hostTokenFile() string  { return filepath.Join(s.Dir, "hostd", "mgmt.token") }
func (s *state) hostdRunDir() string    { return filepath.Join(s.runDir(), "h") }
func (s *state) hostdSock() string      { return filepath.Join(s.hostdRunDir(), "hostd.sock") }
func (s *state) logDir() string         { return filepath.Join(s.Dir, "logs") }
func (s *state) pidFile() string        { return filepath.Join(s.Dir, "supervisor.pid") }
func (s *state) log(name string) string { return filepath.Join(s.logDir(), name+".log") }

func (s *state) supervisorPID() int {
	b, err := os.ReadFile(s.pidFile())
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	// After a reboot (or a power cut) the file is stale and its pid may
	// belong to another process, even another twd: a supervisor wrote the
	// file after it started, so a process that started later isn't it.
	if st, err := os.Stat(s.pidFile()); err == nil {
		if start, ok := procStartTime(pid); ok && start.After(st.ModTime().Add(2*time.Second)) {
			return 0
		}
	}
	return pid
}

// procStartTime is when pid started (Linux /proc; ok=false elsewhere).
func procStartTime(pid int) (time.Time, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}, false
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return time.Time{}, false
	}
	f := strings.Fields(string(b)[i+1:])
	if len(f) < 20 {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64) // field 22: starttime, clock ticks after boot
	if err != nil {
		return time.Time{}, false
	}
	sb, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, ln := range strings.Split(string(sb), "\n") {
		if v, ok := strings.CutPrefix(ln, "btime "); ok {
			bt, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return time.Unix(bt, 0).Add(time.Duration(ticks) * time.Second / 100), true // USER_HZ = 100
		}
	}
	return time.Time{}, false
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	// After a reboot the pid file's number may belong to another process:
	// on Linux, only a twd binary counts.
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		argv0, _, _ := strings.Cut(string(b), "\x00")
		return strings.HasPrefix(filepath.Base(argv0), "twd")
	}
	return true
}

func (s *state) supervisorAlive() bool { return pidAlive(s.supervisorPID()) }

// mgmt is the CLI's client for mgmt. Locally (`twd up` on this machine)
// it is the unix socket with the per-host token hostd uses (RT-03):
// same-uid peercred alone admits nobody, and the token file is readable
// only by the twd user. Linked (`twd link`), it is HTTPS to mgmt's tailnet
// node over this machine's own Tailscale connection, authenticated by
// WhoIs: the caller's tailnet identity, no token.
func (s *state) loopbackPath() string { return filepath.Join(s.Dir, "mgmt", "loopback.json") }
func (s *state) hostJoinFile() string { return filepath.Join(s.Dir, "hostd", "join.env") }
func (s *state) hostTsnetDir() string { return filepath.Join(s.Dir, "hostd", "tsnet") }

// loopback reads mgmt's loopback descriptor (nil if mgmt is not up).
func (s *state) loopback() *mgmt.Loopback {
	b, err := os.ReadFile(s.loopbackPath())
	if err != nil {
		return nil
	}
	var lb mgmt.Loopback
	if json.Unmarshal(b, &lb) != nil || lb.Addr == "" || lb.MgmtIP == "" {
		return nil
	}
	return &lb
}

// transport names how this CLI reaches mgmt (twd doctor, twd status).
func (s *state) transport() string {
	switch {
	case s.remote() && s.Link.Tsnet != nil:
		return "tsnet (link, CLI node)"
	case s.remote():
		return "tsnet (link, this machine's Tailscale)"
	case s.Cfg.LocalDev:
		return "local-dev (unix socket + host token)"
	}
	return "tsnet (loopback through mgmt's node)"
}

// mgmt is the client for this state: the linked endpoint, the local-dev
// unix socket, or (normal operation) mgmt's own tailnet IP dialled through
// the node's SOCKS5 loopback proxy, so the request carries mgmt's identity
// and WhoIs makes it the operator.
func (s *state) mgmt() *mgmtapi.Client {
	if s.remote() {
		return s.Link.client()
	}
	if s.Cfg.LocalDev {
		c, _ := mgmtapi.NewClient("unix://" + s.mgmtSock())
		c.TokenFile = s.hostTokenFile()
		return c
	}
	lb := s.loopback()
	if lb == nil {
		// mgmt not up yet (or not in this mode): a client that fails fast.
		c, _ := mgmtapi.NewClient("http://127.0.0.1:1")
		c.HTTP = &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, fmt.Errorf("twd-mgmt's loopback descriptor %s is not there (mgmt not running?)", s.loopbackPath())
		}}}
		return c
	}
	c, _ := mgmtapi.NewClient("http://" + lb.MgmtIP)
	c.HTTP = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{DialContext: socksDialer(lb.Addr, lb.ProxyCred), MaxIdleConnsPerHost: 8}}
	return c
}

// socksDialer dials through tsnet's loopback SOCKS5 proxy (user "tsnet").
func socksDialer(addr, cred string) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		d, err := proxy.SOCKS5("tcp", addr, &proxy.Auth{User: "tsnet", Password: cred}, &net.Dialer{Timeout: 10 * time.Second})
		if err != nil {
			return nil, err
		}
		if cd, ok := d.(proxy.ContextDialer); ok {
			return cd.DialContext(ctx, network, address)
		}
		return d.Dial(network, address)
	}
}

func (s *state) hostd() *mgmtapi.Client {
	c, _ := mgmtapi.NewClient("unix://" + s.hostdSock())
	return c
}

func (s *state) mgmtHealthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.mgmt().Do(ctx, "GET", "/v1/healthz", nil, nil) == nil
}

// ---- binaries ----

func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Dir(p)
}

// findBin finds a twd binary next to this executable, then on PATH.
func findBin(name string) (string, error) {
	if d := exeDir(); d != "" {
		if p := filepath.Join(d, name); isFile(p) {
			return p, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fail("not_installed", name+" not found next to twd ("+exeDir()+") or on PATH").withFix("make build (in the twd repo), or reinstall twd")
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// repoRoots are directories that may hold a twd checkout or install
// (share/twd): above this executable and above the working directory.
func repoRoots() []string {
	var out []string
	seen := map[string]bool{}
	add := func(start string) {
		for d := start; d != "" && d != "/" && d != "."; d = filepath.Dir(d) {
			if !seen[d] {
				seen[d] = true
				out = append(out, d, filepath.Join(d, "share", "twd"))
			}
		}
	}
	add(exeDir())
	if wd, err := os.Getwd(); err == nil {
		add(wd)
	}
	return out
}

func findWorkerd(flagVal string) (string, error) {
	cands := []string{flagVal, os.Getenv("TWD_WORKERD")}
	if d := exeDir(); d != "" {
		cands = append(cands, filepath.Join(d, "workerd"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, ".twd", "bin", "workerd"))
	}
	for _, c := range cands {
		if c != "" && isFile(c) {
			return filepath.Abs(c)
		}
	}
	if p, err := exec.LookPath("workerd"); err == nil {
		return p, nil
	}
	for _, r := range repoRoots() {
		if p := filepath.Join(r, "node_modules", ".bin", "workerd"); isFile(p) {
			return p, nil
		}
	}
	return "", fail("not_installed", "workerd not found").withFix("pass --workerd <path> or set $TWD_WORKERD (twd pins workerd 1.20261108.1)")
}

// createTwdDir finds the create-twd package (bundler + templates).
func createTwdDir() (string, error) {
	if d := os.Getenv("TWD_CREATE_TWD"); d != "" {
		return d, nil
	}
	for _, r := range repoRoots() {
		for _, c := range []string{filepath.Join(r, "js", "packages", "create-twd"), filepath.Join(r, "create-twd"), filepath.Join(r, "node_modules", "create-twd")} {
			if isFile(filepath.Join(c, "lib", "build.mjs")) {
				return c, nil
			}
		}
	}
	return "", fail("not_installed", "create-twd (bundler and templates) not found").withFix("set $TWD_CREATE_TWD to js/packages/create-twd of a twd checkout")
}

// nodePaths are module fallbacks for bundling apps outside the twd repo
// (@twd/tailnet, @twd/mcp, agents, … from twd's own node_modules).
func nodePaths(createTwd string) []string {
	var out []string
	for d := createTwd; d != "/" && d != "."; d = filepath.Dir(d) {
		if st, err := os.Stat(filepath.Join(d, "node_modules")); err == nil && st.IsDir() {
			out = append(out, filepath.Join(d, "node_modules"))
		}
	}
	return out
}
