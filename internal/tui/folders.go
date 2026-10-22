package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Folders is the user's chosen data and code folders (*app.Workspace in
// production): /data and /code set them, the status bar shows them, and
// the runtime tells Claude about them every turn. Kinds are "code" and
// "data". Choosing a folder records only its path; nothing is read or sent.
type Folders interface {
	// Folder returns the chosen folder of kind ("" when none).
	Folder(kind string) string
	// SetFolder checks path (app.CheckFolder: an existing folder, not a
	// drive root, not a system, hidden or secret-looking folder; a leading
	// "~" is home) and records it; "" clears. It returns the stored path.
	SetFolder(kind, path string) (string, error)
	// Short shortens a folder for display ("~/…/mri/code").
	Short(path string) string
}

// folderCmd reports whether text is a /data or /code command and returns
// its kind and argument (trimmed).
func folderCmd(text string) (kind, arg string, ok bool) {
	for _, k := range []string{"data", "code"} {
		c := "/" + k
		if text == c {
			return k, "", true
		}
		if strings.HasPrefix(text, c+" ") || strings.HasPrefix(text, c+"\t") {
			return k, strings.TrimSpace(text[len(c):]), true
		}
	}
	return "", "", false
}

// unquote drops one pair of matching surrounding quotes (a pasted path
// with spaces).
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// folderCommand runs /code and /data: with no argument it shows the
// folder, "-" clears it, anything else sets it (a relative path is taken
// from the current folder). The answer is a notice line in the chat (an
// error line when the folder is refused). It never starts a turn, so it
// also works while one runs; the next message carries the change.
func (m *model) folderCommand(kind, arg string) {
	label := strings.ToUpper(kind[:1]) + kind[1:] + " folder"
	if m.o.Folders == nil {
		m.addEntry(roleError, "Choosing folders isn't available here.")
		return
	}
	switch arg = unquote(arg); arg {
	case "":
		if f := m.o.Folders.Folder(kind); f != "" {
			m.addEntry(roleNotice, fmt.Sprintf("%s: %s (/%s - clears it)", label, f, kind))
		} else {
			m.addEntry(roleNotice, fmt.Sprintf("No %s folder chosen. Use /%s <folder>, for example /%s ~/projects/mri.", kind, kind, kind))
		}
		return
	case "-":
		if _, err := m.o.Folders.SetFolder(kind, ""); err != nil {
			m.addEntry(roleError, err.Error())
			return
		}
		m.addEntry(roleNotice, label+" cleared.")
		return
	}
	if !filepath.IsAbs(arg) && !strings.HasPrefix(arg, "~") {
		if abs, err := filepath.Abs(arg); err == nil {
			arg = abs
		}
	}
	got, err := m.o.Folders.SetFolder(kind, arg)
	if err != nil {
		m.addEntry(roleError, err.Error())
		return
	}
	m.addEntry(roleNotice, fmt.Sprintf("%s: %s. Your next message tells Labmill; nothing is sent until you approve it.", label, got))
}

// foldersStatus is the status bar's folder part (" · Data: ~/y · Code: ~/x"),
// "" when none is chosen. Data comes first (M24): many users have only data.
func (m *model) foldersStatus() string {
	if m.o.Folders == nil {
		return ""
	}
	var s string
	if f := m.o.Folders.Folder("data"); f != "" {
		s += " · Data: " + m.o.Folders.Short(f)
	}
	if f := m.o.Folders.Folder("code"); f != "" {
		s += " · Code: " + m.o.Folders.Short(f)
	}
	return s
}

// maxCompletions is how many candidates a Tab shows in the status bar.
const maxCompletions = 8

// completeFolder completes the folder argument of a /code or /data line:
// it returns the new input and, when several folders match, their names
// (at most maxCompletions, sorted). Hidden folders are offered only when
// the typed name starts with ".". A leading "~" stays as typed; home is
// used to list it. ok is false when input is not a folder command.
func completeFolder(input, home string) (out string, matches []string, ok bool) {
	if input == "/code" || input == "/data" {
		return input + " ", nil, true
	}
	kind, arg, isCmd := folderCmd(input)
	if !isCmd {
		return input, nil, false
	}
	prefix := "/" + kind + " "
	sep := string(filepath.Separator)
	typed := arg
	if typed == "" {
		typed = "." + sep
	}
	real := typed
	if home != "" && (real == "~" || strings.HasPrefix(real, "~/") || strings.HasPrefix(real, "~"+sep)) {
		real = home + real[1:]
		if typed == "~" {
			typed, real = "~"+sep, home+sep
		}
	}
	dir, base := real, ""
	tdir := typed
	if !strings.HasSuffix(real, sep) && !strings.HasSuffix(real, "/") {
		dir, base = filepath.Dir(real), filepath.Base(real)
		tdir = typed[:len(typed)-len(base)]
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return input, nil, true
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, base) || (strings.HasPrefix(n, ".") && !strings.HasPrefix(base, ".")) || sanitize(n) != n {
			continue // hidden unless asked for; never a name with control characters
		}
		if fi, err := os.Stat(filepath.Join(dir, n)); err != nil || !fi.IsDir() {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	switch len(names) {
	case 0:
		return input, nil, true
	case 1:
		return prefix + tdir + names[0] + sep, nil, true
	}
	common := names[0]
	for _, n := range names[1:] {
		for !strings.HasPrefix(n, common) {
			common = common[:len(common)-1]
		}
	}
	for !utf8.ValidString(common) {
		common = common[:len(common)-1]
	}
	if len(names) > maxCompletions {
		names = append(names[:maxCompletions:maxCompletions], "…")
	}
	return prefix + tdir + common, names, true
}
