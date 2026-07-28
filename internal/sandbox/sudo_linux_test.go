//go:build linux

package sandbox

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSudoOutputMentionsPassword(t *testing.T) {
	// What sudo -n actually prints when it declines to authenticate. We force
	// LC_ALL=C so this stays the message we get (see cLocaleEnv).
	require.True(t, sudoOutputMentionsPassword([]byte("sudo: a password is required")))
	require.True(t, sudoOutputMentionsPassword([]byte("[sudo] Password for deploy:")))
	// Command failures must NOT be classified as auth problems — a retry would
	// re-run a command that already ran.
	require.False(t, sudoOutputMentionsPassword([]byte("RTNETLINK answers: File exists")))
	// "no terminal to read the password" is also an auth failure: sudo wants a
	// password and cannot ask, which is precisely the retry-on-a-tty case.
	require.True(t, sudoOutputMentionsPassword([]byte(
		"sudo: a terminal is required to read the password; either use the -S option")))
	// A policy rejection is not an auth failure; prompting cannot fix it.
	require.False(t, sudoOutputMentionsPassword([]byte(
		"Sorry, user deploy is not allowed to execute '/usr/sbin/ip' as root")))
	// The same refusal worded without the word "password", which older sudo
	// emits. This used to be caught by a `sudo -n true` fallback probe — and
	// that probe also caught every ordinary command failure on a sudoers that
	// permits `ip` alone, which is why matching the message is the whole test
	// now (see sudoOutputMentionsPassword).
	require.True(t, sudoOutputMentionsPassword([]byte(
		"sudo: no tty present and no askpass program specified")))
	// A host that grants NOPASSWD for ip only: `sudo -n true` fails there on
	// every host, so nothing may treat a command's own failure as an auth
	// problem — runSudo would re-run a privileged command that already ran.
	require.False(t, sudoOutputMentionsPassword([]byte(
		"Cannot find device \"clawkbr1234abcd\"")))
}

func TestCLocaleEnv(t *testing.T) {
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	t.Setenv("LANG", "fr_FR.UTF-8")
	t.Setenv("CLAWK_KEEP_ME", "yes")

	env := cLocaleEnv()
	var lcAll, lang []string
	var kept bool
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "LC_ALL="):
			lcAll = append(lcAll, kv)
		case strings.HasPrefix(kv, "LANG="):
			lang = append(lang, kv)
		case kv == "CLAWK_KEEP_ME=yes":
			kept = true
		}
	}
	require.Equal(t, []string{"LC_ALL=C"}, lcAll, "exactly one LC_ALL, forced to C")
	require.Empty(t, lang, "a localized LANG would still translate sudo's message")
	require.True(t, kept, "the rest of the environment must survive")
	require.Len(t, env, len(os.Environ())-1, "LANG dropped, LC_ALL replaced")
}
