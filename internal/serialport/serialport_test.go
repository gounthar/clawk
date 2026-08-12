//go:build darwin || linux

package serialport

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/serialfwd"
	"github.com/clawkwork/clawk/internal/serialport/serialporttest"
	"github.com/stretchr/testify/require"
)

func TestResolveLiteralPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ttyFAKE0")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	got, err := Resolve(path)
	require.NoError(t, err)
	require.Equal(t, path, got)
}

// An unplugged board is the common case, not an exceptional one — the proxy
// retries on ErrNoMatch instead of failing the attach outright, so the
// sentinel has to survive Resolve.
func TestResolveMissingLiteralIsErrNoMatch(t *testing.T) {
	_, err := Resolve(filepath.Join(t.TempDir(), "nope"))
	require.ErrorIs(t, err, ErrNoMatch)
}

func TestResolveGlob(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cu.usbmodem1101")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	got, err := Resolve(filepath.Join(dir, "cu.usbmodem*"))
	require.NoError(t, err)
	require.Equal(t, path, got)
}

func TestResolveGlobMatchingNothing(t *testing.T) {
	_, err := Resolve(filepath.Join(t.TempDir(), "cu.usbmodem*"))
	require.ErrorIs(t, err, ErrNoMatch)
}

// Two boards behind one pattern must be an error naming both. Silently
// picking the first would flash the wrong device, which is the single most
// expensive way this package could be wrong.
func TestResolveAmbiguousGlob(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"cu.usbmodem1101", "cu.usbmodem2201"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), nil, 0o600))
	}

	_, err := Resolve(filepath.Join(dir, "cu.usbmodem*"))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNoMatch)
	require.Contains(t, err.Error(), "cu.usbmodem1101")
	require.Contains(t, err.Error(), "cu.usbmodem2201")
}

func TestOpenAppliesModeAndPumpsBytes(t *testing.T) {
	master, slavePath := serialporttest.OpenPTYPair(t)

	port, err := Open(slavePath, serialfwd.Mode{
		Baud: 115200, Bits: 8, Parity: serialfwd.ParityNone, Stop: 1,
	})
	require.NoError(t, err)
	defer port.Close()
	require.Equal(t, slavePath, port.Path())

	// Device → host. Raw mode has to be in force for this to arrive
	// untouched: with OPOST still set the tty layer would rewrite the \n.
	_, err = master.WriteString("from board\n")
	require.NoError(t, err)

	buf := make([]byte, 64)
	n, err := readWithin(t, port, buf, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, "from board\n", string(buf[:n]))

	// Host → device. ECHO must be off, or this comes straight back at us
	// and the next read sees its own bytes.
	_, err = port.Write([]byte("to board\n"))
	require.NoError(t, err)

	n, err = readWithin(t, master, buf, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, "to board\n", string(buf[:n]))
}

// The proxy tears a port down by closing it, from a different goroutine
// than the one parked in Read. That only works because Open leaves the
// descriptor non-blocking and therefore registered with the Go poller; if
// that ever regresses the read blocks forever and the daemon leaks a
// goroutine per unplugged board.
func TestCloseUnblocksBlockedRead(t *testing.T) {
	_, slavePath := serialporttest.OpenPTYPair(t)

	port, err := Open(slavePath, serialfwd.DefaultMode())
	require.NoError(t, err)

	readErr := make(chan error, 1)
	go func() {
		_, err := port.Read(make([]byte, 16))
		readErr <- err
	}()

	// Give the read time to actually park before pulling the rug.
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, port.Close())

	select {
	case err := <-readErr:
		require.Error(t, err, "Read should fail once the port is closed")
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return after Close — the fd is not pollable")
	}
}

// Every mode the guest can legally ask for has to survive the round trip to
// the kernel. The baud rates matter most: 250000 and 460800 have no B<rate>
// constant on Linux, which is the whole reason applyMode goes through
// termios2/BOTHER there.
func TestConfigureAcceptsEveryLegalMode(t *testing.T) {
	_, slavePath := serialporttest.OpenPTYPair(t)
	port, err := Open(slavePath, serialfwd.DefaultMode())
	require.NoError(t, err)
	defer port.Close()

	for _, mode := range []serialfwd.Mode{
		{Baud: 1200, Bits: 8, Parity: serialfwd.ParityNone, Stop: 1},
		{Baud: 9600, Bits: 7, Parity: serialfwd.ParityEven, Stop: 2},
		{Baud: 57600, Bits: 8, Parity: serialfwd.ParityOdd, Stop: 1},
		{Baud: 115200, Bits: 8, Parity: serialfwd.ParityNone, Stop: 1},
		{Baud: 250000, Bits: 8, Parity: serialfwd.ParityNone, Stop: 1},
		{Baud: 460800, Bits: 8, Parity: serialfwd.ParityNone, Stop: 1},
		{Baud: 921600, Bits: 8, Parity: serialfwd.ParityNone, Stop: 1},
		{Baud: 5, Bits: 5, Parity: serialfwd.ParityNone, Stop: 1},
	} {
		t.Run(mode.String(), func(t *testing.T) {
			require.NoError(t, port.Configure(mode))
		})
	}
}

// A mode arrives from the guest, so a bad one is a wire-level input rather
// than a programming error. It must be refused before any ioctl, and before
// the device is opened at all.
func TestOpenRejectsInvalidModeBeforeTouchingTheDevice(t *testing.T) {
	_, err := Open("/definitely/not/a/device", serialfwd.Mode{Baud: 0})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNoMatch, "should fail on the mode, not the path")
}

func TestConfigureRejectsInvalidMode(t *testing.T) {
	_, slavePath := serialporttest.OpenPTYPair(t)
	port, err := Open(slavePath, serialfwd.DefaultMode())
	require.NoError(t, err)
	defer port.Close()

	require.Error(t, port.Configure(serialfwd.Mode{Baud: 9600, Bits: 99, Parity: "n", Stop: 1}))
}

// readWithin fails the test rather than hanging when a read doesn't land.
func readWithin(t *testing.T, r io.Reader, buf []byte, d time.Duration) (int, error) {
	t.Helper()
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := r.Read(buf)
		ch <- result{n, err}
	}()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-time.After(d):
		return 0, errors.New("timed out waiting for read")
	}
}
