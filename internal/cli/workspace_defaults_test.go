package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/template"
	"github.com/stretchr/testify/require"
)

// Workspace-position `env` and `agent` blocks used to be dropped on the floor:
// only repo Clawkfiles fed RequiredEnv and the agent docs. That position is
// also where the host-wide clawk.mod lands (see template/global.go), so the
// layer would have been silently half-applied.
func TestApplyWorkspaceLevelDefaults(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "house-rules.md"),
		[]byte("house rule"), 0o644))

	ws := &template.Workspace{
		Root: root,
		File: &template.Template{
			Env:          []string{"GLOBAL_TOKEN"},
			Instructions: []template.AgentDoc{{Path: "./house-rules.md"}},
			Memory:       []template.AgentDoc{{Text: "global memory"}},
		},
	}
	sb := &config.Sandbox{
		Name:         "x",
		RequiredEnv:  []string{"NS_TOKEN"},
		Instructions: []string{"namespace rule"},
		Memory:       "namespace memory",
	}

	require.NoError(t, applyWorkspaceLevelDefaults(sb, ws))

	// Scope-outward ordering: workspace (and the global layer under it) first,
	// then the namespace, then the repo — which for env is what decides
	// precedence, since the last occurrence of a name wins.
	require.Equal(t, []string{"GLOBAL_TOKEN", "NS_TOKEN"}, sb.RequiredEnv)
	require.Equal(t, []string{"house rule", "namespace rule"}, sb.Instructions)
	require.Equal(t, "global memory\n\nnamespace memory", sb.Memory)
}

func TestApplyWorkspaceLevelDefaultsMissingDoc(t *testing.T) {
	ws := &template.Workspace{
		Root: t.TempDir(),
		File: &template.Template{
			Instructions: []template.AgentDoc{{Path: "./absent.md"}},
		},
	}
	err := applyWorkspaceLevelDefaults(&config.Sandbox{Name: "x"}, ws)
	require.ErrorContains(t, err, "absent.md")
}
