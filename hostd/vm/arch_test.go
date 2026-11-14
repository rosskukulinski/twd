package vm

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeELF is the first 64 bytes of a little-endian ELF64 for machine m.
func fakeELF(m uint16) []byte {
	b := make([]byte, 64)
	copy(b, "\x7fELF")
	b[4], b[5] = 2, 1
	binary.LittleEndian.PutUint16(b[18:], m)
	return b
}

// fakeImage is the first 64 bytes of an arm64 Linux Image (MZ stub, magic at 56).
func fakeImage() []byte {
	b := make([]byte, 64)
	copy(b, "MZ")
	copy(b[56:], "ARM\x64")
	return b
}

func write(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKernelArch(t *testing.T) {
	d := t.TempDir()
	for _, c := range []struct {
		name string
		b    []byte
		want string
	}{
		{"x86-vmlinux", fakeELF(62), "amd64"},
		{"arm64-elf", fakeELF(183), "arm64"},
		{"arm64-image", fakeImage(), "arm64"},
	} {
		got, err := KernelArch(write(t, d, c.name, c.b))
		if err != nil || got != c.want {
			t.Errorf("%s: KernelArch = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	for _, c := range []struct {
		name string
		b    []byte
	}{
		{"riscv-elf", fakeELF(243)},
		{"bzImage-ish", append([]byte("MZ"), make([]byte, 100)...)},
		{"short", []byte("\x7fELF")},
	} {
		if got, err := KernelArch(write(t, d, c.name, c.b)); err == nil {
			t.Errorf("%s: KernelArch = %q, want an error", c.name, got)
		}
	}
}

// A host must refuse to boot another arch's guest kernel or image (it can't
// run under KVM; without the check the VM would just never become ready),
// and accept its own, including images whose manifest predates "arch".
func TestCheckArchRefusesForeignGuest(t *testing.T) {
	for _, host := range []string{"amd64", "arm64"} {
		other := map[string]string{"amd64": "arm64", "arm64": "amd64"}[host]
		kern := map[string][]byte{"amd64": fakeELF(62), "arm64": fakeImage()}
		d := t.TempDir()
		own := write(t, d, "own-vmlinux", kern[host])
		foreign := write(t, d, "foreign-vmlinux", kern[other])
		imgDir := func(arch string) string {
			dd := t.TempDir()
			rootfs := write(t, dd, "rootfs.ext4", nil)
			if arch != "-" {
				write(t, dd, "manifest.json", []byte(`{"rootfs":"rootfs.ext4","arch":"`+arch+`"}`))
			}
			return rootfs
		}
		if err := CheckArch(own, imgDir(host), host); err != nil {
			t.Errorf("%s host, own kernel + image: %v", host, err)
		}
		if err := CheckArch(own, imgDir("-"), host); err != nil {
			t.Errorf("%s host, own kernel, image without manifest: %v", host, err)
		}
		if err := CheckArch(own, imgDir(""), host); err != nil {
			t.Errorf("%s host, own kernel, manifest without arch: %v", host, err)
		}
		err := CheckArch(foreign, imgDir(host), host)
		if err == nil || !strings.Contains(err.Error(), "is "+other) {
			t.Errorf("%s host, %s kernel: err = %v, want a refusal naming %s", host, other, err, other)
		}
		err = CheckArch(own, imgDir(other), host)
		if err == nil || !strings.Contains(err.Error(), "is "+other) {
			t.Errorf("%s host, %s image: err = %v, want a refusal naming %s", host, other, err, other)
		}
	}
}

func TestDefaultBootArgs(t *testing.T) {
	for _, a := range []string{"amd64", "arm64"} {
		ba := DefaultBootArgs(a)
		for _, w := range []string{"console=ttyS0", "panic=1", "reboot=k", " ro ", "init=/sbin/twd-guestd"} {
			if !strings.Contains(ba, w) {
				t.Errorf("%s: %q lacks %q", a, ba, w)
			}
		}
	}
	if !strings.Contains(DefaultBootArgs("amd64"), "pci=off") || strings.Contains(DefaultBootArgs("arm64"), "pci=off") {
		t.Errorf("pci=off: amd64 %q, arm64 %q", DefaultBootArgs("amd64"), DefaultBootArgs("arm64"))
	}
}
