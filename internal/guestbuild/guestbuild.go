// Package guestbuild cross-compiles the guest-side binaries (clawk-init,
// clawk-pty-agent, clawk-time-sync) on the host, for injection into
// OCI-image sandbox disks.
//
// Arbitrary OCI images have no Go toolchain, so the embedded sources
// are compiled here on the host with GOOS=linux CGO_ENABLED=0 and the
// static binaries are baked into the rootfs by machine/oci's inject
// support.
//
// Release binaries carry these prebuilt (see internal/agentembed/prebuilt.go),
// so an installed clawk needs no Go toolchain at all. Building from source
// leaves them out and they are compiled here on first use instead, cached
// under <cacheDir>/guestbin/<hash>/ keyed by source content, target arch and
// toolchain version — so the ~seconds cost is paid once per clawk version.
package guestbuild

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/clawkwork/clawk/internal/agentembed"
)

// Binaries holds the absolute paths of the built guest binaries.
type Binaries struct {
	Init     string
	Agent    string
	TimeSync string

	// Cached reports that a previous build was reused — callers use it
	// to avoid announcing build work that didn't happen.
	Cached bool
}

// module is one standalone guest program: an embedded main.go + go.mod
// pair producing one static binary.
type module struct {
	name string // binary name, also the build subdir
	main []byte
	gmod []byte
}

func modules() []module {
	return []module{
		{name: "clawk-init", main: agentembed.InitMainGo, gmod: agentembed.InitGoMod},
		{name: "clawk-pty-agent", main: agentembed.AgentMainGo, gmod: agentembed.AgentGoMod},
		{name: "clawk-time-sync", main: agentembed.TimeSyncMainGo, gmod: agentembed.TimeSyncGoMod},
	}
}

// Build returns the guest binaries for linux/<arch>.
//
// A release binary embeds them (see internal/agentembed/prebuilt.go), so this
// only unpacks and returns — no Go toolchain, no network. A source build has
// nothing embedded and compiles them from the embedded sources instead,
// caching the result; that path needs `go` on PATH, and network the first time
// for the guest modules' dependencies.
func Build(ctx context.Context, cacheDir, arch string) (Binaries, error) {
	if cacheDir == "" {
		return Binaries{}, fmt.Errorf("guestbuild: cacheDir is required")
	}
	if arch == "" {
		return Binaries{}, fmt.Errorf("guestbuild: arch is required")
	}
	if bins, ok := fromPrebuilt(cacheDir, arch); ok {
		return bins, nil
	}
	return buildFromSource(ctx, cacheDir, arch)
}

// Prebuilt reports whether this binary carries guest binaries for arch, i.e.
// whether a sandbox can boot with no Go toolchain present. `clawk doctor` asks,
// so it can tell a hard prerequisite from an optional one.
func Prebuilt(arch string) bool {
	_, m, ok := agentembed.Prebuilt()
	return ok && m.Arch == arch && m.SourcesSHA256 == agentembed.SourcesHash()
}

// buildFromSource compiles the guest binaries, reusing a previous build when
// sources, arch and toolchain are unchanged.
func buildFromSource(ctx context.Context, cacheDir, arch string) (Binaries, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return Binaries{}, fmt.Errorf(
			"guestbuild: `go` not found — this clawk was built from source, so it needs the Go " +
				"toolchain to compile the guest agent (install from https://go.dev/dl or brew install go). " +
				"Release binaries ship the guest agent prebuilt and need no toolchain")
	}

	key, err := cacheKey(goBin, arch)
	if err != nil {
		return Binaries{}, err
	}
	outDir := filepath.Join(cacheDir, "guestbin", key)
	bins := Binaries{
		Init:     filepath.Join(outDir, "clawk-init"),
		Agent:    filepath.Join(outDir, "clawk-pty-agent"),
		TimeSync: filepath.Join(outDir, "clawk-time-sync"),
	}
	if allExist(bins.Init, bins.Agent, bins.TimeSync) {
		bins.Cached = true
		return bins, nil
	}

	// Build into a sibling temp dir and rename, so a crashed build never
	// leaves a half-populated cache entry that allExist would accept later.
	if err := os.MkdirAll(filepath.Dir(outDir), 0o755); err != nil {
		return Binaries{}, fmt.Errorf("guestbuild: cache dir: %w", err)
	}
	tmpDir, err := os.MkdirTemp(filepath.Dir(outDir), "build-*")
	if err != nil {
		return Binaries{}, fmt.Errorf("guestbuild: temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	for _, m := range modules() {
		if err := buildModule(ctx, goBin, m, tmpDir, arch); err != nil {
			return Binaries{}, err
		}
	}

	if err := os.Rename(tmpDir, outDir); err != nil {
		// A concurrent Build may have won the rename; if the result is
		// complete, use it.
		if allExist(bins.Init, bins.Agent, bins.TimeSync) {
			bins.Cached = true
			return bins, nil
		}
		return Binaries{}, fmt.Errorf("guestbuild: promoting build: %w", err)
	}
	return bins, nil
}

// buildModule writes m's sources into a scratch module dir and compiles a
// static linux binary into destDir.
func buildModule(ctx context.Context, goBin string, m module, destDir, arch string) error {
	src, err := os.MkdirTemp("", "clawk-guestbuild-*")
	if err != nil {
		return fmt.Errorf("guestbuild: scratch dir: %w", err)
	}
	defer os.RemoveAll(src)

	if err := os.WriteFile(filepath.Join(src, "main.go"), m.main, 0o644); err != nil {
		return fmt.Errorf("guestbuild %s: %w", m.name, err)
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), m.gmod, 0o644); err != nil {
		return fmt.Errorf("guestbuild %s: %w", m.name, err)
	}

	env := append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")

	// go mod tidy resolves go.sum for the embedded require list. Needs
	// network the first time; module cache after.
	tidy := exec.CommandContext(ctx, goBin, "mod", "tidy")
	tidy.Dir = src
	tidy.Env = env
	if out, err := tidy.CombinedOutput(); err != nil {
		return fmt.Errorf("guestbuild %s: go mod tidy: %w\n%s", m.name, err, out)
	}

	build := exec.CommandContext(ctx, goBin,
		"build", "-trimpath", "-ldflags=-s -w",
		"-o", filepath.Join(destDir, m.name), ".")
	build.Dir = src
	build.Env = env
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("guestbuild %s: go build: %w\n%s", m.name, err, out)
	}
	return nil
}

// cacheKey hashes everything that determines the binaries' content: each
// module's sources, the target arch, and the toolchain version.
func cacheKey(goBin, arch string) (string, error) {
	h := sha256.New()
	for _, m := range modules() {
		h.Write([]byte(m.name))
		h.Write(m.main)
		h.Write(m.gmod)
	}
	h.Write([]byte(arch))
	ver, err := exec.Command(goBin, "env", "GOVERSION").Output()
	if err != nil {
		return "", fmt.Errorf("guestbuild: go env GOVERSION: %w", err)
	}
	h.Write(ver)
	return fmt.Sprintf("%x", h.Sum(nil))[:16], nil
}

func allExist(paths ...string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}
