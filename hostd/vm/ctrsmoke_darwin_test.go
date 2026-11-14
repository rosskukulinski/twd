//go:build darwin

package vm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/image"
	"github.com/rosskukulinski/twd/hostd/sandbox"
)

const guestSeesCPUCap = true

// ctrSmokeBackend is vz (TWD_VM_KERNEL, TWD_VM_ROOTFS, TWD_VZ_VMM; with
// TWD_VM_ROSETTA the guests run linux/amd64 images under Rosetta, so
// TWD_CTR_IMAGES must then hold the amd64 set).
func ctrSmokeBackend(t *testing.T) (sandbox.ContainerBackend, []string) {
	t.Helper()
	b := vzBackend(t, os.Getenv("TWD_VM_ROSETTA") != "")
	if err := b.ContainersUsable(); err != nil {
		t.Fatalf("vz containers: %v", err)
	}
	return b, b.EmulatedArches()
}

// checkCtrConfinement: vz has no jail; the fractional vCPU is cpu.max on
// the guest's container cgroup (checkCPUShare measures that it holds),
// and the VMM is one process of its own (the VM service holds guest RAM).
func checkCtrConfinement(t *testing.T, info sandbox.Info, cpuMax string) {
	t.Helper()
	if j := info.Jail; j == nil || !strings.HasPrefix(j.CPUMax, cpuMax) {
		t.Fatalf("cpu.max: %+v", j)
	}
	if info.PID <= 0 || info.PID == os.Getpid() || info.ServicePID <= 0 {
		t.Fatalf("VMM / VM service: %+v", info)
	}
}

// A container that can't start fails BootContainer with the reason, fast,
// and leaves no run dir (scratch disk) behind: a missing image, and an
// entrypoint the image doesn't have.
func TestVZContainerStartFailures(t *testing.T) {
	imgs := os.Getenv("TWD_CTR_IMAGES")
	if imgs == "" {
		t.Skip("TWD_CTR_IMAGES not set")
	}
	b := vzBackend(t, os.Getenv("TWD_VM_ROSETTA") != "")
	ctx := context.Background()
	spec := sandbox.ContainerSpec{ID: "nope", Image: filepath.Join(t.TempDir(), "missing.erofs"), VCPUs: 1, MemMiB: 256, ScratchMiB: 64, Argv: []string{"true"}}
	if _, err := b.BootContainer(ctx, spec); err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("missing image: %v", err)
	}
	st := &image.Store{Dir: filepath.Join(t.TempDir(), "cache"), Sources: []image.Source{image.DirSource{Dir: imgs}}, Emulated: b.EmulatedArches()}
	img, err := st.Get(ctx, "t", "twd.local/debian-trixie-python:1")
	if err != nil {
		t.Fatal(err)
	}
	spec.ID, spec.Image, spec.Argv = "badentry", img.Path, []string{"/no/such/entrypoint"}
	t0 := time.Now()
	if _, err := b.BootContainer(ctx, spec); err == nil || !strings.Contains(err.Error(), "entrypoint") {
		t.Fatalf("bad entrypoint: %v", err)
	} else {
		t.Logf("bad entrypoint refused in %v: %v", time.Since(t0), err)
	}
	if time.Since(t0) > 10*time.Second {
		t.Fatal("a failed start waited for the ready timeout")
	}
	if ents, _ := os.ReadDir(b.cfg.RunDir); len(ents) != 0 {
		t.Fatalf("run dirs left behind: %v", ents)
	}
}
