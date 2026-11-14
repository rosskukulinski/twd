//go:build !linux && !darwin

package hostd

func osMemInfo() (totalMiB, hugeMiB int) { return 0, 0 }
