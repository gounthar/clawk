package cli

import (
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/template"
	"github.com/stretchr/testify/require"
)

func src(origin, host, guest string) serialSource {
	return serialSource{
		Origin: origin,
		Spec:   template.SerialSpec{HostPath: host, GuestName: guest},
	}
}

func TestComposeSerials(t *testing.T) {
	got, err := composeSerials([]serialSource{
		src("workspace", "/dev/cu.usbmodem1101", ""),
		src("firmware", "/dev/cu.usbserial-A50285BI", "ttyUSB0"),
	})
	require.NoError(t, err)
	require.Equal(t, []config.SerialDevice{
		{HostPath: "/dev/cu.usbmodem1101", GuestName: "cu.usbmodem1101"},
		{HostPath: "/dev/cu.usbserial-A50285BI", GuestName: "ttyUSB0"},
	}, got)
}

// The workspace and a repo both declaring the same board is ordinary, not a
// conflict — it only becomes one when they disagree.
func TestComposeSerialsIdenticalDuplicateIsFine(t *testing.T) {
	got, err := composeSerials([]serialSource{
		src("workspace", "/dev/cu.usbmodem1101", "ttyACM0"),
		src("firmware", "/dev/cu.usbmodem1101", "ttyACM0"),
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
}

// Both collision kinds must name their contributors: with several repos in
// a workspace, "something clashed" is not actionable.
func TestComposeSerialsRejectsNameClash(t *testing.T) {
	_, err := composeSerials([]serialSource{
		src("workspace", "/dev/cu.usbmodem1101", "ttyACM0"),
		src("firmware", "/dev/cu.usbmodem2201", "ttyACM0"),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "workspace")
	require.Contains(t, err.Error(), "firmware")
	require.Contains(t, err.Error(), "ttyACM0")
}

func TestComposeSerialsRejectsSamePortTwice(t *testing.T) {
	_, err := composeSerials([]serialSource{
		src("workspace", "/dev/cu.usbmodem1101", "ttyACM0"),
		src("firmware", "/dev/cu.usbmodem1101", "ttyACM1"),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "forwarded twice")
	require.Contains(t, err.Error(), "/dev/cu.usbmodem1101")
}

func TestComposeSerialsRejectsRelativePath(t *testing.T) {
	_, err := composeSerials([]serialSource{src("workspace", "cu.usbmodem1101", "")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "absolute path")
}

func TestComposeSerialsRejectsGlobWithoutName(t *testing.T) {
	_, err := composeSerials([]serialSource{src("workspace", "/dev/cu.usbmodem*", "")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "needs an explicit guest name")
}

// The name becomes /dev/<name> in the guest, so a clawk.mod is just as much
// an untrusted-ish input as the wire is — it may come from a cloned repo.
func TestComposeSerialsRejectsUnsafeGuestName(t *testing.T) {
	for _, name := range []string{"../escape", "sub/dir", ".hidden", "has space"} {
		t.Run(name, func(t *testing.T) {
			_, err := composeSerials([]serialSource{
				src("workspace", "/dev/cu.usbmodem1101", name),
			})
			require.Error(t, err)
		})
	}
}

func TestComposeSerialsEmpty(t *testing.T) {
	got, err := composeSerials(nil)
	require.NoError(t, err)
	require.Empty(t, got)
}
