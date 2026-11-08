// twd-hostd is the twd host agent (process tier): it registers with
// twd-mgmt, heartbeats, pulls assignments, and runs each deployment as one
// supervised workerd process with per-object tailnet nodes (netd library).
//
//	twd-hostd -mgmt unix:///Users/me/.twd/run/mgmt.sock -host-id h1 -data-dir ~/.twd/hostd
//
// The push API (mgmt → hostd) and status live on <run-dir>/hostd.sock:
//
//	POST /v0/push/kill {objectID, reason}   POST /v0/push/assignments   GET /v0/status
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/vm/helperapi"
	"github.com/rosskukulinski/twd/netd"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "twd-hostd:", netd.Redact(err.Error()))
		os.Exit(1)
	}
}

func run() error {
	home, _ := os.UserHomeDir()
	var (
		mgmtAddr   = flag.String("mgmt", "unix://"+filepath.Join(home, ".twd", "run", "mgmt.sock"), "twd-mgmt address (unix:///path or http(s)://)")
		hostID     = flag.String("host-id", "", "this host's ID (default: hostname)")
		dataDir    = flag.String("data-dir", filepath.Join(home, ".twd", "hostd"), "node state, DO storage, objects.json")
		runDir     = flag.String("run-dir", "", "sockets and generated configs (short path; default a new /tmp dir)")
		workerd    = flag.String("workerd", "", "workerd binary (default: $TWD_WORKERD, ~/.twd/bin/workerd, or PATH)")
		hb         = flag.Duration("heartbeat", 3*time.Second, "heartbeat / assignment poll period")
		ephemeral  = flag.Bool("ephemeral", false, "join nodes as ephemeral (dev/test)")
		prefix     = flag.String("hostname-prefix", "", "prefix for every node hostname")
		controlURL = flag.String("control-url", "", "override every tenant's control URL (testing)")
		capacity   = flag.Int("capacity-nodes", 1000, "advertised node capacity")
		tombstone  = flag.Duration("kill-tombstone", 10*time.Second, "keep killed nodes RST-ing new connections this long")
		logLevel   = flag.String("log-level", "info", "debug|info|warn|error")
		tsnetLogs  = flag.Bool("tsnet-logs", false, "forward tsnet logs at debug level")
		tier       = flag.String("default-tier", "process", "tier for deployments and tenants that name none: process | microvm")
		vmKernel   = flag.String("vm-kernel", vmAsset(home, "vmlinux"), "microVM tier: guest kernel (Linux only)")
		vmRootfs   = flag.String("vm-rootfs", vmAsset(home, "rootfs.ext4"), "microVM tier: guest image from guest/build-image.sh")
		vmHelper   = flag.String("vm-helper", helperapi.DefaultSocket, "microVM tier: twd-vmhelper socket (sudo twd host setup --microvm)")
		vmFC       = flag.String("firecracker", "/usr/local/bin/firecracker", "microVM tier, as root without the helper service: firecracker binary")
		vmJailer   = flag.String("jailer", "/usr/local/bin/jailer", "microVM tier, as root without the helper service: jailer binary")
		vmVolMiB   = flag.Int("vm-volume-mib", 1024, "microVM tier: size of new DO volumes (sparse)")
		vmMem      = flag.Int("vm-mem-mib", 512, "microVM tier: guest memory")
		vmCPUs     = flag.Int("vm-vcpus", 2, "microVM tier: guest vCPUs")
		vmHuge     = flag.String("vm-hugepages", "auto", "microVM tier: back guest RAM with 2 MiB pages: auto | on | off")
		sandboxF   = flag.String("sandbox", "auto", "process-tier workerd sandbox: auto (seatbelt on macOS) | seatbelt | none")
		sbDeny     = flag.String("sandbox-deny", "", "comma-separated extra paths no workerd may read, write or connect to (twd up: its state and run dirs)")
		nodeBin    = flag.String("node", "", "Node process tier (runtime \"node\"): node binary (default: $TWD_NODE, or node on PATH)")
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
	mc, err := mgmtapi.NewClient(*mgmtAddr)
	if err != nil {
		return err
	}
	h, err := hostd.New(hostd.Config{HostID: *hostID, Mgmt: mc, DataDir: *dataDir, RunDir: *runDir, WorkerdBin: wd,
		HeartbeatEvery: *hb, Ephemeral: *ephemeral, HostnamePrefix: *prefix, ControlURL: *controlURL,
		CapacityNodes: *capacity, KillTombstone: *tombstone, Logger: log, TSNetLogs: *tsnetLogs,
		Sandbox: *sandboxF, SandboxDeny: splitList(*sbDeny),
		DefaultTier: *tier, NodeBin: *nodeBin, VMVolumeMiB: *vmVolMiB,
		VM: vmBackend(vmOptions{kernel: *vmKernel, rootfs: *vmRootfs, firecracker: *vmFC, jailer: *vmJailer, helper: *vmHelper,
			memMiB: *vmMem, vcpus: *vmCPUs, hugePages: *vmHuge}, log)})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = h.Start(ctx)
	cancel()
	if err != nil {
		return err
	}
	log.Info("twd-hostd running", "host", *hostID, "mgmt", *mgmtAddr, "push", h.PushSocket(), "workerd", wd)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("shutting down (node state kept for rejoin)")
	return h.Close()
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
