// Package proto is the twd-guestd ⇄ twd-hostd contract for the microVM
// tier: the guest config delivered on the config drive, and the
// newline-delimited JSON control channel over vsock.
//
// Nothing in here is secret, and nothing tailnet-related ever crosses it:
// the guest gets workerd's config and bundle, a per-instance egress MAC key
// (platform attribution, not a tailnet credential), and its network
// addresses. Node keys and auth keys stay in host netd.
//
// The one credential is guest-internal: GuestConfig.CtlToken, a random
// per-boot token that authenticates guestd's control connections to the
// host (so a process in the guest other than guestd can't open, take over
// or forge the control channel), and the per-exec Msg.Token that does the
// same for an exec's stdio stream. Neither is worth anything outside that
// one VM's boot.
package proto

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Config drive layout (/dev/vdb): a plain ustar stream (no filesystem), padded
// to a 4 KiB multiple. guestd extracts it into AppDir.
const (
	// GuestConfigFile is the JSON GuestConfig inside the config tar.
	GuestConfigFile = "guest.json"
	// AppDir is where the generated workerd instance dir is extracted.
	AppDir = "/run/twd/app"
	// DODir is the guest's DO storage directory: the persistent volume
	// (GuestConfig.Volume) mounted there, else guest tmpfs.
	DODir = "/run/twd/do"
	// VolumeDev is the persistent volume's device (the third drive).
	VolumeDev = "/dev/vdc"

	// Container guests (ADR-032) have no DO volume: the third drive is the
	// image (erofs, read-only) and the fourth the instance's scratch disk
	// (ext4, made fresh on the host for every boot; the overlay upper).
	ImageDev   = "/dev/vdc"
	ScratchDev = "/dev/vdd"
)

// Defaults for the guest network (every VM lives in its own network
// namespace on the host, so all of them can use the same addresses).
const (
	GatewayIP   = "192.168.72.1"
	GuestIP     = "192.168.72.2"
	PrefixLen   = 24
	IngressPort = 8080 // workerd ingress (cfBlobHeader x-twd-cf); only host netd dials it
	EgressObj   = 3128 // facet egress (platform Egress, MAC'd object attribution)
	EgressWL    = 3129 // tenant service egress (workload identity)
	HostAPI     = 3130 // local host API (ensure/spawn/ready/kill/audit)
	// VsockPort is the host port guestd connects to (CID 2). Firecracker
	// forwards it to the unix socket <vsock uds>_<VsockPort> on the host.
	VsockPort = 1024
)

// GuestConfig is guest.json.
type GuestConfig struct {
	// UnixNano is host wall-clock time when the drive was written. The
	// guest has no trustworthy clock at boot; the platform's egress MAC
	// timestamps need one within ±2 min.
	UnixNano int64  `json:"unixNano"`
	IP       string `json:"ip"`
	Gateway  string `json:"gateway"`
	Prefix   int    `json:"prefix"`
	MTU      int    `json:"mtu,omitempty"`
	// Workerd is the argv after the binary (config path relative to AppDir).
	WorkerdArgs []string `json:"workerdArgs"`
	// HealthPath is requested on 127.0.0.1:IngressPort with HealthCF as
	// the x-twd-cf header; 200 means ready.
	HealthCF string `json:"healthCF"`
	// Debug enables the probe op (connectivity tests).
	Debug bool `json:"debug,omitempty"`
	// Volume is the block device holding DO storage (ext4, created and
	// decrypted on the host), mounted at DODir. "" = tmpfs (not persistent).
	Volume string `json:"volume,omitempty"`
	// VolumeFormat: Volume is blank and the guest formats it (ext4) before
	// mounting it. The vz backend (macOS) has no mkfs.ext4 on the host and
	// decrypts on the host (an encrypted disk image), so the guest formats
	// plaintext it already sees; under Firecracker the host formats it.
	VolumeFormat bool `json:"volumeFormat,omitempty"`
	// Rosetta is the virtiofs tag of a Rosetta for Linux share (vz on Apple
	// silicon): guestd mounts it read-only at RosettaDir and registers it
	// with binfmt_misc, so x86_64 ELF binaries run in the arm64 guest.
	Rosetta string `json:"rosetta,omitempty"`
	// CtlToken authenticates guestd to the host: the first line of every
	// control connection ("hello") carries it, and the host refuses a
	// connection without it (random per boot; guestd keeps it in memory,
	// and container processes can't read the config drive).
	CtlToken string `json:"ctlToken,omitempty"`
	// Container, if set, makes this a container guest: no workerd; guestd
	// mounts the image + scratch as an overlay root and runs the image's
	// entrypoint in it as a supervised child.
	Container *ContainerConfig `json:"container,omitempty"`
}

