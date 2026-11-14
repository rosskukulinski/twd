package vm

import (
	"bytes"
	"fmt"
	"os"
)

// KernelHasErofs reports whether a guest kernel image (an uncompressed
// vmlinux or arm64 Image) has erofs built in, which container guests mount
// their image with (ADR-032 D3). A built-in filesystem registers its name,
// so the image holds the NUL-terminated string "erofs"; the Firecracker CI
// kernels have overlayfs but not erofs (guest/build-kernel.sh adds it).
func KernelHasErofs(kernel string) error {
	b, err := os.ReadFile(kernel)
	if err != nil {
		return err
	}
	if !bytes.Contains(b, []byte("\x00erofs\x00")) {
		return fmt.Errorf("guest kernel %s has no erofs (container images need it: build one with guest/build-kernel.sh and pass -vm-container-kernel)", kernel)
	}
	return nil
}
