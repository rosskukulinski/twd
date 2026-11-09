package hostd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
)

// VMCheckReport is POST /v1/vm/check: a workload microVM's guest stats,
// DO volume latency and (optionally) the no-keys-in-guest scan. It never
// contains key material: only counts and needle *names*.
type VMCheckReport struct {
	Workload string       `json:"workload"`
	PID      int          `json:"pid"`
	RSSKB    int64        `json:"rssKB"`
	Stats    *proto.Stats `json:"stats,omitempty"`
	FSBench  *proto.Bench `json:"fsbench,omitempty"`
	KeyScan  *KeyScan     `json:"keyscan,omitempty"`
}

// KeyScan: where the scan looked and what it found (needle names only).
type KeyScan struct {
	Needles     int      `json:"needles"`
	NeedleKinds []string `json:"needleKinds"`
	MemoryMiB   int64    `json:"memoryMiB"`
	MemdumpMS   int64    `json:"memdumpMs"`
	Disks       []string `json:"disks"`
	GuestFiles  int      `json:"guestFiles"`
	Found       []string `json:"found"`           // needle names found in the guest (must be empty)
	GuestStateF []string `json:"guestStateFiles"` // tailscale-looking file names in the guest (must be empty)
	Control     string   `json:"positiveControl"` // the instance MAC key is in guest RAM (the dump is real)
}

// VMCheck runs the checks on tenant/workload's running microVM ("" tenant:
// the only tenant that has that workload).
func (h *Host) VMCheck(ctx context.Context, tenantID, wlName string, keyscan bool) (*VMCheckReport, error) {
	h.mu.Lock()
	var w *workload
	for k, ww := range h.workloads {
		if ww.name == wlName && (tenantID == "" || k == tenantID+"/"+wlName) {
			w = ww
		}
	}
	h.mu.Unlock()
	if w == nil {
		return nil, fmt.Errorf("no workload %q", wlName)
	}
	inst := w.current()
	if inst == nil || inst.currentVM() == nil {
		return nil, fmt.Errorf("workload %q has no running microVM", wlName)
	}
	vm := inst.currentVM()
	rep := &VMCheckReport{Workload: wlName, PID: vm.Info().PID, RSSKB: vm.RSSKB()}
	if r, err := vm.Op(ctx, proto.Msg{Op: "stats"}); err == nil {
		rep.Stats = r.Stats
	}
	if r, err := vm.Op(ctx, proto.Msg{Op: "fsbench", N: 200}); err == nil {
		rep.FSBench = r.Bench
	}
	if !keyscan {
		return rep, nil
	}
	needles := h.keyNeedles(inst)
	ks := &KeyScan{Needles: len(needles), Disks: vm.Disks()}
	kinds := map[string]bool{}
	for n := range needles {
		kinds[strings.Fields(n)[0]] = true
	}
	for k := range kinds {
		ks.NeedleKinds = append(ks.NeedleKinds, k)
	}
	if len(needles) < 3 {
		return nil, errors.New("positive control failed: found no node keys on the host to look for")
	}
	// Disks the guest sees: rootfs, config drive (the volume image is
	// ciphertext on the host; its plaintext is in guest RAM/fs, scanned below).
	for i, d := range ks.Disks {
		if i >= 2 {
			break
		}
		ks.Found = append(ks.Found, scanFile(d, needles)...)
	}
	r, err := vm.Op(ctx, proto.Msg{Op: "fsscan"})
	if err != nil {
		return nil, err
	}
	ks.GuestFiles, ks.GuestStateF = r.Scanned, r.Found
	dir, err := os.MkdirTemp(h.cfg.DataDir, "memscan-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	st := time.Now()
	mem, err := vm.MemoryDump(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("memory dump: %w", err)
	}
	ks.MemdumpMS = time.Since(st).Milliseconds()
	if fi, err := os.Stat(mem); err == nil {
		ks.MemoryMiB = fi.Size() >> 20
	}
	ks.Found = append(ks.Found, scanFile(mem, needles)...)
	if mac := macKeyOf(inst.dir); mac != "" && len(scanFile(mem, map[string][]byte{"mac": []byte(mac)})) > 0 {
		ks.Control = "instance MAC key found in guest RAM (the dump is real)"
	} else {
		ks.Control = "FAILED: the instance MAC key is not in the dump"
	}
	os.Remove(mem)
	if ks.Found == nil {
		ks.Found = []string{}
	}
	rep.KeyScan = ks
	return rep, nil
}

// keyNeedles: what must never be in a guest: "tskey-", every node and
// machine private key in the host's tsnet state (this host's data dir), and
// the workload's DO volume key, each as text, hex and raw bytes.
func (h *Host) keyNeedles(inst *instance) map[string][]byte {
	n := map[string][]byte{"tskey- prefix": []byte("tskey-")}
	re := regexp.MustCompile(`privkey:[0-9a-f]{64}`)
	var add func(b []byte, depth int)
	add = func(b []byte, depth int) {
		for _, k := range re.FindAll(b, -1) {
			raw, _ := hex.DecodeString(string(k[8:]))
			if bytes.Equal(raw, make([]byte, len(raw))) {
				continue
			}
			n["nodekey-text "+string(k[8:16])] = append([]byte(nil), k...)
			n["nodekey-raw "+string(k[8:16])] = raw
		}
		if depth > 2 {
			return
		}
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			for _, v := range m {
				if s, ok := v.(string); ok {
					if d, err := base64.StdEncoding.DecodeString(s); err == nil {
						add(d, depth+1)
					}
				}
			}
		}
	}
	filepath.WalkDir(h.cfg.DataDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && strings.HasSuffix(p, ".state") {
			if b, err := os.ReadFile(p); err == nil {
				add(b, 0)
			}
		}
		return nil
	})
	if v := inst.spec.Volume; v != nil && len(v.Key) > 0 {
		n["volumekey raw"] = v.Key
		n["volumekey hex"] = []byte(hex.EncodeToString(v.Key))
		n["volumekey base64"] = []byte(base64.StdEncoding.EncodeToString(v.Key))
	}
	return n
}

func scanFile(path string, needles map[string][]byte) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const chunk, overlap = 16 << 20, 128
	buf := make([]byte, chunk+overlap)
	keep := 0
	hit := map[string]bool{}
	for {
		n, err := io.ReadFull(f, buf[keep:])
		data := buf[:keep+n]
		for name, nd := range needles {
			if len(nd) > 0 && !hit[name] && bytes.Contains(data, nd) {
				hit[name] = true
			}
		}
		if err != nil || len(data) < overlap {
			break
		}
		keep = copy(buf, data[len(data)-overlap:])
	}
	var out []string
	for k := range hit {
		out = append(out, k)
	}
	return out
}

func macKeyOf(instDir string) string {
	b, err := os.ReadFile(filepath.Join(instDir, "config.capnp"))
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`name = "MAC_KEY", text = "([0-9a-f]+)"`).FindSubmatch(b)
	if m == nil {
		return ""
	}
	return string(m[1])
}
