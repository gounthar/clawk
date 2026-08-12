package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/machine"
	"github.com/stretchr/testify/require"
)

// TestCollectSandboxShares_ConsolidatesWorktrees pins the worktree device
// consolidation: every managed (non-in-place) worktree collapses into a
// single WorkspaceShareTag device backed by store.WorktreeDir, distinct
// source repos still get one src_<repo> alias each (deduped across worktrees
// of the same repo), and in-place phases keep their own device.
func TestCollectSandboxShares_ConsolidatesWorktrees(t *testing.T) {
	withTempStore(t)

	sb := &config.Sandbox{
		Name: "box",
		Phases: []config.Phase{
			{Worktree: "/wt/box/proj", Repo: "/code/proj"},
			{Worktree: "/wt/box/proj2", Repo: "/code/proj2"},
			// Second worktree of the same repo — its src alias must dedupe.
			{Worktree: "/wt/box/proj-2", Repo: "/code/proj"},
			{Worktree: "/code/here", Repo: "/code/here", InPlace: true},
		},
	}

	byTag := map[string]machine.Share{}
	for _, s := range collectSandboxShares(sb) {
		if _, dup := byTag[s.Tag]; dup {
			t.Fatalf("duplicate virtio-fs tag %q — vz requires unique tags per VM", s.Tag)
		}
		byTag[s.Tag] = s
	}

	// Exactly one consolidated worktree device, backed by the sandbox's
	// worktree parent dir — the whole point of the change.
	ws, ok := byTag[sandbox.WorkspaceShareTag]
	require.True(t, ok, "consolidated workspace share missing")
	require.Equal(t, store.WorktreeDir("box"), ws.HostPath)

	// Managed worktrees no longer get their own devices; they ride the parent.
	require.NotContains(t, byTag, "proj")
	require.NotContains(t, byTag, "proj2")
	require.NotContains(t, byTag, "proj-2")

	// One src alias per DISTINCT managed repo; the in-place repo gets none.
	require.Contains(t, byTag, "src_proj")
	require.Contains(t, byTag, "src_proj2")
	require.NotContains(t, byTag, "src_here")

	// In-place phase keeps its own device at its own host path.
	here, ok := byTag["here"]
	require.True(t, ok, "in-place worktree device missing")
	require.Equal(t, "/code/here", here.HostPath)
}

// TestCollectSandboxShares_InPlaceOnly verifies an in-place-only sandbox gets
// no consolidated parent device — WorkspaceRoot stays a plain guest dir and
// each in-place phase keeps its own device.
func TestCollectSandboxShares_InPlaceOnly(t *testing.T) {
	withTempStore(t)

	sb := &config.Sandbox{
		Name:   "box",
		Phases: []config.Phase{{Worktree: "/code/here", Repo: "/code/here", InPlace: true}},
	}

	byTag := map[string]bool{}
	for _, s := range collectSandboxShares(sb) {
		byTag[s.Tag] = true
	}
	require.NotContains(t, byTag, sandbox.WorkspaceShareTag,
		"in-place-only sandbox needs no consolidated parent")
	require.Contains(t, byTag, "here")
}

// TestCollectSandboxShares_AgentStateDevices pins the host-device side of the
// per-runner state mounts. Every entry in sandbox.AgentStateDirs must get a
// virtio-fs device here, or the guest manifest asks clawk-init to mount a tag
// vz never exposed — the runner then writes to the rootfs, which vz re-clones
// from the image on every boot, and its history disappears on `clawk up`.
func TestCollectSandboxShares_AgentStateDevices(t *testing.T) {
	withTempStore(t)

	sb := &config.Sandbox{Name: "box"}
	byTag := map[string]machine.Share{}
	for _, s := range collectSandboxShares(sb) {
		byTag[s.Tag] = s
	}

	stateRoot := store.StateDir("box")
	for _, d := range sandbox.AgentStateDirs {
		sh, ok := byTag[d.Tag]
		require.Truef(t, ok, "%s state device (tag %s) missing", d.Agent, d.Tag)
		require.Equal(t, filepath.Join(stateRoot, d.Sub), sh.HostPath,
			"%s state must come from the sandbox's host state dir, which survives destroy", d.Agent)
		require.Falsef(t, sh.ReadOnly, "%s state must be writable", d.Agent)
	}
}

// TestCollectSandboxShares_MatchesGuestManifest is the lock-step check the
// comment on collectSandboxShares demands: the vz device list and the guest
// mount manifest are built by two separate functions, and a tag in one but
// not the other is either a mount of a device that doesn't exist or a device
// nothing ever mounts. Both failures are silent at boot.
func TestCollectSandboxShares_MatchesGuestManifest(t *testing.T) {
	withTempStore(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude", "agents"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex", "skills"), 0o755))

	sb := &config.Sandbox{
		Name:  "box",
		Image: "golang:1.25",
		Phases: []config.Phase{
			{Worktree: filepath.Join(store.WorktreeDir("box"), "proj"), Repo: "/code/proj"},
			{Worktree: "/code/here", Repo: "/code/here", InPlace: true},
		},
		Shares: []config.HostShare{
			{HostPath: "/Users/u/.aws", GuestPath: sandbox.GuestHome + "/.aws", ReadOnly: true},
		},
	}

	deviceTags := map[string]bool{}
	for _, s := range collectSandboxShares(sb) {
		deviceTags[s.Tag] = true
	}

	m, err := sandbox.OCIGuestManifest(sb, store.StateDir("box"), store.CacheDir(), store.RootDir())
	require.NoError(t, err)
	mountTags := map[string]bool{}
	for _, mt := range m.Mounts {
		if mt.Block != "" {
			continue // block devices carry no virtio-fs tag
		}
		mountTags[mt.Tag] = true
	}

	for tag := range mountTags {
		require.Containsf(t, deviceTags, tag,
			"guest mounts tag %q but no virtio-fs device exposes it", tag)
	}
	for tag := range deviceTags {
		require.Containsf(t, mountTags, tag,
			"virtio-fs device %q is exposed but the guest never mounts it", tag)
	}
	require.Contains(t, mountTags, "codex_home", "test set up no agent state mounts")
}
