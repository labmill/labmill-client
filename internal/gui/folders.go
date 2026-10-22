package gui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"

	"labmill-client/internal/app"
	"labmill-client/internal/store"
	"labmill-client/internal/textsafe"
)

// The data and code folders (M20; chosen through the chat box's + menu, its
// chips and drops since M24) and "Open folder". Choosing a
// folder only records its path in the runtime's app.Workspace (checked with
// app.CheckFolder); nothing is read, staged or sent until the user asks in
// the chat and approves, as always.

// FolderHost connects the folder menu and "Open folder" to the runtime:
// launch's app.Env, the Wails folder dialog and OpenFolder in production;
// stubs in the self-test and the unit tests.
type FolderHost struct {
	// Env returns the runtime's Env (Workspace, ResultsDir, LocalRoots);
	// nil until the runtime is ready.
	Env func() *app.Env
	// Pick shows the native folder dialog titled title, starting in start
	// ("" lets the system choose). An empty result means the user canceled.
	Pick func(title, start string) (string, error)
	// Open opens a folder in the file manager (OpenFolder in production).
	Open func(dir string) error
}

// FolderView is a folder for the page: the full path (for the tooltip) and
// the shortened one shown on the button.
type FolderView struct {
	Path  string `json:"path"`
	Short string `json:"short"`
}

// FolderState is what the folder menu and chips show: the chosen folders (zero
// when none) and the recent ones (most recent first, at most 5 each).
type FolderState struct {
	Code       FolderView   `json:"code"`
	Data       FolderView   `json:"data"`
	RecentCode []FolderView `json:"recentCode"`
	RecentData []FolderView `json:"recentData"`
}

// SetFolderHost installs the folder host (call before the page loads).
func (a *App) SetFolderHost(h FolderHost) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.folders = h
}

// workspace returns the runtime's Workspace, or an error while it starts.
func (a *App) workspace() (*app.Env, *app.Workspace, error) {
	a.mu.Lock()
	h := a.folders
	a.mu.Unlock()
	if h.Env == nil {
		return nil, nil, errors.New("choosing folders isn't available")
	}
	env := h.Env()
	if env == nil {
		return nil, nil, errors.New("Labmill is still starting up")
	}
	if env.Workspace == nil {
		return env, nil, errors.New("choosing folders isn't available")
	}
	return env, env.Workspace, nil
}

func folderView(p, home string) FolderView {
	return FolderView{Path: textsafe.Clean(p), Short: textsafe.Clean(app.ShortFolder(p, home))}
}

func stateOf(ws *app.Workspace) FolderState {
	home := ws.Home()
	st := FolderState{
		Code:       folderView(ws.Folder(app.FolderCode), home),
		Data:       folderView(ws.Folder(app.FolderData), home),
		RecentCode: []FolderView{},
		RecentData: []FolderView{},
	}
	for _, p := range ws.Recent(app.FolderCode) {
		st.RecentCode = append(st.RecentCode, folderView(p, home))
	}
	for _, p := range ws.Recent(app.FolderData) {
		st.RecentData = append(st.RecentData, folderView(p, home))
	}
	return st
}

// Folders returns the current FolderState.
func (a *App) Folders() (FolderState, error) {
	_, ws, err := a.workspace()
	if err != nil {
		return FolderState{RecentCode: []FolderView{}, RecentData: []FolderView{}}, err
	}
	return stateOf(ws), nil
}

// folderTitle is the folder dialog's title for kind.
func folderTitle(kind string) string {
	if kind == app.FolderData {
		return "Choose your data folder"
	}
	return "Choose your code folder"
}

// PickFolder shows the native folder dialog for kind ("code" or "data"),
// starting in the current choice (else home), and sets the chosen folder
// (SetFolder's rules). A canceled dialog changes nothing and is no error.
func (a *App) PickFolder(kind string) (FolderState, error) {
	if kind != app.FolderCode && kind != app.FolderData {
		return a.folderError(fmt.Errorf("unknown folder kind %q", kind))
	}
	_, ws, err := a.workspace()
	if err != nil {
		return a.folderError(err)
	}
	a.mu.Lock()
	pick := a.folders.Pick
	a.mu.Unlock()
	if pick == nil {
		return a.folderError(errors.New("the folder dialog isn't available"))
	}
	start := ws.Folder(kind)
	if start == "" {
		start = ws.Home()
	}
	p, err := pick(folderTitle(kind), start)
	if err != nil {
		return a.folderError(fmt.Errorf("the folder dialog failed: %w", err))
	}
	if p == "" {
		return stateOf(ws), nil
	}
	return a.SetFolder(kind, p)
}

