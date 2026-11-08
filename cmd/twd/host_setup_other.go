//go:build !linux

package main

func hostSetupMicroVM(string, int, string, string) (*setupResult, error) {
	return nil, fail("unsupported", "the microVM tier needs Linux with KVM",
		"on this OS every workload runs in the process tier; nothing to set up")
}

func hostUndo() (*setupResult, error) {
	return &setupResult{Action: "undo", Steps: []string{"✓ nothing to undo (the microVM tier is Linux only)"}}, nil
}
