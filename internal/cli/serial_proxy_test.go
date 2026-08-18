//go:build darwin || linux

package cli

import (
	"bufio"
	"context"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/serialfwd"
	"github.com/clawkwork/clawk/internal/serialport/serialporttest"
	"github.com/stretchr/testify/require"
)

// startTestSerialProxy runs a serialProxy over a loopback TCP listener
// instead of vsock, the same trick startTestProxy uses for reverse
// forwarding: the protocol is transport-agnostic, so everything below
// exercises the real accept/handshake/bridge path.
func startTestSerialProxy(t *testing.T, devices ...config.SerialDevice) (*serialProxy, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	p := newSerialProxy(log.New(io.Discard, "", 0))
	p.Set(devices)
	ctx, cancel := context.WithCancel(context.Background())
	p.serve(ctx, cancel, ln)
	t.Cleanup(p.Stop)
	return p, ln.Addr().String()
}

// dialSerial opens a connection and sends one greeting, returning the
// connection plus the reader that must be used for everything after it.
func dialSerial(t *testing.T, addr string, g serialfwd.Greeting) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
	require.NoError(t, serialfwd.WriteLine(conn, g))
	return conn, serialfwd.NewReader(conn)
}

// fakeBoard is a PTY standing in for a plugged-in microcontroller: the
// proxy opens the slave as its serial port, and the test drives the master.
type fakeBoard struct {
	master *os.File
	path   string
}

func newFakeBoard(t *testing.T) fakeBoard {
	t.Helper()
	master, slavePath := serialporttest.OpenPTYPair(t)
	return fakeBoard{master: master, path: slavePath}
}

// device is the config entry pointing at this board.
func (b fakeBoard) device(guestName string) config.SerialDevice {
	return config.SerialDevice{HostPath: b.path, GuestName: guestName}
}

func TestSerialControlSendsSnapshotAndUpdates(t *testing.T) {
	p, addr := startTestSerialProxy(t,
		config.SerialDevice{HostPath: "/dev/cu.usbmodem1101", GuestName: "ttyACM0"})

	_, r := dialSerial(t, addr, serialfwd.Greeting{Op: serialfwd.OpControl, V: serialfwd.ProtoVersion})

	var snap serialfwd.Snapshot
	require.NoError(t, serialfwd.ReadLine(r, &snap))
	require.Equal(t, []serialfwd.Device{{Name: "ttyACM0"}}, snap.Devices)

	// An edit to a running sandbox pushes a fresh full set — the guest
	// reconciles rather than applying deltas.
	p.Set([]config.SerialDevice{
		{HostPath: "/dev/cu.usbmodem1101", GuestName: "ttyACM0"},
		{HostPath: "/dev/cu.usbserial-A50285BI", GuestName: "ttyUSB0"},
	})
	require.NoError(t, serialfwd.ReadLine(r, &snap))
	require.Equal(t, []serialfwd.Device{{Name: "ttyACM0"}, {Name: "ttyUSB0"}}, snap.Devices)
}

// The host path is the guest's business to never learn: it names devices,
// and the mapping to the Mac's /dev stays on the host.
func TestSerialSnapshotOmitsHostPaths(t *testing.T) {
	_, addr := startTestSerialProxy(t,
		config.SerialDevice{HostPath: "/dev/cu.usbmodem1101", GuestName: "ttyACM0"})

	conn, r := dialSerial(t, addr, serialfwd.Greeting{Op: serialfwd.OpControl, V: serialfwd.ProtoVersion})
	_ = conn

	line, err := r.ReadString('\n')
	require.NoError(t, err)
	require.NotContains(t, line, "usbmodem")
	require.Contains(t, line, "ttyACM0")
}

