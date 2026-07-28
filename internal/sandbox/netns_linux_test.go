//go:build linux

package sandbox

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// buildClawk compiles the real binary: the namespace helpers are re-execs of
// it, and os.Executable() under `go test` is the test binary, which has no
// InitNetNSHelpers dispatch.
func buildClawk(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("`go` not on PATH")
	}
	bin := filepath.Join(t.TempDir(), "clawk")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/clawkwork/clawk/cmd/clawk").CombinedOutput()
	require.NoErrorf(t, err, "building clawk:\n%s", out)
	return bin
}

// TestSandboxNetNS exercises the whole rootless path on a host that allows it:
// the unprivileged user+network namespace, the netlink topology inside it, the
// TAP fd crossing back over SCM_RIGHTS, and the exec trampoline.
//
// Skips (rather than fails) where the host forbids it — an unprivileged
// user namespace can be denied by AppArmor (Ubuntu 24.04+) or a sysctl, and
// /dev/net/tun is not always accessible to non-root. Those hosts fall back to
// bridge mode in production, which is the behavior asserted below.
func TestSandboxNetNS(t *testing.T) {
	ns, err := startSandboxNetNS(buildClawk(t), nil)
	if err != nil {
		require.ErrorIs(t, err, errNetNSUnavailable,
			"a host that can't do rootless must say so in a way the provider can fall back on")
		t.Skipf("rootless networking unavailable here: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.HostTAP.Close()
		_ = ns.Close()
	})

	t.Run("tap fd is poller-owned", func(t *testing.T) {
		// machine.UserMode.HostTAPFile requires nonblocking, or the frame pump
		// can't be shut down. The backend rejects a blocking fd, so this is
		// the contract both sides agree on.
		rc, err := ns.HostTAP.SyscallConn()
		require.NoError(t, err)
		var flags int
		require.NoError(t, rc.Control(func(fd uintptr) {
			r, _, _ := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
			flags = int(r)
		}))
		require.NotZero(t, flags&syscall.O_NONBLOCK, "tap fd must be nonblocking")
	})

	t.Run("topology exists inside the namespace", func(t *testing.T) {
		out := inNetNS(t, ns, "sh", "-c",
			"cat /sys/class/net/"+nsBridge+"/flags; "+
				"basename $(readlink /sys/class/net/"+nsGuestTAP+"/master); "+
				"basename $(readlink /sys/class/net/"+nsHostTAP+"/master)")
		lines := strings.Fields(out)
		require.Len(t, lines, 3, "expected bridge flags + two masters, got %q", out)

		flags, err := parseHexFlags(lines[0])
		require.NoError(t, err)
		require.NotZero(t, flags&iffUp, "bridge must be up (flags=%s)", lines[0])
		require.Equal(t, nsBridge, lines[1], "guest tap must be enslaved to the bridge")
		require.Equal(t, nsBridge, lines[2], "gvproxy tap must be enslaved to the bridge")
	})

	t.Run("host network is untouched", func(t *testing.T) {
		// The point of the namespace: no clawk interfaces on the host at all,
		// so nothing to leak and nothing to clean up.
		for _, dev := range []string{nsBridge, nsGuestTAP, nsHostTAP} {
			require.False(t, linkExists(dev),
				"%s must not exist in the host namespace", dev)
		}
	})

	t.Run("namespace is not the host's", func(t *testing.T) {
		inside := strings.TrimSpace(inNetNS(t, ns, "readlink", "/proc/self/ns/net"))
		outside, err := os.Readlink("/proc/self/ns/net")
		require.NoError(t, err)
		require.NotEqual(t, outside, inside, "trampoline did not change namespace")
	})
}

// TestSandboxNetNSFailsFast pins the behavior that made the first version of
// this code take 15s to report a dead anchor: the parent must not hold the
// child's socket end, or a helper that dies instantly costs a full handshake
// timeout instead of surfacing its error.
func TestSandboxNetNSFailsFast(t *testing.T) {
	// A binary that exits immediately stands in for an anchor that dies on its
	// first syscall.
	fake := filepath.Join(t.TempDir(), "false")
	require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\nexit 3\n"), 0o755))

	done := make(chan error, 1)
	go func() {
		_, err := startSandboxNetNS(fake, nil)
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
		require.ErrorIs(t, err, errNetNSUnavailable)
	case <-time.After(anchorStartTimeout - time.Second):
		t.Fatalf("a dead anchor should fail immediately, not wait out the %s handshake timeout",
			anchorStartTimeout)
	}
}

