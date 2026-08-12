package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// emptySandbox is the smallest valid global file: a sandbox block declaring
// nothing. Used by the path-resolution tests, which only care about location.
const emptySandbox = "sandbox (\n)\n"

// withGlobalMod writes src as the host-wide clawk.mod inside a fresh
// XDG_CONFIG_HOME and enables the layer for the duration of the test. Returns
// the file's path. HOME is redirected too, so the ~/.clawk fallback can never
// resolve to the developer's real one.
func withGlobalMod(t *testing.T, src string) string {
	t.Helper()
	home := t.TempDir()
	xdg := filepath.Join(home, ".config")
	require.NoError(t, os.MkdirAll(filepath.Join(xdg, "clawk"), 0o755))
	path := filepath.Join(xdg, "clawk", RepoFileName)
	require.NoError(t, os.WriteFile(path, []byte(src), 0o644))

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv(GlobalModEnvVar, "")

	prev := GlobalDisabled
	GlobalDisabled = false
	t.Cleanup(func() { GlobalDisabled = prev })
	return path
}

// newRepo creates an initialised git repo with an optional clawk.mod.
func newRepo(t *testing.T, name, clawkmod string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initRepo(t, repo)
	if clawkmod != "" {
		require.NoError(t, os.WriteFile(
			filepath.Join(repo, RepoFileName), []byte(clawkmod), 0o644))
	}
	return repo
}

func TestGlobalModPathPrecedence(t *testing.T) {
	t.Run("env var wins", func(t *testing.T) {
		xdgPath := withGlobalMod(t, emptySandbox)
		explicit := filepath.Join(t.TempDir(), "custom.mod")
		require.NoError(t, os.WriteFile(explicit, []byte(emptySandbox), 0o644))
		t.Setenv(GlobalModEnvVar, explicit)

		got, err := GlobalModPath()
		require.NoError(t, err)
		require.Equal(t, explicit, got)
		require.NotEqual(t, xdgPath, got)
	})

	t.Run("env var pointing nowhere is an error", func(t *testing.T) {
		withGlobalMod(t, emptySandbox)
		t.Setenv(GlobalModEnvVar, filepath.Join(t.TempDir(), "absent.mod"))

		_, err := GlobalModPath()
		require.ErrorContains(t, err, GlobalModEnvVar)
		require.ErrorContains(t, err, "no such file")
	})

	t.Run("xdg location", func(t *testing.T) {
		path := withGlobalMod(t, emptySandbox)
		got, err := GlobalModPath()
		require.NoError(t, err)
		require.Equal(t, path, got)
	})

	t.Run("legacy ~/.clawk fallback", func(t *testing.T) {
		xdgPath := withGlobalMod(t, emptySandbox)
		require.NoError(t, os.Remove(xdgPath))

		legacy := filepath.Join(os.Getenv("HOME"), ".clawk", RepoFileName)
		require.NoError(t, os.MkdirAll(filepath.Dir(legacy), 0o755))
		require.NoError(t, os.WriteFile(legacy, []byte(emptySandbox), 0o644))

		got, err := GlobalModPath()
		require.NoError(t, err)
		require.Equal(t, legacy, got)
	})

	t.Run("both locations is an error, not a silent pick", func(t *testing.T) {
		withGlobalMod(t, emptySandbox)
		legacy := filepath.Join(os.Getenv("HOME"), ".clawk", RepoFileName)
		require.NoError(t, os.MkdirAll(filepath.Dir(legacy), 0o755))
		require.NoError(t, os.WriteFile(legacy, []byte(emptySandbox), 0o644))

		_, err := GlobalModPath()
		require.ErrorContains(t, err, "two host-wide clawk.mod files")
	})

	t.Run("no file at all", func(t *testing.T) {
		p := withGlobalMod(t, emptySandbox)
		require.NoError(t, os.Remove(p))

		_, err := GlobalModPath()
		require.ErrorIs(t, err, ErrNoGlobalMod)
	})
}