func TestSerialAttachRefusesUnknownDevice(t *testing.T) {
	_, addr := startTestSerialProxy(t,
		config.SerialDevice{HostPath: "/dev/cu.usbmodem1101", GuestName: "ttyACM0"})

	_, r := dialSerial(t, addr, serialfwd.Greeting{
		Op: serialfwd.OpAttach, V: serialfwd.ProtoVersion, Name: "ttyACM9",
	})

	var reply serialfwd.AttachReply
	require.NoError(t, serialfwd.ReadLine(r, &reply))
	require.False(t, reply.OK)
	require.Contains(t, reply.Error, "ttyACM9")
}

// A guest speaking a version the host doesn't gets hung up on rather than
// half-served — the alternative is a stream neither end can parse.
func TestSerialRejectsProtocolMismatch(t *testing.T) {
	_, addr := startTestSerialProxy(t)

	_, r := dialSerial(t, addr, serialfwd.Greeting{
		Op: serialfwd.OpControl, V: serialfwd.ProtoVersion + 1,
	})

	_, err := r.ReadString('\n')
	require.Error(t, err, "host should close the connection")
}

func TestSerialAttachBridgesBytesBothWays(t *testing.T) {
	board := newFakeBoard(t)
	_, addr := startTestSerialProxy(t, board.device("ttyACM0"))

	conn, r := dialSerial(t, addr, serialfwd.Greeting{
		Op: serialfwd.OpAttach, V: serialfwd.ProtoVersion, Name: "ttyACM0",
		Mode: &serialfwd.Mode{Baud: 115200, Bits: 8, Parity: serialfwd.ParityNone, Stop: 1},
	})

	var reply serialfwd.AttachReply
	require.NoError(t, serialfwd.ReadLine(r, &reply))
	require.True(t, reply.OK, "attach refused: %s", reply.Error)

	// Guest → board.
	require.NoError(t, serialfwd.WriteFrame(conn, serialfwd.FrameData, []byte("AT\r\n")))
	buf := make([]byte, 64)
	require.NoError(t, board.master.SetDeadline(time.Now().Add(5*time.Second)))
	n, err := board.master.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "AT\r\n", string(buf[:n]))

	// Board → guest.
	_, err = board.master.WriteString("OK\r\n")
	require.NoError(t, err)
	typ, payload, err := serialfwd.ReadFrame(r)
	require.NoError(t, err)
	require.Equal(t, serialfwd.FrameData, typ)
	require.Equal(t, "OK\r\n", string(payload))
}

// The greeting's mode has to reach the hardware before the first byte does.
// A tool that opens at 115200 and immediately writes must not have that
// write go out at the port's previous rate.
func TestSerialAttachAppliesGreetingMode(t *testing.T) {
	board := newFakeBoard(t)
	_, addr := startTestSerialProxy(t, board.device("ttyACM0"))

	_, r := dialSerial(t, addr, serialfwd.Greeting{
		Op: serialfwd.OpAttach, V: serialfwd.ProtoVersion, Name: "ttyACM0",
		Mode: &serialfwd.Mode{Baud: 115200, Bits: 8, Parity: serialfwd.ParityNone, Stop: 1},
	})

	var reply serialfwd.AttachReply
	require.NoError(t, serialfwd.ReadLine(r, &reply))
	require.True(t, reply.OK, "attach refused: %s", reply.Error)

	require.Equal(t, 115200, serialporttest.Speed(t, int(board.master.Fd())))
}

// The 1200-baud touch: a mid-stream mode change is how a native-USB board is
// told to reboot into its bootloader, so it has to actually reach the tty.
func TestSerialModeFrameReconfiguresPort(t *testing.T) {
	board := newFakeBoard(t)
	_, addr := startTestSerialProxy(t, board.device("ttyACM0"))

	conn, r := dialSerial(t, addr, serialfwd.Greeting{
		Op: serialfwd.OpAttach, V: serialfwd.ProtoVersion, Name: "ttyACM0",
		Mode: &serialfwd.Mode{Baud: 115200, Bits: 8, Parity: serialfwd.ParityNone, Stop: 1},
	})
	var reply serialfwd.AttachReply
	require.NoError(t, serialfwd.ReadLine(r, &reply))
	require.True(t, reply.OK, "attach refused: %s", reply.Error)

	require.NoError(t, serialfwd.WriteModeFrame(conn, serialfwd.Mode{
		Baud: 1200, Bits: 8, Parity: serialfwd.ParityNone, Stop: 1,
	}))

	require.Eventually(t, func() bool {
		return serialporttest.Speed(t, int(board.master.Fd())) == 1200
	}, 5*time.Second, 20*time.Millisecond, "mode frame never reached the port")
}

