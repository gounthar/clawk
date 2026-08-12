package template

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSerialBlock(t *testing.T) {
	tmpl, err := parseBody(`serial (
    /dev/cu.usbmodem1101
    /dev/cu.usbserial-A50285BI ttyUSB0
    /dev/cu.usbmodem* ttyACM0
)
`)
	require.NoError(t, err)
	require.Len(t, tmpl.Serials, 3)

	require.Equal(t, "/dev/cu.usbmodem1101", tmpl.Serials[0].HostPath)
	require.Empty(t, tmpl.Serials[0].GuestName, "a bare entry defaults its name downstream")

	require.Equal(t, "/dev/cu.usbserial-A50285BI", tmpl.Serials[1].HostPath)
	require.Equal(t, "ttyUSB0", tmpl.Serials[1].GuestName)

	require.Equal(t, "/dev/cu.usbmodem*", tmpl.Serials[2].HostPath)
	require.Equal(t, "ttyACM0", tmpl.Serials[2].GuestName)

	// Positions are recorded so a downstream name clash can point back at
	// the line that caused it.
	require.NotZero(t, tmpl.Serials[0].Line)
}

func TestParseSerialBlockRejectsThirdToken(t *testing.T) {
	_, err := parseBody("serial (\n    /dev/cu.usbmodem1101 ttyACM0 extra\n)\n")
	require.Error(t, err)
	require.Contains(t, err.Error(), "one host device and optional guest name per line")
}

func TestParseSerialBlockRequiresParen(t *testing.T) {
	_, err := parseBody("serial /dev/cu.usbmodem1101\n")
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected '(' after 'serial'")
}

// The directive has to be listed in the unknown-directive hint, or a typo
// sends the user looking for a feature the error says doesn't exist.
func TestUnknownDirectiveMentionsSerial(t *testing.T) {
	_, err := parseBody("serialz (\n    /dev/cu.usbmodem1101\n)\n")
	require.Error(t, err)
	require.Contains(t, err.Error(), `"serial"`)
}
