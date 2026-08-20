// Package cli holds the flag and exit plumbing the three long-running commands share.
package cli

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// MultiFlag collects a repeatable string flag, as in --el a --el b.
type MultiFlag []string

func (m *MultiFlag) String() string     { return strings.Join(*m, ",") }
func (m *MultiFlag) Set(v string) error { *m = append(*m, v); return nil }

// Fatal logs through the given logger and exits non-zero.
//
// The logger is a parameter rather than slog.Default() because the commands do not agree on
// a destination: pbtchaos writes to stdout and never installs a default, while pbtmonitor and
// pbthammer install a stderr one. Reaching for the default here would silently move pbtchaos's
// last words to another stream.
func Fatal(log *slog.Logger, format string, args ...any) {
	log.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}

// Trim shortens a server response for an error message. Error text from disruptoor and from
// the engine API can be a whole JSON document, and a log line is not the place for it.
func Trim(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
