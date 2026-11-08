package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	Tailnet           string `json:"tailnet"`
	TenantID          string `json:"tenantID,omitempty"`
	APIKeyFile        string `json:"apiKeyFile"` // path only; the key itself is never copied
	ControlURL        string `json:"controlURL"`
	TSAPIURL          string `json:"tsapiURL,omitempty"`
	HostID            string `json:"hostID"`
	HostnamePrefix    string `json:"hostnamePrefix,omitempty"`
	Ephemeral         bool   `json:"ephemeral,omitempty"`
	Workerd           string `json:"workerd"`
	Heartbeat         string `json:"heartbeat"`
	KillTombstone     string `json:"killTombstone"`
	DeviceDeleteDelay string `json:"deviceDeleteDelay"`
	CreatedAt         string `json:"createdAt"`
	// Tenants are the tenants added with `twd tenant add` (the operator
	// tenant above is the default for every command).
	Tenants []TenantRef `json:"tenants,omitempty"`
}

type state struct {
	Dir string
	Cfg Config
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

func (s *state) requireUp() error {
	if !s.configured() {
		return fail("not_up", "twd is not set up in "+s.Dir).withFix("twd up --tailnet <tailnet> --api-key-file <file>")
	}
	if !s.supervisorAlive() {
		return fail("not_running", "twd is set up in "+s.Dir+" but not running").withFix("twd up")
	}
	return nil
}

func (s *state) configPath() string { return filepath.Join(s.Dir, "twd.json") }

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
	return pid
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func (s *state) supervisorAlive() bool { return pidAlive(s.supervisorPID()) }

func (s *state) mgmt() *mgmtapi.Client {
	c, _ := mgmtapi.NewClient("unix://" + s.mgmtSock())
	return c
}

func (s *state) hostd() *mgmtapi.Client {
	c, _ := mgmtapi.NewClient("unix://" + s.hostdSock())
	return c
}

func (s *state) mgmtHealthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.mgmt().Do(ctx, "GET", "/v0/healthz", nil, nil) == nil
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
