package agentembed

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/clawkwork/clawk/internal/serialfwd"
	"github.com/stretchr/testify/require"
)

// The guest agent builds standalone inside the guest, so it can't import
// internal/serialfwd — the serial protocol is transcribed into main.go.in by
// hand. Nothing else notices when the two drift: a renamed JSON field or a
// moved frame type compiles fine on both sides and fails only as a sandbox
// where a board is visible but mute.
//
// So: check the parts that have to match byte-for-byte against the
// parent-module definition, deriving them from the real declarations rather
// than restating them, so this test can't drift either. Same rule and same
// shape as TestReverseForwardProtocolMirroredInAgent.
func TestSerialProtocolMirroredInAgent(t *testing.T) {
	src := string(AgentMainGo)

	require.Contains(t, src, fmt.Sprintf("serialVSockPort = %d", serialfwd.VSockPort),
		"guest dials a different vsock port than the host listens on")
	require.Contains(t, src, fmt.Sprintf("serialProtoVersion = %d", serialfwd.ProtoVersion),
		"guest announces a protocol version the host will hang up on")
	require.Contains(t, src, fmt.Sprintf("serialMaxLine = %d", serialfwd.MaxLineBytes/1024)+" * 1024",
		"guest and host disagree on the control-line cap")

	// Framing constants. A mismatch here desynchronises the stream rather
	// than failing cleanly, which is the worst way for this to break.
	require.Contains(t, src, fmt.Sprintf("serialFrameHeaderBytes = %d", serialfwd.FrameHeaderBytes),
		"guest frames a different header length than the host parses")
	require.Contains(t, src, fmt.Sprintf("serialFrameData byte = 0x%02x", byte(serialfwd.FrameData)),
		"guest tags data frames differently from the host")
	require.Contains(t, src, fmt.Sprintf("serialFrameMode byte = 0x%02x", byte(serialfwd.FrameMode)),
		"guest tags mode frames differently from the host")

	// Op values are string literals on the wire, sent by the guest and
	// switched on by the host.
	for _, op := range []string{serialfwd.OpControl, serialfwd.OpAttach} {
		require.Contains(t, src, fmt.Sprintf("Op: %q", op),
			"guest never sends the %q greeting", op)
	}

	// Parity travels as a one-letter string; a guest that spelled it
	// differently would be rejected by the host's mode validation and the
	// port would silently keep its old settings.
	for _, parity := range []string{serialfwd.ParityNone, serialfwd.ParityEven, serialfwd.ParityOdd} {
		require.Contains(t, src, fmt.Sprintf("%q", parity),
			"guest never produces parity %q", parity)
	}

	for _, typ := range []any{
		serialfwd.Greeting{}, serialfwd.Snapshot{}, serialfwd.Device{},
		serialfwd.AttachReply{}, serialfwd.Mode{},
	} {
		rt := reflect.TypeOf(typ)
		for i := range rt.NumField() {
			tag, ok := rt.Field(i).Tag.Lookup("json")
			if !ok {
				continue
			}
			want := fmt.Sprintf("`json:%q`", tag)
			require.Contains(t, src, want,
				"guest is missing the %s.%s field tag %s",
				rt.Name(), rt.Field(i).Name, want)
		}
	}
}

// The guest turns a published device name into /dev/<name>. The host
// validates it first, but the guest must not rely on that — this checks the
// second line of defence is actually wired up rather than merely present.
func TestSerialDeviceNameValidatedInAgent(t *testing.T) {
	src := string(AgentMainGo)
	require.Contains(t, src, "func validSerialName(",
		"guest has no device-name validation")
	require.Contains(t, src, "if err := validSerialName(d.Name); err != nil",
		"guest validates device names but never calls the validator on a published set")
}
