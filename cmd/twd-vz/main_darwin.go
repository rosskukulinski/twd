//go:build darwin

// twd-vz is the macOS microVM VMM (ADR-032 D7): one process per guest, on
// Apple's Virtualization.framework (via github.com/Code-Hex/vz). twd-hostd
// spawns it with the NIC, control and vsock descriptors described in
// hostd/vm/vzapi and the boot config on stdin; it boots the twd guest
// (kernel + read-only rootfs + config drive [+ DO volume]) and then only
// relays lifecycle events. It holds no tailnet state and opens no network
// connection: the guest's NIC is a datagram socket whose other end is
// hostd's userspace stack.
//
// It must be signed with the com.apple.security.virtualization
// entitlement (`make build` signs it ad hoc; see twd-vz.entitlements).
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/rosskukulinski/twd/hostd/vm/vzapi"
)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "twd-vz[%d]: "+format+"\n", append([]any{os.Getpid()}, args...)...)
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "-check" {
		// hostd's availability probe: can this (signed) binary configure
		// a VM with this kernel? Exit 0 and print the Rosetta state.
		if err := check(os.Args[2]); err != nil {
			fmt.Println("unavailable:", err)
			os.Exit(1)
		}
		fmt.Println("ok rosetta=" + rosettaState())
		return
	}
	var cfg vzapi.Config
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&cfg); err != nil {
		logf("config: %v", err)
		os.Exit(2)
	}
	ctlConn, err := fileConn(vzapi.FDCtl, "ctl")
	if err != nil {
		logf("ctl fd: %v", err)
		os.Exit(2)
	}
	c := &ctl{enc: json.NewEncoder(ctlConn)}
	if err := run(&cfg, c, ctlConn); err != nil {
		logf("%v", err)
		c.send(vzapi.Ctl{Event: "stopped", Error: err.Error()})
		os.Exit(1)
	}
}

type ctl struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (c *ctl) send(m vzapi.Ctl) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enc.Encode(m)
}

func fileConn(fd int, name string) (net.Conn, error) {
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		return nil, fmt.Errorf("fd %d not open", fd)
	}
	defer f.Close()
	return net.FileConn(f)
}

// check validates a minimal VM configuration and creates the VM (not
// started): it fails without the virtualization entitlement or on a host
// without the framework.
func check(kernel string) error {
	bl, err := vz.NewLinuxBootLoader(kernel)
	if err != nil {
		return err
	}
	vmc, err := vz.NewVirtualMachineConfiguration(bl, 1, vz.VirtualMachineConfigurationMinimumAllowedMemorySize())
	if err != nil {
		return err
	}
	if ok, err := vmc.Validate(); !ok || err != nil {
		return fmt.Errorf("invalid configuration: %v", err)
	}
	_, err = vz.NewVirtualMachine(vmc)
	return err
}

func run(cfg *vzapi.Config, c *ctl, ctlConn net.Conn) error {
	t0 := time.Now()
	vmc, err := configure(cfg)
	if err != nil {
		return err
	}
	machine, err := vz.NewVirtualMachine(vmc)
	if err != nil {
		return fmt.Errorf("new VM: %w", err)
	}
	if err := machine.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	socks := machine.SocketDevices()
	if len(socks) != 1 {
		return errors.New("no vsock device")
	}
	ln, err := socks[0].Listen(cfg.VsockPort)
	if err != nil {
		return fmt.Errorf("vsock listen: %w", err)
	}
	vsockOut, err := fileConn(vzapi.FDVsock, "vsock")
	if err != nil {
		return fmt.Errorf("vsock fd: %w", err)
	}
	go relayVsock(ln, vsockOut.(*net.UnixConn))
	st := vzapi.Ctl{Event: "started", PID: os.Getpid(), StartMS: time.Since(t0).Milliseconds()}
	if n := len(cfg.Disks); n > 0 {
		st.ServicePID = vmServicePID(cfg.Disks[n-1].Path) // the config drive: this VM's alone
	}
	if cfg.Rosetta {
		st.Rosetta = rosettaState()
	}
	c.send(st)

	stopped := make(chan error, 1)
	go func() {
		for s := range machine.StateChangedNotify() {
			switch s {
			case vz.VirtualMachineStateStopped:
				stopped <- nil
				return
			case vz.VirtualMachineStateError:
				stopped <- errors.New("virtual machine error")
				return
			}
		}
	}()
	ops := make(chan vzapi.Ctl)
	hostGone := make(chan struct{})
	go func() {
		defer close(hostGone)
		sc := bufio.NewScanner(ctlConn)
		for sc.Scan() {
			var m vzapi.Ctl
			if json.Unmarshal(sc.Bytes(), &m) == nil && m.Op != "" {
				ops <- m
			}
		}
	}()
	for {
		select {
		case err := <-stopped:
			if err == nil {
				err = errors.New("guest powered off")
			}
			c.send(vzapi.Ctl{Event: "stopped", Error: err.Error()})
			os.Exit(0)
		case <-hostGone:
			// hostd went away: the VM must not outlive it.
			if machine.CanStop() {
				machine.Stop()
			}
			os.Exit(0)
		case m := <-ops:
			r := vzapi.Ctl{Event: "resp", ID: m.ID}
			if err := doOp(machine, m); err != nil {
				r.Error = err.Error()
			}
			c.send(r)
			if m.Op == "stop" {
				os.Exit(0)
			}
		}
	}
}

