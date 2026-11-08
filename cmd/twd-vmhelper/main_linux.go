//go:build linux

// twd-vmhelper is the privileged half of twd's microVM tier (see
// hostd/vm/helper). `sudo twd host setup --microvm` installs it as the
// systemd service twd-vmhelper.service, with a root-owned copy of this
// binary, Firecracker and the jailer in /usr/local/libexec/twd and its
// config in /etc/twd/vmhelper.json. twd-hostd, running as the user,
// talks to it on /run/twd/vmhelper.sock; only the uids in the config may.
//
//	twd-vmhelper [-config /etc/twd/vmhelper.json] [-socket /run/twd/vmhelper.sock]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/rosskukulinski/twd/hostd/vm/helper"
	"github.com/rosskukulinski/twd/hostd/vm/helperapi"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "twd-vmhelper:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "/etc/twd/vmhelper.json", "config (allowed uids, binaries, jail dir)")
	sock := flag.String("socket", helperapi.DefaultSocket, "unix socket (SOCK_SEQPACKET) for twd-hostd")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := helper.DefaultConfig()
	b, err := os.ReadFile(*cfgPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("%s: %w", *cfgPath, err)
	}
	if st, err := os.Stat(*cfgPath); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && (sys.Uid != 0 || st.Mode().Perm()&0o022 != 0) {
			return fmt.Errorf("%s must be owned by root and not group/world-writable", *cfgPath)
		}
	}
	cfg.Log = log
	srv, err := helper.New(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*sock), 0o755); err != nil {
		return err
	}
	os.Remove(*sock)
	ln, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: *sock, Net: "unixpacket"})
	if err != nil {
		return err
	}
	// Anyone may connect; the helper authorizes each connection by its
	// peer uid (SO_PEERCRED) against allowedUIDs.
	os.Chmod(*sock, 0o666)
	log.Info("twd-vmhelper running", "socket", *sock, "allowed_uids", cfg.AllowedUIDs, "jail", cfg.JailBase,
		"firecracker", cfg.Firecracker, "jailer", cfg.Jailer)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Info("stopping: every VM is stopped and its volume released")
		ln.Close()
	}()
	srv.Serve(ln)
	srv.Close()
	return nil
}
