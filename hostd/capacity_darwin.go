package hostd

import "golang.org/x/sys/unix"

// osMemInfo returns the Mac's RAM in MiB (hw.memsize); macOS has no huge
// page pool (vz guests use ordinary memory).
func osMemInfo() (totalMiB, hugeMiB int) {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, 0
	}
	return int(n >> 20), 0
}
