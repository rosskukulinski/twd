package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/vm/helperapi"
)

// doctorMicroVM (Linux): can this user run the microVM tier? KVM, the
// guest kernel and image from the release, and the twd-vmhelper service
// (installed by `sudo twd host setup --microvm`) answering this uid. If
// not, it's a warning, not a failure: microvm deployments fall back to the
// process tier.
func doctorMicroVM(s *state) (string, string, string) {
	const setup = "sudo ~/.twd/bin/twd host setup --microvm   (one time; then twd runs as you)"
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return "warn", "no /dev/kvm: microvm deployments fall back to the process tier", "run on a host with KVM (bare metal, or a VM with nested virtualization)"
	}
	share := filepath.Join(filepath.Dir(exeDir()), "share", "twd", "vm")
	for _, a := range []string{"vmlinux", "rootfs.ext4"} {
		if !isFile(filepath.Join(share, a)) {
			return "warn", "this release has no microVM guest (" + filepath.Join(share, a) + " missing): process tier only", "install a linux release built with the microVM assets"
		}
	}
	r, err := helperapi.Ping(helperapi.DefaultSocket)
	if err != nil {
		why := err.Error()
		if os.IsNotExist(err) || strings.Contains(why, "no such file") {
			why = "the twd-vmhelper service is not installed"
		} else if helperapi.IsCode(err, helperapi.CodeUnauthorized) {
			why = fmt.Sprintf("the twd-vmhelper service does not allow uid %d", os.Getuid())
		}
		return "warn", why + ": microvm deployments fall back to the process tier", setup
	}
	if r.Version != helperapi.Version {
		return "warn", fmt.Sprintf("twd-vmhelper speaks protocol %d, this twd %d", r.Version, helperapi.Version), setup
	}
	detail := fmt.Sprintf("KVM ok; guest kernel + image in %s; twd-vmhelper answers uid %d (jailer %s, %s)", share, r.UID, r.Jailer, r.Crypt)
	if s != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var hv hostd.StatusView
		if s.hostd().Do(ctx, "GET", "/v0/status", nil, &hv) == nil {
			n, vms := 0, 0
			for _, w := range hv.Workloads {
				if w.Tier == "microvm" {
					n++
					if w.VM != nil && w.VM.Jail != nil {
						vms++
					}
				}
			}
			if !strings.Contains(strings.Join(hv.Tiers, ","), "microvm") {
				return "warn", detail + "; but the running twd-hostd has no microVM tier (restart it: twd down && twd up …)", "twd logs --component hostd"
			}
			detail += fmt.Sprintf("; running: %d/%d microvm workloads jailed (%s)", vms, n, hv.VMVia)
		}
	}
	return "ok", detail, ""
}
