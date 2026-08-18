package cli

import (
	"testing"

	"github.com/clawkwork/clawk/internal/template"
	"github.com/stretchr/testify/require"
)

// TestResolveSwap covers the workspace-merge rule: the max size across the
// workspace file and every repo Clawkfile, with zero as "unset" — except
// that an explicit "off" (negative) beats any size, the same asymmetry
// resolveIdleTimeout has and for the same reason.
func TestResolveSwap(t *testing.T) {
	ws := func(fileMiB int64, repoMiB ...int64) *template.Workspace {
		w := &template.Workspace{File: &template.Template{SwapMiB: fileMiB}}
		for _, m := range repoMiB {
			w.Repos = append(w.Repos, template.Repo{
				Clawkfile: &template.Template{SwapMiB: m},
			})
		}
		return w
	}

	require.Equal(t, int64(0), resolveSwap(ws(0)), "unset everywhere stays unset")
	require.Equal(t, int64(8192), resolveSwap(ws(8192)), "workspace value")
	require.Equal(t, int64(16384), resolveSwap(ws(8192, 16384)), "larger repo wins")
	require.Equal(t, int64(8192), resolveSwap(ws(8192, 4096)), "smaller repo ignored")
	require.Equal(t, int64(-1), resolveSwap(ws(8192, -1)), "a repo's 'off' wins over a size")
	require.Equal(t, int64(-1), resolveSwap(ws(-1, 8192)), "the workspace's 'off' wins too")
	require.Equal(t, int64(-1), resolveSwap(ws(0, 4096, -1, 16384)), "off wins wherever it appears")

	// A repo with no Clawkfile must not panic or count.
	w := ws(2048)
	w.Repos = append(w.Repos, template.Repo{Clawkfile: nil})
	require.Equal(t, int64(2048), resolveSwap(w))
}
