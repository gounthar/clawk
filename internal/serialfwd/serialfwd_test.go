package serialfwd

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteFrame(&buf, FrameData, []byte("hello")))
	require.NoError(t, WriteModeFrame(&buf, Mode{Baud: 115200, Bits: 8, Parity: ParityNone, Stop: 1}))
	require.NoError(t, WriteFrame(&buf, FrameData, nil))

	r := NewReader(&buf)

	typ, payload, err := ReadFrame(r)
	require.NoError(t, err)
	require.Equal(t, FrameData, typ)
	require.Equal(t, []byte("hello"), payload)

	typ, payload, err = ReadFrame(r)
	require.NoError(t, err)
	require.Equal(t, FrameMode, typ)
	require.JSONEq(t, `{"baud":115200,"bits":8,"parity":"n","stop":1}`, string(payload))

	// An empty data frame is legal and must not be confused with EOF.
	typ, payload, err = ReadFrame(r)
	require.NoError(t, err)
	require.Equal(t, FrameData, typ)
	require.Empty(t, payload)

	_, _, err = ReadFrame(r)
	require.ErrorIs(t, err, io.EOF)
}

// Frames and handshake lines share one connection, and the reader that read
// the greeting has already buffered whatever followed it. Reusing that
// reader is a documented requirement of the protocol, so it gets a test:
// the failure mode if it regresses is a first frame that silently vanishes.
func TestHandshakeReaderCarriesBufferedFrames(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteLine(&buf, Greeting{Op: OpAttach, V: ProtoVersion, Name: "ttyACM0"}))
	require.NoError(t, WriteFrame(&buf, FrameData, []byte("pipelined")))

	r := NewReader(&buf)
	var g Greeting
	require.NoError(t, ReadLine(r, &g))
	require.Equal(t, OpAttach, g.Op)
	require.Equal(t, "ttyACM0", g.Name)

	typ, payload, err := ReadFrame(r)
	require.NoError(t, err)
	require.Equal(t, FrameData, typ)
	require.Equal(t, []byte("pipelined"), payload)
}

func TestGreetingCarriesMode(t *testing.T) {
	var buf bytes.Buffer
	mode := Mode{Baud: 1200, Bits: 7, Parity: ParityEven, Stop: 2}
	require.NoError(t, WriteLine(&buf, Greeting{
		Op: OpAttach, V: ProtoVersion, Name: "ttyACM0", Mode: &mode,
	}))

	var got Greeting
	require.NoError(t, ReadLine(NewReader(&buf), &got))
	require.NotNil(t, got.Mode)
	require.Equal(t, mode, *got.Mode)

	// Absent rather than zero-valued when unset: a guest that doesn't state
	// a mode must not be read as asking for 0 baud.
	buf.Reset()
	require.NoError(t, WriteLine(&buf, Greeting{Op: OpAttach, V: ProtoVersion, Name: "x"}))
	require.NotContains(t, buf.String(), "mode")
}

func TestReadFrameRejectsOversizedLength(t *testing.T) {
	// A header claiming more than MaxFrameBytes must be refused before the
	// allocation, not after.
	var hdr [FrameHeaderBytes]byte
	hdr[0] = byte(FrameData)
	binary.BigEndian.PutUint32(hdr[1:], MaxFrameBytes+1)

	_, _, err := ReadFrame(NewReader(bytes.NewReader(hdr[:])))
	require.ErrorIs(t, err, ErrFrameTooLong)
}

func TestWriteFrameRejectsOversizedPayload(t *testing.T) {
	err := WriteFrame(io.Discard, FrameData, make([]byte, MaxFrameBytes+1))
	require.ErrorIs(t, err, ErrFrameTooLong)
}

// A truncated payload is a broken peer, not a tidy hangup. Callers treat
// io.EOF between frames as "the other end went away cleanly", so a short
// read in the middle of one has to be distinguishable.
func TestReadFrameTruncatedPayload(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteFrame(&buf, FrameData, []byte("0123456789")))
	truncated := buf.Bytes()[:FrameHeaderBytes+4]

	_, _, err := ReadFrame(NewReader(bytes.NewReader(truncated)))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestReadLineRejectsOversizedLine(t *testing.T) {
	line := append(bytes.Repeat([]byte("x"), MaxLineBytes+10), '\n')
	var g Greeting
	err := ReadLine(NewReader(bytes.NewReader(line)), &g)
	require.ErrorIs(t, err, ErrLineTooLong)
}

func TestModeValidate(t *testing.T) {
	require.NoError(t, DefaultMode().Validate())
	require.NoError(t, Mode{Baud: 115200, Bits: 8, Parity: ParityNone, Stop: 1}.Validate())
	require.NoError(t, Mode{Baud: 300, Bits: 5, Parity: ParityOdd, Stop: 2}.Validate())

	for name, m := range map[string]Mode{
		"zero baud":     {Baud: 0, Bits: 8, Parity: ParityNone, Stop: 1},
		"negative baud": {Baud: -1, Bits: 8, Parity: ParityNone, Stop: 1},
		"bits too few":  {Baud: 9600, Bits: 4, Parity: ParityNone, Stop: 1},
		"bits too many": {Baud: 9600, Bits: 9, Parity: ParityNone, Stop: 1},
		"bad parity":    {Baud: 9600, Bits: 8, Parity: "mark", Stop: 1},
		"empty parity":  {Baud: 9600, Bits: 8, Parity: "", Stop: 1},
		"bad stop":      {Baud: 9600, Bits: 8, Parity: ParityNone, Stop: 3},
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, m.Validate())
		})
	}
}

func TestModeString(t *testing.T) {
	require.Equal(t, "115200-8N1", Mode{Baud: 115200, Bits: 8, Parity: ParityNone, Stop: 1}.String())
	require.Equal(t, "9600-7E2", Mode{Baud: 9600, Bits: 7, Parity: ParityEven, Stop: 2}.String())
}

// The guest turns this name into /dev/<name>. Everything that could escape
// that directory, or land somewhere surprising inside it, has to be refused
// on the host before it is ever published.
func TestValidDeviceName(t *testing.T) {
	for _, ok := range []string{"ttyACM0", "ttyUSB0", "cu.usbmodem1101", "arduino-uno", "a_b", "x"} {
		require.NoError(t, ValidDeviceName(ok), "%q should be accepted", ok)
	}
	for _, bad := range []string{
		"",
		"..",
		".hidden",
		"../../etc/passwd",
		"sub/dir",
		`back\slash`,
		"has space",
		"null\x00byte",
		"emoji✨",
		strings.Repeat("a", 65),
	} {
		require.Error(t, ValidDeviceName(bad), "%q should be refused", bad)
	}
}

func TestFrameTypeString(t *testing.T) {
	require.Equal(t, "data", FrameData.String())
	require.Equal(t, "mode", FrameMode.String())
	require.Equal(t, "unknown(0x7f)", FrameType(0x7f).String())
}

// WriteFrame emits the header and body as separate writes. A conn that
// reports a short write on the header must not leave the caller thinking
// the frame landed.
func TestWriteFrameSurfacesWriteErrors(t *testing.T) {
	want := errors.New("conn closed")
	err := WriteFrame(errWriter{want}, FrameData, []byte("x"))
	require.ErrorIs(t, err, want)
}

type errWriter struct{ err error }

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }
