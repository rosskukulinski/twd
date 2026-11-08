// Package proto is the twd-guestd ⇄ twd-hostd contract for the microVM
// tier: the guest config delivered on the config drive, and the
// newline-delimited JSON control channel over vsock.
//
// Nothing in here is secret, and nothing tailnet-related ever crosses it:
// the guest gets workerd's config and bundle, a per-instance egress MAC key
// (platform attribution, not a tailnet credential), and its network
// addresses. Node keys and auth keys stay in host netd.
package proto

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
}

// Msg is one control-channel message (either direction).
//
// guest → host: Type "boot" (once, on connect), "ready" (workerd healthy),
// "workerd-exit", "log", or "resp" (answer to an op, same ID).
// host → guest: Op "probe", "stats", "fsscan", "ping", "shutdown" (stop
// workerd, sync and unmount the volume: the host may then release it),
// "fsbench" (write+fsync latency on the DO volume).
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

	// fsscan: files whose name looks like tailscale state
	Found   []string `json:"found,omitempty"`
	Scanned int      `json:"scanned,omitempty"`
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
