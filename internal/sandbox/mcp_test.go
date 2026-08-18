package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

func TestRenderMCPConfigRemote(t *testing.T) {
	content, ok, err := RenderMCPConfig([]config.MCPServer{{
		Name:      "linear",
		Transport: config.MCPTransportHTTP,
		URL:       "https://mcp.linear.app/mcp",
		Headers:   []string{"Authorization: Bearer ${LINEAR_TOKEN}", "X-Env: prod"},
	}})
	require.NoError(t, err)
	require.True(t, ok)

	var got struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(content, &got))
	srv := got.MCPServers["linear"]
	require.Equal(t, "http", srv.Type)
	require.Equal(t, "https://mcp.linear.app/mcp", srv.URL)
	require.Equal(t, "Bearer ${LINEAR_TOKEN}", srv.Headers["Authorization"],
		"the ${VAR} reference must survive verbatim — the runner expands it in-guest")
	require.Equal(t, "prod", srv.Headers["X-Env"])
}

func TestRenderMCPConfigStdio(t *testing.T) {
	content, ok, err := RenderMCPConfig([]config.MCPServer{{
		Name:      "github",
		Transport: config.MCPTransportStdio,
		Command:   []string{"npx", "-y", "@modelcontextprotocol/server-github"},
		Env:       []string{"GITHUB_TOKEN"},
	}})
	require.NoError(t, err)
	require.True(t, ok)

	var got struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(content, &got))
	srv := got.MCPServers["github"]
	require.Equal(t, "npx", srv.Command)
	require.Equal(t, []string{"-y", "@modelcontextprotocol/server-github"}, srv.Args)
	require.Equal(t, map[string]string{"GITHUB_TOKEN": "${GITHUB_TOKEN}"}, srv.Env,
		"clawk names the variable; the runner's environment supplies the value")
	require.Empty(t, srv.Type, "stdio is implied by having a command")
}

// TestRenderMCPConfigCarriesNoSecrets is the invariant that lets this file
// live on host disk inside the sandbox state dir: it holds references, never
// values, exactly like config.Sandbox.RequiredEnv.
func TestRenderMCPConfigCarriesNoSecrets(t *testing.T) {
	t.Setenv("LINEAR_TOKEN", "super-secret-value")
	content, _, err := RenderMCPConfig([]config.MCPServer{{
		Name:      "linear",
		Transport: config.MCPTransportHTTP,
		URL:       "https://mcp.linear.app/mcp",
		Headers:   []string{"Authorization: Bearer ${LINEAR_TOKEN}"},
	}})
	require.NoError(t, err)
	require.NotContains(t, string(content), "super-secret-value")
	require.Contains(t, string(content), "${LINEAR_TOKEN}")
}

// TestRenderMCPConfigStable: the file is rewritten on every boot into a
// virtio-fs mount, so identical input must produce identical bytes.
func TestRenderMCPConfigStable(t *testing.T) {
	servers := []config.MCPServer{
		{Name: "b", Transport: config.MCPTransportHTTP, URL: "https://b/mcp"},
		{Name: "a", Transport: config.MCPTransportHTTP, URL: "https://a/mcp"},
	}
	first, _, err := RenderMCPConfig(servers)
	require.NoError(t, err)
	second, _, err := RenderMCPConfig(servers)
	require.NoError(t, err)
	require.Equal(t, string(first), string(second))
	require.Less(t, strings.Index(string(first), `"a"`), strings.Index(string(first), `"b"`),
		"keys are sorted, so declaration order can't churn the file")
}

func TestRenderMCPConfigRejectsIncomplete(t *testing.T) {
	_, _, err := RenderMCPConfig([]config.MCPServer{{Name: "x", Transport: config.MCPTransportStdio}})
	require.ErrorContains(t, err, "no command")

	_, _, err = RenderMCPConfig([]config.MCPServer{{Name: "x", Transport: config.MCPTransportHTTP}})
	require.ErrorContains(t, err, "no URL")

	_, _, err = RenderMCPConfig([]config.MCPServer{{Name: "x", Transport: "carrier-pigeon"}})
	require.ErrorContains(t, err, "unknown transport")
}

func TestRenderMCPConfigEmpty(t *testing.T) {
	content, ok, err := RenderMCPConfig(nil)
	require.NoError(t, err)
	require.False(t, ok)
	require.Nil(t, content)
}

// TestSeedClaudeMCP covers the full lifecycle at the path the guest sees
// through the ~/.claude mount: written before boot, refreshed on the next
// one, and removed when the declaration goes away.
func TestSeedClaudeMCP(t *testing.T) {
	stateRoot := t.TempDir()
	path := filepath.Join(stateRoot, "claude", "mcp", "clawk.json")

	require.NoError(t, SeedClaudeMCP(stateRoot, []config.MCPServer{{
		Name: "linear", Transport: config.MCPTransportHTTP, URL: "https://mcp.linear.app/mcp",
	}}))
	first, err := os.ReadFile(path)
	require.NoError(t, err, "seeded before the VM boots, so the dir is created on demand")
	require.Contains(t, string(first), "mcp.linear.app")

	// A clawk.mod edit propagates on the next up.
	require.NoError(t, SeedClaudeMCP(stateRoot, []config.MCPServer{{
		Name: "notion", Transport: config.MCPTransportHTTP, URL: "https://mcp.notion.com/mcp",
	}}))
	second, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(second), "mcp.notion.com")
	require.NotContains(t, string(second), "mcp.linear.app", "rewritten, not merged")

	// Dropping the block retires the servers rather than leaving a stale file.
	require.NoError(t, SeedClaudeMCP(stateRoot, nil))
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "stale config must not survive an emptied mcp block")

	// Clearing an already-absent file is not an error (every boot calls this).
	require.NoError(t, SeedClaudeMCP(stateRoot, nil))
}

func TestSeedClaudeMCPNoStateRoot(t *testing.T) {
	require.NoError(t, SeedClaudeMCP("", []config.MCPServer{{Name: "x"}}),
		"an opted-out state root is a no-op, matching the other seed functions")
}