func TestLoadGlobalScopeRejections(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			name: "named sandbox block",
			src:  "sandbox house (\n    vm (\n        cpu 2\n    )\n)\n",
			want: "must be anonymous",
		},
		{
			name: "includes",
			src:  "sandbox (\n    includes (\n        ~/code/a\n    )\n)\n",
			want: "cannot be host-wide",
		},
		{
			name: "namespace block",
			src:  "namespace work (\n    env (\n        TOKEN\n    )\n)\n",
			want: "not accepted in the host-wide clawk.mod",
		},
		{
			name: "unwired lifecycle hook",
			src:  "sandbox (\n    on down (\n        \"echo bye\"\n    )\n)\n",
			want: "reserved",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withGlobalMod(t, tc.src)
			_, err := LoadGlobal()
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestLoadGlobalAbsolutisesHostPaths(t *testing.T) {
	path := withGlobalMod(t, `sandbox (
    vm (
        kernel ./kernels/vmlinux
    )
    files (
        ./house.netrc /home/agent/.netrc
    )
    shares (
        ~/.aws
    )
    agent (
        instructions ./house-rules.md
    )
)
`)
	dir := filepath.Dir(path)

	g, err := LoadGlobal()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "kernels", "vmlinux"), g.Template.Kernel)
	require.Equal(t, filepath.Join(dir, "house.netrc"), g.Template.Files[0].HostPath)
	require.Equal(t, filepath.Join(dir, "house-rules.md"), g.Template.Instructions[0].Path)
	// A home-relative spelling is the user's own and stays verbatim, so error
	// messages echo what they wrote.
	require.Equal(t, "~/.aws", g.Template.Shares[0].HostPath)
}

func TestLoadGlobalLeavesKernelURLAlone(t *testing.T) {
	withGlobalMod(t, "sandbox (\n    vm (\n        kernel https://example.com/vmlinux\n    )\n)\n")
	g, err := LoadGlobal()
	require.NoError(t, err)
	require.Equal(t, "https://example.com/vmlinux", g.Template.Kernel)
}

func TestLoadGlobalPolicyOnlyFile(t *testing.T) {
	withGlobalMod(t, "policy house (\n    allow example.com\n)\n")
	g, err := LoadGlobal()
	require.NoError(t, err)
	require.NotNil(t, g.Template)
	require.Len(t, g.Policies, 1)
	require.Equal(t, "house", g.Policies[0].Name)
}

func TestGlobalDisabledSkipsTheLayer(t *testing.T) {
	withGlobalMod(t, "sandbox (\n    vm (\n        cpu 8\n    )\n)\n")
	GlobalDisabled = true

	_, err := LoadGlobal()
	require.ErrorIs(t, err, ErrNoGlobalMod)
}

// The single-repo shape: the layer folds under the repo's own clawk.mod, so the
// repo wins scalars and its list entries follow the global ones.
func TestGlobalUnderStandaloneRepo(t *testing.T) {
	globalPath := withGlobalMod(t, `sandbox (
    vm (
        cpu        2
        memory_max 8GiB
        provider   vz
    )
    network (
        allow global.example.com
    )
    env (
        GITHUB_TOKEN = ${HOST_TOKEN}
    )
    shares (
        ~/.claude/skills/idiomatic-go
    )
)
`)
	repo := newRepo(t, "proj", `sandbox (
    vm (
        cpu 4
    )
    network (
        allow repo.example.com
    )
)
`)

	ws, err := LoadStandaloneClawkfile(repo)
	require.NoError(t, err)
	require.Equal(t, globalPath, ws.GlobalPath)

	tmpl := ws.Repos[0].Clawkfile
	require.EqualValues(t, 4, tmpl.CPU, "repo cpu must beat the global default")
	require.EqualValues(t, 8192, tmpl.MemoryMaxMiB, "global memory applies where the repo is silent")
	require.Equal(t, "vz", tmpl.Provider)
	require.Equal(t, []string{"global.example.com", "repo.example.com"}, tmpl.Domains,
		"lists union with the global entries first")
	require.Equal(t, []string{"GITHUB_TOKEN=${HOST_TOKEN}"}, tmpl.Env)
	require.Len(t, tmpl.Shares, 1)
	// The repo's own name is untouched by the (necessarily anonymous) layer.
	require.Equal(t, "proj", ws.Repos[0].Name)
}