// TestSandboxNetNSClose covers teardown without needing a real namespace: the
// anchor is any process that reads stdin, since that is the whole contract —
// closing the pipe ends it, and the namespace dies with it.
func TestSandboxNetNSClose(t *testing.T) {
	t.Run("reaps an anchor that exits on stdin close", func(t *testing.T) {
		cmd, stdin := fakeAnchor(t, "cat") // exits at EOF
		ns := &sandboxNetNS{anchor: cmd, stdin: stdin}

		start := time.Now()
		require.NoError(t, ns.Close())
		require.Less(t, time.Since(start), anchorExitGrace,
			"should exit on stdin close, not by hitting the grace timeout")
		// Reaped, not merely signalled: no zombie left behind.
		require.Error(t, cmd.Process.Signal(syscall.Signal(0)))
	})

	t.Run("kills an anchor that ignores stdin", func(t *testing.T) {
		// A wedged helper must never hang a daemon shutdown.
		old := anchorExitGrace
		anchorExitGrace = 50 * time.Millisecond
		t.Cleanup(func() { anchorExitGrace = old })

		cmd, stdin := fakeAnchor(t, "sleep", "60")
		ns := &sandboxNetNS{anchor: cmd, stdin: stdin}

		done := make(chan struct{})
		go func() { _ = ns.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Close hung on an anchor that ignores stdin")
		}
		require.Error(t, cmd.Process.Signal(syscall.Signal(0)))
	})

	t.Run("is idempotent", func(t *testing.T) {
		cmd, stdin := fakeAnchor(t, "cat")
		ns := &sandboxNetNS{anchor: cmd, stdin: stdin}
		require.NoError(t, ns.Close())
		require.NoError(t, ns.Close())
	})
}

// fakeAnchor starts a stand-in anchor process wired like the real one: stdin is
// a pipe the caller holds, stderr is a teeBuffer.
func fakeAnchor(t *testing.T, argv ...string) (*exec.Cmd, *os.File) {
	t.Helper()
	if _, err := exec.LookPath(argv[0]); err != nil {
		t.Skipf("%s not available", argv[0])
	}
	r, w, err := os.Pipe()
	require.NoError(t, err)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = r
	cmd.Stderr = &teeBuffer{max: 64}
	require.NoError(t, cmd.Start())
	require.NoError(t, r.Close()) // the child holds it now
	return cmd, w
}

func TestNetNSHint(t *testing.T) {
	t.Run("permission errors name the likely cause", func(t *testing.T) {
		hint := netNSHint(errors.New("fork/exec: operation not permitted"))
		require.NotEmpty(t, hint)
		// Whichever branch this host takes, the hint has to name something the
		// user can act on rather than echo errno.
		require.Regexp(t, `apparmor|max_user_namespaces|refused`, hint)
	})
	t.Run("other errors pass through", func(t *testing.T) {
		require.Contains(t, netNSHint(errors.New("no such file or directory")),
			"no such file or directory")
	})
}

func TestTeeBuffer(t *testing.T) {
	var out bytes.Buffer
	tb := &teeBuffer{out: &out, max: 8}
	n, err := tb.Write([]byte("hello "))
	require.NoError(t, err)
	require.Equal(t, 6, n)
	_, err = tb.Write([]byte("world\n"))
	require.NoError(t, err)

	require.Equal(t, "hello world", strings.TrimSpace(out.String()), "must forward everything")
	require.Equal(t, "hello wo", tb.String(), "must retain only the first max bytes")
}

// inNetNS runs a command inside the namespace via the exec trampoline and
// returns its combined output.
func inNetNS(t *testing.T, ns *sandboxNetNS, argv ...string) string {
	t.Helper()
	full := append(append([]string{}, ns.ExecPrefix...), argv...)
	out, err := exec.Command(full[0], full[1:]...).CombinedOutput()
	require.NoErrorf(t, err, "%v:\n%s", full, out)
	return string(out)
}

func parseHexFlags(s string) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(s), 0, 64)
}
