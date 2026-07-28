package guestbuild

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/clawkwork/clawk/internal/agentembed"
	"github.com/stretchr/testify/require"
)

// TestBuildUsesPrebuiltWithoutGo is the point of the whole mechanism: when the
// binary carries prebuilt guest binaries, a sandbox can be prepared with no Go
// toolchain on PATH at all.
//
// Skips on a source build (nothing embedded), which is the normal state of a
// contributor's checkout — run `make guestbin` to exercise it, as CI and the
// release workflow do.
func TestBuildUsesPrebuiltWithoutGo(t *testing.T) {
	if !Prebuilt(runtime.GOARCH) {
		t.Skip("no embedded guest binaries in this build; run `make guestbin` first")
	}

	// An empty PATH is the strongest form of the claim: not "go is elsewhere",
	// but "no go exists".
	t.Setenv("PATH", "")

	cache := t.TempDir()
	bins, err := Build(context.Background(), cache, runtime.GOARCH)
	require.NoError(t, err, "a prebuilt clawk must not need a Go toolchain")

	for _, p := range []string{bins.Init, bins.Agent, bins.TimeSync} {
		st, err := os.Stat(p)
		require.NoError(t, err)
		require.Greater(t, st.Size(), int64(500<<10), "%s looks hollow", p)
		require.NotZero(t, st.Mode()&0o111, "%s must be executable", p)
	}

	t.Run("second call reuses the extraction", func(t *testing.T) {
		again, err := Build(context.Background(), cache, runtime.GOARCH)
		require.NoError(t, err)
		require.True(t, again.Cached)
		require.Equal(t, bins.Init, again.Init)
	})

	t.Run("a foreign arch falls through instead of lying", func(t *testing.T) {
		// The embedded set is for one arch. Asking for another must not hand
		// back binaries of the wrong architecture; with no `go` to fall back
		// to, that means an error.
		other := "amd64"
		if runtime.GOARCH == "amd64" {
			other = "arm64"
		}
		_, err := Build(context.Background(), t.TempDir(), other)
		require.Error(t, err)
		require.Contains(t, err.Error(), "`go` not found")
	})
}

// TestPrebuiltRejectsStaleSources pins the integrity check: binaries built from
// different sources than the ones embedded beside them must be ignored, because
// shipping them would silently run a guest older than the source tree.
func TestPrebuiltRejectsStaleSources(t *testing.T) {
	if !Prebuilt(runtime.GOARCH) {
		t.Skip("no embedded guest binaries in this build")
	}
	_, m, ok := agentembed.Prebuilt()
	require.True(t, ok)
	require.Equal(t, agentembed.SourcesHash(), m.SourcesSHA256,
		"this build's embedded binaries and sources disagree — `make guestbin` is stale")
}

// TestGeneratePrebuilt covers the generator `make guestbin` runs.
func TestGeneratePrebuilt(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles three guest binaries")
	}
	dest := t.TempDir()
	require.NoError(t, GeneratePrebuilt(context.Background(), dest, runtime.GOARCH))

	for _, name := range []string{"clawk-init", "clawk-pty-agent", "clawk-time-sync"} {
		st, err := os.Stat(filepath.Join(dest, name))
		require.NoError(t, err)
		require.Greater(t, st.Size(), int64(500<<10))
		require.NotZero(t, st.Mode()&0o111, "%s must be executable", name)
	}
	data, err := os.ReadFile(filepath.Join(dest, agentembed.PrebuiltManifestName))
	require.NoError(t, err)
	require.Contains(t, string(data), agentembed.SourcesHash(),
		"the manifest must record the sources it was built from")
	require.Contains(t, string(data), runtime.GOARCH)
}
