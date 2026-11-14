//go:build darwin

package vm

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/hostd/sandbox"
	"github.com/rosskukulinski/twd/hostd/vm/helperapi"
)

// A DO volume on macOS (L3 on the vz backend): an AES-256 encrypted sparse
// disk image (hdiutil), attached without mounting; the guest gets the
// decrypted block device as its third virtio-blk disk. As with dm-crypt on
// Linux the key stays on the host (hostd hands it to hdiutil on a pipe; it
// is never on a command line, on disk, or in the guest), the image on disk
// is ciphertext, and the guest sees plaintext ext4.
//
// Like the Linux helper: at most one attach at a time (an exclusive flock
// on <image>.lock, which also holds across hostd processes), and never at
// an epoch older than the newest it has been attached at (<image>.fence),
// so a hostd whose namespace lease was taken over can't remount a volume
// its successor is using. Error codes are the helper's (helperapi).
type vzVolume struct {
	img      string
	dev      string   // /dev/diskN
	f        *os.File // /dev/rdiskN, read-write, for the VMM
	lock     *os.File
	format   bool // blank: the guest formats it
	attachMS float64
}

// volumeImage is the encrypted image for v (hdiutil insists on the suffix).
func volumeImage(v *sandbox.Volume) string {
	if strings.HasSuffix(v.Path, ".sparseimage") {
		return v.Path
	}
	return v.Path + ".sparseimage"
}

var diskRe = regexp.MustCompile(`^/dev/disk[0-9]+$`)

func attachVolume(v *sandbox.Volume) (vol *vzVolume, err error) {
	st := time.Now()
	if len(v.Key) != 32 {
		return nil, &helperapi.Err{Code: helperapi.CodeBadRequest, Msg: "volume key must be 32 bytes"}
	}
	img := volumeImage(v)
	if err := os.MkdirAll(filepath.Dir(img), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(img+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, &helperapi.Err{Code: helperapi.CodeVolumeBusy, Msg: fmt.Sprintf("volume %s is attached to a running VM", v.Name)}
	}
	vol = &vzVolume{img: img, lock: lock}
	defer func() {
		if err != nil {
			vol.release()
		}
	}()
	fence := img + ".fence"
	var rec volRecord
	if b, err := os.ReadFile(fence); err == nil {
		json.Unmarshal(b, &rec)
	}
	if v.Epoch < rec.Epoch {
		return vol, &helperapi.Err{Code: helperapi.CodeVolumeFenced, Msg: fmt.Sprintf("volume %s was attached at epoch %d; refusing epoch %d", v.Name, rec.Epoch, v.Epoch)}
	}
	pass := []byte(hex.EncodeToString(v.Key))
	defer clear(pass)
	if _, err := os.Stat(img); errors.Is(err, os.ErrNotExist) {
		size := v.SizeMiB
		if size == 0 {
			size = 1024
		}
		// No partition map and no filesystem: a blank device (the guest
		// makes ext4 on it).
		if out, err := hdiutil(pass, "create", "-quiet", "-size", fmt.Sprintf("%dm", size), "-type", "SPARSE", "-layout", "NONE",
			"-encryption", "AES-256", "-stdinpass", strings.TrimSuffix(img, ".sparseimage")); err != nil {
			return vol, fmt.Errorf("hdiutil create: %v: %s", err, out)
		}
	}
	out, err := hdiutil(pass, "attach", "-nomount", "-noverify", "-noautofsck", "-noautoopen", "-stdinpass", img)
	if err != nil {
		if strings.Contains(out, "Authentication error") {
			return vol, &helperapi.Err{Code: helperapi.CodeVolumeKey, Msg: fmt.Sprintf("the key does not open volume %s", v.Name)}
		}
		return vol, fmt.Errorf("hdiutil attach: %v: %s", err, out)
	}
	for _, f := range strings.Fields(out) {
		if diskRe.MatchString(f) {
			vol.dev = f
			break
		}
	}
	if vol.dev == "" {
		return vol, fmt.Errorf("hdiutil attach: no device in %q", out)
	}
	vol.f, err = os.OpenFile("/dev/r"+strings.TrimPrefix(vol.dev, "/dev/"), os.O_RDWR, 0)
	if err != nil {
		return vol, err
	}
	head := make([]byte, 4096)
	if _, err := vol.f.ReadAt(head, 0); err != nil {
		return vol, fmt.Errorf("read volume: %w", err)
	}
	switch {
	case head[1080] == 0x53 && head[1081] == 0xef: // ext4 superblock magic
	case bytes.Equal(head, make([]byte, len(head))):
		vol.format = true
	default:
		return vol, &helperapi.Err{Code: helperapi.CodeVolumeBad, Msg: fmt.Sprintf("volume %s is neither blank nor ext4", v.Name)}
	}
	// Opened with the right key: record the epoch before the VM can write.
	b, _ := json.Marshal(volRecord{Name: v.Name, Epoch: v.Epoch, VM: vol.dev})
	if err := writeFileSync(fence, b); err != nil {
		return vol, fmt.Errorf("persist volume fence: %w", err)
	}
	vol.attachMS = float64(time.Since(st).Microseconds()) / 1000
	return vol, nil
}

type volRecord struct {
	Name  string `json:"name"`
	Epoch int64  `json:"epoch"`
	VM    string `json:"lastDevice"`
}

// release detaches the image (after the VMM is gone) and drops the lock.
func (vol *vzVolume) release() error {
	var err error
	if vol.f != nil {
		vol.f.Close()
		vol.f = nil
	}
	if vol.dev != "" {
		for i := 0; ; i++ {
			args := []string{"detach", "-quiet", vol.dev}
			if i >= 10 {
				args = []string{"detach", "-quiet", "-force", vol.dev}
			}
			var out string
			if out, err = hdiutil(nil, args...); err == nil || i >= 12 {
				if err != nil {
					err = fmt.Errorf("hdiutil detach %s: %v: %s", vol.dev, err, out)
				}
				break
			}
			time.Sleep(100 * time.Millisecond) // the VM service may still hold it for a moment
		}
		vol.dev = ""
	}
	if vol.lock != nil {
		vol.lock.Close()
		vol.lock = nil
	}
	return err
}

// hdiutil runs hdiutil; stdin (the passphrase, if any) goes on a pipe.
func hdiutil(stdin []byte, args ...string) (string, error) {
	cmd := exec.Command("/usr/bin/hdiutil", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	// hdiutil prints a deprecation notice on every call; keep only the rest.
	var keep []string
	for _, ln := range strings.Split(string(out), "\n") {
		if !strings.Contains(ln, "is deprecated") && strings.TrimSpace(ln) != "" {
			keep = append(keep, ln)
		}
	}
	return strings.Join(keep, "\n"), err
}

func writeFileSync(p string, b []byte) error {
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, p)
}
