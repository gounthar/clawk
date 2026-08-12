// Package serialfwd is the wire protocol for serial forwarding: a physical
// serial port on the host presented as a PTY inside the guest.
//
// The motivating case is microcontroller work — an Arduino or ESP32 plugged
// into the Mac, flashed and monitored by tooling running in the sandbox.
// Passing the USB device itself through is not an option: Virtualization.
// framework exposes no physical USB passthrough (its USB controller carries
// virtual mass-storage devices only) and firecracker has no USB at all. But
// none of the tooling actually wants USB — avrdude, esptool and every serial
// monitor want a tty and a baud rate. That is a byte stream plus a little
// out-of-band state, which vsock carries fine.
//
// Shape, per connection (the guest always dials, the host always listens on
// VSockPort — the same asymmetry as internal/revfwd, and the same reason
// this is vz-only):
//
//	guest → host   one JSON Greeting, newline-terminated
//	op=control     host replies with a Snapshot line now and another on
//	               every change to the device set, until the connection is
//	               closed. This is how the guest learns which PTYs to
//	               create, so `clawk serial add` applies live.
//	op=attach      host validates Name against the current set, opens the
//	               physical port, replies with one AttachReply line, and
//	               then speaks frames (below) for the rest of the
//	               connection.
//
// Validation is host-side on purpose: the guest names a *device*, never a
// host path, and a name the user didn't configure is refused. The host
// holds the mapping from name to /dev/cu.usbmodem…, so a process in the
// sandbox can reach exactly the ports the user attached and no others.
//
// # Connection lifetime is the open/close signal
//
// An attach connection exists for exactly as long as some guest process
// holds the PTY open, and the host opens and closes the physical port to
// match. That is not just bookkeeping — it is how auto-reset works. The
// classic Arduino reset circuit pulses RESET from the DTR line, and opening
// a serial port asserts DTR; this is why opening the Arduino IDE's serial
// monitor reboots an Uno. Tying the host-side open to the guest-side open
// reproduces that pulse at the one moment the tooling expects it, without
// the guest ever naming a modem-control line.
//
// It also means the port is free whenever the sandbox isn't using it, so
// the Arduino IDE on the Mac can still have it.
//
// # What a PTY cannot carry
//
// A PTY has no modem-control lines: TIOCMGET/TIOCMSET on either end return
// ENOTTY on Linux. So a guest tool that toggles DTR or RTS explicitly gets
// an error, and no protocol here can fix that — see docs/serial.md for
// which boards that affects and the workarounds. What a PTY *does* carry is
// the termios state (master and slave share it, so the guest agent can read
// back the baud rate its client set) and the open/close edges. Those are
// the two things the FrameMode message and the connection lifetime
// respectively convey, and between them they cover the 1200-baud touch that
// native-USB boards use to enter their bootloader.
//
// # Framing
//
// The handshake is JSON lines like revfwd's, because it's a handful of
// messages and being greppable in a log is worth more than the bytes. What
// follows is *not* a raw byte stream like revfwd's, though: serial data and
// mode changes have to stay in order relative to each other. A tool that
// writes a command, changes baud, then writes another command must have
// those land in that order on the wire — so both travel as frames on the
// one connection rather than data inline and mode on the side.
//
// The guest half is inlined in internal/agentembed/main.go.in (the agent
// builds standalone inside the guest and can't import this package). Any
// change here must be mirrored there — same rule as internal/revfwd, and
// TestSerialProtocolMirroredInAgent enforces it.
package serialfwd

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// VSockPort is the host-side AF_VSOCK port the guest dials for both
// connection kinds. Disjoint from the other fixed ports: 1024 pty-agent,
// 1025 time-sync, 1026 ssh-agent, 1027 mem-report, 1028 reverse-forward,
// 1100+ 9p caches.
const VSockPort uint32 = 1029

// ProtoVersion is the current wire version. Bumped only on a breaking
// change; the guest binary is rebuilt from these sources and re-injected on
// every `clawk up`, so host and guest can't drift within a release.
const ProtoVersion = 1

// Greeting is the first line of every connection.
type Greeting struct {
	// Op is OpControl or OpAttach.
	Op string `json:"op"`

	// V is the sender's ProtoVersion.
	V int `json:"v"`

	// Name is the guest-visible device name to attach to, without any
	// directory part — the same string the host published in a Snapshot.
	// Set for OpAttach only.
	Name string `json:"name,omitempty"`

	// Mode is the PTY's termios state at the moment the guest attached, so
	// the host can configure the port before the first byte moves rather
	// than opening at some default and correcting. Set for OpAttach only;
	// nil means "whatever the port already had".
	Mode *Mode `json:"mode,omitempty"`
}

