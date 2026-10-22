package gui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/app"
	"labmill-client/internal/store"
)

type folderFixture struct {
	app     *App
	env     *app.Env
	home    string
	picked  string // what the stub dialog returns
	pickErr error
	picks   []string // "title|start" per dialog
	opened  []string
	jobs    []store.Job
}

func newFolderFixture(t *testing.T) *folderFixture {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"code", "data", ".hidden", "labmill-results/job7"} {
		if err := os.MkdirAll(filepath.Join(home, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &folderFixture{home: home}
	f.env = &app.Env{LocalRoots: []string{home}, ResultsDir: filepath.Join(home, "labmill-results"), Workspace: app.NewWorkspace(home, "")}
	f.app = NewApp(NewBridge(func(string, any) {}), &stepSession{}, func(context.Context) ([]store.Job, error) { return f.jobs, nil }, "")
	f.app.SetFolderHost(FolderHost{
		Env: func() *app.Env { return f.env },
		Pick: func(title, start string) (string, error) {
			f.picks = append(f.picks, title+"|"+start)
			return f.picked, f.pickErr
		},
		Open: func(dir string) error { f.opened = append(f.opened, dir); return nil },
	})
	return f
}

func TestPickAndSetFolder(t *testing.T) {
	f := newFolderFixture(t)
	code := filepath.Join(f.home, "code")

	// Canceled dialog: nothing changes, no error.
	st, err := f.app.PickFolder(app.FolderCode)
	if err != nil || st.Code.Path != "" || len(f.picks) != 1 || f.picks[0] != "Choose your code folder|"+f.home {
		t.Fatalf("canceled pick: %+v, %v, picks %q", st, err, f.picks)
	}
	f.picked = code
	st, err = f.app.PickFolder(app.FolderCode)
	if err != nil || st.Code.Path != code || st.Code.Short == "" || st.Code.Short == code || len(st.RecentCode) != 1 {
		t.Fatalf("pick: %+v, %v", st, err)
	}
	if f.picks[1] != "Choose your code folder|"+f.home {
		t.Errorf("second dialog: %q", f.picks[1])
	}
	f.app.PickFolder(app.FolderCode)
	if f.picks[2] != "Choose your code folder|"+code {
		t.Errorf("the dialog doesn't start in the chosen folder: %q", f.picks[2])
	}
	if f.env.Workspace.Folder(app.FolderCode) != code {
		t.Errorf("workspace code = %q", f.env.Workspace.Folder(app.FolderCode))
	}

	// A refused folder (dialog or drop) keeps the state and says why.
	f.picked = filepath.Join(f.home, ".hidden")
	st, err = f.app.PickFolder(app.FolderData)
	if err == nil || st.Data.Path != "" || st.Code.Path != code {
		t.Errorf("hidden folder: %+v, %v", st, err)
	}
	if _, err := f.app.SetFolder(app.FolderData, "/"); err == nil {
		t.Error("a drive root was accepted")
	}
	f.pickErr = errors.New("no display")
	if _, err := f.app.PickFolder(app.FolderData); err == nil || !strings.Contains(err.Error(), "no display") {
		t.Errorf("dialog error: %v", err)
	}
	if _, err := f.app.PickFolder("music"); err == nil {
		t.Error("unknown kind accepted")
	}

	// Drop / recent menu, then clear.
	st, err = f.app.SetFolder(app.FolderData, filepath.Join(f.home, "data"))
	if err != nil || st.Data.Path != filepath.Join(f.home, "data") {
		t.Fatalf("set data: %+v, %v", st, err)
	}
	st, err = f.app.SetFolder(app.FolderCode, "")
	if err != nil || st.Code.Path != "" || len(st.RecentCode) != 1 || st.RecentCode[0].Path != code {
		t.Errorf("clear: %+v, %v", st, err)
	}
	if st, _ := f.app.Folders(); st.Data.Path == "" || st.RecentData == nil {
		t.Errorf("Folders: %+v", st)
	}
}

func TestFoldersBeforeRuntime(t *testing.T) {
	a := NewApp(NewBridge(func(string, any) {}), &stepSession{}, nil, "")
	if _, err := a.Folders(); err == nil {
		t.Error("Folders without a host succeeded")
	}
	a.SetFolderHost(FolderHost{Env: func() *app.Env { return nil }})
	st, err := a.PickFolder(app.FolderCode)
	if err == nil || !strings.Contains(err.Error(), "starting") || st.RecentCode == nil {
		t.Errorf("PickFolder while starting: %+v, %v", st, err)
	}
	if err := a.OpenJobFolder(1); err == nil {
		t.Error("OpenJobFolder while starting succeeded")
	}
}

func TestOpenJobFolder(t *testing.T) {
	f := newFolderFixture(t)
	res := filepath.Join(f.home, "labmill-results", "job7")
	f.jobs = []store.Job{
		{ID: 7, State: store.StateFetched, RemoteID: "1", LocalDir: res},
		{ID: 8, State: store.StateCompleted, RemoteID: "2", LocalDir: res},
		{ID: 9, State: store.StateFetched, RemoteID: "3", LocalDir: filepath.Join(f.home, ".hidden")},
		{ID: 10, State: store.StateFetched, RemoteID: "4", LocalDir: filepath.Join(f.home, "labmill-results", "gone")},
	}
	if err := f.app.OpenJobFolder(7); err != nil || !reflect.DeepEqual(f.opened, []string{res}) {
		t.Fatalf("open job 7: %v, opened %q", err, f.opened)
	}
	if err := f.app.JobAction(7, ActOpen); err != nil || len(f.opened) != 2 {
		t.Errorf("JobAction open: %v, opened %q", err, f.opened)
	}
	for _, id := range []int64{8, 9, 10, 99} {
		if err := f.app.OpenJobFolder(id); err == nil {
			t.Errorf("job %d opened", id)
		}
	}
	if len(f.opened) != 2 {
		t.Errorf("refused jobs reached the opener: %q", f.opened)
	}
}

func TestOpenCommand(t *testing.T) {
	for _, c := range []struct {
		goos, dir, name string
		ok              bool
	}{
		{"linux", "/home/ann/labmill-results/j", "xdg-open", true},
		{"darwin", "/Users/ann/labmill-results/j", "/usr/bin/open", true},
		{"windows", `C:\Users\ann\labmill-results\j`, `\explorer.exe`, true},
		{"windows", `\\server\share\j`, `\explorer.exe`, true},
		{"linux", "relative/dir", "", false},
		{"linux", "-n", "", false},
		{"darwin", "", "", false},
		{"windows", `C:\Users\ann\a,b`, "", false},
		{"windows", `C:\Users\ann\"x`, "", false},
		{"windows", `relative\dir`, "", false},
		{"linux", "/home/ann/x\x1b[2J", "", false},
		{"linux", "/home/ann/x\ny", "", false},
	} {
		name, args, err := openCommand(c.goos, c.dir)
		if (err == nil) != c.ok {
			t.Errorf("openCommand(%s, %q) err = %v, want ok=%v", c.goos, c.dir, err, c.ok)
			continue
		}
		want := c.dir
		if c.goos == "windows" {
			want += `\` // Explorer opens folders only for an argument ending in a backslash
		}
		if c.ok && (!strings.HasSuffix(name, c.name) || !reflect.DeepEqual(args, []string{want})) {
			t.Errorf("openCommand(%s, %q) = %q %q", c.goos, c.dir, name, args)
		}
	}
}

func TestOpenFolderStartsCommand(t *testing.T) {
	dir := t.TempDir()
	var got []string
	old := startCommand
	defer func() { startCommand = old }()
	startCommand = func(name string, args ...string) error { got = append([]string{name}, args...); return nil }
	if err := OpenFolder(dir); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != dir {
		t.Errorf("started %q", got)
	}
	if err := OpenFolder(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing folder was opened")
	}
}

// A successful fetch_results result carries its call's job_id; an error
// result, another tool or a declined fetch does not.
func TestBridgeFetchResultsJobID(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	b.Event(agent.Event{Kind: agent.EventToolCall, Tool: "fetch_results", Input: []byte(`{"job_id":12,"paths":["out"]}`)})
	b.Event(agent.Event{Kind: agent.EventToolResult, Tool: "fetch_results", Text: "Downloaded 1 file to /x"})
	b.Event(agent.Event{Kind: agent.EventToolCall, Tool: "fetch_results", Input: []byte(`{"job_id":13}`)})
	b.Event(agent.Event{Kind: agent.EventToolResult, Tool: "fetch_results", Text: "failed", IsError: true})
	b.Event(agent.Event{Kind: agent.EventToolCall, Tool: "fetch_results", Input: []byte(`{"job_id":14}`)})
	b.Event(agent.Event{Kind: agent.EventDeclined, Tool: "fetch_results", Text: "no"})
	b.Event(agent.Event{Kind: agent.EventToolResult, Tool: "fetch_results", Text: "late"})
	b.Event(agent.Event{Kind: agent.EventToolCall, Tool: "job_status", Input: []byte(`{"job_id":15}`)})
	b.Event(agent.Event{Kind: agent.EventToolResult, Tool: "job_status", Text: "running"})
	var ids []int64
	for _, e := range rec.all(EvTool) {
		te := e.(ToolEvent)
		if te.Kind == "result" {
			ids = append(ids, te.JobID)
		}
	}
	if !reflect.DeepEqual(ids, []int64{12, 0, 0, 0}) {
		t.Errorf("result job IDs = %v, want [12 0 0 0]", ids)
	}
}

func TestSetRecentFolderOnlyRecent(t *testing.T) {
	f := newFolderFixture(t)
	code := filepath.Join(f.home, "code")
	other := filepath.Join(f.home, "data")
	f.picked = code
	if _, err := f.app.PickFolder(app.FolderCode); err != nil {
		t.Fatal(err)
	}
	// Not in the recent list (even though CheckFolder would accept it).
	if _, err := f.app.SetRecentFolder(app.FolderCode, other); err == nil {
		t.Error("a folder outside the recent list was accepted")
	}
	// Recent of the other kind doesn't count.
	if _, err := f.app.SetRecentFolder(app.FolderData, code); err == nil {
		t.Error("a code-recent folder was accepted for data")
	}
	if st, err := f.app.SetRecentFolder(app.FolderCode, code); err != nil || st.Code.Path != code {
		t.Errorf("recent folder: %+v, %v", st, err)
	}
	if st, err := f.app.SetRecentFolder(app.FolderCode, ""); err != nil || st.Code.Path != "" {
		t.Errorf("clear: %+v, %v", st, err)
	}
}
