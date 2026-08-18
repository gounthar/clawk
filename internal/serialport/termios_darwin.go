package serialport

import (
	"github.com/clawkwork/clawk/internal/serialfwd"
	"golang.org/x/sys/unix"
)

// applyMode configures the port through TIOCGETA/TIOCSETA.
//
// Darwin needs no equivalent of Linux's BOTHER dance: its B<rate> constants
// are the numeric rates themselves (B115200 is 115200), and c_ispeed and
// c_ospeed are plain speed_t fields, so assigning the requested baud
// directly is both correct for the standard rates and the way non-standard
// ones are expressed. A rate the driver can't divide down to is rejected by
// the driver at TIOCSETA, which is the error the caller wants anyway.
func applyMode(fd int, mode serialfwd.Mode) error {
	t, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return err
	}

	cbits, err := cflagBits(mode)
	if err != nil {
		return err
	}

	t.Iflag &^= uint64(rawIflagClear)
	t.Oflag &^= uint64(rawOflagClear)
	t.Lflag &^= uint64(rawLflagClear)
	t.Cflag &^= uint64(rawCflagClear)
	t.Cflag |= uint64(rawCflagSet) | cbits

	t.Ispeed = uint64(mode.Baud)
	t.Ospeed = uint64(mode.Baud)

	// See the Linux implementation for why VMIN=1/VTIME=0 is the only safe
	// pairing here.
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0

	// TIOCSETA applies immediately. The draining variants (TIOCSETAW /
	// TIOCSETAF) would wait for output to flush, and a mode change arrives
	// here precisely when a tool is mid-handshake with a board that may
	// have stopped listening — blocking there would wedge the pump.
	return unix.IoctlSetTermios(fd, unix.TIOCSETA, t)
}
