package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func mkdirs(t *testing.T, base string, rels ...string) {
	t.Helper()
	for _, r := range rels {
		if err := os.MkdirAll(filepath.Join(base, filepath.FromSlash(r)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// outsideDir returns a folder outside home that CheckFolder may accept, or
// skips when the system's temp folder itself counts as a system folder
// (macOS keeps it under /private/var).
func outsideDir(t *testing.T) string {
	t.Helper()
	d := realPath(t, t.TempDir())
	if systemFolder(d) {
		t.Skipf("the temp folder %s is under a system folder here", d)
	}
	return d
}

func TestCheckFolderRules(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix paths")
	}
	home := realPath(t, t.TempDir())
	mkdirs(t, home, "proj/code", ".config/x", "Library/Mobile", "secrets/a", "proj/my_credentials", "AppData/Local")
	if err := os.WriteFile(filepath.Join(home, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".config"), filepath.Join(home, "cfg")); err != nil {
		t.Fatal(err)
	}
	old := privacyOS
	defer func() { privacyOS = old }()

	ok := []struct{ in, want string }{
		{filepath.Join(home, "proj", "code"), filepath.Join(home, "proj", "code")},
		{"~/proj/code", filepath.Join(home, "proj", "code")},
		{"  " + filepath.Join(home, "proj") + "/  ", filepath.Join(home, "proj")},
		{home, home},
		{filepath.Join(home, "Library", "Mobile"), filepath.Join(home, "Library", "Mobile")}, // Library is private on macOS only
	}
	privacyOS = "linux"
	for _, c := range ok {
		got, err := CheckFolder(c.in, home)
		if err != nil || got != c.want {
			t.Errorf("CheckFolder(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	refused := []struct{ in, why string }{
		{"", "Choose"},
		{"proj/code", "full path"},
		{filepath.Join(home, "missing"), "can't find"},
		{filepath.Join(home, "notes.txt"), "not a folder"},
		{filepath.Join(home, ".config", "x"), "private"},
		{filepath.Join(home, "cfg"), "private"}, // a symlink into a hidden folder
		{filepath.Join(home, "secrets"), "private"},
		{filepath.Join(home, "proj", "my_credentials"), "private"},
		{filepath.Dir(home), "contains your home"},
		{"/", "whole drive"},
		{"/etc", "system folder"},
		{"/usr/share", "system folder"},
		{filepath.Join(home, "proj") + "\x1b[2J", "unusual character"},
	}
	for _, c := range refused {
		got, err := CheckFolder(c.in, home)
		if err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("CheckFolder(%q) = %q, %v; want an error containing %q", c.in, got, err, c.why)
		}
	}
	privacyOS = "darwin"
	if _, err := CheckFolder(filepath.Join(home, "Library", "Mobile"), home); err == nil {
		t.Error("~/Library/Mobile was accepted on macOS")
	}
	privacyOS = "windows"
	if _, err := CheckFolder(filepath.Join(home, "AppData", "Local"), home); err == nil {
		t.Error("AppData was accepted on Windows")
	}

	// Outside home: an ordinary folder is fine, hidden components are not.
	privacyOS = "linux"
	out := outsideDir(t)
	mkdirs(t, out, "data/run1", ".cache/x")
	if got, err := CheckFolder(filepath.Join(out, "data", "run1"), home); err != nil || got != filepath.Join(out, "data", "run1") {
		t.Errorf("outside home: %q, %v", got, err)
	}
	if _, err := CheckFolder(filepath.Join(out, ".cache", "x"), home); err == nil {
		t.Error("a hidden folder outside home was accepted")
	}
}

func TestSystemFolder(t *testing.T) {
	old := privacyOS
	defer func() { privacyOS = old }()
	privacyOS = "linux"
	for p, want := range map[string]bool{
		"/etc": true, "/usr/local/data": true, "/System/Library": true, "/private/var/x": true, "/opt/conda": true,
		"/mnt": true, "/mnt/c": true, "/mnt/c/Windows/System32": true, "/mnt/c/Users": true, "/mnt/c/Users/ann": true,
		"/mnt/c/Users/ann/AppData/Local": true, "/mnt/c/Users/ann/Documents/data": false, "/mnt/c/data": false,
		"/mnt/nas/data": false, "/media": true, "/media/usb": false, "/Volumes": true, "/Volumes/External/data": false,
		"/run/user/1000": true, "/run/media/ann": true, "/run/media/ann/usb": false,
		"/home/ann/data": false, "/data/mri": false, "/tmp/x": false, "/scratch/x": false,
	} {
		if got := systemFolder(p); got != want {
			t.Errorf("systemFolder(%q) = %v, want %v", p, got, want)
		}
	}
	if runtime.GOOS == "windows" {
		privacyOS = "windows"
		for p, want := range map[string]bool{
			`C:\Windows\Temp`: true, `C:\Program Files\x`: true, `D:\ProgramData`: true, `C:\Users\Public`: true,
			`D:\data\mri`: false, `\\server\share\lab`: false,
		} {
			if got := systemFolder(p); got != want {
				t.Errorf("systemFolder(%q) = %v, want %v", p, got, want)
			}
		}
	}
}

func TestWorkspaceSetRecentAndPersist(t *testing.T) {
	home := realPath(t, t.TempDir())
	mkdirs(t, home, "a", "b", "c", "d", "e", "f", "g", ".hidden")
	path := filepath.Join(t.TempDir(), "cfg", "workspace.json")
	w := NewWorkspace(home, path)

	if got, err := w.SetFolder(FolderCode, filepath.Join(home, "a")); err != nil || got != filepath.Join(home, "a") {
		t.Fatalf("SetFolder: %q, %v", got, err)
	}
	if _, err := w.SetFolder(FolderData, "~/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.SetFolder("music", filepath.Join(home, "c")); err == nil {
		t.Error("an unknown kind was accepted")
	}
	if _, err := w.SetFolder(FolderCode, filepath.Join(home, ".hidden")); err == nil {
		t.Error("a hidden folder was accepted")
	}
	if w.Folder(FolderCode) != filepath.Join(home, "a") {
		t.Errorf("a refused folder changed the choice: %q", w.Folder(FolderCode))
	}
	for _, d := range []string{"c", "d", "e", "f", "g", "c"} {
		if _, err := w.SetFolder(FolderCode, filepath.Join(home, d)); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"c", "g", "f", "e", "d"}
	rec := w.Recent(FolderCode)
	if len(rec) != len(want) {
		t.Fatalf("recent = %q", rec)
	}
	for i, d := range want {
		if rec[i] != filepath.Join(home, d) {
			t.Errorf("recent[%d] = %q, want %q", i, rec[i], filepath.Join(home, d))
		}
	}
	if _, err := w.SetFolder(FolderCode, ""); err != nil || w.Folder(FolderCode) != "" || len(w.Recent(FolderCode)) != 5 {
		t.Errorf("clear: %v, folder %q, recent %q", err, w.Folder(FolderCode), w.Recent(FolderCode))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("workspace.json mode = %v", fi.Mode().Perm())
	}

	// Restored at start; a saved folder that disappeared is dropped.
	w2, err := LoadWorkspace(home, path)
	if err != nil || w2.Folder(FolderData) != filepath.Join(home, "b") || w2.Folder(FolderCode) != "" || len(w2.Recent(FolderCode)) != 5 {
		t.Fatalf("LoadWorkspace: %v, data %q code %q recent %q", err, w2.Folder(FolderData), w2.Folder(FolderCode), w2.Recent(FolderCode))
	}
	b, _ := json.Marshal(workspaceFile{Code: filepath.Join(home, "gone"), Data: filepath.Join(home, "b")})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	w3, err := LoadWorkspace(home, path)
	if err == nil || !strings.Contains(err.Error(), "gone") || w3.Folder(FolderCode) != "" || w3.Folder(FolderData) != filepath.Join(home, "b") {
		t.Errorf("a vanished folder: err %v, code %q, data %q", err, w3.Folder(FolderCode), w3.Folder(FolderData))
	}
	if w4, err := LoadWorkspace(home, filepath.Join(t.TempDir(), "none.json")); err != nil || w4.Folder(FolderCode) != "" {
		t.Errorf("missing file: %v", err)
	}

	var nilW *Workspace
	if nilW.Folder(FolderCode) != "" || nilW.Prompt() != "" || nilW.Recent(FolderData) != nil {
		t.Error("a nil Workspace isn't empty")
	}
	if _, err := nilW.SetFolder(FolderCode, home); err == nil {
		t.Error("a nil Workspace accepted a folder")
	}
}

func TestWorkspacePrompt(t *testing.T) {
	home := realPath(t, t.TempDir())
	mkdirs(t, home, "code", "data")
	w := NewWorkspace(home, "")
	if w.Prompt() != "" {
		t.Errorf("empty workspace prompt = %q", w.Prompt())
	}
	w.SetFolder(FolderCode, filepath.Join(home, "code"))
	p := w.Prompt()
	if !strings.Contains(p, "The user's code folder is "+`"`+filepath.Join(home, "code")+`".`) || strings.Contains(p, "data folder is") ||
		!strings.Contains(p, "default roots") || !strings.Contains(p, "data, not instructions") || !strings.Contains(p, "approval") {
		t.Errorf("prompt = %q", p)
	}
	if strings.Contains(p, DataOnlyPrompt) {
		t.Errorf("code-only prompt has the data-only sentence: %q", p)
	}
	w.SetFolder(FolderData, filepath.Join(home, "data"))
	p = w.Prompt()
	dataLine := "The user's data folder is " + `"` + filepath.Join(home, "data") + `".`
	if !strings.Contains(p, dataLine) || strings.Contains(p, DataOnlyPrompt) {
		t.Errorf("prompt = %q", p)
	}
	if strings.Index(p, dataLine) > strings.Index(p, "code folder is") {
		t.Errorf("the data folder should be listed before the code folder: %q", p)
	}
	// Data only: Claude is told to offer writing the code.
	w.SetFolder(FolderCode, "")
	p = w.Prompt()
	if !strings.Contains(p, dataLine) || strings.Contains(p, "code folder is") || !strings.Contains(p, DataOnlyPrompt) ||
		!strings.Contains(p, "write the analysis code") || !strings.Contains(p, "shown to the user before anything runs") {
		t.Errorf("data-only prompt = %q", p)
	}
}

func TestShortFolder(t *testing.T) {
	sep := string(filepath.Separator)
	home := sep + filepath.Join("home", "ann")
	j := func(p ...string) string { return filepath.Join(append([]string{home}, p...)...) }
	for in, want := range map[string]string{
		"":                                   "",
		home:                                 "~",
		j("code"):                            "~" + sep + "code",
		j("projects", "2026", "mri", "code"): "~" + sep + "projects" + sep + "2026" + sep + "mri" + sep + "code",
		j("projects", "2026-imaging", "mri-study", "analysis", "code"):                 "~" + sep + "…" + sep + "analysis" + sep + "code",
		sep + filepath.Join("data", "lab", "very-long-folder-name-for-a-study", "raw"): "…" + sep + "raw",
	} {
		if got := ShortFolder(in, home); got != want {
			t.Errorf("ShortFolder(%q) = %q, want %q", in, got, want)
		}
	}
	long := j(strings.Repeat("x", 50))
	if got := ShortFolder(long, home); len([]rune(got)) > 36 || !strings.HasSuffix(got, "xxxx") {
		t.Errorf("ShortFolder(long) = %q", got)
	}
}

// The local tools default to the chosen folders, and a chosen folder
// outside the allowed roots becomes one (privacy rules unchanged).
func TestToolsUseWorkspaceDefaults(t *testing.T) {
	h := newHarness(t)
	home := realPath(t, h.home)
	mkdirs(t, home, "code/proj/src", "code/proj/.git")
	os.WriteFile(filepath.Join(home, "code", "proj", "train.py"), []byte("import torch\nprint('hi')\n"), 0o644)
	os.WriteFile(filepath.Join(home, "code", "proj", "src", "model.py"), []byte("import torch.nn\n"), 0o644)
	os.WriteFile(filepath.Join(home, "code", "proj", ".env"), []byte("TOKEN=x\n"), 0o644)
	h.env.Workspace = NewWorkspace(home, "")

	// Without a choice: the old behavior.
	if _, err := h.run(t, "stage_data", map[string]any{"paths": []string{"train.py"}, "job_name": "j0"}); err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("stage_data without root or folder: %v", err)
	}
	if _, err := h.run(t, "preview_file", map[string]any{"path": "train.py"}); err == nil || !strings.Contains(err.Error(), "full path") {
		t.Errorf("relative preview without folder: %v", err)
	}

	code := filepath.Join(home, "code", "proj")
	if _, err := h.env.Workspace.SetFolder(FolderCode, code); err != nil {
		t.Fatal(err)
	}
	if _, err := h.env.Workspace.SetFolder(FolderData, filepath.Join(home, "data")); err != nil {
		t.Fatal(err)
	}

	// find_local_data searches the chosen folders only, unless told otherwise.
	out, err := h.run(t, "find_local_data", map[string]any{"query": "mri"})
	if err != nil || !strings.Contains(out, "mri-2026") {
		t.Errorf("find in the data folder: %q, %v", out, err)
	}
	out, err = h.run(t, "find_local_data", map[string]any{"query": "documents"})
	if err != nil || strings.Contains(out, "Documents") {
		t.Errorf("find searched outside the chosen folders: %q, %v", out, err)
	}
	out, err = h.run(t, "find_local_data", map[string]any{"query": "documents", "under": home})
	if err != nil || !strings.Contains(out, "Documents") {
		t.Errorf("find with under=home: %q, %v", out, err)
	}

	// preview_file resolves a relative path in the code folder, then data.
	out, err = h.run(t, "preview_file", map[string]any{"path": "train.py"})
	if err != nil || !strings.Contains(out, "import torch") {
		t.Errorf("relative preview: %q, %v", out, err)
	}
	out, err = h.run(t, "preview_file", map[string]any{"path": "mri-2026/labels.csv"})
	if err != nil || !strings.Contains(out, "id,label") {
		t.Errorf("relative preview in the data folder: %q, %v", out, err)
	}
	if _, err := h.run(t, "preview_file", map[string]any{"path": ".env"}); err == nil {
		t.Error("a secret file in the code folder was previewed")
	}
	if _, err := h.run(t, "preview_file", map[string]any{"path": "../../.hidden/mri-secret/x.dcm"}); err == nil {
		t.Error("a relative path escaped into a hidden folder")
	}

	// stage_data stages from the data folder by default.
	out, err = h.run(t, "stage_data", map[string]any{"paths": []string{"mri-2026"}, "job_name": "j1"})
	if err != nil || !strings.Contains(out, `"files":3`) {
		t.Errorf("stage from the data folder: %q, %v", out, err)
	}
	// ...and from the code folder when there is no data folder; secrets stay out.
	h.env.Workspace.SetFolder(FolderData, "")
	out, err = h.run(t, "stage_data", map[string]any{"paths": []string{code}, "job_name": "j2"})
	if err != nil || !strings.Contains(out, `"files":2`) || !strings.Contains(out, ".env") {
		t.Errorf("stage from the code folder: %q, %v", out, err)
	}

	// A chosen folder outside home becomes an allowed root.
	out2 := outsideDir(t)
	mkdirs(t, out2, "lab/run7")
	os.WriteFile(filepath.Join(out2, "lab", "run7", "a.csv"), []byte("x,y\n1,2\n"), 0o644)
	if _, err := h.run(t, "preview_file", map[string]any{"path": filepath.Join(out2, "lab", "run7", "a.csv")}); err == nil {
		t.Fatal("a folder outside the roots was readable before it was chosen")
	}
	if _, err := h.env.Workspace.SetFolder(FolderData, filepath.Join(out2, "lab")); err != nil {
		t.Fatal(err)
	}
	out, err = h.run(t, "preview_file", map[string]any{"path": filepath.Join(out2, "lab", "run7", "a.csv")})
	if err != nil || !strings.Contains(out, "x,y") {
		t.Errorf("preview in a chosen outside folder: %q, %v", out, err)
	}
	if _, err := h.run(t, "preview_file", map[string]any{"path": filepath.Join(out2, "other.csv")}); err == nil {
		t.Error("the parent of the chosen outside folder became readable")
	}
}

func TestOpenableFolder(t *testing.T) {
	h := newHarness(t)
	home := realPath(t, h.home)
	res := filepath.Join(home, "labmill-results", "job1")
	mkdirs(t, home, "labmill-results/job1", ".ssh/x")
	os.WriteFile(filepath.Join(home, "file.txt"), []byte("x"), 0o644)
	if got, err := h.env.OpenableFolder(res); err != nil || got != res {
		t.Errorf("OpenableFolder(results) = %q, %v", got, err)
	}
	for _, bad := range []string{"", "relative/dir", filepath.Join(home, "labmill-results", "gone"), filepath.Join(home, "file.txt"), filepath.Join(home, ".ssh", "x"), filepath.Dir(home)} {
		if got, err := h.env.OpenableFolder(bad); err == nil {
			t.Errorf("OpenableFolder(%q) = %q, want an error", bad, got)
		}
	}
}

func TestCheckResolvedDarwinCase(t *testing.T) {
	old := privacyOS
	defer func() { privacyOS = old }()
	privacyOS = "darwin"
	const rhome = "/Users/alice"
	for _, p := range []string{
		"/users/bob/Library/Mail", "/USERS/bob/Library", "/users", "/Users/bob/library/x", "/Users", "/Users/bob",
		"/USERS", "/users/ALICE/Library/x", "/users/alice/.ssh", "/Users/alice/library",
	} {
		if err := checkResolved(p, rhome); err == nil {
			t.Errorf("%s was accepted on macOS", p)
		}
	}
	// A parent of home in another case still contains home.
	if err := checkResolved("/USERS", rhome); err == nil || !strings.Contains(err.Error(), "contains your home") && !strings.Contains(err.Error(), "system folder") {
		t.Errorf("/USERS: %v", err)
	}
	for _, p := range []string{"/Users/bob/Projects/x", "/Users/alice/Projects/x", "/users/alice/Projects"} {
		if err := checkResolved(p, rhome); err != nil {
			t.Errorf("%s refused: %v", p, err)
		}
	}
	privacyOS = "windows"
	if err := checkResolved(`/c/users/ALICE/appdata`, "/c/Users/alice"); err == nil {
		t.Error("lower-cased AppData accepted")
	}
}

func TestRootsDropsSwappedSymlink(t *testing.T) {
	home := realPath(t, t.TempDir())
	out := outsideDir(t)
	mkdirs(t, out, "data", "elsewhere")
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	w := NewWorkspace(home, "")
	data := filepath.Join(out, "data")
	if _, err := w.SetFolder(FolderData, data); err != nil {
		t.Fatal(err)
	}
	ts := &toolset{env: &Env{LocalRoots: []string{home}, Workspace: w}}
	if got := ts.roots(); len(got) != 2 || got[1] != data {
		t.Fatalf("roots = %q", got)
	}
	// Swap the folder for a symlink to another place.
	moved := filepath.Join(out, "data-old")
	if err := os.Rename(data, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(out, "elsewhere"), data); err != nil {
		t.Fatal(err)
	}
	if got := ts.roots(); len(got) != 1 {
		t.Errorf("roots after the swap = %q", got)
	}
}

func TestSetFolderMergesOtherProcess(t *testing.T) {
	home := realPath(t, t.TempDir())
	mkdirs(t, home, "code", "data", "code2")
	path := filepath.Join(home, "cfg", "workspace.json")
	a := NewWorkspace(home, path) // started early
	b := NewWorkspace(home, path) // another process
	if _, err := b.SetFolder(FolderData, filepath.Join(home, "data")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetFolder(FolderCode, filepath.Join(home, "code")); err != nil {
		t.Fatal(err)
	}
	got, err := LoadWorkspace(home, path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Folder(FolderData) != filepath.Join(home, "data") || got.Folder(FolderCode) != filepath.Join(home, "code") {
		t.Errorf("saved: code %q data %q", got.Folder(FolderCode), got.Folder(FolderData))
	}
	if len(got.Recent(FolderData)) != 1 {
		t.Errorf("recent data = %q", got.Recent(FolderData))
	}
}

func TestFindLocalDataNestedFoldersNoDuplicates(t *testing.T) {
	for _, dataContainsCode := range []bool{true, false} {
		h := newHarness(t)
		home := realPath(t, h.home)
		mkdirs(t, home, "lab/code/imgs")
		os.WriteFile(filepath.Join(home, "lab", "code", "imgs", "a.png"), []byte("x"), 0o644)
		h.env.Workspace = NewWorkspace(home, "")
		outer, inner := filepath.Join(home, "lab"), filepath.Join(home, "lab", "code")
		code, data := inner, outer
		if !dataContainsCode {
			code, data = outer, inner
		}
		h.env.Workspace.SetFolder(FolderCode, code)
		h.env.Workspace.SetFolder(FolderData, data)
		out, err := h.run(t, "find_local_data", map[string]any{"query": "imgs"})
		if err != nil || strings.Count(out, "imgs") != 1 {
			t.Errorf("dataContainsCode=%v: %q, %v", dataContainsCode, out, err)
		}
	}
}
