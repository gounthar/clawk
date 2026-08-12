package cli

import (
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/stretchr/testify/require"
)

// TestPiRunnerRegistered pins the pi harness's registry entry. pi ships no
// built-in sandbox (its security docs say isolation must come from a VM,
// which is what clawk provides), so the only thing to pre-answer is project
// trust: without --approve a repo carrying .pi/settings.json or .pi/extensions
// stops the run on an interactive prompt that means nothing inside a sandbox.
func TestPiRunnerRegistered(t *testing.T) {
	pi, err := agentByName("pi")
	require.NoError(t, err)
	require.Equal(t, []string{"--approve"}, pi.DefaultArgs)
	require.Empty(t, pi.MCPConfigFlag,
		"pi loads MCP through an extension, not a config-file flag")

	// The user's own args land after clawk's, so an explicit --no-approve
	// still wins.
	withMCP := &config.Sandbox{MCP: []config.MCPServer{{Name: "linear"}}}
	require.Equal(t, []string{"--approve", "--no-approve"},
		launchArgs(withMCP, pi, []string{"--no-approve"}),
		"a runner clawk can't render MCP config for gets no flag, and user args go last")
}

// TestAgentRegistryNames guards the invariants the registry's doc comment
// states: names are unique, and each is usable both as a `clawk run` argument
// and as a bare command on the guest's PATH.
func TestAgentRegistryNames(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range agents {
		require.Falsef(t, seen[a.Name], "duplicate runner name %q", a.Name)
		seen[a.Name] = true
		require.Equalf(t, strings.ToLower(a.Name), a.Name, "runner %q must be lowercase", a.Name)
		require.NotContainsf(t, a.Name, " ", "runner %q must not contain whitespace", a.Name)
	}
	require.Subset(t, seen, map[string]bool{"claude": true, "codex": true, "pi": true, "opencode": true})

	// Runner names are reserved as sandbox names — a sandbox called "pi"
	// would make `clawk run pi` ambiguous.
	require.Contains(t, reservedAgentNames(), "pi")
}

// TestAgentStateDirsNameRealRunners keeps the persistence list honest: every
// home directory clawk mounts must belong to a runner someone can actually
// launch. A typo there costs a PCIe device per sandbox and persists nothing.
func TestAgentStateDirsNameRealRunners(t *testing.T) {
	for _, d := range sandbox.AgentStateDirs {
		_, err := agentByName(d.Agent)
		require.NoErrorf(t, err, "AgentStateDirs names %q, which is not a registered runner", d.Agent)
	}
}
