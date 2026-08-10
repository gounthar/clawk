package cli

import (
	"testing"

	"github.com/clawkwork/clawk/internal/template"
	"github.com/stretchr/testify/require"
)

// TestResolveDisk covers the workspace-merge rule: the shared rootfs takes
// the max DiskMiB across the workspace file and every repo Clawkfile, with
// zero as the "unset" sentinel.
func TestResolveDisk(t *testing.T) {
	// ws builds a workspace from a workspace-level DiskMiB plus one repo per
	// extra value, mirroring idle_test.go's helper.
	ws := func(fileMiB uint64, repoMiB ...uint64) *template.Workspace {
		w := &template.Workspace{File: &template.Template{DiskMiB: fileMiB}}
		for _, m := range repoMiB {
			w.Repos = append(w.Repos, template.Repo{
				Clawkfile: &template.Template{DiskMiB: m},
			})
		}
		return w
	}

	require.Equal(t, uint64(0), resolveDisk(ws(0)), "unset everywhere stays unset")
	require.Equal(t, uint64(32*1024), resolveDisk(ws(32*1024)), "workspace value")
	require.Equal(t, uint64(64*1024), resolveDisk(ws(32*1024, 64*1024)), "larger repo wins")
	require.Equal(t, uint64(32*1024), resolveDisk(ws(32*1024, 16*1024)), "smaller repo ignored")
	require.Equal(t, uint64(48*1024), resolveDisk(ws(0, 16*1024, 48*1024)), "max across repos")

	// A repo with no Clawkfile must not panic or count.
	w := ws(24 * 1024)
	w.Repos = append(w.Repos, template.Repo{Clawkfile: nil})
	require.Equal(t, uint64(24*1024), resolveDisk(w))
}

// TestValidateDisk locks the floor: zero (unset) is fine, anything from
// 1 GiB up is fine, and a sub-1-GiB value (the classic 32M-means-32G unit
// typo) is rejected.
func TestValidateDisk(t *testing.T) {
	require.NoError(t, validateDisk(0), "unset is allowed")
	require.NoError(t, validateDisk(1024), "exactly 1 GiB")
	require.NoError(t, validateDisk(64*1024))
	require.Error(t, validateDisk(32), "32 MiB (likely 32G typo) rejected")
	require.Error(t, validateDisk(minDiskMiB-1))
}
