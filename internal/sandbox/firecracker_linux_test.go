//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

// hasRestorableState gates whether Create skips building the disks. It must
// answer "no" unless EVERY disk buildSpec attaches is already there: skipping
// the build leaves whatever is on disk as the whole disk set, so a suspend
// state next to an incomplete one means a cold boot against a drive that
// doesn't exist, which firecracker refuses outright.
func TestHasRestorableState(t *testing.T) {
	root := t.TempDir()
	f := &FirecrackerProvider{store: config.NewStoreAt(root)}
	sb := &config.Sandbox{Name: "proj"}
	vmDir := f.vmDir(sb)
	rootfs := filepath.Join(vmDir, "rootfs.raw")

	touch := func(path string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))
	}

	require.False(t, f.hasRestorableState(sb, vmDir, rootfs), "no suspend state at all")

	touch(filepath.Join(vmDir, "suspend", "snapshot.state"))
	require.False(t, f.hasRestorableState(sb, vmDir, rootfs), "state but no rootfs")

	touch(rootfs)
	touch(filepath.Join(vmDir, "guestcfg.img"))
	require.False(t, f.hasRestorableState(sb, vmDir, rootfs),
		"a state saved before the worktree had its own disk must not be trusted")

	touch(f.worktreeDiskPath(sb))
	require.True(t, f.hasRestorableState(sb, vmDir, rootfs), "the full disk set is present")

	require.NoError(t, os.Remove(filepath.Join(vmDir, "guestcfg.img")))
	require.False(t, f.hasRestorableState(sb, vmDir, rootfs), "guestcfg.img is attached too")
}

func TestWorktreeDiskSize(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), make([]byte, 1024), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "b"), make([]byte, 2048), 0o644))
	// Symlinks and dirs must not be counted as content.
	require.NoError(t, os.Symlink("a", filepath.Join(dir, "link")))

	size, err := worktreeDiskSize(dir)
	require.NoError(t, err)
	// 3 KiB of content is dwarfed by the slack floor, which is the point:
	// small repos still get room for the agent to build in.
	require.Equal(t, int64(3072+worktreeDiskSlack), size)

	t.Run("large trees scale past the floor", func(t *testing.T) {
		// A tree bigger than the slack doubles instead of adding a constant.
		big := t.TempDir()
		f, err := os.Create(filepath.Join(big, "big"))
		require.NoError(t, err)
		// Sparse: Truncate reserves no blocks, but Size() reports it, which is
		// what the sizing walk reads.
		require.NoError(t, f.Truncate(worktreeDiskSlack+1))
		require.NoError(t, f.Close())

		size, err := worktreeDiskSize(big)
		require.NoError(t, err)
		require.Equal(t, int64(2*(worktreeDiskSlack+1)), size)
	})

	t.Run("empty tree still gets headroom", func(t *testing.T) {
		size, err := worktreeDiskSize(t.TempDir())
		require.NoError(t, err)
		require.Equal(t, int64(worktreeDiskSlack), size)
	})

	t.Run("missing tree is an error", func(t *testing.T) {
		_, err := worktreeDiskSize(filepath.Join(dir, "absent"))
		require.Error(t, err)
	})
}