// Ordering is the reason frames exist here rather than a raw byte stream
// with mode on the side. Data written before a mode change must be on the
// wire before the port is reconfigured.
func TestSerialDataBeforeModeChangeIsNotReordered(t *testing.T) {
	board := newFakeBoard(t)
	_, addr := startTestSerialProxy(t, board.device("ttyACM0"))

	conn, r := dialSerial(t, addr, serialfwd.Greeting{
		Op: serialfwd.OpAttach, V: serialfwd.ProtoVersion, Name: "ttyACM0",
		Mode: &serialfwd.Mode{Baud: 115200, Bits: 8, Parity: serialfwd.ParityNone, Stop: 1},
	})
	var reply serialfwd.AttachReply
	require.NoError(t, serialfwd.ReadLine(r, &reply))
	require.True(t, reply.OK)

	require.NoError(t, serialfwd.WriteFrame(conn, serialfwd.FrameData, []byte("before")))
	require.NoError(t, serialfwd.WriteModeFrame(conn, serialfwd.Mode{
		Baud: 1200, Bits: 8, Parity: serialfwd.ParityNone, Stop: 1,
	}))

	buf := make([]byte, 64)
	require.NoError(t, board.master.SetDeadline(time.Now().Add(5*time.Second)))
	n, err := board.master.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "before", string(buf[:n]))

	require.Eventually(t, func() bool {
		return serialporttest.Speed(t, int(board.master.Fd())) == 1200
	}, 5*time.Second, 20*time.Millisecond)
}

// One port, one reader. A second attach has to be refused rather than
// queued, or two guest processes silently steal each other's bytes.
func TestSerialSecondAttachIsRefused(t *testing.T) {
	board := newFakeBoard(t)
	_, addr := startTestSerialProxy(t, board.device("ttyACM0"))

	greeting := serialfwd.Greeting{
		Op: serialfwd.OpAttach, V: serialfwd.ProtoVersion, Name: "ttyACM0",
	}
	_, r1 := dialSerial(t, addr, greeting)
	var first serialfwd.AttachReply
	require.NoError(t, serialfwd.ReadLine(r1, &first))
	require.True(t, first.OK, "first attach refused: %s", first.Error)

	_, r2 := dialSerial(t, addr, greeting)
	var second serialfwd.AttachReply
	require.NoError(t, serialfwd.ReadLine(r2, &second))
	require.False(t, second.OK)
	require.Contains(t, second.Error, "already in use")
}

// Detaching has to release the claim, or a board is usable exactly once per
// boot.
func TestSerialReattachAfterDetach(t *testing.T) {
	board := newFakeBoard(t)
	_, addr := startTestSerialProxy(t, board.device("ttyACM0"))

	greeting := serialfwd.Greeting{
		Op: serialfwd.OpAttach, V: serialfwd.ProtoVersion, Name: "ttyACM0",
	}
	conn, r := dialSerial(t, addr, greeting)
	var reply serialfwd.AttachReply
	require.NoError(t, serialfwd.ReadLine(r, &reply))
	require.True(t, reply.OK)
	require.NoError(t, conn.Close())

	// The release happens as the handler unwinds, so give it a moment.
	require.Eventually(t, func() bool {
		c, rr := dialSerial(t, addr, greeting)
		defer c.Close()
		var again serialfwd.AttachReply
		if err := serialfwd.ReadLine(rr, &again); err != nil {
			return false
		}
		return again.OK
	}, 5*time.Second, 50*time.Millisecond, "device never became reattachable")
}

