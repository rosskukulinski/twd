//go:build darwin || linux

package vm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// dgramPair returns a connected AF_UNIX SOCK_DGRAM pair sized for an
// ethernet link: [0] for the host stack, [1] for the VMM (the
// Virtualization.framework file-handle NIC). Apple asks for SO_RCVBUF of
// at least twice (ideally four times) SO_SNDBUF on the client side.
func dgramPair() (host, vmm *os.File, err error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, nil, err
	}
	for _, fd := range fds {
		unix.CloseOnExec(fd)
		unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 1<<20)
		unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<20)
	}
	return os.NewFile(uintptr(fds[0]), "nic-host"), os.NewFile(uintptr(fds[1]), "nic-vmm"), nil
}

// newDgramVnet builds the host stack on a datagram socket whose every
// datagram is one ethernet frame (f is the host end of dgramPair; it is
// owned by the vnet from here on and closed with it).
func newDgramVnet(f *os.File, mtu uint32, gwMAC net.HardwareAddr, gw, guest netip.Addr, prefix int, handlers map[uint16]http.Handler) (*vnet, error) {
	ep, closeLink, err := newDgramLink(f, mtu, gwMAC)
	if err != nil {
		return nil, err
	}
	v, err := newVnetOn(ep, gw, guest, prefix, handlers, nil, closeLink)
	if err != nil {
		closeLink()
		return nil, err
	}
	return v, nil
}

// newDgramLink is an ethernet link endpoint (address mac) on a datagram
// socket of frames. It takes ownership of f; closeLink stops it.
func newDgramLink(f *os.File, mtu uint32, mac net.HardwareAddr) (ep stack.LinkEndpoint, closeLink func(), err error) {
	pc, err := net.FilePacketConn(f)
	f.Close()
	if err != nil {
		return nil, nil, err
	}
	conn := pc.(*net.UnixConn)
	ch := channel.New(1024, mtu+header.EthernetMinimumSize, tcpip.LinkAddress(mac))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { // wire → stack
		buf := make([]byte, 65536)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
					return
				}
				continue
			}
			if n < header.EthernetMinimumSize {
				continue
			}
			pb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[:n]...))})
			ch.InjectInbound(0, pb) // the ethernet endpoint parses the header
			pb.DecRef()
		}
	}()
	go func() { // stack → wire
		for {
			pkt := ch.ReadContext(ctx)
			if pkt == nil {
				return
			}
			frame := pkt.ToView().AsSlice()
			pkt.DecRef()
			for i, backoff := 0, 50*time.Microsecond; i < 8; i, backoff = i+1, backoff*2 {
				_, err := conn.Write(frame)
				if !errors.Is(err, unix.ENOBUFS) {
					break
				}
				time.Sleep(backoff) // the peer is behind: retry briefly, then drop (TCP retransmits)
			}
		}
	}()
	return ethernet.New(ch), func() {
		cancel()
		conn.Close()
		ch.Close()
	}, nil
}
