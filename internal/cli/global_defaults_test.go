package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/template"
	"github.com/stretchr/testify/require"
)

// withGlobalMod writes src as the host-wide clawk.mod inside a fresh
// XDG_CONFIG_HOME and enables the layer for the duration of the test (the
// package otherwise runs with it off — see TestMain). Returns its directory.
func withGlobalMod(t *testing.T, src string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "clawk")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, template.RepoFileName), []byte(src), 0o644))

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv(template.GlobalModEnvVar, "")

	template.GlobalDisabled = false
	noGlobalFlag = false
	t.Cleanup(func() {
		template.GlobalDisabled = true
		noGlobalFlag = true
	})
	return dir
}

// End to end through `clawk work`: a host-wide clawk.mod reaches the sandbox
// record for every directive group, and the repo's own file still wins.
func TestGlobalDefaultsReachTheRecord(t *testing.T) {
	setupTest(t)
	dir := withGlobalMod(t, `sandbox (
    vm (
        memory_max 8GiB
        provider   vz
    )
    network (
        allow global.example.com
    )
    env (
        GLOBAL_TOKEN
    )
    files (
        ./house.netrc /home/agent/.netrc
    )
    agent (
        instructions "house rule"
    )
)
`)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "house.netrc"), []byte("x"), 0o600))

	repo := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	gitInit(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, template.RepoFileName), []byte(`sandbox (
    vm (
        memory_max 16GiB
    )
    network (
        allow repo.example.com
    )
    env (
        REPO_TOKEN
    )
    agent (
        instructions "repo rule"
    )
)
`), 0o644))

	_, err := executeCommand("work", filepath.Join(repo, template.RepoFileName), "TICKET-9", "--bare")
	require.NoError(t, err)

	sb, err := store.Load("TICKET-9")
	require.NoError(t, err)

	require.EqualValues(t, 16384, sb.MemoryMaxMiB, "the repo's own memory_max must win")
	require.Equal(t, config.Provider("vz"), sb.Provider, "global provider applies where the repo is silent")

	mod := sb.Network.Block(config.BlockOriginMod)
	require.Contains(t, mod.AllowDomains, "global.example.com")
	require.Contains(t, mod.AllowDomains, "repo.example.com")

	require.True(t, slices.Contains(sb.RequiredEnv, "GLOBAL_TOKEN"), "env: %v", sb.RequiredEnv)
	require.True(t, slices.Contains(sb.RequiredEnv, "REPO_TOKEN"), "env: %v", sb.RequiredEnv)

	// The relative host path resolved against the global file's directory, not
	// the repo or the process CWD.
	var netrc *config.HostFile
	for i := range sb.Files {
		if sb.Files[i].GuestPath == "/home/agent/.netrc" {
			netrc = &sb.Files[i]
		}
	}
	require.NotNil(t, netrc, "global file entry missing: %v", sb.Files)
	require.Equal(t, filepath.Join(dir, "house.netrc"), netrc.HostPath)

	require.Equal(t, []string{"house rule", "repo rule"}, sb.Instructions,
		"the broader scope's instructions read first")
}

// A broken host-wide file must be reported as such. resolveSource walks
// workspace → standalone → bare-git-repo and treats each failure as "not this
// shape", which swallowed the real error and answered "no clawk.mod found, and
// X is not a git repo" — for a directory that had a clawk.mod and was a git
// repo. See template.ErrGlobalMod.
func TestBrokenGlobalIsReportedNotSwallowed(t *testing.T) {
	setupTest(t)
	// A header name is the scope violation with the most misleading fallback:
	// the file parses, so nothing else complains.
	withGlobalMod(t, "sandbox house (\n    vm (\n        cpu 2\n    )\n)\n")

	repo := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	gitInit(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, template.RepoFileName),
		[]byte("sandbox (\n)\n"), 0o644))

	t.Chdir(repo)
	_, err := resolveSource("", "")
	require.ErrorIs(t, err, template.ErrGlobalMod)
	require.ErrorContains(t, err, "must be anonymous")
	require.NotContains(t, err.Error(), "is not a git repo")
}

func TestNoGlobalFlagDropsTheLayer(t *testing.T) {
	setupTest(t)
	withGlobalMod(t, `sandbox (
    network (
        allow global.example.com
    )
    env (
        GLOBAL_TOKEN
    )
)
`)
	repo := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	gitInit(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, template.RepoFileName),
		[]byte("sandbox (\n    network (\n        allow repo.example.com\n    )\n)\n"), 0o644))

	_, err := executeCommand("work", filepath.Join(repo, template.RepoFileName),
		"TICKET-10", "--bare", "--no-global")
	require.NoError(t, err)

	sb, err := store.Load("TICKET-10")
	require.NoError(t, err)
	mod := sb.Network.Block(config.BlockOriginMod)
	require.Contains(t, mod.AllowDomains, "repo.example.com")
	require.NotContains(t, mod.AllowDomains, "global.example.com")
	require.Empty(t, sb.RequiredEnv)
}

// The here-mode path (`clawk` bare) must refuse a broken host-wide file for
// the same reason resolveSource does — and it matters more here, because the
// standalone loader fails as a WHOLE: degrading to "no defaults" threw away
// the repo's own clawk.mod too, creating a sandbox with none of its forwards,
// env or instructions and only a line on stderr to say so.
func TestHereModeRefusesBrokenGlobal(t *testing.T) {
	setupTest(t)
	// A header name parses fine and violates only the global scope, so
	// nothing but the ErrGlobalMod check stands between it and a silently
	// misconfigured sandbox.
	withGlobalMod(t, "sandbox house (\n    vm (\n        cpu 2\n    )\n)\n")

	repo := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	gitInit(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, template.RepoFileName),
		[]byte("sandbox (\n    forwards ( 3000 )\n    env ( MY_TOKEN )\n)\n"), 0o644))

	_, _, _, err := loadHereClawkfile(repo)
	require.ErrorIs(t, err, template.ErrGlobalMod)
	require.ErrorContains(t, err, "must be anonymous")
}

// The repo's own clawk.mod must still come back intact when the host-wide
// layer is merely absent — the overwhelmingly common case, and the one the
// error path above must not swallow.
func TestHereModeKeepsRepoConfigWithoutGlobal(t *testing.T) {
	setupTest(t)

	repo := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	gitInit(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, template.RepoFileName),
		[]byte("sandbox (\n    forwards ( 3000 )\n    env ( MY_TOKEN )\n)\n"), 0o644))

	tmpl, _, globalPath, err := loadHereClawkfile(repo)
	require.NoError(t, err)
	require.Empty(t, globalPath)
	require.NotNil(t, tmpl)
	require.Equal(t, []string{"MY_TOKEN"}, tmpl.Env)
	require.Len(t, tmpl.Forwards, 1)
}
