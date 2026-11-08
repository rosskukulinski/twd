package hostd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Process-tier sandboxes (Config.Sandbox).
const (
	SandboxNone = "none"
	// SandboxSeatbelt runs each workerd under macOS Seatbelt
	// (sandbox-exec). Every twd process runs as one uid on the process
	// tier, so 0700 dirs alone don't keep tenant A's workerd out of tenant
	// B's state: the profile denies every hostd data and run dir (node
	// keys, other tenants' DO storage and sockets, mgmt's admin socket,
	// API key files) except the instance's own dir and its own DO storage.
	SandboxSeatbelt = "seatbelt"
	// SandboxAuto is seatbelt on macOS when sandbox-exec exists, else none.
	SandboxAuto = "auto"
)

const sandboxExec = "/usr/bin/sandbox-exec"

// ResolveSandbox maps "auto"/"" to what this host supports.
func ResolveSandbox(s string) (string, error) {
	switch s {
	case "", SandboxNone:
		return SandboxNone, nil
	case SandboxAuto:
		if runtime.GOOS == "darwin" && isFileExec(sandboxExec) {
			return SandboxSeatbelt, nil
		}
		return SandboxNone, nil
	case SandboxSeatbelt:
		if runtime.GOOS != "darwin" || !isFileExec(sandboxExec) {
			return "", fmt.Errorf("sandbox %q needs macOS %s", s, sandboxExec)
		}
		return SandboxSeatbelt, nil
	}
	return "", fmt.Errorf("unknown sandbox %q (none | seatbelt | auto)", s)
}

func isFileExec(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

// realPath resolves symlinks (/tmp → /private/tmp): Seatbelt matches
// resolved paths. Missing paths resolve their longest existing prefix.
func realPath(p string) string {
	p, _ = filepath.Abs(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, base := filepath.Split(p)
	if dir == "" || dir == p || dir == "/" {
		return p
	}
	return filepath.Join(realPath(strings.TrimSuffix(dir, "/")), base)
}

func sbQuote(p string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p) + `"`
}

// SeatbeltProfile returns a profile that denies reads, writes and unix
// socket connects under each of deny, then allows them again under each of
// own, and reads of each of readOnly (Seatbelt: the last matching rule
// wins). The ancestors of own stay stat-able (workerd resolves its paths).
func SeatbeltProfile(own, readOnly, deny []string) string {
	return seatbeltProfile(own, readOnly, nil, deny)
}

// seatbeltProfile is SeatbeltProfile plus readOnlyTrees: dirs readable (not
// writable) below, such as a node install prefix under the home dir.
func seatbeltProfile(own, readOnly, readOnlyTrees, deny []string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	var denyR []string
	for _, d := range deny {
		if d == "" {
			continue
		}
		r := realPath(d)
		if !slices.Contains(denyR, r) {
			denyR = append(denyR, r)
		}
	}
	for _, r := range denyR {
		fmt.Fprintf(&b, "(deny file-read* file-write* (subpath %s))\n", sbQuote(r))
		fmt.Fprintf(&b, "(deny network-outbound (remote unix-socket (subpath %s)))\n", sbQuote(r))
	}
	var meta []string
	for _, o := range append(append([]string{}, own...), readOnlyTrees...) {
		r := realPath(o)
		for d := filepath.Dir(r); d != "/" && d != "."; d = filepath.Dir(d) {
			if !slices.Contains(meta, d) {
				meta = append(meta, d)
			}
		}
	}
	for _, m := range meta {
		fmt.Fprintf(&b, "(allow file-read-metadata (literal %s))\n", sbQuote(m))
	}
	for _, o := range readOnly {
		for _, p := range []string{o, realPath(o)} {
			fmt.Fprintf(&b, "(allow file-read* (literal %s))\n", sbQuote(p))
		}
	}
	for _, o := range readOnlyTrees {
		fmt.Fprintf(&b, "(allow file-read* (subpath %s))\n", sbQuote(realPath(o)))
	}
	for _, o := range own {
		r := realPath(o)
		fmt.Fprintf(&b, "(allow file-read* file-write* (subpath %s))\n", sbQuote(r))
		fmt.Fprintf(&b, "(allow network-outbound (remote unix-socket (subpath %s)))\n", sbQuote(r))
	}
	return b.String()
}

// sandboxDeny is what no workerd may touch: the user's home dir (API key
// files, including those of tenants added after this workerd started, and
// everything else there), hostd's data and run dirs, the configured extra
// paths (twd up: its state and run dirs), and the API key file of every
// tenant this host serves.
func (h *Host) sandboxDeny() []string {
	deny := append([]string{h.cfg.DataDir, h.cfg.RunDir}, h.cfg.SandboxDeny...)
	if home, err := os.UserHomeDir(); err == nil && home != "/" && home != "" {
		deny = append(deny, home)
	}
	h.mu.Lock()
	for _, t := range h.tenants {
		if t.info.APIKeyFile != "" {
			deny = append(deny, t.info.APIKeyFile)
		}
	}
	h.mu.Unlock()
	// Every tenant mgmt knows, including ones this host has no workload of
	// yet (best effort: the local socket allows it, a remote mgmt may not).
	ctx, cancel := context.WithTimeout(h.ctx, 3*time.Second)
	defer cancel()
	if ts, err := h.mgmt.Tenants(ctx); err == nil {
		for _, t := range ts {
			if t.APIKeyFile != "" {
				deny = append(deny, t.APIKeyFile)
			}
		}
	}
	return deny
}

// workerdCommand builds the instance's workerd command line, wrapped in its
// tenant's sandbox.
func (inst *instance) workerdCommand() (*exec.Cmd, error) {
	return inst.sandboxed(inst.h.cfg.WorkerdBin, []string{"serve", inst.res.Config, "--experimental"},
		[]string{inst.dir, inst.doStorage}, []string{inst.h.cfg.WorkerdBin}, nil)
}

// nodeCommand builds the Node tier's command line under the same sandbox as
// workerd: tenant code too. It may write only its instance dir and its own
// data dir; it may read the node binary and, when node lives in a
// "<prefix>/bin" dir (nvm, Homebrew, a tarball), that install prefix.
func (inst *instance) nodeCommand(args []string) (*exec.Cmd, error) {
	np := inst.node
	return inst.sandboxed(np.bin, args, []string{inst.dir, np.dataDir}, []string{np.bin}, nodePrefix(np.bin))
}

// nodePrefix is node's install prefix (…/bin/node → …), if it has one.
func nodePrefix(bin string) []string {
	r := realPath(bin)
	if d := filepath.Dir(r); filepath.Base(d) == "bin" {
		if p := filepath.Dir(d); p != "/" && p != "/usr" && p != "." {
			return []string{p}
		}
	}
	return nil
}

// sandboxed returns bin args, run under the host's sandbox (Seatbelt:
// sandbox-exec execs bin in place, so the pid is bin's). The profile is
// written to the instance dir and recorded as inst.profile.
func (inst *instance) sandboxed(bin string, args, own, readOnly, readOnlyTrees []string) (*exec.Cmd, error) {
	if inst.h.sandbox != SandboxSeatbelt {
		return exec.Command(bin, args...), nil
	}
	prof := seatbeltProfile(own, readOnly, readOnlyTrees, inst.h.sandboxDeny())
	p := filepath.Join(inst.dir, "sandbox.sb")
	if err := os.WriteFile(p, []byte(prof), 0o600); err != nil {
		return nil, err
	}
	inst.mu.Lock()
	inst.profile = p
	inst.mu.Unlock()
	return exec.Command(sandboxExec, append([]string{"-f", p, bin}, args...)...), nil
}
