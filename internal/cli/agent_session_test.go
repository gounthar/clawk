package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/stretchr/testify/require"
)

// isGuestExit is the gate that keeps a clean guest session that exited
// nonzero (e.g. 130 from ^C-then-exit) from being mistaken for a transport
// failure — the bug that spuriously opened a second shell on `clawk run
// shell` and retried/fell back the claude attach. Only a sandbox.ExitError
// (wrapped or not) counts; every transport/agent error must not.
func TestIsGuestExit(t *testing.T) {
	require.True(t, isGuestExit(&sandbox.ExitError{Code: 130}), "bare ExitError")
	require.True(t, isGuestExit(fmt.Errorf("attach: %w", &sandbox.ExitError{Code: 1})), "wrapped ExitError")

	require.False(t, isGuestExit(nil), "nil")
	require.False(t, isGuestExit(errAgentSocketMissing), "socket-missing sentinel")
	require.False(t, isGuestExit(errors.New("agent disconnected before exit frame")), "transport failure")
}

// TestBuildVSockEnvForwardsOAuthToken guards the wiring that gets the
// long-lived Claude Code OAuth token into the in-VM `claude` process.
// The pty agent spawns its child non-login with a custom env (see
// agentembed/main.go.in: buildChildEnv) — so /etc/profile.d/ never
// gets sourced and the token must ride the handshake env or it won't
// reach claude at all.
func TestBuildVSockEnvForwardsOAuthToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	require.NoError(t, sandbox.SaveOAuthToken(filepath.Join(home, ".clawk"), "sk-test-vsock"))

	env := buildVSockEnv(&config.Sandbox{Name: "sb"})
	want := "CLAUDE_CODE_OAUTH_TOKEN=sk-test-vsock"
	for _, e := range env {
		if e == want {
			return
		}
	}
	t.Errorf("missing %q in buildVSockEnv() output: %v", want, env)
}

func TestBuildVSockEnvOmitsTokenWhenAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	// Sanity: no token file under HOME/.clawk.
	_, err := os.Stat(filepath.Join(home, ".clawk", "claude-oauth-token"))
	require.True(t, os.IsNotExist(err), "expected no token file, got err=%v", err)

	for _, e := range buildVSockEnv(&config.Sandbox{Name: "sb"}) {
		if strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") {
			t.Errorf("unexpected token entry when unconfigured: %q", e)
		}
	}
}

// TestBuildVSockEnvForwardsModEnv is the guard for the same class of bug as
// the OAuth token above, for clawk.mod `env ( … )`: the runner process is
// spawned non-login, so a var delivered only through /etc/profile.d/ is
// invisible to it — visible to its shell children, but not to the runner
// itself. Anything reading the runner's own environment therefore breaks,
// notably `${VAR}` expansion inside the generated MCP config, where an
// unforwarded PAT silently becomes an empty Authorization header.
func TestBuildVSockEnvForwardsModEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAWK_TEST_PAT", "pat-abc123")

	sb := &config.Sandbox{Name: "sb", RequiredEnv: []string{
		"LINEAR_TOKEN=${CLAWK_TEST_PAT}",
		"LITERAL=plain",
	}}
	env := buildVSockEnv(sb)
	require.Contains(t, env, "LINEAR_TOKEN=pat-abc123", "aliased host var must reach the runner")
	require.Contains(t, env, "LITERAL=plain", "literal must reach the runner")
}

// TestBuildVSockEnvClawkVarsWinOrdering pins the layering for names the
// sandbox did NOT declare: the guest agent folds the handshake env into a
// map in slice order (agentembed/main.go.in, buildChildEnv), so the LAST
// occurrence wins, and clawk's own vars are emitted after the sandbox's
// declared env.
func TestBuildVSockEnvClawkVarsWinOrdering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("COLORTERM", "")
	require.NoError(t, sandbox.SaveOAuthToken(filepath.Join(home, ".clawk"), "sk-real"))

	sb := &config.Sandbox{Name: "sb", RequiredEnv: []string{"UNRELATED=x"}}
	env := buildVSockEnv(sb)

	idx := func(prefix string) int {
		for i, e := range env {
			if strings.HasPrefix(e, prefix) {
				return i
			}
		}
		return -1
	}
	require.Contains(t, env, "CLAUDE_CODE_OAUTH_TOKEN=sk-real")
	require.Greater(t, idx("CLAUDE_CODE_OAUTH_TOKEN="), idx("UNRELATED="),
		"undeclared clawk vars still come after the sandbox's own entries")
}

