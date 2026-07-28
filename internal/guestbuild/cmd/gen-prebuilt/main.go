// gen-prebuilt cross-compiles the in-guest binaries and writes them, with a
// manifest, into internal/agentembed/prebuilt/ for go:embed to pick up. Run by
// `make guestbin` before building a release artifact, so the shipped clawk
// needs no Go toolchain to boot a sandbox.
//
// The guest architecture must match the artifact's host architecture —
// hardware virtualization cannot cross architectures, so an arm64 clawk only
// ever boots arm64 guests. It defaults to the host's GOARCH; -arch overrides
// it when cross-building a release for another platform.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/clawkwork/clawk/internal/agentembed"
	"github.com/clawkwork/clawk/internal/guestbuild"
)

func main() {
	arch := flag.String("arch", runtime.GOARCH, "guest GOARCH to build for (must match the target host's)")
	out := flag.String("out", defaultOut(), "directory to write the binaries and manifest into")
	flag.Parse()

	if err := guestbuild.GeneratePrebuilt(context.Background(), *out, *arch); err != nil {
		fmt.Fprintf(os.Stderr, "gen-prebuilt: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  guest binaries for linux/%s → %s (sources %.12s)\n",
		*arch, *out, agentembed.SourcesHash())
}

// defaultOut locates the embed directory from this file's own path, so the
// target works from any working directory.
func defaultOut() string {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("internal", "agentembed", agentembed.PrebuiltDir)
	}
	// .../internal/guestbuild/cmd/gen-prebuilt/main.go → .../internal/agentembed/prebuilt
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(self))))
	return filepath.Join(root, "agentembed", agentembed.PrebuiltDir)
}
