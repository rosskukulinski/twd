//go:build linux

package vm

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"

	"github.com/rosskukulinski/twd/hostd/sandbox"
)

// newVnet builds the stack on fd (a TAP, ethernet frames) and serves the
// handlers on gateway:port.
func newVnet(fd int, mtu uint32, gwMAC net.HardwareAddr, gw, guest netip.Addr, prefix int, handlers map[uint16]http.Handler, gnet sandbox.ContainerNet) (*vnet, error) {
	ep, err := fdbased.New(&fdbased.Options{
		FDs: []int{fd}, MTU: mtu, EthernetHeader: true, Address: tcpip.LinkAddress(gwMAC),
		PacketDispatchMode: fdbased.Readv, IsPacketSocket: []bool{false},
	})
	if err != nil {
		return nil, fmt.Errorf("fdbased: %w", err)
	}
	return newVnetOn(ep, gw, guest, prefix, handlers, gnet, nil)
}
