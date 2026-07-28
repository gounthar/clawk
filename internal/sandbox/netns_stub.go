//go:build !linux

package sandbox

// InitNetNSHelpers is a no-op off Linux. The hidden subcommands it dispatches
// are re-execs of the clawk binary into a network namespace, which only the
// firecracker provider needs (see netns_linux.go); every other platform has no
// such helper to dispatch.
//
// It exists because main calls it unconditionally, before cobra — the helper
// paths must not touch flag parsing or the config store, so they cannot be
// hidden behind a build-tagged provider. Deleting the previous half of this
// pair (InitRootHelpers' !linux stub, removed with the loop mount) is what
// broke the macOS build once; keep the two definitions in lock-step.
func InitNetNSHelpers() {}
