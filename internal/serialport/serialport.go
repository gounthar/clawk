// Package serialport opens and configures a physical serial port on the
// host. It is the host end of `clawk serial` — internal/serialfwd carries
// the bytes, internal/cli/serial_proxy.go brokers them, and this is what
// actually talks to the tty.
//
// Only what forwarding needs is here: open a port (possibly named by a
// glob), put it in raw mode, apply a line configuration the guest asked
// for, and read and write bytes until someone closes it. No enumeration,
// no modem-line control, no flow control — see the serialfwd package
// comment for why the last of those can't be plumbed through a PTY anyway.
package serialport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/clawkwork/clawk/internal/serialfwd"
)

// Port is an open serial port.
//
// It embeds no lock: Read and Write may be called concurrently (they are,
// by the two halves of the proxy's pump), which is safe on an *os.File, but
// two concurrent Writes will interleave and two concurrent Reads will race
// for bytes. The proxy runs exactly one of each.
type Port struct {
	f *os.File
	// path is the resolved device, which for a glob is not what the user
	// configured. Every log line uses this rather than the pattern, so a
	// board that came back as usbmodem14201 says so.
	path string
}

// ErrNoMatch reports a device path that matched nothing. It is worth
// distinguishing because it is the ordinary "the board is unplugged, or is
// mid-reset" case, and the caller retries on it rather than giving up.
var ErrNoMatch = errors.New("serialport: no device matches")

// Open resolves pattern, opens the device, and puts it in raw mode with
// mode's line configuration.
//
// pattern may be a literal path or a glob. A glob is resolved here, at open
// time, rather than when the device was configured: a board that reboots
// into its bootloader disappears from /dev and comes back — often under a
// neighbouring name — and re-globbing on each open is what lets a forward
// survive that. A glob matching several devices is an error rather than a
// guess, because picking the wrong board silently is worse than saying so.
func Open(pattern string, mode serialfwd.Mode) (*Port, error) {
	if err := mode.Validate(); err != nil {
		return nil, err
	}
	path, err := Resolve(pattern)
	if err != nil {
		return nil, err
	}

	// O_NOCTTY: this is a daemon, and acquiring a controlling terminal
	// would hand the board's line disciplines a say in our signal
	// handling.
	//
	// O_NONBLOCK does two jobs. It stops the open itself from blocking on
	// carrier detect (which /dev/tty.* on macOS does, unlike the /dev/cu.*
	// callout device users should be naming), and it makes the descriptor
	// pollable, so os.NewFile hands back a File registered with the Go
	// runtime poller. That is what allows Close to unblock a Read parked
	// waiting for a byte that may never come — the whole teardown path
	// depends on it, so TestCloseUnblocksBlockedRead guards it.
	fd, err := openNonblock(path)
	if err != nil {
		return nil, fmt.Errorf("serialport: opening %s: %w", path, err)
	}
	p := &Port{f: os.NewFile(uintptr(fd), path), path: path}
	if p.f == nil {
		_ = closeFD(fd)
		return nil, fmt.Errorf("serialport: %s: invalid descriptor", path)
	}
	if err := p.Configure(mode); err != nil {
		_ = p.f.Close()
		return nil, err
	}
	return p, nil
}

// Resolve turns a device pattern into a single concrete path. A literal
// path is returned once it is confirmed to exist, so an unplugged board
// fails here with ErrNoMatch rather than at open with a bare ENOENT.
func Resolve(pattern string) (string, error) {
	if !isGlob(pattern) {
		if _, err := os.Stat(pattern); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("%w %s", ErrNoMatch, pattern)
			}
			return "", fmt.Errorf("serialport: %s: %w", pattern, err)
		}
		return pattern, nil
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("serialport: bad device pattern %q: %w", pattern, err)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w %s", ErrNoMatch, pattern)
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", fmt.Errorf(
			"serialport: pattern %s matches %d devices (%s) — narrow it so it names one",
			pattern, len(matches), strings.Join(matches, ", "))
	}
}

func isGlob(s string) bool { return strings.ContainsAny(s, "*?[") }

// Configure applies mode to the open port, leaving it in raw mode.
func (p *Port) Configure(mode serialfwd.Mode) error {
	if err := mode.Validate(); err != nil {
		return err
	}
	if err := applyMode(int(p.f.Fd()), mode); err != nil {
		return fmt.Errorf("serialport: configuring %s to %s: %w", p.path, mode, err)
	}
	return nil
}

// Path is the resolved device this port is open on.
func (p *Port) Path() string { return p.path }

func (p *Port) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *Port) Write(b []byte) (int, error) { return p.f.Write(b) }

// Close releases the port. On a tty configured by Open this lowers DTR,
// which on a board wired for auto-reset is half of the reset pulse the next
// Open completes — see the serialfwd package comment.
func (p *Port) Close() error { return p.f.Close() }
