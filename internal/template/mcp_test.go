package template

import (
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

// TestParseMCPBlock covers the three line shapes an `mcp ( … )` entry can
// take, plus the repeatable modifiers. The bare-URL form is the one users
// write most, so its http default is pinned explicitly.
func TestParseMCPBlock(t *testing.T) {
	src := `mcp (
    linear   https://mcp.linear.app/mcp   header "Authorization: Bearer ${LINEAR_TOKEN}"
    sentry   sse https://mcp.sentry.dev/sse
    corridor http https://app.corridor.dev/api/mcp  header "X-Api-Key: ${CORRIDOR_KEY}"  header "X-Env: prod"
    github   stdio "npx -y @modelcontextprotocol/server-github"  env GITHUB_TOKEN
)
`
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	require.Len(t, tmpl.MCP, 4)

	linear := tmpl.MCP[0]
	require.Equal(t, "linear", linear.Name)
	require.Equal(t, config.MCPTransportHTTP, linear.Transport, "a bare URL defaults to http")
	require.Equal(t, "https://mcp.linear.app/mcp", linear.URL)
	require.Equal(t, []string{"Authorization: Bearer ${LINEAR_TOKEN}"}, linear.Headers)

	require.Equal(t, config.MCPTransportSSE, tmpl.MCP[1].Transport)
	require.Equal(t, "https://mcp.sentry.dev/sse", tmpl.MCP[1].URL)

	corridor := tmpl.MCP[2]
	require.Equal(t, config.MCPTransportHTTP, corridor.Transport)
	require.Equal(t, []string{"X-Api-Key: ${CORRIDOR_KEY}", "X-Env: prod"}, corridor.Headers,
		"header is repeatable and order-preserving")

	gh := tmpl.MCP[3]
	require.Equal(t, config.MCPTransportStdio, gh.Transport)
	require.Equal(t, []string{"npx", "-y", "@modelcontextprotocol/server-github"}, gh.Command)
	require.Equal(t, []string{"GITHUB_TOKEN"}, gh.Env)
	require.Empty(t, gh.URL)
}

// TestParseMCPKeepsCredentialReferencesLiteral is the invariant that keeps
// secrets off host disk: a ${VAR} in a header is stored as written, never
// resolved at parse time. The runner expands it inside the guest.
func TestParseMCPKeepsCredentialReferencesLiteral(t *testing.T) {
	t.Setenv("LINEAR_TOKEN", "should-not-be-read")
	tmpl, err := parseBody(`mcp (
    linear https://mcp.linear.app/mcp header "Authorization: Bearer ${LINEAR_TOKEN}"
)
`)
	require.NoError(t, err)
	require.Equal(t, []string{"Authorization: Bearer ${LINEAR_TOKEN}"}, tmpl.MCP[0].Headers)
}

func TestParseMCPErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "no target",
			src:  "mcp (\n    linear\n)\n",
			want: "expected a URL or a transport",
		},
		{
			name: "non-http scheme",
			src:  "mcp (\n    linear ftp://mcp.linear.app/mcp\n)\n",
			want: "must use http or https",
		},
		{
			name: "url without host",
			src:  "mcp (\n    linear https:///mcp\n)\n",
			want: "has no host",
		},
		{
			name: "stdio without quoted command",
			src:  "mcp (\n    github stdio npx\n)\n",
			want: "expected a quoted command",
		},
		{
			name: "header without colon",
			src:  "mcp (\n    linear https://mcp.linear.app/mcp header \"Authorization Bearer x\"\n)\n",
			want: "want \"Name: value\"",
		},
		{
			name: "header on stdio server",
			src:  "mcp (\n    github stdio \"npx server\" header \"X: y\"\n)\n",
			want: "'header' applies to http/sse servers",
		},
		{
			name: "env on remote server",
			src:  "mcp (\n    linear https://mcp.linear.app/mcp env LINEAR_TOKEN\n)\n",
			want: "'env' applies to stdio servers",
		},
		{
			name: "invalid env name",
			src:  "mcp (\n    github stdio \"npx server\" env 9BAD\n)\n",
			want: "9BAD",
		},
		{
			name: "invalid server name",
			src:  "mcp (\n    my:server https://mcp.linear.app/mcp\n)\n",
			want: "invalid mcp server name",
		},
		{
			name: "trailing junk",
			src:  "mcp (\n    linear https://mcp.linear.app/mcp wat\n)\n",
			want: "unexpected token after mcp entry",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseBody(tc.src)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestParseMCPInNamespaceBlock guards the namespace scope: the org-wide
// server set is the whole reason per-sandbox MCP setup can be zero-touch,
// so `mcp ( … )` has to be accepted there too.
func TestParseMCPInNamespaceBlock(t *testing.T) {
	f, err := ParseFileString(`namespace acme (
    mcp (
        linear https://mcp.linear.app/mcp header "Authorization: Bearer ${LINEAR_TOKEN}"
    )
)
`)
	require.NoError(t, err)
	require.Len(t, f.Namespaces, 1)
	require.Len(t, f.Namespaces[0].Template.MCP, 1)
	require.Equal(t, "linear", f.Namespaces[0].Template.MCP[0].Name)
}

// A URL with userinfo is the one way a secret VALUE could reach the sandbox
// record and the rendered guest config, both of which live unencrypted on
// host disk — the invariant the whole `${VAR}`-reference design exists to
// hold. Refuse it and name the supported spelling.
func TestMCPURLRejectsEmbeddedCredentials(t *testing.T) {
	_, err := parseBody(`mcp (
    sentry https://user:s3cr3t@mcp.sentry.dev/mcp
)
`)
	require.Error(t, err)
	require.ErrorContains(t, err, "embeds credentials")
	require.ErrorContains(t, err, "Authorization: Bearer")
	require.NotContains(t, err.Error(), "s3cr3t",
		"the error must not echo the secret it is refusing")
}
