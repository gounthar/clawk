//go:build !darwin && !linux

package serialport

import (
	"errors"

	"github.com/clawkwork/clawk/internal/serialfwd"
)

// errUnsupported keeps the package compiling on platforms clawk doesn't
// host a VM on. Nothing reaches these: serial forwarding is driven by the
// vz daemon (darwin) and its tests run on darwin and linux.
var errUnsupported = errors.New("serialport: serial ports are not supported on this platform")

func openNonblock(string) (int, error) { return -1, errUnsupported }

func closeFD(int) error { return errUnsupported }

func applyMode(int, serialfwd.Mode) error { return errUnsupported }
