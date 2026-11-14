//go:build !linux

package main

import "runtime"

func hostSetupMicroVM(string, int, string, string) (*setupResult, error) {
	if runtime.GOOS == "darwin" {
		return nil, fail("unsupported", "`twd host setup --microvm` is for Linux hosts",
			"on macOS the microVM tier runs on Virtualization.framework (bin/twd-vz, signed with the virtualization entitlement, plus share/twd/vm) and needs no setup; without them microvm deployments are refused (tier_unavailable), never run in the process tier")
	}
	return nil, fail("unsupported", "the microVM tier needs Linux with KVM or macOS",
		"this OS offers the process tier only; microvm deployments are refused (tier_unavailable), never downgraded; nothing to set up")
}

func hostUndo() (*setupResult, error) {
	return &setupResult{Action: "undo", Steps: []string{"✓ nothing to undo (`twd host setup --microvm` is Linux only)"}}, nil
}