// TestBuildVSockEnvDeclarationOverridesClawkVar is the counterpart, and the
// reason the rule above carves out declared names: a sandbox pointed at a
// non-Anthropic provider must be able to stop clawk injecting an Anthropic
// credential, or the runner authenticates against the wrong service and
// every request fails with an unrecognized-model error. Before this, the
// declaration was silently overwritten and `clawk auth clear` — which
// disarms EVERY sandbox on the host — was the only lever.
func TestBuildVSockEnvDeclarationOverridesClawkVar(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	require.NoError(t, sandbox.SaveOAuthToken(filepath.Join(home, ".clawk"), "sk-real"))

	sb := &config.Sandbox{Name: "sb", RequiredEnv: []string{
		`CLAUDE_CODE_OAUTH_TOKEN=""`,
	}}
	env := buildVSockEnv(sb)

	var got []string
	for _, e := range env {
		if strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") {
			got = append(got, e)
		}
	}
	require.Equal(t, []string{"CLAUDE_CODE_OAUTH_TOKEN="}, got,
		"the declared value must be the only occurrence — clawk must not re-add its own")
}

// TestBuildVSockEnvDeclarationOverridesTerminalVar pins that the carve-out
// is a general precedence rule, not a special case for the OAuth token.
func TestBuildVSockEnvDeclarationOverridesTerminalVar(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("COLORTERM", "truecolor")

	sb := &config.Sandbox{Name: "sb", RequiredEnv: []string{"COLORTERM=256"}}
	env := buildVSockEnv(sb)

	var got []string
	for _, e := range env {
		if strings.HasPrefix(e, "COLORTERM=") {
			got = append(got, e)
		}
	}
	require.Equal(t, []string{"COLORTERM=256"}, got)
}

// TestBuildVSockEnvSurvivesUnresolvableEntry covers the best-effort
// contract: attach is the hot path, so a `${HOST:?msg}` that no longer
// resolves in this shell must not make the sandbox unreachable. The
// failing entry drops out; everything else still goes through.
func TestBuildVSockEnvSurvivesUnresolvableEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAWK_TEST_PRESENT", "here")

	sb := &config.Sandbox{Name: "sb", RequiredEnv: []string{
		"GONE=${CLAWK_TEST_ABSENT:?set it on the host}",
		"KEPT=${CLAWK_TEST_PRESENT}",
	}}
	env := buildVSockEnv(sb)
	require.Contains(t, env, "KEPT=here", "resolvable entries must survive a failing sibling")
	for _, e := range env {
		require.False(t, strings.HasPrefix(e, "GONE="), "unresolvable entry must be dropped: %q", e)
	}
}

// TestBuildVSockEnvDeclarationOverridesEvenWhenUnresolvable is the security
// counterpart to TestBuildVSockEnvSurvivesUnresolvableEntry: best-effort
// resolution must not become best-effort PRECEDENCE.
//
// A sandbox pointed at a non-Anthropic gateway disowns clawk's token by
// declaring the name itself. If that declaration reads a host variable that
// has since left the shell, the entry drops out of ResolveEnv's output — and
// deriving the "declared" set from that output handed the name straight back
// to clawk, which injected its own sk-ant-oat-… as an Authorization header to
// the third-party endpoint. Exactly the leak the declaration existed to stop,
// reachable from any shell missing the gateway variable.
func TestBuildVSockEnvDeclarationOverridesEvenWhenUnresolvable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	// "sk-real" like the fixtures above, deliberately NOT the live sk-ant-
	// prefix: this repo is public, and a literal shaped like a real Anthropic
	// token trips secret scanners and push protection for no benefit. The
	// assertion only needs a string distinctive enough to spot in the output.
	require.NoError(t, sandbox.SaveOAuthToken(filepath.Join(home, ".clawk"), "sk-real"))

	sb := &config.Sandbox{Name: "sb", RequiredEnv: []string{
		"CLAUDE_CODE_OAUTH_TOKEN=${CLAWK_TEST_GATEWAY:?set it on the host}",
	}}
	env := buildVSockEnv(sb)

	for _, e := range env {
		require.NotContains(t, e, "sk-real",
			"a declared-but-unresolvable name must stay suppressed, not fall back to clawk's token")
	}
}
