package serialport

import (
	"github.com/clawkwork/clawk/internal/serialfwd"
	"golang.org/x/sys/unix"
)

// applyMode configures the port through the termios2 interface.
//
// TCGETS2/TCSETS2 rather than the plain TCGETS/TCSETS pair, and BOTHER
// rather than a B<rate> constant, so any baud rate works. The classic
// interface can only express the rates that have a constant, which leaves
// out everything from an ESP-PROG at 460800 to the 250000 that DMX and some
// 3D-printer firmwares use. With BOTHER set in the speed field the kernel
// reads the rate from c_ispeed/c_ospeed instead, and the driver rounds to
// whatever its hardware can divide down to.
//
// golang.org/x/sys/unix.Termios is already the termios2 layout on Linux —
// it carries Ispeed and Ospeed after Cc, which the kernel's `struct
// termios` does not — so the same struct serves both ioctls.
func applyMode(fd int, mode serialfwd.Mode) error {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS2)
	if err != nil {
		return err
	}

	cbits, err := cflagBits(mode)
	if err != nil {
		return err
	}

	t.Iflag &^= uint32(rawIflagClear)
	t.Oflag &^= uint32(rawOflagClear)
	t.Lflag &^= uint32(rawLflagClear)
	t.Cflag &^= uint32(rawCflagClear)
	t.Cflag |= uint32(rawCflagSet) | uint32(cbits)

	// Speed: clear the CBAUD field, select BOTHER, and put the real rate in
	// the dedicated fields.
	t.Cflag = (t.Cflag &^ uint32(unix.CBAUD)) | uint32(unix.BOTHER)
	t.Ispeed = uint32(mode.Baud)
	t.Ospeed = uint32(mode.Baud)

	// VMIN=1/VTIME=0: a read blocks until at least one byte arrives and
	// never returns early empty. The alternative (VTIME as a poll timeout)
	// would surface as a zero-length read, which os.File reports as io.EOF
	// — indistinguishable from the port going away. Blocking is safe here
	// only because the descriptor is registered with the Go poller, so
	// Close can still interrupt it.
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0

	return unix.IoctlSetTermios(fd, unix.TCSETS2, t)
}
