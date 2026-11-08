// Package helperapi is the contract between twd-hostd (running as a normal
// user) and twd-vmhelper, the small privileged service that does the parts
// of the microVM tier that need root: network namespaces and TAP devices,
// the Firecracker jailer (chroot, per-VM uid/gid, cgroups v2), and the
// dm-crypt volumes that hold Durable Object storage.
//
// Transport: a SOCK_SEQPACKET unix socket (DefaultSocket, root-owned; the
// helper authorizes each connection by its SO_PEERCRED uid). One packet is
// one JSON message. File descriptors travel as SCM_RIGHTS on the packet
// that names them, so the helper never opens a path the caller chose and
// the caller never gets a path into the jail:
//
//	start  → kernel, rootfs, config drive, console log, [volume image]
//	start  ← gv0 TAP (hostd's side of the VM's network), vsock listener
//	memdump ← the guest memory file of a full snapshot
//
// One connection is one VM: the helper kills the VM and releases
// everything (volume, netns, jail, cgroup) when the VM exits or the
// connection closes, then sends an "exit" event if it still can.
//
// Secrets: the volume key travels only over this socket, from hostd to the
// helper, which hands it to cryptsetup on a pipe. It is never logged, put
// on a command line, written to disk, or given to the guest.
package helperapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// DefaultSocket is where `twd host setup --microvm` puts the helper.
const DefaultSocket = "/run/twd/vmhelper.sock"

// Version is bumped when the protocol changes incompatibly.
const Version = 1

// Error codes.
const (
	CodeUnauthorized = "unauthorized"  // peer uid not allowed
	CodeVolumeBusy   = "volume_busy"   // attached to another running VM
	CodeVolumeFenced = "volume_fenced" // epoch older than the volume's
	CodeVolumeKey    = "volume_key"    // the key does not open the volume
	CodeVolumeBad    = "volume_invalid"
	CodeCapacity     = "capacity" // no free VM slot
	CodeBadRequest   = "bad_request"
	CodeInternal     = "internal"
)

// Request is one client → helper message.
type Request struct {
	Op    string        `json:"op"` // ping | start | kill | memdump
	Start *StartRequest `json:"start,omitempty"`
}

// StartRequest boots one VM. File descriptors, in order: kernel (r),
// rootfs (r), config drive (r), console log (w, append), and the volume
// image (rw) if Volume is set.
type StartRequest struct {
	Name      string  `json:"name"` // caller's short VM name (logs only)
	MemMiB    int     `json:"memMiB"`
	VCPUs     int     `json:"vcpus"`
	HugePages string  `json:"hugePages,omitempty"` // "" | auto | on | off
	BootArgs  string  `json:"bootArgs"`
	MTU       int     `json:"mtu"`
	GuestMAC  string  `json:"guestMAC"`
	GuestCID  int     `json:"guestCID"`
	VsockPort int     `json:"vsockPort"` // host port the guest connects to
	Volume    *Volume `json:"volume,omitempty"`
}

// Volume is a persistent, encrypted block device for the guest (/dev/vdc).
type Volume struct {
	// Name identifies the volume for fencing and logs ("tenant/workload").
	Name string `json:"name"`
	// Epoch is the namespace lease epoch (ADR-011) the caller holds. The
	// helper refuses an epoch older than the newest it has attached.
	Epoch int64 `json:"epoch"`
	// Key opens (or, with Format, creates) the LUKS2 volume. 32 bytes.
	Key []byte `json:"key"`
	// Format: the image is new (all zeros); create LUKS2 + ext4 on it.
	Format bool `json:"format,omitempty"`
}

// String never prints the key.
func (v *Volume) String() string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("volume{%s epoch %d format %v}", v.Name, v.Epoch, v.Format)
}

// Response is one helper → client message: the answer to an op (Re) or an
// event (Event "exit").
type Response struct {
	Re    string `json:"re,omitempty"`
	Event string `json:"event,omitempty"`
	OK    bool   `json:"ok"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`

	// ping
	Version int    `json:"version,omitempty"`
	UID     int    `json:"uid,omitempty"` // the caller's uid, as the helper sees it
	Jailer  string `json:"jailer,omitempty"`
	Crypt   string `json:"crypt,omitempty"`

	// start
	PID       int                `json:"pid,omitempty"`       // firecracker (in the jail)
	StartedAt int64              `json:"startedAt,omitempty"` // unix ns of InstanceStart
	Jail      *Jail              `json:"jail,omitempty"`
	Timings   map[string]float64 `json:"timingsMs,omitempty"`
	HugePages bool               `json:"hugePages,omitempty"`
	VolumeNew bool               `json:"volumeNew,omitempty"`
}

// Jail describes how the VMM is confined.
type Jail struct {
	ID      string `json:"id"`
	UID     int    `json:"uid"`
	GID     int    `json:"gid"`
	Chroot  string `json:"chroot"`
	Cgroup  string `json:"cgroup"`
	CPUMax  string `json:"cpuMax"`
	MemMax  int64  `json:"memMax"`
	Seccomp string `json:"seccomp"`
	Netns   string `json:"netns"`
}