// ContainerConfig is a container guest's process and filesystem config:
// the OCI image config with the start() overrides already applied by the
// host.
type ContainerConfig struct {
	Image   string   `json:"image"`   // block device with the erofs image
	Scratch string   `json:"scratch"` // block device with the fresh ext4 scratch ("" = tmpfs upper)
	Argv    []string `json:"argv"`    // ENTRYPOINT + CMD (or the start() entrypoint)
	Env     []string `json:"env"`     // KEY=VALUE, image env then start() env
	Cwd     string   `json:"cwd,omitempty"`
	User    string   `json:"user,omitempty"` // "", "name", "uid", "uid:gid", "name:group"
	// Pids is pids.max of the container's cgroup in the guest (0 = 4096).
	Pids     int    `json:"pids,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	// DNS is the container's nameserver (the gateway, answered by the
	// host's sandbox egress proxy); "" = no DNS (an empty resolv.conf).
	DNS string `json:"dns,omitempty"`
	// CACert is the PEM certificate of the container's HTTPS-intercept CA
	// (ADR-032 D4), installed at CACertPath and appended to the image's
	// trust bundles. Its key never enters the guest.
	CACert string `json:"caCert,omitempty"`
	// Canary is held in guestd's memory as the no-keys scan's positive
	// control (it is not a secret).
	Canary string `json:"canary,omitempty"`
	// ScratchFormat: Scratch is a blank device the guest formats itself
	// (vz: macOS has no mkfs.ext4; Firecracker's helper makes it instead).
	ScratchFormat bool `json:"scratchFormat,omitempty"`
	// CPUQuotaUS, if > 0, is cpu.max (µs per 100 ms) of the guest's
	// container cgroup: the fractional-vCPU cap where the host can't cap
	// the VMM itself (vz). Firecracker caps the VMM's cgroup instead.
	CPUQuotaUS int `json:"cpuQuotaUs,omitempty"`
}

// CACertPath is where a container finds its HTTPS-intercept CA certificate
// (Cloudflare's path).
const CACertPath = "/etc/cloudflare/certs/cloudflare-containers-ca.crt"

// CATrustBundles are the image trust stores the CA is appended to, where
// they exist (Debian/Ubuntu/Alpine, RHEL/Fedora, and the OpenSSL default).
var CATrustBundles = []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/cert.pem"}

// Container-mode stream framing.
const (
	// ExecStreamHello is the Type of the first line a guest sends on a new
	// vsock connection that carries one exec's stdio (Msg.Exec names the
	// exec, Msg.Token is the stream token the host issued in the exec op)
	// instead of the control channel.
	ExecStreamHello = "stream"

	// Exec stream frames: a 1-byte kind, a 4-byte big-endian length, then
	// the payload. guest → host: FrameStdout, FrameStderr, FrameExit (int32
	// big-endian exit code; the last frame). host → guest: FrameStdin,
	// FrameStdinEOF (half-close; empty).
	FrameStdin    = 0
	FrameStdout   = 1
	FrameStderr   = 2
	FrameExit     = 3
	FrameStdinEOF = 4
	// MaxFrame bounds one frame's payload.
	MaxFrame = 1 << 20
)

// ExecSpec is an "exec" op's process (container guests).
type ExecSpec struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env,omitempty"` // KEY=VALUE, over the container's env
	Cwd  string   `json:"cwd,omitempty"`
	User string   `json:"user,omitempty"`
	// Stdin: the host streams stdin (else /dev/null).
	Stdin bool `json:"stdin,omitempty"`
	// Stdout "pipe" (default) | "ignore"; Stderr "pipe" (default) |
	// "ignore" | "combined" (into stdout).
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	// PTY runs it on a pseudo-terminal (stdout carries the terminal's
	// output; there is no separate stderr).
	PTY  bool `json:"pty,omitempty"`
	Cols int  `json:"cols,omitempty"`
	Rows int  `json:"rows,omitempty"`
}

// RosettaDir is where the Rosetta share is mounted in the guest.
const RosettaDir = "/run/rosetta"

