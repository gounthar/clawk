package cli

import (
	"os"
	"testing"

	"github.com/clawkwork/clawk/internal/template"
)

// TestMain runs the whole package as if --no-global had been passed: a
// developer's own ~/.config/clawk/clawk.mod must never leak into the sandbox
// records these tests compose.
//
// Both knobs are set. GlobalDisabled covers tests that call the loaders
// directly; noGlobalFlag covers those going through executeCommand, whose
// PersistentPreRunE re-derives GlobalDisabled from the flag on every
// invocation. Tests for the layer itself flip both (see withGlobalMod in
// global_defaults_test.go).
func TestMain(m *testing.M) {
	template.GlobalDisabled = true
	noGlobalFlag = true
	os.Exit(m.Run())
}