// Err is a helper error with a code.
type Err struct {
	Code, Msg string
}

func (e *Err) Error() string { return e.Code + ": " + e.Msg }

// IsCode reports whether err is a helper error with code.
func IsCode(err error, code string) bool {
	var e *Err
	return errors.As(err, &e) && e.Code == code
}

const maxPacket = 1 << 20

// Conn is a client connection (one VM).
type Conn struct {
	c      *net.UnixConn
	opMu   sync.Mutex
	resp   chan packet
	events chan Response
	closed chan struct{}
	once   sync.Once
	err    error
}

type packet struct {
	r   Response
	fds []int
}

// Dial connects to the helper.
func Dial(sock string) (*Conn, error) {
	c, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: sock, Net: "unixpacket"})
	if err != nil {
		return nil, err
	}
	cn := &Conn{c: c, resp: make(chan packet, 1), events: make(chan Response, 4), closed: make(chan struct{})}
	go cn.read()
	return cn, nil
}

func (cn *Conn) read() {
	defer cn.once.Do(func() { close(cn.closed) })
	buf := make([]byte, maxPacket)
	oob := make([]byte, syscall.CmsgSpace(16*4))
	for {
		n, oobn, _, _, err := cn.c.ReadMsgUnix(buf, oob)
		if err != nil || n == 0 {
			cn.err = err
			return
		}
		fds := ParseRights(oob[:oobn])
		var r Response
		if json.Unmarshal(buf[:n], &r) != nil {
			closeFDs(fds)
			continue
		}
		if r.Event != "" {
			closeFDs(fds)
			select {
			case cn.events <- r:
			default:
			}
			continue
		}
		cn.resp <- packet{r, fds}
	}
}

// Events delivers helper events ("exit").
func (cn *Conn) Events() <-chan Response { return cn.events }

// Closed is closed when the connection is gone (helper exit, or Close).
func (cn *Conn) Closed() <-chan struct{} { return cn.closed }

// Do sends a request (with fds) and waits for its answer.
func (cn *Conn) Do(req Request, files []*os.File, timeout time.Duration) (Response, []int, error) {
	cn.opMu.Lock()
	defer cn.opMu.Unlock()
	b, err := json.Marshal(req)
	if err != nil {
		return Response{}, nil, err
	}
	var oob []byte
	if len(files) > 0 {
		fds := make([]int, len(files))
		for i, f := range files {
			fds[i] = int(f.Fd())
		}
		oob = syscall.UnixRights(fds...)
	}
	if _, _, err := cn.c.WriteMsgUnix(b, oob, nil); err != nil {
		return Response{}, nil, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case p := <-cn.resp:
		if !p.r.OK {
			closeFDs(p.fds)
			return p.r, nil, &Err{Code: p.r.Code, Msg: p.r.Error}
		}
		return p.r, p.fds, nil
	case <-cn.closed:
		return Response{}, nil, fmt.Errorf("helper connection closed: %v", cn.err)
	case <-t.C:
		return Response{}, nil, fmt.Errorf("helper %s: timeout after %v", req.Op, timeout)
	}
}

// Close closes the connection (the helper then stops the VM).
func (cn *Conn) Close() error { return cn.c.Close() }

// Ping checks the helper is up and this uid may use it.
func Ping(sock string) (Response, error) {
	c, err := Dial(sock)
	if err != nil {
		return Response{}, err
	}
	defer c.Close()
	r, _, err := c.Do(Request{Op: "ping"}, nil, 5*time.Second)
	return r, err
}

// ParseRights extracts SCM_RIGHTS file descriptors from oob.
func ParseRights(oob []byte) []int {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	var fds []int
	for _, m := range msgs {
		if f, err := syscall.ParseUnixRights(&m); err == nil {
			for _, fd := range f {
				// Never leak a passed fd (a volume's lock, a vsock
				// listener) into a child process (another VM's jailer).
				syscall.CloseOnExec(fd)
			}
			fds = append(fds, f...)
		}
	}
	return fds
}

func closeFDs(fds []int) {
	for _, fd := range fds {
		syscall.Close(fd)
	}
}

// WriteResponse sends r (with fds) on c (helper side).
func WriteResponse(c *net.UnixConn, r Response, fds ...int) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var oob []byte
	if len(fds) > 0 {
		oob = syscall.UnixRights(fds...)
	}
	_, _, err = c.WriteMsgUnix(b, oob, nil)
	return err
}

// ReadRequest reads one request and its fds (helper side).
func ReadRequest(c *net.UnixConn) (Request, []int, error) {
	buf := make([]byte, maxPacket)
	oob := make([]byte, syscall.CmsgSpace(16*4))
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return Request{}, nil, err
	}
	if n == 0 {
		return Request{}, nil, errors.New("EOF")
	}
	fds := ParseRights(oob[:oobn])
	var r Request
	if err := json.Unmarshal(buf[:n], &r); err != nil {
		closeFDs(fds)
		return Request{}, nil, fmt.Errorf("bad request: %w", err)
	}
	return r, fds, nil
}