// Msg is one control-channel message (either direction).
//
// guest → host: Type "hello" (the first line of a control connection;
// Token = GuestConfig.CtlToken), "boot" (once), "ready" (workerd healthy),
// "workerd-exit", "log", or "resp" (answer to an op, same ID).
// host → guest: Op "probe", "stats", "fsscan", "ping", "shutdown" (stop
// workerd, sync and unmount the volume: the host may then release it),
// "fsbench" (write+fsync latency on the DO volume), and in debug mode
// "run" (run Argv from AppDir, return its output and exit code: platform
// tests such as the Rosetta check, not a tenant exec API).
//
// Container guests: guest → host Type "ctr-ready" (the entrypoint runs:
// PID), "ctr-exit" (it exited: ExitCode, 128+n for signal n), "exec-exit"
// (Exec, ExitCode). host → guest Op "exec" (Exec id + ExecSpec + Token: the
// guest first opens the exec's stdio stream, naming Exec and Token, then
// answers with the PID),
// "signal" (Signal to the entrypoint, or to Exec's process if set),
// "resize" (Exec, Cols, Rows) and "status" (Running, PID, Exited, ExitCode).
type Msg struct {
	Type string `json:"type,omitempty"`
	ID   int64  `json:"id,omitempty"`
	Op   string `json:"op,omitempty"`

	// boot / ready
	UptimeMS  int64  `json:"uptimeMs,omitempty"`
	WorkerdMS int64  `json:"workerdMs,omitempty"`
	ExitCode  int    `json:"exitCode,omitempty"`
	Restarts  int    `json:"restarts,omitempty"`
	Text      string `json:"text,omitempty"`
	Error     string `json:"error,omitempty"`

	// probe
	Targets []string      `json:"targets,omitempty"`
	Results []ProbeResult `json:"results,omitempty"`

	// stats
	Stats *Stats `json:"stats,omitempty"`

	// fsbench: N writes of 4 KiB + fdatasync on DODir
	N     int    `json:"n,omitempty"`
	Bench *Bench `json:"bench,omitempty"`

	// run (debug): argv; the answer's Text is stdout+stderr (capped)
	Argv []string `json:"argv,omitempty"`

	// fsscan: files whose name looks like tailscale state
	Found   []string `json:"found,omitempty"`
	Scanned int      `json:"scanned,omitempty"`

	// hello: GuestConfig.CtlToken; exec / ExecStreamHello: the exec's
	// stream token (random per exec, issued by the host)
	Token string `json:"token,omitempty"`

	// container guests
	Exec     string    `json:"exec,omitempty"`
	ExecSpec *ExecSpec `json:"execSpec,omitempty"`
	PID      int       `json:"pid,omitempty"`
	Signal   int       `json:"signal,omitempty"`
	Cols     int       `json:"cols,omitempty"`
	Rows     int       `json:"rows,omitempty"`
	Running  bool      `json:"running,omitempty"`
	Exited   bool      `json:"exited,omitempty"`
}

// WriteFrame writes one exec stream frame.
func WriteFrame(w io.Writer, kind byte, p []byte) error {
	var h [5]byte
	h[0] = kind
	binary.BigEndian.PutUint32(h[1:], uint32(len(p)))
	if _, err := w.Write(append(h[:], p...)); err != nil {
		return err
	}
	return nil
}

// ReadFrame reads one exec stream frame (payloads over MaxFrame are an
// error).
func ReadFrame(r io.Reader) (kind byte, p []byte, err error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("exec stream frame of %d bytes", n)
	}
	p = make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return h[0], p, nil
}

// ProbeResult is the outcome of one TCP connect from inside the guest.
type ProbeResult struct {
	Target string `json:"target"`
	Result string `json:"result"` // connected | refused | timeout | unreachable | error
	Detail string `json:"detail,omitempty"`
	MS     int64  `json:"ms"`
}

// Bench is fsbench's result (microseconds).
type Bench struct {
	N          int    `json:"n"`
	FS         string `json:"fs"` // the mount's source and type
	WriteP50US int64  `json:"writeP50us"`
	WriteP95US int64  `json:"writeP95us"`
	SyncP50US  int64  `json:"fsyncP50us"`
	SyncP95US  int64  `json:"fsyncP95us"`
}

// Stats is guest memory accounting.
type Stats struct {
	MemTotalKB     int64 `json:"memTotalKB"`
	MemAvailableKB int64 `json:"memAvailableKB"`
	WorkerdRSSKB   int64 `json:"workerdRssKB"`
	UptimeMS       int64 `json:"uptimeMs"`
	// DO storage: the mount's source ("/dev/vdc ext4" or "tmpfs") and usage.
	DOFS     string `json:"doFS,omitempty"`
	DOUsedKB int64  `json:"doUsedKB,omitempty"`
}
