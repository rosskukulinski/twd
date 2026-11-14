package vm

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/sandbox"
)

// The guest network is the same in every VM (each has its own host-side
// stack, so addresses never collide).
var (
	gatewayIP = netip.MustParseAddr(proto.GatewayIP)
	guestIP   = netip.MustParseAddr(proto.GuestIP)
	gwMAC     = net.HardwareAddr{0x06, 0x00, 0xc0, 0xa8, 0x48, 0x01}
)

const guestMAC = "06:00:c0:a8:48:02"

// ---- config drive ----

// writeConfigDrive writes the config drive: guest.json + the instance dir,
// as a raw tar padded to 4 KiB. extra adjusts guest.json (backend-specific
// fields) before it is written.
func writeConfigDrive(path string, s sandbox.Spec, mtu int, extra func(*proto.GuestConfig)) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	gc := proto.GuestConfig{UnixNano: time.Now().UnixNano(), IP: proto.GuestIP, Gateway: proto.GatewayIP, Prefix: proto.PrefixLen,
		MTU: mtu, WorkerdArgs: s.WorkerdArgs, HealthCF: s.HealthCF, Debug: s.Debug}
	if s.Volume != nil {
		gc.Volume = proto.VolumeDev
	}
	if extra != nil {
		extra(&gc)
	}
	gb, _ := json.Marshal(gc)
	if err := tw.WriteHeader(&tar.Header{Name: proto.GuestConfigFile, Mode: 0o600, Size: int64(len(gb)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	tw.Write(gb)
	err := filepath.WalkDir(s.AppDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(s.AppDir, p)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel) + "/", Mode: 0o700, Typeflag: tar.TypeDir})
		}
		if !d.Type().IsRegular() {
			return nil // sockets etc.
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mode := int64(0o600)
		if fi, err := d.Info(); err == nil && fi.Mode()&0o100 != 0 {
			mode = 0o700 // keep the owner exec bit (platform test binaries)
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: mode, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if pad := buf.Len() % 4096; pad != 0 {
		buf.Write(make([]byte, 4096-pad))
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}
