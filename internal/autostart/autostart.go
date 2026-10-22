// Package autostart registers the desktop app to start when the user logs
// in, for the current user only and only when the user turns the setting on
// ("Start Labmill in the tray when I log in", off by default):
//
//   - Windows: the value Name under HKCU\Software\Microsoft\Windows\CurrentVersion\Run.
//     Disable deletes that value.
//   - macOS: ~/Library/LaunchAgents/<Label>.plist with RunAtLoad. Disable
//     rewrites it with Disabled=true (launchd skips it) instead of deleting it.
//   - Linux: $XDG_CONFIG_HOME/autostart/<Name>.desktop (default ~/.config).
//     Disable rewrites it with Hidden=true, which the XDG autostart spec
//     treats as removed.
//
// Nothing needs administrator rights and nothing runs until the next login.
package autostart

import (
	"fmt"
	"strings"
)

// Entry describes the program to start at login.
type Entry struct {
	Name  string   // "labmill": Run value name (Windows) and .desktop file name (Linux)
	Label string   // reverse-DNS launchd label (macOS), e.g. "org.labmill.client"
	Exe   string   // absolute path of the executable
	Args  []string // e.g. ["--tray"]
}

// Enable registers e to start at login (replacing an earlier registration).
func Enable(e Entry) error { return enable(e) }

// Disable stops e from starting at login. Not being registered is no error.
func Disable(e Entry) error { return disable(e) }

// Enabled reports whether e is registered (and not disabled) to start at
// login.
func Enabled(e Entry) (bool, error) { return enabled(e) }

// windowsCommand is the Run value: the quoted executable and its arguments.
func windowsCommand(e Entry) string {
	parts := []string{`"` + e.Exe + `"`}
	for _, a := range e.Args {
		if strings.ContainsAny(a, " \t\"") {
			a = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// desktopEntry is the XDG autostart file; hidden marks it removed.
func desktopEntry(e Entry, hidden bool) []byte {
	var b strings.Builder
	b.WriteString("[Desktop Entry]\nType=Application\nName=labmill\n")
	b.WriteString("Comment=Start Labmill in the tray\n")
	b.WriteString("Exec=" + desktopExec(append([]string{e.Exe}, e.Args...)) + "\n")
	b.WriteString("Terminal=false\nX-GNOME-Autostart-enabled=" + fmt.Sprint(!hidden) + "\n")
	if hidden {
		b.WriteString("Hidden=true\n")
	}
	return []byte(b.String())
}

// desktopExec quotes argv for an Exec key (Desktop Entry spec: arguments
// with reserved characters in double quotes, with ", `, $ and \ escaped by a
// backslash; a literal % is written %%).
func desktopExec(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		a = strings.ReplaceAll(a, "%", "%%")
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\><~|&;$*?#()`") {
			r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`)
			a = `"` + r.Replace(a) + `"`
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

// launchAgentPlist is the launchd job; disabled sets Disabled=true.
func launchAgentPlist(e Entry, disabled bool) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	b.WriteString("\t<key>Label</key>\n\t<string>" + xmlEscape(e.Label) + "</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range append([]string{e.Exe}, e.Args...) {
		b.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>ProcessType</key>\n\t<string>Interactive</string>\n")
	if disabled {
		b.WriteString("\t<key>Disabled</key>\n\t<true/>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}
