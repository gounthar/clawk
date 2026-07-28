package oci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// The image is validated with debugfs (see debugfsRun in
// integration_test.go): read-only, unprivileged, no loop mount — the same
// property that makes WriteDirDisk worth having.
func TestWriteDirDisk(t *testing.T) {
	requireTool(t, "debugfs")

	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "sub", "deep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "README.md"), []byte("hello worktree\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "sub", "deep", "nested.txt"), []byte("deep\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "build.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.Symlink("README.md", filepath.Join(src, "link-to-readme")))
	// An empty dir must survive: a git worktree has plenty.
	require.NoError(t, os.MkdirAll(filepath.Join(src, "empty"), 0o755))

	disk := filepath.Join(t.TempDir(), "worktree.img")
	require.NoError(t, WriteDirDisk(src, disk, 32<<20))

	st, err := os.Stat(disk)
	require.NoError(t, err)
	require.GreaterOrEqual(t, st.Size(), int64(32<<20), "image should be padded to the requested size")

	t.Run("content and layout", func(t *testing.T) {
		require.Contains(t, debugfsRun(t, disk, "cat README.md"), "hello worktree")
		require.Contains(t, debugfsRun(t, disk, "cat sub/deep/nested.txt"), "deep")
		root := debugfsRun(t, disk, "ls -l /")
		for _, want := range []string{"README.md", "build.sh", "sub", "empty", "link-to-readme"} {
			require.Contains(t, root, want)
		}
	})

	t.Run("modes and ownership survive", func(t *testing.T) {
		// stat prints e.g. "Mode:  0755" and "User:  1000   Group:  1000".
		require.Contains(t, debugfsRun(t, disk, "stat build.sh"), "0755")
		require.Contains(t, debugfsRun(t, disk, "stat sub/deep/nested.txt"), "0600")
		// debugfs column-pads: "User:   501   Group:    20".
		out := debugfsRun(t, disk, "stat README.md")
		require.Regexp(t,
			`User:\s+`+strconv.Itoa(os.Getuid())+`\s+Group:\s+`+strconv.Itoa(os.Getgid()),
			out, "host uid/gid must be preserved (cp -a parity)")
	})

	// The regression that motivated emitting the root's own tar entry: the ext4
	// writer creates its root inode as 0755 root:root, so without it the guest
	// user owns every file in the tree but cannot create anything AT the root —
	// no `git init`, no node_modules, no new top-level file — while `ls -l`
	// looks entirely normal.
	t.Run("root directory is not root-owned", func(t *testing.T) {
		require.NoError(t, os.Chmod(src, 0o750))
		disk := filepath.Join(t.TempDir(), "root-owner.img")
		require.NoError(t, WriteDirDisk(src, disk, 8<<20))

		out := debugfsRun(t, disk, "stat <2>") // inode 2 is the ext4 root
		require.Regexp(t,
			`User:\s+`+strconv.Itoa(os.Getuid())+`\s+Group:\s+`+strconv.Itoa(os.Getgid()),
			out, "the image root must belong to the source's owner, not root:\n%s", out)
		require.Contains(t, out, "0750", "the source root's mode must survive too:\n%s", out)
	})

	t.Run("symlink is a symlink", func(t *testing.T) {
		out := debugfsRun(t, disk, "stat link-to-readme")
		require.Contains(t, out, "symlink")
		require.Contains(t, out, "README.md", "link target should be stored:\n%s", out)
	})

	t.Run("filesystem is consistent and writable", func(t *testing.T) {
		requireTool(t, "e2fsck")
		// -fn: force a full check, answer no to every repair prompt. Exit
		// codes: 0 clean, 4 = errors left uncorrected (we answered no).
		out, err := exec.Command("e2fsck", "-fn", disk).CombinedOutput()
		if err != nil {
			var code int
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
			require.Failf(t, "e2fsck reported problems", "exit=%d\n%s", code, out)
		}
		// A read-only-compat image would make the guest's rw mount fail.
		require.NotContains(t, debugfsRun(t, disk, "features"), "read-only")
	})
}

func TestWriteDirDiskRejectsNonDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

	err := WriteDirDisk(file, filepath.Join(dir, "out.img"), 1<<20)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a directory")

	err = WriteDirDisk(filepath.Join(dir, "absent"), filepath.Join(dir, "out.img"), 1<<20)
	require.Error(t, err)
}

// TestWriteDirDiskFollowsSymlinkedSource covers the quietest way this can go
// wrong: os.Stat follows symlinks and filepath.WalkDir does not, so a srcDir
// whose last component is a link to a directory used to pass validation and
// then yield a single walk entry — a well-formed, EMPTY image, no error.
func TestWriteDirDiskFollowsSymlinkedSource(t *testing.T) {
	requireTool(t, "debugfs")

	real := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(real, "README.md"), []byte("hi\n"), 0o644))
	link := filepath.Join(t.TempDir(), "worktree")
	require.NoError(t, os.Symlink(real, link))

	disk := filepath.Join(t.TempDir(), "out.img")
	require.NoError(t, WriteDirDisk(link, disk, 4<<20))
	require.Contains(t, debugfsRun(t, disk, "cat README.md"), "hi",
		"a symlinked source must be walked through, not silently produce an empty image")
}

// TestWriteDirDiskNoPartialOnFailure proves the .tmp + rename discipline:
// a failed build leaves no file at the destination path.
func TestWriteDirDiskNoPartialOnFailure(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "sub", "out.img")
	// Size 0 with content still converts, so force failure differently: an
	// unwritable parent.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o500))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "sub"), 0o700) })
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a"), []byte("a"), 0o644))

	require.Error(t, WriteDirDisk(src, dst, 1<<20))
	_, err := os.Stat(dst)
	require.True(t, os.IsNotExist(err), "no partial image should remain")
}
