package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// fakeFolders is a Folders that refuses paths containing "secret".
type fakeFolders struct {
	m    map[string]string
	sets []string // "kind=path" per SetFolder call
}

func (f *fakeFolders) Folder(kind string) string { return f.m[kind] }
func (f *fakeFolders) SetFolder(kind, path string) (string, error) {
	f.sets = append(f.sets, kind+"="+path)
	if strings.Contains(path, "secret") {
		return "", errors.New(path + " is private, so I won't use it.")
	}
	f.m[kind] = path
	return path, nil
}
func (f *fakeFolders) Short(p string) string { return "…/" + filepath.Base(p) }

type countSession struct{ texts []string }

func (s *countSession) Send(_ context.Context, text string) { s.texts = append(s.texts, text) }

func newFolderModel(t *testing.T, home string) (*model, *fakeFolders, *countSession) {
	t.Helper()
	f := &fakeFolders{m: map[string]string{}}
	s := &countSession{}
	m := newModel(Options{Status: "hpcone (Slurm)", Folders: f, Home: home, Session: s, Jobs: nil})
	m.now = advancingClock()
	step(t, m, tea.WindowSizeMsg{Width: 140, Height: 30})
	return m, f, s
}

func lastEntry(m *model) entry { return m.entries[len(m.entries)-1] }

// enter puts text in the input and presses Enter.
func enter(t *testing.T, m *model, text string) {
	t.Helper()
	m.input.SetValue(text)
	step(t, m, kp(tea.KeyEnter))
}

func TestFolderCommands(t *testing.T) {
	m, f, s := newFolderModel(t, "/home/ann")

	enter(t, m, "/code")
	if e := lastEntry(m); e.role != roleNotice || !strings.Contains(e.text, "No code folder chosen") {
		t.Errorf("/code alone: %+v", e)
	}
	enter(t, m, "/code /home/ann/proj")
	if f.m["code"] != "/home/ann/proj" || !strings.Contains(lastEntry(m).text, "Code folder: /home/ann/proj") {
		t.Errorf("/code set: %v, %+v", f.m, lastEntry(m))
	}
	enter(t, m, `/data "/home/ann/my data"`)
	if f.m["data"] != "/home/ann/my data" {
		t.Errorf("/data with quotes: %v", f.m)
	}
	if v := content(m); !strings.Contains(v, "Code: …/proj") || !strings.Contains(v, "Data: …/my data") {
		t.Errorf("status bar lacks the folders:\n%s", v)
	} else if strings.Index(v, "Data: …/my data") > strings.Index(v, "Code: …/proj") {
		t.Errorf("status bar should list the data folder before the code folder:\n%s", v)
	}
	enter(t, m, "/code")
	if !strings.Contains(lastEntry(m).text, "Code folder: /home/ann/proj") {
		t.Errorf("/code show: %+v", lastEntry(m))
	}
	enter(t, m, "/data /home/ann/secret")
	if e := lastEntry(m); e.role != roleError || !strings.Contains(e.text, "private") || f.m["data"] != "/home/ann/my data" {
		t.Errorf("refused folder: %+v, %v", e, f.m)
	}
	enter(t, m, "/code -")
	if f.m["code"] != "" || !strings.Contains(lastEntry(m).text, "cleared") {
		t.Errorf("/code -: %v, %+v", f.m, lastEntry(m))
	}
	if strings.Contains(content(m), "Code:") {
		t.Error("the status bar still shows the cleared code folder")
	}
	// A relative folder is taken from the current folder; "~" is left to Folders.
	enter(t, m, "/data sub")
	wd, _ := os.Getwd()
	if f.m["data"] != filepath.Join(wd, "sub") {
		t.Errorf("relative /data = %q", f.m["data"])
	}
	enter(t, m, "/data ~/x")
	if f.m["data"] != "~/x" {
		t.Errorf("~ /data = %q", f.m["data"])
	}
	// None of this started a turn; other slash text still goes to Labmill.
	if len(s.texts) != 0 || m.running {
		t.Errorf("folder commands started a turn: %q", s.texts)
	}
	enter(t, m, "/codes are fun")
	if m.input.Value() != "" || !m.running {
		t.Errorf("a non-command was not sent (running=%v input=%q)", m.running, m.input.Value())
	}
}

func TestFolderCommandsWithoutFolders(t *testing.T) {
	m := newDirect(t, 100, 30)
	enter(t, m, "/code /tmp")
	if e := lastEntry(m); e.role != roleError || m.running {
		t.Errorf("without Folders: %+v running=%v", e, m.running)
	}
}

func TestCompleteFolder(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{"projects/mri", "projects/mice", "papers", ".config", "photos"} {
		os.MkdirAll(filepath.Join(home, filepath.FromSlash(d)), 0o755)
	}
	os.WriteFile(filepath.Join(home, "pa.txt"), []byte("x"), 0o644)
	ctrl := runtime.GOOS != "windows" && os.Mkdir(filepath.Join(home, "pz\x1b[2J"), 0o755) == nil
	sep := string(filepath.Separator)
	h := func(p string) string { return filepath.Join(home, filepath.FromSlash(p)) }
	for _, c := range []struct {
		in, want string
		matches  int
		ok       bool
	}{
		{"/code " + h("proj"), "/code " + h("projects") + sep, 0, true},
		{"/code " + h("projects") + sep + "m", "/code " + h("projects") + sep + "m", 2, true},
		{"/code " + h("projects") + sep + "mr", "/code " + h("projects") + sep + "mri" + sep, 0, true},
		{"/code " + h("pa"), "/code " + h("papers") + sep, 0, true}, // the file pa.txt is no candidate
		{"/data " + home + sep + ".", "/data " + h(".config") + sep, 0, true},
		{"/data " + h("p"), "/data " + h("p"), 3, true},
		{"/data " + h("zzz"), "/data " + h("zzz"), 0, true},
		{"/code ~" + sep + "pro", "/code ~" + sep + "projects" + sep, 0, true},
		{"/code ~", "", 0, true}, // home is listed: checked below
		{"/code", "/code ", 0, true},
		{"hello", "hello", 0, false},
	} {
		got, matches, ok := completeFolder(c.in, home)
		if c.in == "/data "+h("p") && ctrl && len(matches) != 3 {
			t.Errorf("a name with control characters was offered: %q", matches)
		}
		if c.in == "/code ~" {
			if !ok || !strings.HasPrefix(got, "/code ~"+sep) {
				t.Errorf("completeFolder(%q) = %q, %v", c.in, got, ok)
			}
			continue
		}
		if got != c.want || len(matches) != c.matches || ok != c.ok {
			t.Errorf("completeFolder(%q) = %q, %q, %v; want %q, %d matches, %v", c.in, got, matches, ok, c.want, c.matches, c.ok)
		}
	}
}

// Tab completes a folder command instead of toggling the jobs panel.
func TestTabCompletesFolderCommand(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "projects"), 0o755)
	m, _, _ := newFolderModel(t, home)
	m.o.Jobs = nil
	m.input.SetValue("/code ~" + string(filepath.Separator) + "pro")
	step(t, m, kp(tea.KeyTab))
	if got := m.input.Value(); got != "/code ~"+string(filepath.Separator)+"projects"+string(filepath.Separator) {
		t.Errorf("after Tab: %q", got)
	}
	if m.showJobs {
		t.Error("Tab toggled the jobs panel")
	}
}