// Greeting Op values.
const (
	OpControl = "control"
	OpAttach  = "attach"
)

// Snapshot is the host's reply on an OpControl connection: the complete set
// of forwarded serial devices as of now. Every update resends the full set —
// the guest reconciles against it rather than applying deltas, so a
// dropped-and-redialed control connection converges the same way.
type Snapshot struct {
	Devices []Device `json:"devices"`
}

// Device is one host serial port exposed in the guest.
type Device struct {
	// Name is the guest-visible name, created at /dev/<Name>. The host
	// path is deliberately not sent: the guest has no use for it and
	// shouldn't learn the shape of the host's /dev.
	Name string `json:"name"`
}

// AttachReply is the host's verdict on an OpAttach greeting. Frames flow
// only after ok=true; on ok=false the host closes the connection.
type AttachReply struct {
	OK bool `json:"ok"`
	// Error is a short human-readable reason when OK is false — an unknown
	// device name, or the port being unplugged or already held by another
	// process. The guest logs it; it's the only place an unplugged board
	// surfaces.
	Error string `json:"error,omitempty"`
}

// Mode is the line configuration of a serial port. It is the subset of
// termios that describes the wire format, which is all a remote end can
// meaningfully apply — flow control and the special characters belong to
// whichever end is doing the cooking.
type Mode struct {
	// Baud is the symbol rate. Both directions are always set to it:
	// split-speed serial has no users left and no way to express itself
	// through the PTY the guest reads this back from.
	Baud int `json:"baud"`

	// Bits is the character size, 5 through 8.
	Bits int `json:"bits"`

	// Parity is ParityNone, ParityEven or ParityOdd.
	Parity string `json:"parity"`

	// Stop is the number of stop bits, 1 or 2.
	Stop int `json:"stop"`
}

// Parity values.
const (
	ParityNone = "n"
	ParityEven = "e"
	ParityOdd  = "o"
)

// DefaultMode is what a port is configured to when the guest attaches
// without stating a mode. 9600-8N1 because that is what a tty defaults to
// nearly everywhere, so a guest tool that never calls tcsetattr sees the
// same thing it would on real hardware.
func DefaultMode() Mode {
	return Mode{Baud: 9600, Bits: 8, Parity: ParityNone, Stop: 1}
}

func (m Mode) String() string {
	return fmt.Sprintf("%d-%d%s%d", m.Baud, m.Bits, strings.ToUpper(m.Parity), m.Stop)
}

// Validate reports whether m is a line configuration that can actually be
// applied. It is called on the host before touching the port, because the
// values arrive from the guest: a nonsense character size would otherwise
// become a confusing tcsetattr failure at the far end of the stack.
func (m Mode) Validate() error {
	if m.Baud <= 0 {
		return fmt.Errorf("serialfwd: baud %d out of range", m.Baud)
	}
	if m.Bits < 5 || m.Bits > 8 {
		return fmt.Errorf("serialfwd: character size %d out of range (5-8)", m.Bits)
	}
	switch m.Parity {
	case ParityNone, ParityEven, ParityOdd:
	default:
		return fmt.Errorf("serialfwd: unknown parity %q", m.Parity)
	}
	if m.Stop != 1 && m.Stop != 2 {
		return fmt.Errorf("serialfwd: stop bits %d out of range (1-2)", m.Stop)
	}
	return nil
}

// ────────────────────────────────────────────────────────────────────────
// Framing
// ────────────────────────────────────────────────────────────────────────

// FrameType tags a post-handshake message.
type FrameType byte

const (
	// FrameData carries raw serial bytes, in either direction.
	FrameData FrameType = 0x01

	// FrameMode carries a JSON Mode, guest → host, when the guest's PTY
	// client changed the line configuration. Ordered with respect to the
	// FrameData frames around it, which is the entire reason frames exist
	// here — see the package comment.
	FrameMode FrameType = 0x02
)

func (t FrameType) String() string {
	switch t {
	case FrameData:
		return "data"
	case FrameMode:
		return "mode"
	default:
		return fmt.Sprintf("unknown(0x%02x)", byte(t))
	}
}

