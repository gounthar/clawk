package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSerialAdd(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "ard", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
	}))

	out, err := executeCommand("serial", "add", "ard",
		"/dev/cu.usbmodem1101", "/dev/cu.usbserial-A50285BI:ttyUSB0")
	require.NoError(t, err)
	// Which name belongs on which side is the confusion the wording exists
	// to prevent, so it has to show up in the output.
	require.Contains(t, out, "/dev/cu.usbmodem1101 in the guest → /dev/cu.usbmodem1101")
	require.Contains(t, out, "/dev/ttyUSB0 in the guest → /dev/cu.usbserial-A50285BI")
	require.Contains(t, out, "applies on next 'clawk up'")

	sb, err := s.Load("ard")
	require.NoError(t, err)
	require.Equal(t, []config.SerialDevice{
		{HostPath: "/dev/cu.usbmodem1101", GuestName: "cu.usbmodem1101"},
		{HostPath: "/dev/cu.usbserial-A50285BI", GuestName: "ttyUSB0"},
	}, sb.Serials)
}

// A board that isn't plugged in right now is a perfectly reasonable thing
// to configure — it must be a note, not a failure.
func TestSerialAddAbsentDeviceIsANoteNotAnError(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "ard", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
	}))

	out, err := executeCommand("serial", "add", "ard", "/dev/cu.definitelyNotThere")
	require.NoError(t, err)
	require.Contains(t, out, "isn't there right now")

	sb, err := s.Load("ard")
	require.NoError(t, err)
	require.Len(t, sb.Serials, 1)
}

func TestSerialAddIsIdempotent(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "ard", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
	}))

	_, err := executeCommand("serial", "add", "ard", "/dev/cu.usbmodem1101")
	require.NoError(t, err)
	out, err := executeCommand("serial", "add", "ard", "/dev/cu.usbmodem1101")
	require.NoError(t, err)
	require.Contains(t, out, "already forwarded")

	sb, err := s.Load("ard")
	require.NoError(t, err)
	require.Len(t, sb.Serials, 1)
}

// One guest name can only be one device — the guest creates /dev/<name>
// once, and a second claim would silently lose in there.
func TestSerialAddRejectsGuestNameClash(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "ard", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
	}))

	_, err := executeCommand("serial", "add", "ard", "/dev/cu.usbmodem1101:ttyACM0")
	require.NoError(t, err)
	_, err = executeCommand("serial", "add", "ard", "/dev/cu.usbmodem2201:ttyACM0")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already forwarded from /dev/cu.usbmodem1101")

	sb, err := s.Load("ard")
	require.NoError(t, err)
	require.Len(t, sb.Serials, 1)
}

// The same port under two names would let the guest hold it twice, and the
// second open fails pointing at the wrong thing.
func TestSerialAddRejectsDuplicateHostPath(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "ard", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
	}))

	_, err := executeCommand("serial", "add", "ard", "/dev/cu.usbmodem1101:ttyACM0")
	require.NoError(t, err)
	_, err = executeCommand("serial", "add", "ard", "/dev/cu.usbmodem1101:ttyACM1")
	require.Error(t, err)
	require.Contains(t, err.Error(), `already forwarded as "ttyACM0"`)
}

func TestSerialRemove(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "ard", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
		Serials: []config.SerialDevice{
			{HostPath: "/dev/cu.usbmodem1101", GuestName: "ttyACM0"},
			{HostPath: "/dev/cu.usbserial-A50285BI", GuestName: "ttyUSB0"},
		},
	}))

	// Removal by guest name, which is what an error message inside the
	// guest would have shown the user.
	out, err := executeCommand("serial", "remove", "ard", "ttyACM0")
	require.NoError(t, err)
	require.Contains(t, out, "Serial device removed")

	sb, err := s.Load("ard")
	require.NoError(t, err)
	require.Equal(t, []config.SerialDevice{
		{HostPath: "/dev/cu.usbserial-A50285BI", GuestName: "ttyUSB0"},
	}, sb.Serials)
}

