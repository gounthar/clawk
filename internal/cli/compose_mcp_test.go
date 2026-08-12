package cli

import (
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/template"
	"github.com/stretchr/testify/require"
)

// TestMCPPipelineEndToEnd walks a realistic clawk.mod through every stage
// that stands between the file and a working server — parse, compose, derive
// egress, render the guest config, build the runner's argv — because each of
// those lives in a different package and a field dropped between two of them
// is exactly the kind of gap unit tests on either side both pass through.
func TestMCPPipelineEndToEnd(t *testing.T) {
	tmpl, err := template.ParseString(`sandbox proj (
    mcp (
        linear https://mcp.linear.app/mcp  header "Authorization: Bearer ${LINEAR_TOKEN}"
        github stdio "npx -y @modelcontextprotocol/server-github"  env GITHUB_TOKEN
    )
    env (
        LINEAR_TOKEN = ${LINEAR_TOKEN:?create a Linear PAT}
        GITHUB_TOKEN
    )
)
`)
	require.NoError(t, err)

	var sources []mcpSource
	for _, s := range tmpl.MCP {
		sources = append(sources, mcpSource{Origin: "clawk.mod", Spec: s})
	}
	servers, err := composeMCP(sources)
	require.NoError(t, err)

	sb := &config.Sandbox{Name: "proj", MCP: servers, RequiredEnv: tmpl.Env}
	applyMCPNetwork(sb)

	// The remote server's host is reachable; the stdio one adds no egress.
	var allowed []string
	for _, b := range sb.Network.Blocks {
		if b.Origin == config.BlockOriginMCP {
			allowed = b.AllowDomains
		}
	}
	require.Equal(t, []string{"mcp.linear.app"}, allowed)

	// The rendered guest config names both servers and carries references,
	// not values, even with the credential present in this process's env.
	t.Setenv("LINEAR_TOKEN", "leaked-if-this-appears")
	content, ok, err := sandbox.RenderMCPConfig(sb.MCP)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotContains(t, string(content), "leaked-if-this-appears")
	require.Contains(t, string(content), "${LINEAR_TOKEN}")
	require.Contains(t, string(content), "${GITHUB_TOKEN}")

	// The runner is told where to find it.
	claude, err := agentByName("claude")
	require.NoError(t, err)
	require.Contains(t, launchArgs(sb, claude, nil), sandbox.GuestMCPConfigPath)

	// And the credential the config references is one the sandbox carries,
	// so ${LINEAR_TOKEN} has something to expand to at connect time.
	resolved, err := sandbox.ResolveEnv(sb)
	require.NoError(t, err)
	require.Contains(t, resolved, "LINEAR_TOKEN=leaked-if-this-appears",
		"the value travels via the env path, never the config file")
}

func httpSpec(name, url string, headers ...string) template.MCPSpec {
	return template.MCPSpec{
		Name:      name,
		Transport: config.MCPTransportHTTP,
		URL:       url,
		Headers:   headers,
	}
}

