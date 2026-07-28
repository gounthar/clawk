package sandbox

import (
	"fmt"
	"os"
	"strings"
)

// ConsoleTail returns the last n non-empty lines of a guest console log,
// framed for embedding in a boot-failure message. Returns "" when the
// log is missing or empty — callers append it to their error text
// unconditionally.
//
// Boot failures are diagnosed from the guest console essentially every
// time (kernel panics, clawk-init errors, missing init binaries); making
// the CLI surface it directly turns "go read this file" into an answer.
func ConsoleTail(path string, n int) string {
	return LogTail(path, n, "last guest console output")
}

// LogTail is ConsoleTail for any log file, with a caller-chosen label. The
// host-side daemon log needs the same treatment as the guest console: it
// holds the real cause of most boot failures, and on a first `clawk` in a
// directory the rollback deletes it moments later.
func LogTail(path string, n int, label string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return fmt.Sprintf("\n--- %s (%s) ---\n%s\n---",
		label, path, strings.Join(lines, "\n"))
}
