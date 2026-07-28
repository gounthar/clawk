//go:build linux

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeLink materializes one sysfs-shaped link directory: <root>/<name>/flags
// (and a master symlink when master != "").
func fakeLink(t *testing.T, root, name, flags, master string) {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	if flags != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "flags"), []byte(flags), 0o644))
	}
	if master != "" {
		// sysfs points at ../../<bridge>; only the base name is read.
		require.NoError(t, os.Symlink(filepath.Join("..", master), filepath.Join(dir, "master")))
	}
}

func TestLinkExistsIn(t *testing.T) {
	root := t.TempDir()
	fakeLink(t, root, "clawkbr0", "0x1003", "")
	require.NoError(t, os.WriteFile(filepath.Join(root, "notadir"), nil, 0o644))

	require.True(t, linkExistsIn(root, "clawkbr0"))
	require.False(t, linkExistsIn(root, "clawkbr1"))
	require.False(t, linkExistsIn(root, "notadir"), "a plain file is not a link")
}

func TestLinkIsUpIn(t *testing.T) {
	root := t.TempDir()
	// A bridge with no carrier: administratively up (IFF_UP set) even though
	// operstate would read "down". This is the case that must NOT re-run the
	// privileged `ip link set up` on every boot.
	fakeLink(t, root, "carrierless", "0x1003", "")
	fakeLink(t, root, "down", "0x1002", "")
	fakeLink(t, root, "lo", "0x9", "")
	fakeLink(t, root, "noflags", "", "")
	fakeLink(t, root, "garbage", "not-a-number", "")

	require.True(t, linkIsUpIn(root, "carrierless"))
	require.True(t, linkIsUpIn(root, "lo"))
	require.False(t, linkIsUpIn(root, "down"))
	require.False(t, linkIsUpIn(root, "noflags"), "unreadable flags must fail open (re-issue the call)")
	require.False(t, linkIsUpIn(root, "garbage"))
	require.False(t, linkIsUpIn(root, "absent"))
}

func TestLinkMasterIn(t *testing.T) {
	root := t.TempDir()
	fakeLink(t, root, "enslaved", "0x1003", "clawkbr0")
	fakeLink(t, root, "free", "0x1003", "")

	require.Equal(t, "clawkbr0", linkMasterIn(root, "enslaved"))
	require.Equal(t, "", linkMasterIn(root, "free"))
	require.Equal(t, "", linkMasterIn(root, "absent"))
}

func TestBridgeMemberCountIn(t *testing.T) {
	root := t.TempDir()
	fakeLink(t, root, "empty-bridge", "0x1003", "")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "empty-bridge", "brif"), 0o755))
	fakeLink(t, root, "busy-bridge", "0x1003", "")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "busy-bridge", "brif"), 0o755))
	require.NoError(t, os.Symlink("../../tap0", filepath.Join(root, "busy-bridge", "brif", "tap0")))
	fakeLink(t, root, "not-a-bridge", "0x1003", "")

	require.Equal(t, 0, bridgeMemberCountIn(root, "empty-bridge"))
	require.Equal(t, 1, bridgeMemberCountIn(root, "busy-bridge"))
	require.Equal(t, -1, bridgeMemberCountIn(root, "not-a-bridge"), "no brif → not a bridge")
	require.Equal(t, -1, bridgeMemberCountIn(root, "absent"))
}

func TestBridgeDevice(t *testing.T) {
	a, b := bridgeDevice("proj-a"), bridgeDevice("proj-b")
	require.NotEqual(t, a, b, "distinct sandboxes need distinct L2 segments")
	require.Equal(t, a, bridgeDevice("proj-a"), "must be stable across boots")
	require.NotEqual(t, legacyLinuxBridge, a)
	for _, name := range []string{"a", "proj-a", "a-very-long-sandbox-name-indeed"} {
		dev := bridgeDevice(name)
		require.LessOrEqual(t, len(dev), 15, "IFNAMSIZ-1 exceeded: %q", dev)
		require.True(t, strings.HasPrefix(dev, "clawkbr"), dev)
	}
	// The bridge and the TAPs on it must never be the same device name.
	require.NotEqual(t, bridgeDevice("x"), tapDevice("x"))
	require.NotEqual(t, bridgeDevice("x"), gvTapDevice("x"))
}

// EVERY host device name has to carry the uid, not just the bridge. A TAP name
// that collides across users is worse than a clash: the second user's `clawk
// up` finds the device present, skips creation, and re-enslaves the first
// user's live TAP to its own bridge — killing that VM's network — and either
// user's `clawk destroy` deletes the other's devices.
func TestDeviceNamesCarryTheUID(t *testing.T) {
	sum := sha256.Sum256([]byte(strconv.Itoa(os.Getuid()) + ":proj-a"))
	want := hex.EncodeToString(sum[:4])

	require.Equal(t, "clawkbr"+want, bridgeDevice("proj-a"))
	require.Equal(t, "clawk"+want, tapDevice("proj-a"))
	require.Equal(t, "clawk"+want+"g", gvTapDevice("proj-a"))

	for _, name := range []string{"a", "proj-a", "a-very-long-sandbox-name-indeed"} {
		for _, dev := range []string{tapDevice(name), gvTapDevice(name)} {
			require.LessOrEqual(t, len(dev), 15, "IFNAMSIZ-1 exceeded: %q", dev)
		}
	}
}

// The real sysfs must parse with the same readers — the fixtures above only
// prove the parsing, not that the format guess is right. Every Linux host has
// lo, and lo is always administratively up.
func TestLinkReadersAgainstRealSysfs(t *testing.T) {
	if _, err := os.Stat(filepath.Join(sysClassNet, "lo")); err != nil {
		t.Skip("no /sys/class/net/lo")
	}
	require.True(t, linkExists("lo"))
	require.True(t, linkIsUp("lo"), "lo is always up")
	require.Equal(t, "", linkMaster("lo"), "lo has no master")
	require.False(t, linkExists("clawk-nonexistent-device"))
}
