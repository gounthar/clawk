//go:build linux

// gvproxy attachment for the firecracker backend. Firecracker speaks only a
// host TAP device, while gvproxy (gvisor-tap-vsock) speaks a unixgram socket
// carrying raw Ethernet frames ("vfkit" protocol). The two can't attach
// directly, so a machine.UserMode spec in TAP-bridge mode (GuestTAP+HostTAP)
// has the backend run gvproxy in-process and shovel frames between gvproxy's
// VM socket and HostTAP — a daemon-owned TAP sharing an L2 bridge with the
// guest's GuestTAP:
//
//	firecracker ─virtio-net─▶ GuestTAP ─┐
//	                                     ├─ bridge (L2, no IP)
//	             gvproxy ◀─unixgram─ pump ─ HostTAP ─┘
//
// HostTAP is opened by fd (no CAP_NET_RAW, unlike AF_PACKET); the bridge
// segments any GSO frames before they reach the offload-free HostTAP, so
// frames handed to gvproxy are always ≤ MTU.
package firecracker

import (
	"context"
	"fmt"
	"net"
	"os"
	"syscall"
	"unsafe"
)

// TUN/TAP ioctl + flags from <linux/if_tun.h>. Defined locally to avoid a new
// module dependency. TUNSETIFF == _IOW('T', 202, int).
const (
	cTUNSETIFF = 0x400454ca
	cIFFTAP    = 0x0002 // IFF_TAP: Ethernet frames, not IP packets
	cIFFNOPI   = 0x1000 // IFF_NO_PI: no 4-byte packet-info prefix — raw frames
)

// openTAP attaches to an existing persistent TAP device by name and returns a
// poller-backed *os.File over its frame fd. The device must already exist and
// be owned by the current uid (the daemon pre-creates it), so attaching needs
// no elevated capability. IFF_NO_PI yields raw Ethernet frames, matching what
// gvproxy's unixgram transport expects.
func openTAP(name string) (*os.File, error) {
	if len(name) >= 16 { // IFNAMSIZ
		return nil, fmt.Errorf("tap name %q too long", name)
	}
	fd, err := syscall.Open("/dev/net/tun", syscall.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}
	var ifr [40]byte // struct ifreq
	copy(ifr[:15], name)
	flags := uint16(cIFFTAP | cIFFNOPI)
	ifr[16] = byte(flags)
	ifr[17] = byte(flags >> 8)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(cTUNSETIFF), uintptr(unsafe.Pointer(&ifr[0]))); errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("TUNSETIFF %q: %w", name, errno)
	}
	// Nonblocking + os.NewFile registers the fd with Go's runtime poller, so a
	// Close from the lifecycle watcher unblocks any in-flight Read.
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("tap set nonblock: %w", err)
	}
	return os.NewFile(uintptr(fd), "/dev/net/tun:"+name), nil
}

// requireNonblockingTAP verifies a TAP fd handed in from elsewhere (a
// namespace-owning helper, over SCM_RIGHTS) is already nonblocking, as
// openTAP's own fds are.
//
// It checks rather than fixes, because this cannot be fixed from here:
// os.NewFile decides whether to register the fd with Go's runtime poller
// based on its blocking state at that moment, and a File wrapped around a
// blocking fd never joins the poller — so Close could not interrupt the
// pump's in-flight Read, and flipping O_NONBLOCK behind the File's back
// would instead turn that Read into a permanent EAGAIN. The sender has to
// SetNonblock on the raw fd before wrapping it; this catches the mistake
// with a message that says so, instead of a pump that won't shut down.
//
// SyscallConn (not Fd) because Fd itself removes the file from the poller.
func requireNonblockingTAP(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("tap fd: %w", err)
	}
	var flags int
	var errno syscall.Errno
	if cerr := rc.Control(func(fd uintptr) {
		r, _, e := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
		flags, errno = int(r), e
	}); cerr != nil {
		return fmt.Errorf("tap fd: %w", cerr)
	}
	if errno != 0 {
		return fmt.Errorf("tap fd F_GETFL: %w", errno)
	}
	if flags&syscall.O_NONBLOCK == 0 {
		return fmt.Errorf("tap fd is in blocking mode: set O_NONBLOCK on the raw fd " +
			"before wrapping it with os.NewFile, or the frame pump cannot be shut down")
	}
	return nil
}

// frameConn is the message-oriented half of *os.File / *net.UnixConn: each
// Read yields exactly one frame/datagram and each Write sends exactly one.
type frameConn interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
}

// startPump wires a host TAP fd to gvproxy's VM-side unixgram socket and
// relays frames in both directions until ctx is cancelled, at which point both
// fds are closed to unblock the copy loops. vmSocket is the *os.File
// usermode.Stack hands back; ownership transfers here, and it is converted to
// a poller-backed *net.UnixConn so shutdown is clean.
func startPump(ctx context.Context, tap *os.File, vmSocket *os.File) error {
	vm, err := net.FileConn(vmSocket)
	// FileConn dup'd the fd on success; drop our copy either way.
	_ = vmSocket.Close()
	if err != nil {
		return fmt.Errorf("wrapping gvproxy vm socket: %w", err)
	}
	uconn, ok := vm.(*net.UnixConn)
	if !ok {
		vm.Close()
		return fmt.Errorf("gvproxy vm socket is %T, want *net.UnixConn", vm)
	}

	go func() {
		<-ctx.Done()
		tap.Close()
		uconn.Close()
	}()
	go copyFrames(tap, uconn) // guest → gvproxy
	go copyFrames(uconn, tap) // gvproxy → guest
	return nil
}

// copyFrames relays one frame at a time from src to dst. A datagram socket and
// a TAP fd both preserve message boundaries, so a single read→write per
// iteration moves exactly one Ethernet frame with no reframing.
func copyFrames(src, dst frameConn) {
	buf := make([]byte, 65536)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
