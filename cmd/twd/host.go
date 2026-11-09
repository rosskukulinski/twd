package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
)

// cmdHost: `twd host setup --microvm` (one time, with sudo, Linux) installs
// the microVM tier's privileged helper; `twd host setup --undo` removes it.
func cmdHost(args []string) error {
	// The subcommand is the first positional (global flags such as
	// --state-dir X may come first).
	sub := -1
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--state-dir" || a == "-state-dir" {
			i++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			sub = i
			break
		}
	}
	if sub < 0 {
		return fail("bad_request", "usage: twd "+usageOf("host"))
	}
	rest := append(append([]string{}, args[:sub]...), args[sub+1:]...)
	switch args[sub] {
	case "setup":
		return cmdHostSetup(rest)
	case "vmcheck":
		return cmdHostVMCheck(rest)
	case "add":
		return cmdHostAdd(rest)
	case "ls", "list":
		return cmdHostLs(rest)
	case "rm", "remove", "delete":
		return cmdHostRm(rest)
	}
	return fail("bad_request", fmt.Sprintf("unknown host subcommand %q", args[sub]), "usage: twd "+usageOf("host"))
}

// setupResult is `twd host setup --json`.
type setupResult struct {
	Action    string   `json:"action"` // setup | undo
	User      string   `json:"user,omitempty"`
	UID       int      `json:"uid,omitempty"`
	Steps     []string `json:"steps"`
	Socket    string   `json:"socket,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	HugePages int      `json:"hugePages,omitempty"`
}

func cmdHostSetup(args []string) error {
	f := newFlags("host")
	microvm := f.Bool("microvm", false, "install the microVM tier's privileged helper (twd-vmhelper) for --user")
	undo := f.Bool("undo", false, "remove everything `twd host setup --microvm` installed")
	userF := f.String("user", "", "the user twd runs as (default: $SUDO_USER)")
	huge := f.Int("hugepages", -1, "2 MiB huge pages to reserve for guest RAM (default: enough for 4 VMs, at most a quarter of RAM; 0 = none)")
	fc := f.String("firecracker", "", "firecracker binary to install (default: the release's share/twd/vm/firecracker)")
	jl := f.String("jailer", "", "jailer binary to install (default: the release's share/twd/vm/jailer)")
	if _, err := f.parse(args); err != nil {
		return err
	}
	if *microvm == *undo {
		return fail("bad_request", "pass exactly one of --microvm or --undo", "usage: twd "+usageOf("host"))
	}
	var res *setupResult
	var err error
	if *undo {
		res, err = hostUndo()
	} else {
		res, err = hostSetupMicroVM(*userF, *huge, *fc, *jl)
	}
	if err != nil {
		return err
	}
	emit(f, res, func() {
		for _, s := range res.Steps {
			fmt.Println(s)
		}
		if res.Action == "setup" {
			fmt.Printf("\nmicroVM tier ready for %s. As %s (no sudo): twd up …, then deploy with \"tier\": \"microvm\" in twd.jsonc.\n", res.User, res.User)
			fmt.Println("Check it with `twd doctor`. Remove it with `sudo twd host setup --undo`.")
		}
	})
	return nil
}

// cmdHostVMCheck: `twd host vmcheck <workload> [--keyscan]` checks a
// running microVM workload: guest memory, DO volume fsync latency, and
// with --keyscan that no tailnet or volume key is in the guest (its disks,
// files, and a full snapshot of its RAM). Output has no key material.
func cmdHostVMCheck(args []string) error {
	f := newFlags("host")
	keyscan := f.Bool("keyscan", false, "also scan the guest's disks, files and RAM for key material")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fail("bad_request", "usage: twd host vmcheck <workload> [--keyscan]")
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	q := url.Values{"workload": {pos[0]}}
	if *keyscan {
		q.Set("keyscan", "1")
	}
	var rep hostd.VMCheckReport
	if err := s.hostd().Do(ctx, "POST", "/v1/vm/check?"+q.Encode(), nil, &rep); err != nil {
		return err
	}
	emit(f, rep, func() {
		fmt.Printf("✓ microVM %s · VMM pid %d · RSS %d MiB\n", rep.Workload, rep.PID, rep.RSSKB/1024)
		if st := rep.Stats; st != nil {
			fmt.Printf("  guest: %d/%d MiB used, workerd RSS %d MiB · DO storage %s (%d KiB used)\n",
				(st.MemTotalKB-st.MemAvailableKB)/1024, st.MemTotalKB/1024, st.WorkerdRSSKB/1024, st.DOFS, st.DOUsedKB)
		}
		if b := rep.FSBench; b != nil {
			fmt.Printf("  DO volume 4 KiB write+fdatasync: write p50 %d µs, fsync p50 %d µs / p95 %d µs (n=%d)\n", b.WriteP50US, b.SyncP50US, b.SyncP95US, b.N)
		}
		if k := rep.KeyScan; k != nil {
			mark := "✓"
			if len(k.Found) > 0 || len(k.GuestStateF) > 0 {
				mark = "✗"
			}
			fmt.Printf("%s no keys in guest: %d needles (%s) · found %v · %d guest files · %d MiB RAM · %s\n",
				mark, k.Needles, strings.Join(k.NeedleKinds, ", "), k.Found, k.GuestFiles, k.MemoryMiB, k.Control)
		}
	})
	if rep.KeyScan != nil && (len(rep.KeyScan.Found) > 0 || len(rep.KeyScan.GuestStateF) > 0) {
		return fail("keys_in_guest", fmt.Sprintf("key material in the guest: %v %v", rep.KeyScan.Found, rep.KeyScan.GuestStateF))
	}
	return nil
}

// cmdHostAdd is `twd host add <host-id> [--tenant …] [--out <file>]`:
// mgmt records the host (status pending) and mints a single-use,
// 10-minute tag:twd-host join key through the tenant's credential (WIF or
// API key). The key goes to --out as a TS_AUTHKEY=… file (0600; default
// <host-id>.join.env in the current directory), never to the terminal or
// the log. On the new machine: `twd-hostd join --token-file <file>`, then
// start twd-hostd (or twd up on that machine).
func cmdHostAdd(args []string) error {
	f := newFlags("host add")
	tenantF := tenantFlag(f)
	out := f.String("out", "", "write the join key here as TS_AUTHKEY=… (default ./<host-id>.join.env)")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fail("bad_request", "twd host add needs one host ID", "usage: twd "+usageOf("host"))
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	if err := s.requireUp(); err != nil {
		return err
	}
	tn, err := s.tenant(*tenantF)
	if err != nil {
		return err
	}
	path := *out
	if path == "" {
		path = pos[0] + ".join.env"
	}
	path, _ = filepath.Abs(path)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var hj hostJoin
	if err := s.mgmt().DoHeaders(ctx, "POST", "/v1/hosts", map[string]any{"hostID": pos[0], "tenantID": tn.TenantID}, &hj,
		map[string]string{"Twd-Actor": actor()}); err != nil {
		if me, ok := mgmtapi.AsError(err); ok && me.Code == "exists" {
			return fail("exists", me.Message).withFix("twd host rm " + pos[0] + " --drain, then twd host add again")
		}
		return err
	}
	if err := os.WriteFile(path, []byte("TS_AUTHKEY="+hj.AuthKey+"\n"), 0o600); err != nil {
		return err
	}
	res := map[string]any{"hostID": pos[0], "hostname": hj.Hostname, "tags": hj.Tags, "expiresAt": hj.ExpiresAt, "controlURL": hj.ControlURL,
		"tailnet": hj.Tailnet, "joinFile": path, "mgmt": "http://" + s.Cfg.MgmtDNSName}
	emit(f, res, func() {
		fmt.Printf("✓ host     %s added (pending) · join key for %s (%s) written to %s (single use, expires %s)\n", pos[0], hj.Hostname,
			strings.Join(hj.Tags, ","), path, time.UnixMilli(hj.ExpiresAt).Local().Format("15:04:05"))
		fmt.Printf("  on %s:  install twd, copy the file there (scp, 0600), then\n", pos[0])
		fmt.Printf("    twd-hostd join --host-id %s --token-file %s --control-url %s\n", pos[0], filepath.Base(path), hj.ControlURL)
		fmt.Printf("    twd-hostd -host-id %s -mgmt http://%s -tsnet-control-url %s\n", pos[0], s.Cfg.MgmtDNSName, hj.ControlURL)
		fmt.Printf("  the key is used once by the join and the file is deleted; mgmt then binds the node to host %s on its first PUT /v1/hosts/%s\n", pos[0], pos[0])
	})
	return nil
}

// cmdHostLs is `twd host ls` (GET /v1/hosts, every page).
func cmdHostLs(args []string) error {
	f := newFlags("host ls")
	if _, err := f.parse(args); err != nil {
		return err
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	if err := s.requireUp(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type hostRow struct {
		HostID    string   `json:"hostID"`
		Status    string   `json:"status"`
		Tiers     []string `json:"tiers"`
		Pool      string   `json:"pool"`
		NodeID    string   `json:"nodeID"`
		DNSName   string   `json:"dnsName"`
		LiveNodes int      `json:"liveNodes"`
		LastSeen  int64    `json:"lastSeen"`
		Leases    []struct {
			Namespace string `json:"namespace"`
			Epoch     int64  `json:"epoch"`
		} `json:"leases"`
	}
	hosts, err := listAll[hostRow](ctx, s.mgmt(), "/v1/hosts", 0)
	if err != nil {
		return err
	}
	emit(f, hosts, func() {
		fmt.Printf("  %-16s %-8s %-18s %-36s %-6s %-10s %s\n", "HOST", "STATUS", "TIERS", "NODE", "LIVE", "SEEN", "LEASES")
		for _, h := range hosts {
			seen := "never"
			if h.LastSeen > 0 {
				seen = time.Since(time.UnixMilli(h.LastSeen)).Round(time.Second).String() + " ago"
			}
			var ls []string
			for _, l := range h.Leases {
				ls = append(ls, fmt.Sprintf("%s@%d", l.Namespace, l.Epoch))
			}
			fmt.Printf("  %-16s %-8s %-18s %-36s %-6d %-10s %s\n", h.HostID, h.Status, strings.Join(h.Tiers, ","), h.DNSName, h.LiveNodes, seen, strings.Join(ls, " "))
		}
	})
	return nil
}

// cmdHostRm is `twd host rm <host-id> [--drain]` (DELETE /v1/hosts/{id}).
func cmdHostRm(args []string) error {
	f := newFlags("host rm")
	drain := f.Bool("drain", false, "kill the host's objects and release its leases first")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fail("bad_request", "twd host rm needs one host ID", "usage: twd "+usageOf("host"))
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	if err := s.requireUp(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	q := ""
	if *drain {
		q = "?drain=true"
	}
	var res struct {
		HostID         string   `json:"hostID"`
		Killed         []string `json:"killed"`
		LeasesReleased []string `json:"leasesReleased"`
		DevicesLeft    []string `json:"devicesLeft"`
	}
	if err := s.mgmt().DoHeaders(ctx, "DELETE", "/v1/hosts/"+pos[0]+q, nil, &res, map[string]string{"Twd-Actor": actor()}); err != nil {
		if me, ok := mgmtapi.AsError(err); ok && me.Code == "host_holds_leases" {
			return fail("host_holds_leases", me.Message).withFix("twd host rm " + pos[0] + " --drain")
		}
		return err
	}
	res.HostID = pos[0]
	emit(f, res, func() {
		fmt.Printf("✓ host     %s removed · %d objects killed · %d leases released\n", pos[0], len(res.Killed), len(res.LeasesReleased))
		if len(res.DevicesLeft) > 0 {
			fmt.Printf("› devices  delete the host's own node in the admin console: %s\n", strings.Join(res.DevicesLeft, ", "))
		}
	})
	return nil
}
