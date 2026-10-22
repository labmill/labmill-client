package gui

import (
	"os/exec"
	"strings"

	"labmill-client/internal/textsafe"
)

// Desktop notifications (M64.3). The commands run behind function variables
// so tests don't start anything.
var (
	lookPath   = exec.LookPath
	runCommand = func(name string, args ...string) error { return exec.Command(name, args...).Run() }
)

const maxNotifyRunes = 200

// cleanNotify makes untrusted text (a job name) safe and short for the OS.
func cleanNotify(s string) string {
	s = strings.Join(strings.Fields(textsafe.Clean(s)), " ")
	if r := []rune(s); len(r) > maxNotifyRunes {
		s = string(r[:maxNotifyRunes-1]) + "…"
	}
	return s
}

// DesktopNotify shows a desktop notification; it never blocks the caller
// and does nothing where the OS has no usable way (Linux without notify-send).
func DesktopNotify(title, text string) {
	title, text = cleanNotify(title), cleanNotify(text)
	go platformNotify(title, text)
}

// unixNotify is the macOS and Linux part. The text goes as separate
// arguments (AppleScript reads them from argv), so it is never parsed as
// script or as an option.
func unixNotify(goos, title, text string) {
	switch goos {
	case "darwin":
		_ = runCommand("osascript",
			"-e", "on run argv",
			"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
			"-e", "end run", title, text)
	case "linux":
		if _, err := lookPath("notify-send"); err == nil {
			_ = runCommand("notify-send", "-a", "labmill", "--", title, text)
		}
	}
}
