package vm

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// DefaultBootArgs is the guest kernel cmdline for a host arch (Go's name):
// serial console (ttyS0 on both: Firecracker emulates an 8250 UART on
// x86_64 and arm64), panic → reboot, which Firecracker turns into a VM exit
// (reboot=k: the i8042 reset on x86_64; arm64 reboots through PSCI and
// ignores it), twd-guestd as init, read-only root. x86_64 adds pci=off:
// Firecracker's devices are virtio-mmio, and the kernel would otherwise
// probe the PCI config ports at boot; arm64 finds no PCI host bridge in the
// device tree anyway.
func DefaultBootArgs(goarch string) string {
	const common = "console=ttyS0 reboot=k panic=1 quiet ro rootfstype=ext4 init=/sbin/twd-guestd"
	if goarch == "amd64" {
		return common + " pci=off"
	}
	return common
}

// KernelArch reports the CPU architecture (Go's name) of a guest kernel as
// Firecracker boots it: an ELF vmlinux (x86_64; arm64 ELF too) or an arm64
// Linux Image (the "ARM\x64" magic at offset 56 of its header).
func KernelArch(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var h [64]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return "", fmt.Errorf("guest kernel %s: %w", path, err)
	}
	switch {
	case bytes.Equal(h[:4], []byte("\x7fELF")):
		var m uint16
		if h[5] == 2 {
			m = binary.BigEndian.Uint16(h[18:])
		} else {
			m = binary.LittleEndian.Uint16(h[18:])
		}
		switch m {
		case 62:
			return "amd64", nil
		case 183:
			return "arm64", nil
		}
		return "", fmt.Errorf("guest kernel %s: ELF machine %d (want x86-64 or AArch64)", path, m)
	case bytes.Equal(h[56:60], []byte("ARM\x64")):
		return "arm64", nil
	}
	return "", fmt.Errorf("guest kernel %s: not an ELF vmlinux or an arm64 Image", path)
}

// ImageArch reports the arch recorded in the guest image's manifest.json
// (next to the rootfs; written by guest/build-image.sh). "" when there is
// no manifest or it predates the field (older arm64-only images).
func ImageArch(rootfs string) (string, error) {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(rootfs), "manifest.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var m struct {
		Arch string `json:"arch"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("guest image manifest: %w", err)
	}
	return m.Arch, nil
}

// CheckArch refuses a guest kernel or image built for another arch than
// goarch (the host's): KVM runs only native guests, and a mismatch would
// otherwise surface as a VM that never reports ready.
func CheckArch(kernel, rootfs, goarch string) error {
	ka, err := KernelArch(kernel)
	if err != nil {
		return err
	}
	if ka != goarch {
		return fmt.Errorf("guest kernel %s is %s, this host is %s (install the linux-%s release's microVM assets)", kernel, ka, goarch, goarch)
	}
	ia, err := ImageArch(rootfs)
	if err != nil {
		return err
	}
	if ia != "" && ia != goarch {
		return fmt.Errorf("guest image %s is %s, this host is %s (install the linux-%s release's microVM assets)", rootfs, ia, goarch, goarch)
	}
	return nil
}