// A board that is mid-reset isn't there yet. The attach waits out the
// re-enumeration gap rather than failing, which is what makes an upload to
// a native-USB board work.
func TestSerialAttachWaitsForDeviceToAppear(t *testing.T) {
	board := newFakeBoard(t)

	// A symlink under a temp dir stands in for the device node: it can be
	// created after the fact, which a PTY path cannot.
	dir := t.TempDir()
	link := filepath.Join(dir, "cu.usbmodem1101")

	_, addr := startTestSerialProxy(t,
		config.SerialDevice{HostPath: link, GuestName: "ttyACM0"})

	appeared := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = os.Symlink(board.path, link)
		close(appeared)
	}()

	_, r := dialSerial(t, addr, serialfwd.Greeting{
		Op: serialfwd.OpAttach, V: serialfwd.ProtoVersion, Name: "ttyACM0",
	})

	var reply serialfwd.AttachReply
	require.NoError(t, serialfwd.ReadLine(r, &reply))
	<-appeared
	require.True(t, reply.OK, "attach gave up before the device came back: %s", reply.Error)
}

// Past the retry window the attach fails with something the user can act
// on, rather than hanging until the guest gives up.
func TestSerialAttachFailsWhenDeviceNeverAppears(t *testing.T) {
	dir := t.TempDir()
	_, addr := startTestSerialProxy(t, config.SerialDevice{
		HostPath: filepath.Join(dir, "cu.usbmodem1101"), GuestName: "ttyACM0",
	})

	start := time.Now()
	_, r := dialSerial(t, addr, serialfwd.Greeting{
		Op: serialfwd.OpAttach, V: serialfwd.ProtoVersion, Name: "ttyACM0",
	})

	var reply serialfwd.AttachReply
	require.NoError(t, serialfwd.ReadLine(r, &reply))
	require.False(t, reply.OK)
	require.Contains(t, reply.Error, "no device matches")
	require.GreaterOrEqual(t, time.Since(start), openRetryWindow,
		"should have used the whole retry window before giving up")
}

// A glob that matches two boards must refuse rather than guess — flashing
// the wrong device is the worst outcome available here. And because it is
// not an absence, it must not burn the retry window first.
func TestSerialAttachRefusesAmbiguousGlob(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"cu.usbmodem1101", "cu.usbmodem2201"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), nil, 0o600))
	}
	_, addr := startTestSerialProxy(t, config.SerialDevice{
		HostPath: filepath.Join(dir, "cu.usbmodem*"), GuestName: "ttyACM0",
	})

	start := time.Now()
	_, r := dialSerial(t, addr, serialfwd.Greeting{
		Op: serialfwd.OpAttach, V: serialfwd.ProtoVersion, Name: "ttyACM0",
	})

	var reply serialfwd.AttachReply
	require.NoError(t, serialfwd.ReadLine(r, &reply))
	require.False(t, reply.OK)
	require.Contains(t, reply.Error, "matches 2 devices")
	require.Less(t, time.Since(start), openRetryWindow, "should have failed immediately")
}

// Stop has to reach an attachment parked on a silent board, or the daemon
// hangs on shutdown waiting for a byte that never comes.
func TestSerialStopUnblocksIdleAttachment(t *testing.T) {
	board := newFakeBoard(t)
	p, addr := startTestSerialProxy(t, board.device("ttyACM0"))

	_, r := dialSerial(t, addr, serialfwd.Greeting{
		Op: serialfwd.OpAttach, V: serialfwd.ProtoVersion, Name: "ttyACM0",
	})
	var reply serialfwd.AttachReply
	require.NoError(t, serialfwd.ReadLine(r, &reply))
	require.True(t, reply.OK)

	stopped := make(chan struct{})
	go func() { p.Stop(); close(stopped) }()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung on an idle attachment")
	}
}