// FrameHeaderBytes is the fixed header: one type byte then a big-endian
// uint32 payload length.
const FrameHeaderBytes = 5

// MaxFrameBytes caps one frame's payload. Serial is slow — even at 921600
// baud a port produces ~90 KiB/s — so this is far above any real read, and
// exists only so a peer can't make the reader allocate without bound.
const MaxFrameBytes = 256 * 1024

// ErrFrameTooLong reports a frame past MaxFrameBytes. The connection must
// be closed: the reader has no way to resynchronise with the framing.
var ErrFrameTooLong = errors.New("serialfwd: frame exceeds MaxFrameBytes")

// WriteFrame writes one frame. Callers that interleave frames from several
// goroutines must serialise their own writes — a partially written frame
// desynchronises the stream for good.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	if len(payload) > MaxFrameBytes {
		return ErrFrameTooLong
	}
	var hdr [FrameHeaderBytes]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	// One Write for the header and one for the body rather than a copy
	// into a joined buffer: these go to a vsock conn, and the extra
	// allocation per data frame costs more than the extra syscall.
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// WriteModeFrame encodes m and writes it as a FrameMode.
func WriteModeFrame(w io.Writer, m Mode) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("serialfwd: encoding mode: %w", err)
	}
	return WriteFrame(w, FrameMode, b)
}

// ReadFrame reads one frame. The returned slice aliases a fresh allocation,
// so the caller may retain it.
//
// It takes a *bufio.Reader for the same reason ReadLine does: the handshake
// and the frames share a connection, and a reader that buffered past the
// greeting's newline holds bytes belonging to the first frame.
func ReadFrame(r *bufio.Reader) (FrameType, []byte, error) {
	var hdr [FrameHeaderBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrameBytes {
		return 0, nil, ErrFrameTooLong
	}
	if n == 0 {
		return FrameType(hdr[0]), nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		// A truncated payload is a protocol error, not a clean end: report
		// it as unexpected EOF so callers don't mistake it for the peer
		// hanging up tidily between frames.
		if errors.Is(err, io.EOF) {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return FrameType(hdr[0]), buf, nil
}

// ────────────────────────────────────────────────────────────────────────
// Handshake lines
// ────────────────────────────────────────────────────────────────────────

// MaxLineBytes caps one handshake line. Snapshots are a few dozen bytes per
// device; the cap exists so a peer can't make the reader allocate without
// bound.
const MaxLineBytes = 64 * 1024

// ErrLineTooLong reports a handshake line past MaxLineBytes. The connection
// must be closed — the reader is out of sync with the framing.
var ErrLineTooLong = errors.New("serialfwd: control line exceeds MaxLineBytes")

// WriteLine JSON-encodes v and writes it as one newline-terminated line.
func WriteLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("serialfwd: encoding %T: %w", v, err)
	}
	if len(b)+1 > MaxLineBytes {
		return ErrLineTooLong
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ReadLine reads one newline-terminated JSON line into v. The reader must
// be the one the caller keeps using — see ReadFrame.
func ReadLine(r *bufio.Reader, v any) error {
	line, err := r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return ErrLineTooLong
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(line, v); err != nil {
		return fmt.Errorf("serialfwd: decoding %T: %w", v, err)
	}
	return nil
}

// NewReader wraps r with a reader sized for MaxLineBytes, so ReadLine's
// buffer-full case really does mean "line too long" rather than "buffer too
// small".
func NewReader(r io.Reader) *bufio.Reader { return bufio.NewReaderSize(r, MaxLineBytes) }

// ValidDeviceName reports whether name is usable as a guest device name.
//
// The guest creates /dev/<name>, so anything with a directory part, a
// relative-path element, or a leading dot is refused — the host validates
// on the way in and the guest validates again on the way out, because this
// is the one field that becomes a filesystem path on the far side.
func ValidDeviceName(name string) error {
	switch {
	case name == "":
		return errors.New("serial: device name is empty")
	case len(name) > 64:
		return fmt.Errorf("serial: device name %q is longer than 64 characters", name)
	case strings.ContainsAny(name, "/\\"):
		return fmt.Errorf("serial: device name %q must not contain a path separator", name)
	case strings.HasPrefix(name, "."):
		return fmt.Errorf("serial: device name %q must not start with a dot", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return fmt.Errorf("serial: device name %q contains %q; "+
				"use letters, digits, dot, dash or underscore", name, r)
		}
	}
	return nil
}
