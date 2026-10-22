package tui

import (
	"strings"

	"labmill-client/internal/textsafe"
)

// sanitize makes text from the agent or a cluster safe to display; it is
// textsafe.Clean, the sanitizer shared with the GUI. Callers sanitize the
// accumulated text, never single streamed deltas, so a sequence split across
// deltas is still removed whole.
func sanitize(s string) string { return textsafe.Clean(s) }

// oneLine collapses sanitized text to a single line for the status bar.
func oneLine(s string) string {
	return strings.Join(strings.Fields(sanitize(s)), " ")
}
