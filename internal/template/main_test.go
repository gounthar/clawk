package template

import (
	"os"
	"testing"
)

// TestMain disables the host-wide clawk.mod for the whole package. Without
// this, every loader test would silently pick up whatever the developer (or
// CI runner) has in ~/.config/clawk/clawk.mod and assert against a moving
// target. Tests that mean to exercise the layer re-enable it through
// withGlobalMod (see global_test.go).
func TestMain(m *testing.M) {
	GlobalDisabled = true
	os.Exit(m.Run())
}
