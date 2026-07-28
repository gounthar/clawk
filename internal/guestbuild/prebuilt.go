package guestbuild

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/clawkwork/clawk/internal/agentembed"
)

// fromPrebuilt materializes the guest binaries embedded at release time,
// returning ok=false when this build has none (a source build) or has a set
// that doesn't fit the request.
//
// Extraction, rather than using them in place: callers hand these paths to
// machine/oci as inject sources, which opens them as files — an embed.FS entry
// has no path on disk. They land in the same cache layout a source build uses,
// keyed by the manifest's source hash so a prebuilt clawk and a source-built
// one can share a cache dir without colliding.
func fromPrebuilt(cacheDir, arch string) (Binaries, bool) {
	files, m, ok := agentembed.Prebuilt()
	if !ok {
		return Binaries{}, false
	}
	if m.Arch != arch {
		// Not an error: an aarch64 host asking for amd64 guest binaries is a
		// caller bug, but falling through to a source build is the honest
		// response, and hardware virtualization makes it unreachable anyway.
		return Binaries{}, false
	}
	if want := agentembed.SourcesHash(); m.SourcesSHA256 != want {
		// The embedded binaries were built from different sources than the
		// ones embedded beside them — a broken release, and exactly the
		// failure that would otherwise ship a stale guest silently. Say so and
		// let the source build take over.
		fmt.Fprintf(os.Stderr,
			"warning: ignoring embedded guest binaries: built from sources %.12s, this build embeds %.12s\n",
			m.SourcesSHA256, want)
		return Binaries{}, false
	}

	outDir := filepath.Join(cacheDir, "guestbin", fmt.Sprintf("prebuilt-%s-%.16s", arch, m.SourcesSHA256))
	bins := Binaries{
		Init:     filepath.Join(outDir, "clawk-init"),
		Agent:    filepath.Join(outDir, "clawk-pty-agent"),
		TimeSync: filepath.Join(outDir, "clawk-time-sync"),
	}
	if allExist(bins.Init, bins.Agent, bins.TimeSync) {
		bins.Cached = true
		return bins, true
	}
	if err := extractAll(files, outDir); err != nil {
		// A cache we can't write is not fatal while `go` might still be
		// available; warn and let Build fall through.
		fmt.Fprintf(os.Stderr, "warning: unpacking embedded guest binaries: %v\n", err)
		return Binaries{}, false
	}
	bins.Cached = true
	return bins, true
}

// extractAll writes the three binaries into outDir via a temp dir + rename, so
// an interrupted extraction never leaves a partial set that allExist accepts.
func extractAll(files fs.FS, outDir string) error {
	if err := os.MkdirAll(filepath.Dir(outDir), 0o755); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(filepath.Dir(outDir), "unpack-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	for _, m := range modules() {
		src, err := files.Open(m.name)
		if err != nil {
			return fmt.Errorf("%s: %w", m.name, err)
		}
		err = writeExecutable(filepath.Join(tmpDir, m.name), src)
		src.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", m.name, err)
		}
	}

	if err := os.Rename(tmpDir, outDir); err != nil {
		// A concurrent extraction may have won; its result is as good as ours.
		if _, statErr := os.Stat(outDir); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

func writeExecutable(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// GeneratePrebuilt builds the guest binaries for arch and writes them, plus a
// manifest, into destDir for `go:embed` to pick up. It is the implementation
// behind `make guestbin`; nothing at runtime calls it.
//
// It deliberately reuses Build, so the binaries a release embeds are produced
// by the same code path a source build uses — one compiler invocation, one set
// of flags, no second recipe to drift.
func GeneratePrebuilt(ctx context.Context, destDir, arch string) error {
	cache, err := os.MkdirTemp("", "clawk-guestbin-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(cache)

	bins, err := buildFromSource(ctx, cache, arch)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	for name, src := range map[string]string{
		"clawk-init":      bins.Init,
		"clawk-pty-agent": bins.Agent,
		"clawk-time-sync": bins.TimeSync,
	} {
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(destDir, name), data, 0o755); err != nil {
			return err
		}
	}

	m := agentembed.PrebuiltManifest{
		Arch:          arch,
		SourcesSHA256: agentembed.SourcesHash(),
	}
	if out, err := exec.Command("go", "env", "GOVERSION").Output(); err == nil {
		m.GoVersion = string(bytes.TrimSpace(out))
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destDir, agentembed.PrebuiltManifestName), append(data, '\n'), 0o644)
}