func TestSerialRemoveAcceptsEveryIdentifier(t *testing.T) {
	for _, id := range []string{
		"/dev/cu.usbmodem1101",         // host path
		"ttyACM0",                      // guest name
		"/dev/ttyACM0",                 // guest name as it appears in the VM
		"/dev/cu.usbmodem1101:ttyACM0", // the spec as added
	} {
		t.Run(id, func(t *testing.T) {
			s, _ := setupTest(t)
			require.NoError(t, s.Save(&config.Sandbox{
				Name: "ard", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
				Serials: []config.SerialDevice{
					{HostPath: "/dev/cu.usbmodem1101", GuestName: "ttyACM0"},
				},
			}))

			_, err := executeCommand("serial", "remove", "ard", id)
			require.NoError(t, err)

			sb, err := s.Load("ard")
			require.NoError(t, err)
			require.Empty(t, sb.Serials)
		})
	}
}

func TestSerialRemoveUnknownSaysSo(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "ard", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
		Serials: []config.SerialDevice{
			{HostPath: "/dev/cu.usbmodem1101", GuestName: "ttyACM0"},
		},
	}))

	out, err := executeCommand("serial", "remove", "ard", "ttyNOPE")
	require.NoError(t, err)
	require.Contains(t, out, "nothing matched")

	sb, err := s.Load("ard")
	require.NoError(t, err)
	require.Len(t, sb.Serials, 1)
}

func TestSerialListJSONIsNeverNull(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "ard", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
	}))

	out, err := executeCommand("serial", "list", "ard", "--json")
	require.NoError(t, err)
	var devices []config.SerialDevice
	require.NoError(t, json.Unmarshal([]byte(out), &devices))
	require.NotNil(t, devices)
	require.Empty(t, devices)
}

func TestSerialListShowsPresence(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "cu.present")
	require.NoError(t, os.WriteFile(present, nil, 0o600))

	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "ard", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
		Serials: []config.SerialDevice{
			{HostPath: present, GuestName: "ttyACM0"},
			{HostPath: filepath.Join(dir, "cu.absent"), GuestName: "ttyACM1"},
		},
	}))

	out, err := executeCommand("serial", "list", "ard")
	require.NoError(t, err)
	require.Regexp(t, `cu\.present.*/dev/ttyACM0\s+yes`, out)
	require.Regexp(t, `cu\.absent.*/dev/ttyACM1\s+no`, out)
}

// A glob resolves to whichever board is on the bus right now, and that is
// the question the PRESENT column exists to answer.
func TestSerialListResolvesGlob(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cu.usbmodem14201"), nil, 0o600))

	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "ard", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
		Serials: []config.SerialDevice{
			{HostPath: filepath.Join(dir, "cu.usbmodem*"), GuestName: "ttyACM0"},
		},
	}))

	out, err := executeCommand("serial", "list", "ard")
	require.NoError(t, err)
	require.Contains(t, out, "cu.usbmodem14201")
}

func TestParseSerialSpec(t *testing.T) {
	tests := []struct {
		name string
		spec string
		want config.SerialDevice
	}{
		{
			name: "bare path keeps its basename in the guest",
			spec: "/dev/cu.usbmodem1101",
			want: config.SerialDevice{HostPath: "/dev/cu.usbmodem1101", GuestName: "cu.usbmodem1101"},
		},
		{
			name: "explicit guest name",
			spec: "/dev/cu.usbmodem1101:ttyACM0",
			want: config.SerialDevice{HostPath: "/dev/cu.usbmodem1101", GuestName: "ttyACM0"},
		},
		{
			name: "glob with explicit name",
			spec: "/dev/cu.usbmodem*:ttyACM0",
			want: config.SerialDevice{HostPath: "/dev/cu.usbmodem*", GuestName: "ttyACM0"},
		},
		{
			name: "trailing colon falls back to the default name",
			spec: "/dev/ttyACM0:",
			want: config.SerialDevice{HostPath: "/dev/ttyACM0", GuestName: "ttyACM0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSerialSpec(tt.spec)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestParseSerialSpecRejects(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		contains string
	}{
		{"relative path", "cu.usbmodem1101", "absolute path"},
		{"empty", "", "no host path"},
		{"glob without a name", "/dev/cu.usbmodem*", "needs an explicit guest name"},
		// The guest name becomes /dev/<name>, so anything with a path in it
		// has to be refused here rather than inside the VM.
		{"guest name with a slash", "/dev/ttyACM0:sub/dir", "path separator"},
		{"guest name escaping /dev", "/dev/ttyACM0:../../etc/passwd", "path separator"},
		{"guest name starting with a dot", "/dev/ttyACM0:.hidden", "must not start with a dot"},
		{"guest name with a space", "/dev/ttyACM0:tty ACM0", "contains"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseSerialSpec(tt.spec)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.contains)
		})
	}
}
