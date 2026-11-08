//go:build linux

package helper

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/rosskukulinski/twd/hostd/vm/helperapi"
	"golang.org/x/sys/unix"
)

// volumes tracks attached volumes and the epoch fence.
//
// A volume is attached to at most one VM at a time: by (device, inode) of
// its image in memory, and by an exclusive flock on the image held while
// it is attached (so a second helper can't attach it either). And it is
// never attached at an epoch older than the newest it has been attached
// at (volumes.json, keyed by uid and LUKS UUID): a hostd whose namespace
// lease was taken over can't remount a volume its successor is using.
type volumes struct {
	mu     sync.Mutex
	path   string
	busy   map[[2]uint64]string
	epochs map[string]volRecord
}

type volRecord struct {
	Name  string `json:"name"`
	Epoch int64  `json:"epoch"`
	VM    string `json:"lastVM"`
}

type attached struct {
	key       [2]uint64
	f         *os.File
	loop      string
	mapper    string
	name      string
	formatted bool
}

func newVolumes(path string) *volumes {
	v := &volumes{path: path, busy: map[[2]uint64]string{}, epochs: map[string]volRecord{}}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &v.epochs)
	}
	return v
}

var luksMagic = []byte{'L', 'U', 'K', 'S', 0xba, 0xbe}

// attach opens the LUKS2 volume in f (formatting it first if asked and it
// is blank) as /dev/mapper/<dmName> and returns it.
func (v *volumes) attach(f *os.File, req *helperapi.Volume, uid int, dmName string) (a *attached, err error) {
	if len(req.Key) != 32 {
		return nil, herr(helperapi.CodeBadRequest, "volume key must be 32 bytes")
	}
	if req.Name == "" || strings.ContainsAny(req.Name, "\n\x00") || len(req.Name) > 256 {
		return nil, herr(helperapi.CodeBadRequest, "bad volume name")
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, herr(helperapi.CodeVolumeBad, "volume image is not a regular file")
	}
	if int(st.Uid) != uid && uid != 0 {
		return nil, herr(helperapi.CodeVolumeBad, "volume image is owned by uid %d, not the caller (%d)", st.Uid, uid)
	}
	if st.Size < 64<<20 {
		return nil, herr(helperapi.CodeVolumeBad, "volume image is smaller than 64 MiB")
	}
	key := [2]uint64{st.Dev, st.Ino}
	v.mu.Lock()
	if other, ok := v.busy[key]; ok {
		v.mu.Unlock()
		return nil, herr(helperapi.CodeVolumeBusy, "volume %s is attached to running VM %s", req.Name, other)
	}
	v.busy[key] = dmName
	v.mu.Unlock()
	// Our own dup: the request's fds are closed when start returns.
	nfd, err := unix.Dup(int(f.Fd()))
	if err != nil {
		v.unbusy(key)
		return nil, err
	}
	a = &attached{key: key, f: os.NewFile(uintptr(nfd), "volume"), mapper: "/dev/mapper/" + dmName, name: req.Name}
	defer func() {
		if err != nil {
			v.release(a)
		}
	}()
	if err := unix.Flock(int(a.f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return a, herr(helperapi.CodeVolumeBusy, "volume %s is locked by another attach: %v", req.Name, err)
	}
	head := make([]byte, 4096)
	n, _ := a.f.ReadAt(head, 0)
	isLuks := n >= 6 && string(head[:6]) == string(luksMagic)
	blank := !isLuks
	for _, b := range head[:n] {
		if b != 0 {
			blank = false
			break
		}
	}
	switch {
	case isLuks:
	case req.Format && blank:
	case req.Format:
		return a, herr(helperapi.CodeVolumeBad, "refusing to format volume %s: the image is not blank", req.Name)
	default:
		return a, herr(helperapi.CodeVolumeBad, "volume %s is not a LUKS2 volume", req.Name)
	}
	// Loop device over the caller's open file (never its path).
	cmd := exec.Command("losetup", "--find", "--show", "/dev/fd/3")
	cmd.ExtraFiles = []*os.File{a.f}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return a, fmt.Errorf("losetup: %v: %s", err, strings.TrimSpace(string(out)))
	}
	a.loop = strings.TrimSpace(string(out))
	if !isLuks {
		// The key is a random 256-bit DEK, so key stretching buys nothing:
		// PBKDF2 at the minimum iteration count keeps open fast.
		if _, err := runStdin(req.Key, "cryptsetup", "luksFormat", "--batch-mode", "--type", "luks2",
			"--cipher", "aes-xts-plain64", "--key-size", "512", "--hash", "sha256", "--sector-size", "4096",
			"--pbkdf", "pbkdf2", "--pbkdf-force-iterations", "1000", "--key-file", "-", a.loop); err != nil {
			return a, err
		}
		a.formatted = true
	}
	uuidOut, err := runStdin(nil, "cryptsetup", "luksUUID", a.loop)
	if err != nil {
		return a, err
	}
	fenceKey := fmt.Sprintf("%d/%s", uid, strings.TrimSpace(uuidOut))
	v.mu.Lock()
	rec, seen := v.epochs[fenceKey]
	if seen && req.Epoch < rec.Epoch {
		v.mu.Unlock()
		return a, herr(helperapi.CodeVolumeFenced, "volume %s was attached at epoch %d; refusing epoch %d", req.Name, rec.Epoch, req.Epoch)
	}
	v.mu.Unlock()
	if out, err := runStdin(req.Key, "cryptsetup", "open", "--type", "luks2", "--key-file", "-", a.loop, dmName); err != nil {
		if strings.Contains(out, "No key available") {
			return a, herr(helperapi.CodeVolumeKey, "the key does not open volume %s", req.Name)
		}
		return a, err
	}
	// Opened with the right key: record the epoch (persisted before the
	// VM can write anything).
	v.mu.Lock()
	rec = v.epochs[fenceKey]
	if req.Epoch < rec.Epoch {
		v.mu.Unlock()
		return a, herr(helperapi.CodeVolumeFenced, "volume %s was attached at epoch %d; refusing epoch %d", req.Name, rec.Epoch, req.Epoch)
	}
	v.epochs[fenceKey] = volRecord{Name: req.Name, Epoch: req.Epoch, VM: dmName}
	werr := writeJSONFile(v.path, v.epochs)
	v.mu.Unlock()
	if werr != nil {
		return a, fmt.Errorf("persist volume fence: %w", werr)
	}
	if a.formatted {
		if _, err := runStdin(nil, "mkfs.ext4", "-q", "-F", "-L", "twd-do", "-m", "0",
			"-E", "lazy_itable_init=0,lazy_journal_init=0,nodiscard", a.mapper); err != nil {
			return a, err
		}
	}
	return a, nil
}

func (v *volumes) unbusy(key [2]uint64) {
	v.mu.Lock()
	delete(v.busy, key)
	v.mu.Unlock()
}

// release closes the mapping and the loop device, then unlocks the image.
func (v *volumes) release(a *attached) error {
	var errs []error
	if _, err := os.Stat(a.mapper); err == nil {
		var err error
		for i := 0; i < 20; i++ { // udev may still hold it for a moment
			if _, err = runStdin(nil, "cryptsetup", "close", a.mapper); err == nil {
				break
			}
			unix.Nanosleep(&unix.Timespec{Nsec: 50e6}, nil)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if a.loop != "" {
		if _, err := runStdin(nil, "losetup", "-d", a.loop); err != nil {
			errs = append(errs, err)
		}
	}
	if a.f != nil {
		a.f.Close() // drops the flock
	}
	v.unbusy(a.key)
	return errors.Join(errs...)
}
