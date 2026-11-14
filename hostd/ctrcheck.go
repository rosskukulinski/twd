package hostd

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/gen"
)

// ContainerCheckReport is `twd host vmcheck <workload> --container
// <Class>/<name> --keyscan`: the strict no-keys scan of one object's
// container guest (ADR-032 D5). Only needle names, never key material.
type ContainerCheckReport struct {
	Workload, Class, Object string
	PID                     int
	RSSKB                   int64
	KeyScan                 *KeyScan
}

// containerScanner is what the scan needs of a container guest (the
// microVM backend has it; the test fake backend doesn't).
type containerScanner interface {
	MemoryDump(ctx context.Context, dir string) (string, error)
	Disks() []string
	Op(ctx context.Context, m proto.Msg) (proto.Msg, error)
}

// ContainerCheck scans the running container of tenant/workload's object
// class/name for key material. The sandbox is a lower trust domain than
// its parent, so the needles are stricter than a DO guest's: every
// tskey- and node/machine private key on this host, the parent's egress
// MAC key (instance root and the object's derived key), every volume key
// (DEK) of the tenant's workloads here, this host's own secrets (audit
// key, tokens), and the container's HTTPS-intercept CA private key. Each
// must be absent from the guest's disks, files and RAM; the needles are
// read from the host (positive control: they exist), and a canary put
// in the guest's memory must be found in the RAM dump (it is real).
func (h *Host) ContainerCheck(ctx context.Context, tenantID, wlName, class, object string) (*ContainerCheckReport, error) {
	m := h.ctr
	var c *ctr
	m.mu.Lock()
	for k, cc := range m.ctrs {
		if k.workload == wlName && k.class == class && k.name == object && (tenantID == "" || k.tenant == tenantID) {
			c = cc
		}
	}
	m.mu.Unlock()
	if c == nil {
		return nil, fmt.Errorf("no container for %s/%s/%s", wlName, class, object)
	}
	c.mu.Lock()
	vm, pn, canary, k := c.vm, c.net, c.canary, c.key
	c.mu.Unlock()
	if vm == nil || pn == nil {
		return nil, fmt.Errorf("the container of %s/%s/%s is not running", wlName, class, object)
	}
	sc, ok := vm.(containerScanner)
	if !ok {
		return nil, errors.New("this container backend has no guest memory to scan (not a microVM)")
	}
	h.mu.Lock()
	w := h.workloads[k.tenant+"/"+k.workload]
	h.mu.Unlock()
	if w == nil || w.current() == nil {
		return nil, errors.New("the parent workload is not running")
	}
	inst := w.current()
	needles := h.keyNeedles(inst)
	add := func(kind string, b []byte) {
		if len(b) >= 16 {
			needles[kind+" raw"] = b
			needles[kind+" hex"] = []byte(hex.EncodeToString(b))
			needles[kind+" base64"] = []byte(base64.StdEncoding.EncodeToString(b))
		}
	}
	root := macKeyOf(inst.dir)
	if root == "" {
		return nil, errors.New("positive control failed: no parent MAC key on the host")
	}
	needles["parentmac root"] = []byte(root)
	needles["parentmac object"] = []byte(gen.ObjectMACKey(root, k.class, k.name))
	// Every volume key (DEK) of the tenant's workloads on this host.
	var tws []*workload
	h.mu.Lock()
	for key, ow := range h.workloads {
		if strings.HasPrefix(key, k.tenant+"/") {
			tws = append(tws, ow)
		}
	}
	h.mu.Unlock()
	for _, ow := range tws {
		if oi := ow.current(); oi != nil && oi.spec.Volume != nil && len(oi.spec.Volume.Key) > 0 {
			add("volumekey "+ow.name, oi.spec.Volume.Key)
		}
	}
	// This host's own secrets: its audit signing key and any key/token
	// file in its data dir (e.g. registry or mgmt credentials).
	filepath.WalkDir(h.cfg.DataDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		n := strings.ToLower(d.Name())
		if !(strings.HasSuffix(n, ".key") || strings.Contains(n, "token") || strings.Contains(n, "credential")) {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.Size() <= 64<<10 {
			if b, err := os.ReadFile(p); err == nil {
				add("hostsecret "+d.Name(), b)
			}
		}
		return nil
	})
	// The HTTPS-intercept CA's private key: the scalar and its encodings.
	der := pn.CAKeyDER()
	if pk, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if ek, ok := pk.(*ecdsa.PrivateKey); ok {
			sk, _ := ek.Bytes()
			add("cakey scalar", sk)
		}
	}
	add("cakey pkcs8", der)
	if b64 := base64.StdEncoding.EncodeToString(der); len(b64) > 64 {
		needles["cakey pem-line"] = []byte(b64[:64]) // the first line of its PEM
	}
	ks := &KeyScan{Needles: len(needles), Disks: sc.Disks()}
	kinds := map[string]bool{}
	for n := range needles {
		kinds[strings.Fields(n)[0]] = true
	}
	for kd := range kinds {
		ks.NeedleKinds = append(ks.NeedleKinds, kd)
	}
	sort.Strings(ks.NeedleKinds)
	for _, want := range []string{"tskey-", "nodekey-text", "parentmac", "cakey", "hostsecret"} {
		if !kinds[want] {
			return nil, fmt.Errorf("positive control failed: no %s needle found on the host", want)
		}
	}
	for _, d := range ks.Disks {
		ks.Found = append(ks.Found, scanFile(d, needles)...)
	}
	if j := vm.Info().Jail; j != nil && j.Chroot != "" {
		if p := filepath.Join(j.Chroot, "scratch.ext4"); fileReadable(p) {
			ks.Disks = append(ks.Disks, p)
			ks.Found = append(ks.Found, scanFile(p, needles)...)
		}
	}
	if r, err := sc.Op(ctx, proto.Msg{Op: "fsscan"}); err == nil {
		ks.GuestFiles, ks.GuestStateF = r.Scanned, r.Found
	}
	dir, err := os.MkdirTemp(h.cfg.DataDir, "memscan-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	st := time.Now()
	mem, err := sc.MemoryDump(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("memory dump: %w", err)
	}
	ks.MemdumpMS = time.Since(st).Milliseconds()
	if fi, err := os.Stat(mem); err == nil {
		ks.MemoryMiB = fi.Size() >> 20
	}
	ks.Found = append(ks.Found, scanFile(mem, needles)...)
	if canary != "" && len(scanFile(mem, map[string][]byte{"canary": []byte(canary)})) > 0 {
		ks.Control = "guest canary found in guest RAM (the dump is real)"
	} else {
		ks.Control = "FAILED: the guest canary is not in the dump"
	}
	os.Remove(mem)
	if ks.Found == nil {
		ks.Found = []string{}
	}
	sort.Strings(ks.Found)
	return &ContainerCheckReport{Workload: wlName, Class: class, Object: object, PID: vm.Info().PID, RSSKB: vm.RSSKB(), KeyScan: ks}, nil
}

func fileReadable(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	f.Close()
	return true
}