func TestGlobalIsWholeTemplateForRepoWithoutClawkMod(t *testing.T) {
	withGlobalMod(t, `sandbox (
    vm (
        cpu 3
    )
    network (
        allow global.example.com
    )
)
`)
	repo := newRepo(t, "bare", "")

	ws, err := WorkspaceFromGitRepo(repo)
	require.NoError(t, err)
	require.NotNil(t, ws.Repos[0].Clawkfile)
	require.EqualValues(t, 3, ws.Repos[0].Clawkfile.CPU)
	require.Equal(t, []string{"global.example.com"}, ws.Repos[0].Clawkfile.Domains)
}

// In a multi-repo workspace the layer sits at the workspace position, but must
// not inherit the workspace file's authority to settle repo disagreements.
func TestGlobalScalarsDemotedBehindRepos(t *testing.T) {
	withGlobalMod(t, `sandbox (
    vm (
        image      golang:1.25
        cpu        2
        memory_max 8GiB
    )
)
`)

	root := t.TempDir()
	repo := filepath.Join(root, "svc")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initRepo(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, RepoFileName), []byte(`sandbox (
    vm (
        image node:22
        cpu   6
    )
)
`), 0o644))

	wsPath := filepath.Join(root, RepoFileName)
	require.NoError(t, os.WriteFile(wsPath,
		[]byte("sandbox (\n    includes (\n        ./svc\n    )\n)\n"), 0o644))

	ws, err := LoadWorkspace(wsPath)
	require.NoError(t, err)
	require.Empty(t, ws.File.Image, "a global image must not outrank the repo's own")
	require.Zero(t, ws.File.CPU, "a global cpu must not outrank the repo's own")
	require.EqualValues(t, 8192, ws.File.MemoryMaxMiB,
		"but it still applies where no repo declared one")
}

func TestGlobalAtWorkspacePositionUnderTheWorkspaceFile(t *testing.T) {
	withGlobalMod(t, `sandbox (
    vm (
        memory_max 8GiB
    )
    network (
        allow global.example.com
    )
    env (
        GLOBAL_TOKEN
    )
    on up (
        "global-hook"
    )
)
`)
	root := t.TempDir()
	repo := filepath.Join(root, "svc")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initRepo(t, repo)

	wsPath := filepath.Join(root, RepoFileName)
	require.NoError(t, os.WriteFile(wsPath, []byte(`sandbox (
    includes (
        ./svc
    )
    vm (
        memory_max 16GiB
    )
    on up (
        "workspace-hook"
    )
)
`), 0o644))

	ws, err := LoadWorkspace(wsPath)
	require.NoError(t, err)
	require.EqualValues(t, 16384, ws.File.MemoryMaxMiB, "the workspace file wins")
	require.Equal(t, []string{"global.example.com"}, ws.File.Domains)
	require.Equal(t, []string{"GLOBAL_TOKEN"}, ws.File.Env)
	require.Equal(t, []string{"global-hook", "workspace-hook"}, ws.File.OnUp,
		"the broader scope's hooks run first")
}

func TestGlobalProfileOverlay(t *testing.T) {
	path := withGlobalMod(t, "sandbox (\n    network (\n        allow base.example.com\n    )\n)\n")
	require.NoError(t, os.WriteFile(path+".investigation",
		[]byte("sandbox (\n    network (\n        allow deep.example.com\n    )\n)\n"), 0o644))

	repo := newRepo(t, "proj", emptySandbox)

	// The repo has no overlay of its own: the profile is satisfied entirely by
	// the host-wide layer, which must count as a match rather than erroring.
	ws, err := LoadStandaloneClawkfileWithProfile(repo, "investigation")
	require.NoError(t, err)
	require.Equal(t, []string{"base.example.com", "deep.example.com"},
		ws.Repos[0].Clawkfile.Domains)

	// An unknown profile still fails loudly.
	_, err = LoadStandaloneClawkfileWithProfile(repo, "nope")
	require.ErrorContains(t, err, "nope")
}

func TestGlobalParseErrorSurfaces(t *testing.T) {
	withGlobalMod(t, "sandbox (\n    vm (\n        cpu\n    )\n)\n")
	repo := newRepo(t, "proj", emptySandbox)

	// A broken defaults file must not degrade to "no defaults" — that would
	// leave the user with a sandbox quietly missing half its configuration.
	_, err := LoadStandaloneClawkfile(repo)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "cpu"), "got %v", err)
}
