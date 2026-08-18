package sandbox

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

// allocatedBlocks is the file's real footprint on the host in 512-byte
// blocks — st_blocks, which stays at zero across a hole and grows only
// where something was actually written. st_size would report the whole
// ceiling and tell us nothing about what the sandbox costs.
func allocatedBlocks(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	st, ok := fi.Sys().(*syscall.Stat_t)
	require.True(t, ok, "stat is not a syscall.Stat_t")
	return int64(st.Blocks)
}

func TestSwapDiskMiB(t *testing.T) {
	tests := []struct {
		name string
		sb   *config.Sandbox
		want uint64
	}{
		{name: "nil sandbox takes the default", sb: nil, want: DefaultSwapSizeMiB},
		{name: "unset takes the default", sb: &config.Sandbox{}, want: DefaultSwapSizeMiB},
		{name: "explicit size wins", sb: &config.Sandbox{SwapMiB: 8192}, want: 8192},
		// Negative is the "off" sentinel the parser stores for `swap off`,
		// distinct from unset — otherwise disabling swap would be
		// indistinguishable from never having configured it.
		{name: "negative disables", sb: &config.Sandbox{SwapMiB: -1}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, SwapDiskMiB(tt.sb))
		})
	}
}

func TestEnsureSwapDisk(t *testing.T) {
	vmDir := t.TempDir()

	path, err := EnsureSwapDisk(vmDir, 64)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(vmDir, SwapDiskName), path)

	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, int64(64<<20), fi.Size(), "apparent size")

	// The point of the device is that it costs host bytes only as the guest
	// swaps into it. A freshly created one must be a hole, not 64 MiB of
	// zeros — st_blocks, not st_size, is what the host actually pays.
	require.Zero(t, allocatedBlocks(t, path), "a fresh swap disk must be sparse")

	// Re-running is idempotent, and a size change resizes in place.
	again, err := EnsureSwapDisk(vmDir, 64)
	require.NoError(t, err)
	require.Equal(t, path, again)

	_, err = EnsureSwapDisk(vmDir, 128)
	require.NoError(t, err)
	fi, err = os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, int64(128<<20), fi.Size(), "resized")

	// Shrinking works too: swap contents don't survive a boot, so there is
	// nothing to preserve and truncation keeps the file sparse.
	_, err = EnsureSwapDisk(vmDir, 32)
	require.NoError(t, err)
	fi, err = os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, int64(32<<20), fi.Size(), "shrunk")

	// Zero means "swap off": the device from a previous configuration has to
	// go, or buildSpec would keep finding a file it no longer attaches.
	empty, err := EnsureSwapDisk(vmDir, 0)
	require.NoError(t, err)
	require.Empty(t, empty)
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "swap disk removed")

	// Removing an absent device is not an error — the common case is a
	// sandbox that never had swap in the first place.
	empty, err = EnsureSwapDisk(vmDir, 0)
	require.NoError(t, err)
	require.Empty(t, empty)
}