// TestComposeMCPCollapsesIdenticalDeclarations: two repos in one workspace
// both needing the same server is normal, not a conflict.
func TestComposeMCPCollapsesIdenticalDeclarations(t *testing.T) {
	spec := httpSpec("linear", "https://mcp.linear.app/mcp", "Authorization: Bearer ${T}")
	got, err := composeMCP([]mcpSource{
		{Origin: "api", Spec: spec},
		{Origin: "web", Spec: spec},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "linear", got[0].Name)
}

// TestComposeMCPRejectsConflict: the same name meaning two different things
// is a config bug. Silently picking one would hand the agent whichever
// definition composed first — the kind of thing you only notice when a tool
// call goes to the wrong place.
func TestComposeMCPRejectsConflict(t *testing.T) {
	_, err := composeMCP([]mcpSource{
		{Origin: "api", Spec: httpSpec("linear", "https://mcp.linear.app/mcp")},
		{Origin: "web", Spec: httpSpec("linear", "https://staging.linear.app/mcp")},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "declared differently")
	require.Contains(t, err.Error(), "api")
	require.Contains(t, err.Error(), "web")
}

// TestAppendMissingMCPNarrowerScopeWins mirrors appendMissingShares: a
// namespace supplies the org-wide default, a repo's own declaration of the
// same name overrides it.
func TestAppendMissingMCPNarrowerScopeWins(t *testing.T) {
	own := []config.MCPServer{{Name: "linear", Transport: config.MCPTransportHTTP, URL: "https://own/mcp"}}
	ns := []config.MCPServer{
		{Name: "linear", Transport: config.MCPTransportHTTP, URL: "https://ns/mcp"},
		{Name: "notion", Transport: config.MCPTransportHTTP, URL: "https://mcp.notion.com/mcp"},
	}
	got := appendMissingMCP(own, ns)
	require.Len(t, got, 2)
	require.Equal(t, "https://own/mcp", got[0].URL, "sandbox's own entry wins the name")
	require.Equal(t, "notion", got[1].Name, "namespace-only entries still arrive")
}

// TestMCPNetworkBlockDerivesHosts is the fix for the failure this feature
// exists to prevent: a declared remote server that the egress ACL refuses,
// surfacing inside the guest as a bare ConnectionRefused.
func TestMCPNetworkBlockDerivesHosts(t *testing.T) {
	block, ok := mcpNetworkBlock([]config.MCPServer{
		{Name: "linear", Transport: config.MCPTransportHTTP, URL: "https://mcp.linear.app/mcp"},
		{Name: "sentry", Transport: config.MCPTransportSSE, URL: "https://mcp.sentry.dev/sse"},
		{Name: "dup", Transport: config.MCPTransportHTTP, URL: "https://mcp.linear.app/other"},
		{Name: "byip", Transport: config.MCPTransportHTTP, URL: "http://10.20.0.7:8080/mcp"},
		{Name: "local", Transport: config.MCPTransportHTTP, URL: "http://127.0.0.1:9000/mcp"},
		{Name: "named-local", Transport: config.MCPTransportHTTP, URL: "http://localhost:9000/mcp"},
		{Name: "gh", Transport: config.MCPTransportStdio, Command: []string{"npx", "server"}},
	})
	require.True(t, ok)
	require.Equal(t, config.BlockOriginMCP, block.Origin)
	require.ElementsMatch(t, []string{"mcp.linear.app", "mcp.sentry.dev"}, block.AllowDomains,
		"hostnames are matched at DNS time and deduped")
	require.Equal(t, []string{"10.20.0.7"}, block.AllowIPs,
		"a literal address is enforced at SYN time, so it belongs in AllowIPs")
}

func TestMCPNetworkBlockEmptyWhenNothingToAllow(t *testing.T) {
	_, ok := mcpNetworkBlock(nil)
	require.False(t, ok)
	_, ok = mcpNetworkBlock([]config.MCPServer{
		{Name: "gh", Transport: config.MCPTransportStdio, Command: []string{"npx", "server"}},
		{Name: "local", Transport: config.MCPTransportHTTP, URL: "http://127.0.0.1:9000/mcp"},
	})
	require.False(t, ok, "a stdio server and a loopback URL need no egress")
}

// TestApplyMCPNetworkIsIdempotent: the block is rebuilt from the current
// server list on every create, so removing a server retires its allow entry
// instead of leaving it behind forever.
func TestApplyMCPNetworkIsIdempotent(t *testing.T) {
	sb := &config.Sandbox{
		MCP: []config.MCPServer{{Name: "linear", Transport: config.MCPTransportHTTP, URL: "https://mcp.linear.app/mcp"}},
	}
	applyMCPNetwork(sb)
	applyMCPNetwork(sb)
	var mcpBlocks int
	for _, b := range sb.Network.Blocks {
		if b.Origin == config.BlockOriginMCP {
			mcpBlocks++
			require.Equal(t, []string{"mcp.linear.app"}, b.AllowDomains)
		}
	}
	require.Equal(t, 1, mcpBlocks, "re-applying must replace, not accumulate")

	sb.MCP = nil
	applyMCPNetwork(sb)
	for _, b := range sb.Network.Blocks {
		require.NotEqual(t, config.BlockOriginMCP, b.Origin, "dropping the server drops its allow")
	}
}

// TestApplyMCPNetworkRanksBelowUserRules pins the precedence choice: the
// derived allow is a convenience, so an explicit deny the user wrote in
// clawk.mod or via the CLI has to outrank it.
func TestApplyMCPNetworkRanksBelowUserRules(t *testing.T) {
	sb := &config.Sandbox{
		MCP: []config.MCPServer{{Name: "linear", Transport: config.MCPTransportHTTP, URL: "https://mcp.linear.app/mcp"}},
		Network: config.NetworkPolicy{Blocks: []config.NetworkBlock{
			{Origin: config.BlockOriginCustom, DenyDomains: []string{"mcp.linear.app"}},
			{Origin: config.BlockOriginMod, Name: "clawk.mod"},
		}},
	}
	applyMCPNetwork(sb)

	var order []string
	for _, b := range sb.Network.Blocks {
		order = append(order, b.Origin)
	}
	require.Equal(t,
		[]string{config.BlockOriginMCP, config.BlockOriginMod, config.BlockOriginCustom},
		order, "mcp sits below mod and custom in increasing precedence")
}

// TestLaunchArgsMCPConfig covers both directions: a sandbox with servers gets
// the flag, one without must not (the file wouldn't exist), and the user's
// own args always land last so they can override clawk.
func TestLaunchArgsMCPConfig(t *testing.T) {
	claude, err := agentByName("claude")
	require.NoError(t, err)

	withMCP := &config.Sandbox{MCP: []config.MCPServer{{Name: "linear"}}}
	args := launchArgs(withMCP, claude, []string{"--resume"})
	require.Equal(t, []string{
		"--dangerously-skip-permissions",
		"--mcp-config", sandbox.GuestMCPConfigPath,
		"--resume",
	}, args)

	bare := &config.Sandbox{}
	require.Equal(t, []string{"--dangerously-skip-permissions"}, launchArgs(bare, claude, nil),
		"no declared servers means no flag pointing at a missing file")

	codex, err := agentByName("codex")
	require.NoError(t, err)
	require.NotContains(t, launchArgs(withMCP, codex, nil), "--mcp-config",
		"a runner clawk can't render config for gets no flag")
}
