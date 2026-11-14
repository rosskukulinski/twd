// twd-hostd is the twd host agent (process tier): it registers with
// twd-mgmt, heartbeats, pulls assignments, and runs each deployment as one
// supervised workerd process with per-object tailnet nodes (netd library).
//
//	twd-hostd -mgmt unix:///Users/me/.twd/run/mgmt.sock -host-id h1 -data-dir ~/.twd/hostd
//
// The push API (mgmt → hostd) and status live on <run-dir>/hostd.sock:
//
//	POST /v1/push/kill {objectID, reason}   POST /v1/push/assignments   GET /v1/status
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/image"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm/helperapi"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/netd"
	"tailscale.com/tsnet"
)

// hostNodeName is the tsnet hostname of a host's own node (mgmt.HostNodeName).
func hostNodeName(hostID string) string { return "twd-host-" + hostID }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "twd-hostd:", netd.Redact(err.Error()))
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "join" {
		return runJoin(os.Args[2:])
	}
	home, _ := os.UserHomeDir()
	var (
		mgmtAddr   = flag.String("mgmt", "", "twd-mgmt address: http(s)://twd-mgmt.<tailnet> over this host's node (default), or unix:///path with -local-dev")
		localDev   = flag.Bool("local-dev", false, "talk to mgmt over its unix socket with the per-host token instead of this host's own tailnet node (twd dev, hermetic tests)")
		tsDir      = flag.String("tsnet-dir", "", "state of this host's own tailnet node (default <data-dir>/tsnet)")
		tsControl  = flag.String("tsnet-control-url", "", "operator tailnet control URL for this host's node (required unless -local-dev)")
		joinFile   = flag.String("join-token-file", "", "TS_AUTHKEY=… file with the single-use join key mgmt minted (twd host add / twd up); waited for when the node has no state yet. Deleted after use")
		pushPort   = flag.Int("push-port", 7474, "port on this host's node where mgmt pushes kills and assignment changes")
		hostID     = flag.String("host-id", "", "this host's ID (default: hostname)")
		dataDir    = flag.String("data-dir", filepath.Join(home, ".twd", "hostd"), "node state, DO storage, objects.json")
		runDir     = flag.String("run-dir", "", "sockets and generated configs (short path; default a new /tmp dir)")
		workerd    = flag.String("workerd", "", "workerd binary (default: $TWD_WORKERD, ~/.twd/bin/workerd, or PATH)")
		hb         = flag.Duration("heartbeat", 3*time.Second, "heartbeat / assignment poll period")
		fenceAfter = flag.Duration("fence-after", hostd.DefaultFenceAfter, "stop serving stateful workloads after this long without a successful heartbeat (≤ 40 s, below mgmt's 45 s lease; negative disables)")
		ephemeral  = flag.Bool("ephemeral", false, "join nodes as ephemeral (dev/test)")
		prefix     = flag.String("hostname-prefix", "", "prefix for every node hostname")
		controlURL = flag.String("control-url", "", "override every tenant's control URL (testing)")
		capacity   = flag.Int("capacity-nodes", 1000, "advertised node capacity")
		tombstone  = flag.Duration("kill-tombstone", 10*time.Second, "keep killed nodes RST-ing new connections this long")
		logLevel   = flag.String("log-level", "info", "debug|info|warn|error")
		tsnetLogs  = flag.Bool("tsnet-logs", false, "forward tsnet logs at debug level")
		tier       = flag.String("default-tier", "process", "tier for deployments and tenants that name none: process | microvm")
		vmKernel   = flag.String("vm-kernel", vmAsset(home, "vmlinux"), "microVM tier: guest kernel (Firecracker on Linux, Virtualization.framework on macOS)")
		vmRootfs   = flag.String("vm-rootfs", vmAsset(home, "rootfs.ext4"), "microVM tier: guest image from guest/build-image.sh")
		vmHelper   = flag.String("vm-helper", helperapi.DefaultSocket, "microVM tier: twd-vmhelper socket (sudo twd host setup --microvm)")
		vmFC       = flag.String("firecracker", "/usr/local/bin/firecracker", "microVM tier, as root without the helper service: firecracker binary")
		vmJailer   = flag.String("jailer", "/usr/local/bin/jailer", "microVM tier, as root without the helper service: jailer binary")
		vmVolMiB   = flag.Int("vm-volume-mib", 1024, "microVM tier: size of new DO volumes (sparse)")
		vmMem      = flag.Int("vm-mem-mib", 512, "microVM tier: guest memory")
		vmCPUs     = flag.Int("vm-vcpus", 2, "microVM tier: guest vCPUs")
		vmHuge     = flag.String("vm-hugepages", "auto", "microVM tier: back guest RAM with 2 MiB pages: auto | on | off")
		vzVMM      = flag.String("vz-vmm", "", "microVM tier on macOS: the twd-vz VMM, signed with the virtualization entitlement (default: next to twd-hostd)")
		vmRosetta  = flag.Bool("vm-rosetta", false, "microVM tier on macOS (Apple silicon): share Rosetta for Linux into guests so x86_64 binaries run")
		sandboxF   = flag.String("sandbox", "auto", "process-tier workerd/Node sandbox: auto (seatbelt on macOS, landlock on Linux) | seatbelt | landlock | none")
		insecureSH = flag.Bool("insecure-shared-host", false, "without a process sandbox, still run process-tier workloads of more than one tenant (tenants are NOT isolated from each other)")
		sbDeny     = flag.String("sandbox-deny", "", "comma-separated extra paths no workerd may read, write or connect to (twd up: its state and run dirs)")
		nodeBin    = flag.String("node", "", "Node process tier (runtime \"node\"): node binary (default: $TWD_NODE, or node on PATH)")
		tokenFile  = flag.String("mgmt-token-file", filepath.Join(home, ".twd", "hostd", "mgmt.token"), "per-host token file minted by twd-mgmt (sent as Twd-Host-Token on the unix socket; \"\" = none)")
		siblings   = flag.Bool("allow-sibling-egress", false, "let objects reach other objects hosted here over the tailnet (hairpin; single-host tests/demos)")
		ctrOff     = flag.Bool("no-containers", false, "don't offer containers (ctx.container) even with a microVM backend")
		ctrKernel  = flag.String("vm-container-kernel", "", "containers: guest kernel with erofs (guest/build-kernel.sh; default -vm-kernel)")
		ctrCache   = flag.String("container-image-cache", "", "containers: converted image cache (default <data-dir>/images)")
		ctrImages  = flag.String("container-image-dir", "", "containers: dir of image tarballs with an images.json index (until the mgmt registry pull)")
		ctrPublic  = flag.String("container-public-images", "", "containers: comma-separated image refs cached once for all tenants (public bases)")
		ctrIdle    = flag.Duration("container-idle", hostd.DefaultContainerIdle, "containers: default inactivity timeout (setInactivityTimeout overrides, ≤ 6 h)")
		ctrMaxLife = flag.Duration("container-max-lifetime", hostd.DefaultContainerMaxLife, "containers: hard max lifetime of a container")
		ctrGrace   = flag.Duration("container-stop-grace", hostd.DefaultContainerGrace, "containers: cap on destroy()'s SIGTERM → SIGKILL grace")
		ctrMax     = flag.Int("container-max", 0, "containers: max running on this host (0 = unlimited)")
		ctrMemMiB  = flag.Int("container-mem-mib", 0, "containers: max total guest memory on this host (0 = unlimited)")
		ctrVCPU    = flag.Float64("container-vcpu", 0, "containers: max total vCPU on this host (0 = unlimited)")
	)
	flag.Parse()
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		return err
	}
	log := slog.New(netd.NewRedactingHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
	if *hostID == "" {
		h, _ := os.Hostname()
		*hostID = h
	}
	wd, err := findWorkerd(*workerd, home)
	if err != nil {
		return err
	}
	if *nodeBin == "" {
		*nodeBin = os.Getenv("TWD_NODE")
	}
	var mc *mgmtapi.Client
	var ts *tsnet.Server
	var pushLn net.Listener
	if *localDev {
		if *mgmtAddr == "" {
			*mgmtAddr = "unix://" + filepath.Join(home, ".twd", "run", "mgmt.sock")
		}
		if mc, err = mgmtapi.NewClient(*mgmtAddr); err != nil {
			return err
		}
		if strings.HasPrefix(*mgmtAddr, "unix://") || strings.HasPrefix(*mgmtAddr, "/") {
			mc.TokenFile = *tokenFile
		}
	} else {
		if *tsControl == "" {
			return errors.New("-tsnet-control-url is required (normal operation is tsnet only; -local-dev for the unix socket)")
		}
		if *mgmtAddr == "" {
			return errors.New("-mgmt is required: http(s)://twd-mgmt.<tailnet>")
		}
		if *tsDir == "" {
			*tsDir = filepath.Join(*dataDir, "tsnet")
		}
		ts, err = joinHostNode(context.Background(), *tsDir, hostNodeName(*hostID), *tsControl, *joinFile, log)
		if err != nil {
			return err
		}
		defer ts.Close()
		if mc, err = mgmtapi.NewClient(*mgmtAddr); err != nil {
			return err
		}
		mc.HTTP = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{DialContext: ts.Dial, MaxIdleConnsPerHost: 16}}
		if pushLn, err = ts.Listen("tcp", fmt.Sprintf(":%d", *pushPort)); err != nil {
			return fmt.Errorf("push listener on the node: %w", err)
		}
		// mgmt's node must be in our netmap before the first request.
		wctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		for {
			err := mc.Do(wctx, "GET", "/v1/healthz", nil, nil)
			if _, typed := mgmtapi.AsError(err); err == nil || typed {
				break // any answer from mgmt means the path is open
			} else if wctx.Err() != nil {
				cancel()
				return fmt.Errorf("mgmt %s unreachable over the tailnet: %w (does the policy grant tag:twd-host → tag:twd-mgmt?)", *mgmtAddr, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
		cancel()
	}
	hcfg := hostd.Config{HostID: *hostID, Mgmt: mc, DataDir: *dataDir, RunDir: *runDir, WorkerdBin: wd,
		HeartbeatEvery: *hb, FenceAfter: *fenceAfter, Ephemeral: *ephemeral, HostnamePrefix: *prefix, ControlURL: *controlURL,
		CapacityNodes: *capacity, KillTombstone: *tombstone, Logger: log, TSNetLogs: *tsnetLogs,
		Sandbox: *sandboxF, SandboxDeny: splitList(*sbDeny), InsecureSharedHost: *insecureSH, AllowSiblingEgress: *siblings,
		DefaultTier: *tier, NodeBin: *nodeBin, VMVolumeMiB: *vmVolMiB, VMMemMiB: *vmMem, VMVCPUs: *vmCPUs,
		VM: vmBackend(vmOptions{kernel: *vmKernel, rootfs: *vmRootfs, firecracker: *vmFC, jailer: *vmJailer, helper: *vmHelper,
			memMiB: *vmMem, vcpus: *vmCPUs, hugePages: *vmHuge, containerKernel: *ctrKernel, vzVMM: *vzVMM, rosetta: *vmRosetta}, log),
		ContainerIdle: *ctrIdle, ContainerMaxLife: *ctrMaxLife, ContainerGrace: *ctrGrace,
		ContainerCapacity: hostd.ContainerCapacity{Max: *ctrMax, MemMiB: *ctrMemMiB, VCPU: *ctrVCPU}}
	// Containers (ADR-032) run on the microVM backend; without it this host
	// offers none (start() fails with tier_unavailable, no fallback).
	if cb, ok := hcfg.VM.(sandbox.ContainerBackend); ok && hcfg.VM != nil && !*ctrOff {
		st := &image.Store{Dir: *ctrCache, Aliases: image.DefaultAliases, Public: map[string]bool{}}
		if *ctrCache == "" {
			st.Dir = filepath.Join(*dataDir, "images")
		}
		if *ctrImages != "" {
			st.Sources = append(st.Sources, image.DirSource{Dir: *ctrImages})
		}
		for _, r := range splitList(*ctrPublic) {
			st.Public[image.NormalizeRef(r)] = true
		}
		// Offer containers only when they can actually run here (the host
		// advertises the "containers" tier from this; ADR-032 D5).
		if err := containersUsable(cb, st); err != nil {
			log.Info("containers unavailable on this host; not offered", "reason", err)
		} else {
			hcfg.Containers, hcfg.Images = cb, st
			log.Info("containers available", "image_cache", st.Dir, "image_dir", *ctrImages, "idle", *ctrIdle, "max_life", *ctrMaxLife)
		}
	}
	if pushLn != nil {
		hcfg.PushListener, hcfg.PushPort = pushLn, *pushPort
	}
	h, err := hostd.New(hcfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = h.Start(ctx)
	cancel()
	if err != nil {
		return err
	}
	transport := "tsnet"
	if *localDev {
		transport = "local-dev"
	}
	log.Info("twd-hostd running", "host", *hostID, "mgmt", *mgmtAddr, "transport", transport, "push", h.PushSocket(), "workerd", wd, "sandbox", h.SandboxInfo())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("shutting down (node state kept for rejoin)")
	return h.Close()
}

// joinHostNode opens (or joins) this host's own tailnet node. A node with
// saved state re-opens without a key. Otherwise it needs the single-use
// join key: from joinFile, which is waited for (twd up writes it after
// mgmt minted it) and deleted once the node is up. The key never reaches
// the log.
func joinHostNode(ctx context.Context, dir, hostname, control, joinFile string, log *slog.Logger) (*tsnet.Server, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ts := &tsnet.Server{Hostname: hostname, Dir: dir, ControlURL: control, AdvertiseTags: []string{"tag:twd-host"},
		Logf: func(string, ...any) {}, UserLogf: func(string, ...any) {}}
	hasState := false
	if _, err := os.Stat(filepath.Join(dir, "tailscaled.state")); err == nil {
		hasState = true
	}
	if !hasState {
		if joinFile == "" {
			return nil, errors.New("this host's node has no state yet: run `twd-hostd join --token-file <file>` (twd host add mints the key) or pass -join-token-file")
		}
		log.Info("waiting for the join key", "file", joinFile, "node", hostname)
		for {
			if k, err := tsapi.LoadEnvFile(joinFile, "TS_AUTHKEY"); err == nil && k != "" {
				ts.AuthKey = k
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	uctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	st, err := ts.Up(uctx)
	if err != nil {
		ts.Close()
		return nil, fmt.Errorf("join the operator tailnet as %s: %w", hostname, err)
	}
	if !hasState && joinFile != "" {
		os.Remove(joinFile) // single use
	}
	tags := []string{}
	if st.Self != nil && st.Self.Tags != nil {
		tags = st.Self.Tags.AsSlice()
	}
	log.Info("tailnet node", "name", strings.TrimSuffix(st.Self.DNSName, "."), "tags", tags, "ips", st.TailscaleIPs)
	return ts, nil
}

// runJoin is `twd-hostd join --token-file <file> [--token <key>] --control-url … --tsnet-dir …`:
// join the operator tailnet once with the single-use key `twd host add`
// minted, save the node state and exit. The service then starts without a key.
func runJoin(args []string) error {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("twd-hostd join", flag.ContinueOnError)
	hostID := fs.String("host-id", "", "this host's ID (default: hostname)")
	dataDir := fs.String("data-dir", filepath.Join(home, ".twd", "hostd"), "hostd state (the node state goes to <data-dir>/tsnet)")
	tsDir := fs.String("tsnet-dir", "", "node state dir (default <data-dir>/tsnet)")
	control := fs.String("control-url", "https://controlplane.tailscale.com", "operator tailnet control URL")
	tokenFile := fs.String("token-file", "", "TS_AUTHKEY=… file with the join key (twd host add --json > …)")
	token := fs.String("token", "", "the join key itself (prefer --token-file; argv is visible to other users)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *hostID == "" {
		h, _ := os.Hostname()
		*hostID = h
	}
	if *tsDir == "" {
		*tsDir = filepath.Join(*dataDir, "tsnet")
	}
	key := *token
	if *tokenFile != "" {
		k, err := tsapi.LoadEnvFile(*tokenFile, "TS_AUTHKEY")
		if err != nil {
			return err
		}
		key = k
	}
	if key == "" {
		return errors.New("twd-hostd join needs --token-file or --token")
	}
	if err := os.MkdirAll(*tsDir, 0o700); err != nil {
		return err
	}
	ts := &tsnet.Server{Hostname: hostNodeName(*hostID), Dir: *tsDir, ControlURL: *control, AdvertiseTags: []string{"tag:twd-host"}, AuthKey: key,
		Logf: func(string, ...any) {}, UserLogf: func(string, ...any) {}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	st, err := ts.Up(ctx)
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}
	ts.Close()
	if *tokenFile != "" {
		os.Remove(*tokenFile)
	}
	fmt.Printf("joined as %s (%s); node state in %s. Start twd-hostd without a key now.\n", strings.TrimSuffix(st.Self.DNSName, "."), st.TailscaleIPs, *tsDir)
	return nil
}

// vmAsset finds a microVM asset: next to the installed release
// (<prefix>/share/twd/vm, e.g. ~/.twd), else ~/.twd/vm.
func vmAsset(home, name string) string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(filepath.Dir(exe)), "share", "twd", "vm", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(home, ".twd", "vm", name)
}

func findWorkerd(flagVal, home string) (string, error) {
	for _, c := range []string{flagVal, os.Getenv("TWD_WORKERD"), filepath.Join(home, ".twd", "bin", "workerd")} {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	if p, err := exec.LookPath("workerd"); err == nil {
		return p, nil
	}
	return "", errors.New("workerd not found (use -workerd or $TWD_WORKERD)")
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// containersUsable: the container backend can boot guests here (helper,
// kernel with erofs) and the image store can convert images (mkfs.erofs).
func containersUsable(cb sandbox.ContainerBackend, st *image.Store) error {
	if u, ok := cb.(interface{ ContainersUsable() error }); ok {
		if err := u.ContainersUsable(); err != nil {
			return err
		}
	}
	return st.Check()
}
