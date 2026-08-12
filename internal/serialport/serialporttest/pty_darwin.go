package serialporttest

import (
	"bytes"
	"os"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// OpenPTYPair is the darwin half of the Linux function of the same name —
// see there for what it is for. Darwin spells the three steps differently:
// TIOCPTYGRANT and TIOCPTYUNLK for grantpt/unlockpt, and TIOCPTYGNAME to
// read the slave path into a caller-supplied buffer, which x/sys/unix has
// no typed wrapper for.
func OpenPTYPair(t *testing.T) (master *os.File, slavePath string) {
	t.Helper()

	// O_NONBLOCK so os.NewFile registers the master with the Go poller,
	// which is what makes SetDeadline work on it — tests read from this end
	// and need to fail rather than hang when nothing arrives.
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	require.NoError(t, err, "opening /dev/ptmx")
	master = os.NewFile(uintptr(fd), "/dev/ptmx")
	t.Cleanup(func() { _ = master.Close() })

	require.NoError(t, unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0), "grantpt")
	require.NoError(t, unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0), "unlockpt")

	// TIOCPTYGNAME writes a NUL-terminated path into a 128-byte buffer.
	var buf [128]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd),
		uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		t.Fatalf("ptsname: %v", errno)
	}
	name, _, _ := bytes.Cut(buf[:], []byte{0})

	return master, string(name)
}

// Speed reads back the line speed currently set on fd. Darwin stores the
// rate numerically in the termios speed fields, so it needs no decoding.
func Speed(t *testing.T, fd int) int {
	t.Helper()
	tio, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	require.NoError(t, err)
	return int(tio.Ospeed)
}