func doOp(machine *vz.VirtualMachine, m vzapi.Ctl) error {
	switch m.Op {
	case "pause":
		return machine.Pause()
	case "resume":
		return machine.Resume()
	case "stop":
		if machine.CanStop() {
			return machine.Stop()
		}
		return nil
	case "save":
		if err := machine.Pause(); err != nil {
			return fmt.Errorf("pause: %w", err)
		}
		err := machine.SaveMachineStateToPath(m.Path)
		if rerr := machine.Resume(); err == nil && rerr != nil {
			err = fmt.Errorf("resume: %w", rerr)
		}
		return err
	}
	return fmt.Errorf("unknown op %q", m.Op)
}

// relayVsock hands every guest → host vsock connection to hostd: a fresh
// socketpair per connection, one end sent to hostd (one byte + SCM_RIGHTS
// on out), the other spliced to the vsock connection here.
func relayVsock(ln *vz.VirtioSocketListener, out *net.UnixConn) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err != nil {
			logf("vsock: socketpair: %v", err)
			conn.Close()
			continue
		}
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
		_, _, err = out.WriteMsgUnix([]byte{1}, syscall.UnixRights(fds[1]), nil)
		syscall.Close(fds[1])
		if err != nil {
			logf("vsock hand-off: %v", err)
			syscall.Close(fds[0])
			conn.Close()
			return
		}
		mine, err := fileConn(fds[0], "vsock-relay")
		if err != nil {
			conn.Close()
			continue
		}
		go splice(conn, mine)
	}
}

func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	a.Close()
	b.Close()
	<-done
}

func configure(cfg *vzapi.Config) (*vz.VirtualMachineConfiguration, error) {
	opts := []vz.LinuxBootLoaderOption{vz.WithCommandLine(cfg.Cmdline)}
	if cfg.Initrd != "" {
		opts = append(opts, vz.WithInitrd(cfg.Initrd))
	}
	bl, err := vz.NewLinuxBootLoader(cfg.Kernel, opts...)
	if err != nil {
		return nil, fmt.Errorf("boot loader: %w", err)
	}
	vmc, err := vz.NewVirtualMachineConfiguration(bl, uint(cfg.CPUs), uint64(cfg.MemMiB)<<20)
	if err != nil {
		return nil, err
	}
	// Console: the guest's hvc0, appended to a host file.
	ca, err := vz.NewFileSerialPortAttachment(cfg.Console, true)
	if err != nil {
		return nil, fmt.Errorf("console: %w", err)
	}
	cons, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(ca)
	if err != nil {
		return nil, err
	}
	vmc.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{cons})

	var disks []vz.StorageDeviceConfiguration
	for _, d := range cfg.Disks {
		a, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(d.Path, d.ReadOnly, vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeFsync)
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", d.Path, err)
		}
		blk, err := vz.NewVirtioBlockDeviceConfiguration(a)
		if err != nil {
			return nil, err
		}
		disks = append(disks, blk)
	}
	if cfg.Volume {
		f := os.NewFile(vzapi.FDVolume, "volume")
		// Full: a guest flush (fdatasync) reaches the host's storage before
		// it completes, so an acknowledged DO write survives a VMM kill.
		a, err := vz.NewDiskBlockDeviceStorageDeviceAttachment(f, false, vz.DiskSynchronizationModeFull)
		if err != nil {
			return nil, fmt.Errorf("volume: %w", err)
		}
		blk, err := vz.NewVirtioBlockDeviceConfiguration(a)
		if err != nil {
			return nil, err
		}
		disks = append(disks, blk)
	}
	vmc.SetStorageDevicesVirtualMachineConfiguration(disks)

	// NIC: a datagram socket to hostd's userspace stack. No NAT, no bridge:
	// the guest has no path to the host's network.
	nf := os.NewFile(vzapi.FDNet, "nic")
	na, err := vz.NewFileHandleNetworkDeviceAttachment(nf)
	if err != nil {
		return nil, fmt.Errorf("nic: %w", err)
	}
	if cfg.MTU > 1500 {
		if err := na.SetMaximumTransmissionUnit(cfg.MTU); err != nil {
			return nil, err
		}
	}
	nic, err := vz.NewVirtioNetworkDeviceConfiguration(na)
	if err != nil {
		return nil, err
	}
	if cfg.MAC != "" {
		hw, err := net.ParseMAC(cfg.MAC)
		if err != nil {
			return nil, err
		}
		mac, err := vz.NewMACAddress(hw)
		if err != nil {
			return nil, err
		}
		nic.SetMACAddress(mac)
	}
	vmc.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{nic})

	vs, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	vmc.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vs})
	ent, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	vmc.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{ent})

	if cfg.Rosetta {
		if err := addRosetta(vmc); err != nil {
			return nil, err
		}
	}
	if ok, err := vmc.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("invalid VM configuration: %v", err)
	}
	return vmc, nil
}
