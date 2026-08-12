package serialporttest

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// OpenPTYPair returns an open PTY master and the path of its slave.
//
// A PTY is not a UART — it has no modem-control lines and does nothing with
// the baud rate — but it is a real tty, so every ioctl the serial code
// issues goes through the kernel's tty layer rather than a mock, and the
// termios state is genuinely shared between the two ends. That last part is
// what lets a test assert that a mode change actually landed.
func OpenPTYPair(t *testing.T) (master *os.File, slavePath string) {
	t.Helper()

	// O_NONBLOCK so os.NewFile registers the master with the Go poller,
	// which is what makes SetDeadline work on it — tests read from this end
	// and need to fail rather than hang when nothing arrives.
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	require.NoError(t, err, "opening /dev/ptmx")
	master = os.NewFile(uintptr(fd), "/dev/ptmx")
	t.Cleanup(func() { _ = master.Close() })

	// unlockpt, then ptsname.
	require.NoError(t, unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0), "unlockpt")
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	require.NoError(t, err, "ptsname")

	return master, fmt.Sprintf("/dev/pts/%d", n)
}

// Speed reads back the line speed currently set on fd, which for a PTY
// master is the speed its slave was configured to.
//
// Linux keeps the real rate in the termios2 speed fields whenever the CBAUD
// field says BOTHER, which is how internal/serialport sets every rate, so
// that is where this reads it from.
func Speed(t *testing.T, fd int) int {
	t.Helper()
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS2)
	require.NoError(t, err)
	return int(tio.Ospeed)
}
