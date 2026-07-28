package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

// reportDaemonFailure is the last chance to say WHY a first boot failed, because
// the VM dir (and the daemon log in it) is about to be deleted. It walks both
// providers' logs, and must not let an uninformative one stop the walk — a
// daemon log that exists but holds nothing printable is a partially flushed
// file, not an answer.
func TestReportDaemonFailureFallsThroughToTheOtherLog(t *testing.T) {
	report := func(t *testing.T, logs map[string]string) string {
		t.Helper()
		prev := store
		store = config.NewStoreAt(t.TempDir())
		t.Cleanup(func() { store = prev })

		sb := &config.Sandbox{Name: "boot-fail"}
		vmDir := store.VMDir(sb.Name)
		require.NoError(t, os.MkdirAll(vmDir, 0o755))
		for name, body := range logs {
			require.NoError(t, os.WriteFile(filepath.Join(vmDir, name), []byte(body), 0o644))
		}

		r, w, err := os.Pipe()
		require.NoError(t, err)
		orig := os.Stderr
		os.Stderr = w
		defer func() { os.Stderr = orig }()

		reportDaemonFailure(sb)
		require.NoError(t, w.Close())
		buf := make([]byte, 8192)
		n, _ := r.Read(buf)
		return string(buf[:n])
	}

	t.Run("blank fcd.log must not hide vzd.log", func(t *testing.T) {
		out := report(t, map[string]string{
			// Non-empty (so the len==0 skip doesn't fire) but with no printable
			// line: exactly what a boot killed right after openDaemonLog leaves.
			"fcd.log": "\n   \n\t\n",
			"vzd.log": "2026/07/28 10:00:00 FATAL: bridge: sudo ip: a password is required\n",
		})
		require.Contains(t, out, "FATAL: bridge: sudo ip: a password is required",
			"the other daemon's verdict is the only diagnosis there is; got %q", out)
	})

	t.Run("a log with a verdict wins and stops the walk", func(t *testing.T) {
		out := report(t, map[string]string{
			"fcd.log": "2026/07/28 10:00:00 FATAL: firecracker refused the drive\n",
			"vzd.log": "2026/07/28 10:00:00 FATAL: should not be reached\n",
		})
		require.Contains(t, out, "firecracker refused the drive")
		require.NotContains(t, out, "should not be reached")
	})

	t.Run("no logs at all stays silent", func(t *testing.T) {
		require.Empty(t, report(t, nil))
	})
}
