package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/guestcfg"
	"github.com/stretchr/testify/require"
)

// TestPersistentAgentSharesCoverEveryRunnerHome is the regression guard for
// the bug this file exists for: only claude's home was mounted from the host,
// so codex's sessions and login lived on the vz rootfs — which is re-cloned
// from the image on EVERY boot. `clawk down && clawk up` silently wiped codex
// history even though the docs promised it persisted. Each runner clawk
// claims to persist needs its own share here.
func TestPersistentAgentSharesCoverEveryRunnerHome(t *testing.T) {
	stateRoot := t.TempDir()

	byGuest := map[string]HostShare{}
	for _, sh := range PersistentAgentShares(stateRoot) {
		byGuest[sh.GuestPath] = sh
	}

	want := map[string]struct{ sub, tag string }{
		GuestHome + "/.claude": {"claude", "claude_home"},
		GuestHome + "/.codex":  {"codex", "codex_home"},
		GuestHome + "/.pi":     {"pi", "pi_home"},
		// opencode follows the XDG split, so it needs two: the data dir
		// (auth.json, mcp-auth.json, opencode.db) and the config dir.
		GuestHome + "/.local/share/opencode": {"opencode-data", "opencode_data"},
		GuestHome + "/.config/opencode":      {"opencode-config", "opencode_config"},
	}
	require.Len(t, byGuest, len(want), "one share per persisted runner home")
	for guest, w := range want {
		sh, ok := byGuest[guest]
		require.Truef(t, ok, "no persistent share mounted at %s", guest)
		require.Equal(t, filepath.Join(stateRoot, w.sub), sh.HostPath)
		require.Equal(t, w.tag, sh.Tag)
		require.Falsef(t, sh.ReadOnly, "%s must be writable — the runner writes its sessions there", guest)

		// virtiofs refuses a missing source path, so the host dir has to
		// exist by the time the share is handed to the provider.
		info, err := os.Stat(sh.HostPath)
		require.NoErrorf(t, err, "stat %s", sh.HostPath)
		require.Truef(t, info.IsDir(), "%s exists but is not a directory", sh.HostPath)
	}
}

// TestPersistentAgentSharesExcludeVolatileDirs pins the two opencode XDG
// dirs we deliberately leave on the disposable rootfs. ~/.local/state holds
// only locks/, and a lock that outlives the VM that took it is worse than
// none — a hard stop would strand one for the next boot. ~/.cache is a
// cache. Both look like "state opencode writes", so without this guard the
// obvious-seeming fix is to add them.
func TestPersistentAgentSharesExcludeVolatileDirs(t *testing.T) {
	for _, sh := range PersistentAgentShares(t.TempDir()) {
		require.NotContains(t, sh.GuestPath, "/.local/state/",
			"%s persists a lock directory across boots", sh.Tag)
		require.NotContains(t, sh.GuestPath, "/.cache/",
			"%s persists a cache directory, costing a PCIe device for no correctness gain", sh.Tag)
	}
}

func TestPersistentAgentSharesEmptyStateRoot(t *testing.T) {
	require.Nil(t, PersistentAgentShares(""), "empty state root opts out entirely")
}

// TestPersistentAgentSharesIdempotent pins that a second call (every boot
// rewrites the manifest) returns the identical list — a churned tag means the
// guest tries to mount a device vz never exposed.
func TestPersistentAgentSharesIdempotent(t *testing.T) {
	stateRoot := t.TempDir()
	require.Equal(t, PersistentAgentShares(stateRoot), PersistentAgentShares(stateRoot))
}

// TestAgentStateDirsUniqueTagsAndPaths guards the two uniqueness constraints
// the list carries: virtio-fs tags identify a device per VM, and two entries
// mounting the same guest path would have the second shadow the first.
func TestAgentStateDirsUniqueTagsAndPaths(t *testing.T) {
	tags, guests, subs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, d := range AgentStateDirs {
		require.NotEmpty(t, d.Agent, "every entry names its runner")
		require.Falsef(t, tags[d.Tag], "duplicate virtio-fs tag %q", d.Tag)
		require.Falsef(t, guests[d.GuestPath], "duplicate guest path %q", d.GuestPath)
		require.Falsef(t, subs[d.Sub], "duplicate state subdir %q", d.Sub)
		require.Truef(t, strings.HasPrefix(d.GuestPath, GuestHome+"/"),
			"%s must live under the agent's home so clawk-init chowns it", d.GuestPath)
		tags[d.Tag], guests[d.GuestPath], subs[d.Sub] = true, true, true
	}
}

// TestPersistentAgentSharesPrecedeCapabilitySubmounts pins the ordering rule
// both share assemblers depend on. DefaultHostShares mounts capability dirs
// (~/.claude/agents, ~/.codex/skills) that live INSIDE the persisted homes;
// clawk-init mounts in list order, so a home mounted afterwards would shadow
// the capability dir already mounted under it.
func TestPersistentAgentSharesPrecedeCapabilitySubmounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, sub := range []string{".claude/agents", ".claude/commands", ".codex/skills"} {
		require.NoError(t, os.MkdirAll(filepath.Join(home, sub), 0o755))
	}

	// Same assembly order as OCIGuestManifest and collectSandboxShares.
	assembled := append([]HostShare{}, PersistentAgentShares(t.TempDir())...)
	assembled = append(assembled, DefaultHostShares()...)

	idx := map[string]int{}
	for i, sh := range assembled {
		idx[sh.GuestPath] = i
	}
	var checked int
	for _, child := range assembled {
		for _, parent := range assembled {
			if child.GuestPath == parent.GuestPath ||
				!strings.HasPrefix(child.GuestPath, parent.GuestPath+"/") {
				continue
			}
			checked++
			require.Lessf(t, idx[parent.GuestPath], idx[child.GuestPath],
				"parent %s must be mounted before its sub-mount %s",
				parent.GuestPath, child.GuestPath)
		}
	}
	require.Positivef(t, checked, "test set up no nested mounts — it would pass vacuously")
}

// TestOCIGuestManifestMountsEveryAgentHome ties the guest side to the list:
// a runner added to AgentStateDirs but missing from the manifest would write
// to the disposable rootfs instead, which is exactly how codex lost its
// history.
func TestOCIGuestManifestMountsEveryAgentHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex", "skills"), 0o755))

	sb := &config.Sandbox{Name: "box", Image: "golang:1.25"}
	m, err := OCIGuestManifest(sb, t.TempDir(), "", t.TempDir())
	require.NoError(t, err)

	idx := map[string]int{}
	mounts := map[string]guestcfg.Mount{}
	for i, mt := range m.Mounts {
		idx[mt.Tag] = i
		mounts[mt.Tag] = mt
	}
	for _, d := range AgentStateDirs {
		mt, ok := mounts[d.Tag]
		require.Truef(t, ok, "%s state (tag %s) missing from the guest manifest", d.Agent, d.Tag)
		require.Equal(t, d.GuestPath, mt.Path)
		require.Falsef(t, mt.ReadOnly, "%s state must be writable", d.Agent)
	}
	// The one nested pair the default shares produce, spelled out so a
	// reordering of the assembly in OCIGuestManifest fails loudly here.
	require.Contains(t, idx, "codex_skills")
	require.Less(t, idx["codex_home"], idx["codex_skills"],
		"~/.codex must mount before ~/.codex/skills or the skills mount is shadowed")
}
