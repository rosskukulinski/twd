package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/hostd"
)

// cmdHost: `twd host setup --microvm` (one time, with sudo, Linux) installs
// the microVM tier's privileged helper; `twd host setup --undo` removes it.
func cmdHost(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fail("bad_request", "usage: twd "+usageOf("host"))
	}
	switch args[0] {
	case "setup":
		return cmdHostSetup(args[1:])
	case "vmcheck":
		return cmdHostVMCheck(args[1:])
	}
	return fail("bad_request", fmt.Sprintf("unknown host subcommand %q", args[0]), "usage: twd "+usageOf("host"))
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
	if err := s.hostd().Do(ctx, "POST", "/v0/vm/check?"+q.Encode(), nil, &rep); err != nil {
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
