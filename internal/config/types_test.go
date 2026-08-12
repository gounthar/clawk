package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSandboxDisplayName(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{"named sandbox is unchanged", "my-feature", "my-feature"},
		{"anchored basename key is unchanged", "myproj", "myproj"},
		{"collision-disambiguated key is unchanged", "shared_a1b2c3", "shared_a1b2c3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := &Sandbox{Name: tt.key}
			require.Equal(t, tt.want, sb.DisplayName())
		})
	}
}

func TestSandboxNamespaceName(t *testing.T) {
	tests := []struct {
		name string
		ns   string
		want string
	}{
		{"empty resolves to default", "", DefaultNamespace},
		{"explicit value passes through", "team-a", "team-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := &Sandbox{Namespace: tt.ns}
			require.Equal(t, tt.want, sb.NamespaceName())
		})
	}
}

func TestDefaultSerialGuestName(t *testing.T) {
	tests := []struct {
		name     string
		hostPath string
		want     string
	}{
		{"macOS callout device", "/dev/cu.usbmodem1101", "cu.usbmodem1101"},
		{"linux acm device", "/dev/ttyACM0", "ttyACM0"},
		{"bare name", "ttyUSB0", "ttyUSB0"},
		// A glob has no basename worth defaulting to — the caller must
		// insist on an explicit guest name rather than inventing one.
		{"glob has no default", "/dev/cu.usbmodem*", ""},
		{"character class glob", "/dev/ttyUSB[01]", ""},
		{"single-char glob", "/dev/ttyUSB?", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, DefaultSerialGuestName(tt.hostPath))
		})
	}
}

func TestSerialDeviceString(t *testing.T) {
	// The default guest name collapses back to the bare host path, so a
	// round-tripped spec reads the way the user typed it.
	require.Equal(t, "/dev/cu.usbmodem1101",
		SerialDevice{HostPath: "/dev/cu.usbmodem1101", GuestName: "cu.usbmodem1101"}.String())
	require.Equal(t, "/dev/cu.usbmodem1101:ttyACM0",
		SerialDevice{HostPath: "/dev/cu.usbmodem1101", GuestName: "ttyACM0"}.String())
	// A glob never has a default name, so it always shows both halves.
	require.Equal(t, "/dev/cu.usbmodem*:ttyACM0",
		SerialDevice{HostPath: "/dev/cu.usbmodem*", GuestName: "ttyACM0"}.String())
}

func TestSerialDeviceIsGlob(t *testing.T) {
	require.False(t, SerialDevice{HostPath: "/dev/cu.usbmodem1101"}.IsGlob())
	require.True(t, SerialDevice{HostPath: "/dev/cu.usbmodem*"}.IsGlob())
	require.True(t, SerialDevice{HostPath: "/dev/ttyUSB[01]"}.IsGlob())
	require.True(t, SerialDevice{HostPath: "/dev/ttyUSB?"}.IsGlob())
}
