// Package procsb is the Linux process-tier sandbox (the counterpart of
// the macOS Seatbelt profile in hostd/seatbelt.go).
//
// twd-hostd runs every tenant's workerd and Node app as the same uid, so
// file modes alone don't keep tenant A's code out of tenant B's state.
// procsb is a re-exec launcher: hostd starts
//
//	<hostd binary> __twd-sandbox-exec -f <profile.json> -- <bin> <args…>
//
// and the launcher, before execve(bin) in place (same pid), applies to
// itself:
//
//   - Landlock filesystem rules (unprivileged; no user namespaces): read
//     and execute only the system dirs, the runtime binary and its install
//     prefix; read-write only the instance's own dirs. Everything else
//     (the home dir with ~/.twd and API key files, hostd's and mgmt's
//     state, other tenants' dirs, node keys) is denied.
//   - Landlock TCP rules (ABI ≥ 4): bind and connect only the TCP ports in
//     the profile (the Node tier's own app port and egress proxy). Egress
//     can only go through netd.
//   - Landlock scopes (ABI ≥ 6: signals, abstract unix sockets) and
//     pathname unix-socket connects (ABI ≥ 9: only below its own dirs),
//     when the kernel has them.
//   - PR_SET_NO_NEW_PRIVS and a seccomp filter: no ptrace, process_vm_*,
//     kexec, mount, bpf, perf_event_open, keyctl, namespaces, io_uring;
//     sockets only AF_UNIX, TCP (AF_INET/6, not MPTCP/SCTP) and
//     NETLINK_ROUTE (no UDP, ICMP, raw, packet, vsock or sock_diag).
//
// The seccomp filter is also the sandbox's marker: filters are inherited
// and can't be removed, so twd's unix-socket servers (mgmt's admin socket,
// hostd's push socket, an instance's egress sockets) refuse a peer that
// carries more filters than the server (see peer_linux.go). That covers
// kernels whose Landlock can't restrict unix-socket connects (ABI < 9).
package procsb

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// ExecArg is the launcher's argv[1].
const ExecArg = "__twd-sandbox-exec"

// Name is the sandbox's name in hostd status and doctor.
const Name = "landlock"

// Profile is what one sandboxed process may use. It is written as JSON to
// the instance dir (sandbox.json): it holds paths and ports only, never
// secrets.
type Profile struct {
	// Launcher is the binary that applies the profile (twd-hostd, or a
	// test binary that imports hostd).
	Launcher string `json:"launcher"`
	// Exec are binaries the process may read and execute (workerd, node).
	Exec []string `json:"exec,omitempty"`
	// ReadOnly are dirs readable (and executable) below, such as node's
	// install prefix.
	ReadOnly []string `json:"readOnly,omitempty"`
	// Own are the instance's own dirs: full access below (including
	// creating and connecting to unix sockets there).
	Own []string `json:"own"`
	// ConnectTCP and BindTCP are the only TCP ports the process may
	// connect to or bind (Landlock ABI ≥ 4).
	ConnectTCP []uint16 `json:"connectTCP,omitempty"`
	BindTCP    []uint16 `json:"bindTCP,omitempty"`
	// NoUnixSockets denies socket(AF_UNIX, …) (seccomp): the process can't
	// connect to any pathname or abstract unix socket on the host (Landlock
	// < ABI 9 can't refuse those connects). socketpair(AF_UNIX) stays
	// allowed for STREAM/SEQPACKET (internal IPC; such sockets are
	// connected and can't be re-pointed).
	NoUnixSockets bool `json:"noUnixSockets,omitempty"`
	// Deny documents what hostd denies explicitly (the home dir, state
	// and run dirs, key files). Landlock is deny-by-default, so these are
	// denied because they are not allowed; tests and doctor check them.
	Deny []string `json:"deny,omitempty"`
}

// Write stores p at path (0600).
func (p *Profile) Write(path string) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Load reads a profile.
func Load(path string) (*Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("sandbox profile %s: %w", path, err)
	}
	if p.Launcher == "" {
		return nil, fmt.Errorf("sandbox profile %s: no launcher", path)
	}
	return &p, nil
}

// Command runs bin args under the profile at path (launched by launcher).
func Command(launcher, profile, bin string, args ...string) *exec.Cmd {
	return exec.Command(launcher, append([]string{ExecArg, "-f", profile, "--", bin}, args...)...)
}

// Probe ops (ProbeCommand): each exits 0 if the operation succeeded and 3
// if it was refused, printing the errno name.
const (
	ProbeRead        = "read"  // open + read a file
	ProbeList        = "list"  // readdir
	ProbeWrite       = "write" // create/truncate a file
	ProbeExec        = "exec"  // execve a binary (with --version)
	ProbeConnectUnix = "connect-unix"
	ProbeConnectTCP  = "connect-tcp" // host:port
	ProbeBindTCP     = "bind-tcp"    // host:port
	ProbeUDP         = "udp"         // send a datagram to host:port
	ProbePtrace      = "ptrace"      // PTRACE_ATTACH pid
	ProbeVMRead      = "vmread"      // process_vm_readv pid
	ProbeSignal      = "signal"      // kill(pid, 0); pid "self" = its own
	ProbeTgkill      = "tgkill"      // tgkill(pid, pid, 0)
	ProbeSocketpair  = "socketpair"  // socketpair(AF_UNIX, stream|seqpacket|dgram)
)

// ProbeCommand applies the profile at path in a fresh launcher process,
// then (instead of exec'ing a workload) performs op on args and reports.
// The probe runs under exactly the rules, seccomp filter and no_new_privs
// a workload with that profile gets: it is how tests and twd doctor check
// what a sandboxed workerd or Node app can reach.
func ProbeCommand(profile, op string, args ...string) (*exec.Cmd, error) {
	p, err := Load(profile)
	if err != nil {
		return nil, err
	}
	return exec.Command(p.Launcher, append([]string{ExecArg, "-f", profile, "--probe", op}, args...)...), nil
}