// SetRecentFolder is SetFolder restricted to what the page may ask for: ""
// (clear) or a folder in kind's recent list.
func (a *App) SetRecentFolder(kind, path string) (FolderState, error) {
	if path != "" {
		_, ws, err := a.workspace()
		if err != nil {
			return a.folderError(err)
		}
		if !slices.Contains(ws.Recent(kind), path) {
			return a.folderError(errors.New("That folder isn't in your recent list; choose it with Add data folder or Add code folder in the + menu."))
		}
	}
	return a.SetFolder(kind, path)
}

// SetFolder sets the folder of kind to path (a dropped folder or a recent
// one; "" clears it). The path must pass app.CheckFolder: an existing
// folder, not a drive root, not a system folder, nothing hidden or
// secret-looking. Only the path is recorded. On error the returned state is
// the unchanged current one.
func (a *App) SetFolder(kind, path string) (FolderState, error) {
	_, ws, err := a.workspace()
	if err != nil {
		return a.folderError(err)
	}
	if _, err := ws.SetFolder(kind, path); err != nil {
		return stateOf(ws), err
	}
	return stateOf(ws), nil
}

// folderError returns the current state (if there is one) with err.
func (a *App) folderError(err error) (FolderState, error) {
	st, _ := a.Folders()
	return st, err
}

// OpenJobFolder opens the local folder of job id in the file manager. The
// job must have been fetched (state fetched) and its recorded folder must
// pass app.Env.OpenableFolder (an existing folder in the results folder or
// an allowed root). The path comes from the job store, never from the page
// or the model.
func (a *App) OpenJobFolder(id int64) error {
	env, _, err := a.workspace()
	if env == nil {
		return err
	}
	a.mu.Lock()
	open := a.folders.Open
	a.mu.Unlock()
	if open == nil {
		return errors.New("opening folders isn't available")
	}
	if a.jobs == nil {
		return fmt.Errorf("there's no job #%d", id)
	}
	js, err := a.jobs(context.Background())
	if err != nil {
		return fmt.Errorf("listing jobs: %w", err)
	}
	for _, j := range js {
		if j.ID != id {
			continue
		}
		if (j.State != store.StateFetched && !(j.State == store.StateFailed && j.Downloaded)) || j.LocalDir == "" {
			return fmt.Errorf("job #%d has no downloaded results yet", id)
		}
		dir, err := env.OpenableFolder(j.LocalDir)
		if err != nil {
			return err
		}
		return open(dir)
	}
	return fmt.Errorf("there's no job #%d", id)
}

// OpenFolder opens dir in the platform's file manager: explorer.exe on
// Windows, open on macOS, xdg-open elsewhere. No shell is involved: dir is
// one argument, and it must be an absolute path to an existing folder (the
// caller also checks where it lies). It returns once the program started.
func OpenFolder(dir string) error {
	// Re-check right before starting the program, so a folder replaced by a
	// file after the caller's check is not handed to the file manager.
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s isn't a folder", dir)
	}
	name, args, err := openCommand(runtime.GOOS, dir)
	if err != nil {
		return err
	}
	return startCommand(name, args...)
}

// startCommand starts a program without waiting for it (its exit is reaped
// in the background; explorer.exe exits with 1 even when it worked).
var startCommand = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open the folder: %w", err)
	}
	go cmd.Wait() //nolint:errcheck // the exit status means nothing here
	return nil
}

// openCommand returns the program and arguments that open dir on goos.
func openCommand(goos, dir string) (string, []string, error) {
	if dir == "" || textsafe.Clean(dir) != dir || strings.ContainsAny(dir, "\r\n") {
		return "", nil, errors.New("that folder name can't be opened")
	}
	switch goos {
	case "windows":
		// Explorer splits its command line at commas and treats quotes
		// specially, so such names are refused rather than misread.
		if !isAbsWindows(dir) || strings.ContainsAny(dir, `,"`) {
			return "", nil, fmt.Errorf("%s can't be opened in Explorer", dir)
		}
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		// A trailing backslash makes Explorer open a folder, never run a
		// file of that name.
		if !strings.HasSuffix(dir, `\`) {
			dir += `\`
		}
		return root + `\explorer.exe`, []string{dir}, nil
	case "darwin":
		if !strings.HasPrefix(dir, "/") {
			return "", nil, fmt.Errorf("%s isn't a full path", dir)
		}
		return "/usr/bin/open", []string{dir}, nil
	default:
		if !strings.HasPrefix(dir, "/") {
			return "", nil, fmt.Errorf("%s isn't a full path", dir)
		}
		return "xdg-open", []string{dir}, nil
	}
}

// isAbsWindows reports whether p is an absolute Windows path (C:\... or a
// \\server\share UNC path), whatever OS runs the check.
func isAbsWindows(p string) bool {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		c := p[0] | 0x20
		return c >= 'a' && c <= 'z'
	}
	return strings.HasPrefix(p, `\\`) && len(p) > 2
}
