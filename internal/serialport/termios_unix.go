//go:build darwin || linux

package serialport

import (
	"fmt"

	"github.com/clawkwork/clawk/internal/serialfwd"
	"golang.org/x/sys/unix"
)

// openNonblock opens a tty without acquiring it as a controlling terminal
// and without blocking on carrier. See the comment in Open for why both
// matter.
func openNonblock(path string) (int, error) {
	return unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
}

func closeFD(fd int) error { return unix.Close(fd) }

// Raw-mode masks, as uint64 so one definition serves both platforms —
// darwin's termios flags are 64-bit and Linux's are 32-bit, and each
// applyMode narrows these to its own width.
//
// This is cfmakeraw plus the two bits a forwarded port needs on top of it:
// CLOCAL, because nothing here should care about a modem carrier that
// USB-serial adapters don't really have, and CREAD, because a port we
// can't read from is useless.
const (
	rawIflagClear = uint64(unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON)
	rawOflagClear = uint64(unix.OPOST)
	rawLflagClear = uint64(unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN)

	// CRTSCTS goes off with the rest: hardware flow control on a forwarded
	// port would stall on an RTS line the guest has no way to drive.
	rawCflagClear = uint64(unix.CSIZE | unix.PARENB | unix.PARODD | unix.CSTOPB | unix.CRTSCTS)

	// HUPCL lowers DTR when the last descriptor closes. That is deliberate
	// and load-bearing rather than incidental: it is what makes the next
	// open a rising DTR edge, and so what resets an Arduino at exactly the
	// moment a guest process opens the PTY.
	rawCflagSet = uint64(unix.CLOCAL | unix.CREAD | unix.HUPCL)
)

// cflagBits returns the character-size, parity and stop-bit c_cflag bits
// for mode. The caller has already validated mode, so the switches here are
// total; the default arms exist to make that a compile-time-checkable claim
// rather than an assumption.
func cflagBits(mode serialfwd.Mode) (uint64, error) {
	var bits uint64
	switch mode.Bits {
	case 5:
		bits |= unix.CS5
	case 6:
		bits |= unix.CS6
	case 7:
		bits |= unix.CS7
	case 8:
		bits |= unix.CS8
	default:
		return 0, fmt.Errorf("serialport: unsupported character size %d", mode.Bits)
	}
	switch mode.Parity {
	case serialfwd.ParityNone:
	case serialfwd.ParityEven:
		bits |= unix.PARENB
	case serialfwd.ParityOdd:
		bits |= unix.PARENB | unix.PARODD
	default:
		return 0, fmt.Errorf("serialport: unsupported parity %q", mode.Parity)
	}
	if mode.Stop == 2 {
		bits |= unix.CSTOPB
	}
	return bits, nil
}
