package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/stall"
)

// blockingLocal wraps a real file; Read blocks until release is closed.
type blockingLocal struct {
	*os.File
	release chan struct{}
}

func (b blockingLocal) Read(p []byte) (int, error) {
	<-b.release
	return b.File.Read(p)
}

// waitIdle waits until every abandoned read has finished, so a test can
// restore the package variables the reads use.
func waitIdle(t *testing.T) {
	t.Helper()
	if !stall.Default.WaitIdle(5 * time.Second) {
		t.Fatal("abandoned reads never finished")
	}
}

// A preview_file read that never answers returns a "skipped" note in time,
// and a healthy file still previews afterwards.
func TestPreviewFileStalled(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(h.home, "data", "stall")
	os.MkdirAll(dir, 0o755)
	stuck := filepath.Join(dir, "cloud-only.csv")
	good := filepath.Join(dir, "good.csv")
	os.WriteFile(stuck, []byte("a,b\n1,2\n"), 0o644)
	os.WriteFile(good, []byte("x,y\n3,4\n"), 0o644)

	release := make(chan struct{})
	oldOpen, oldTimeout := openLocal, previewTimeout
	t.Cleanup(func() { close(release); waitIdle(t); openLocal, previewTimeout = oldOpen, oldTimeout })
	previewTimeout = 30 * time.Millisecond
	openLocal = func(p string) (localFile, error) {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(p, "cloud-only.csv") {
			return blockingLocal{f, release}, nil
		}
		return f, nil
	}

	start := time.Now()
	_, err := h.run(t, "preview_file", map[string]any{"path": stuck})
	if err == nil || !strings.Contains(err.Error(), "skipped: cloud-only.csv didn't answer within 1 s") {
		t.Fatalf("want a skipped note, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	out, err := h.run(t, "preview_file", map[string]any{"path": good})
	if err != nil || !strings.Contains(out, "3,4") {
		t.Fatalf("healthy file after a stall: %q, %v", out, err)
	}
}

// A find_local_data walk blocked in one entry returns what it found so far
// with a note, instead of hanging.
func TestFindLocalDataStalled(t *testing.T) {
	h := newHarness(t)
	for _, d := range []string{"mri 2026 early", "mri 2026 late"} {
		p := filepath.Join(h.home, "data", strings.ReplaceAll(d, " ", "-"))
		os.MkdirAll(p, 0o755)
		os.WriteFile(filepath.Join(p, "f.dat"), []byte("x"), 0o644)
	}
	release := make(chan struct{})
	oldHook, oldT, oldG := walkHook, findTimeout, findGrace
	t.Cleanup(func() { close(release); waitIdle(t); walkHook, findTimeout, findGrace = oldHook, oldT, oldG })
	// Long enough that the walk reaches the blocking folder first (the
	// hook still blocks, so the test stays deterministic).
	findTimeout, findGrace = 500*time.Millisecond, 30*time.Millisecond
	walkHook = func(p string) {
		if strings.HasSuffix(p, "mri-2026-late") {
			<-release // a mount that stopped answering
		}
	}
	start := time.Now()
	out, err := h.run(t, "find_local_data", map[string]any{"query": "mri 2026"})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	if !strings.Contains(out, "skipped: the folder search didn't answer within") {
		t.Errorf("no skipped note:\n%s", out)
	}
	if !strings.Contains(out, "mri-2026-early") {
		t.Errorf("the folder found before the stall is missing:\n%s", out)
	}
}

func TestStripExtendedPrefix(t *testing.T) {
	for in, want := range map[string]string{
		`\\?\C:\Users\me\data`:     `C:\Users\me\data`,
		`\\?\c:\x`:                 `c:\x`,
		`\\?\UNC\server\share\dir`: `\\server\share\dir`,
		`\\?\unc\server\share`:     `\\server\share`,
		`C:\plain`:                 `C:\plain`,
		`\\server\share`:           `\\server\share`,
		`\\?\Volume{1234}\dir`:     `\\?\Volume{1234}\dir`,
		`/home/me`:                 `/home/me`,
		``:                         ``,
	} {
		if got := stripExtendedPrefix(in); got != want {
			t.Errorf("stripExtendedPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// The folder checks go through evalLinks, so a resolver that sees through a
// junction (as the Windows one does) makes CheckFolder refuse a private place
// behind an innocent-looking path.
func TestCheckFolderUsesResolver(t *testing.T) {
	home := t.TempDir()
	ordinary := filepath.Join(home, "data", "junction")
	os.MkdirAll(ordinary, 0o755)
	private := filepath.Join(home, ".ssh")
	os.MkdirAll(private, 0o700)

	old := evalLinks
	t.Cleanup(func() { evalLinks = old })
	evalLinks = func(p string) (string, error) {
		if p == ordinary {
			return private, nil // what GetFinalPathNameByHandle reports
		}
		return old(p)
	}
	if _, err := CheckFolder(ordinary, home); err == nil {
		t.Error("a junction into a private folder was accepted")
	}
	evalLinks = old
	if _, err := CheckFolder(ordinary, home); err != nil {
		t.Errorf("the same folder without the junction: %v", err)
	}
}

// A walk that blocks before finding anything says so in its own sentence.
func TestFindLocalDataStalledNothingFound(t *testing.T) {
	h := newHarness(t)
	os.MkdirAll(filepath.Join(h.home, "data", "mri-2026"), 0o755)
	release := make(chan struct{})
	oldHook, oldT, oldG := walkHook, findTimeout, findGrace
	t.Cleanup(func() { close(release); waitIdle(t); walkHook, findTimeout, findGrace = oldHook, oldT, oldG })
	findTimeout, findGrace = 20*time.Millisecond, 30*time.Millisecond
	walkHook = func(string) { <-release }
	out, err := h.run(t, "find_local_data", map[string]any{"query": "mri"})
	if err != nil || !strings.Contains(out, "I found no matching folders before that.") || strings.Contains(out, "These are the folders") {
		t.Errorf("out=%q err=%v", out, err)
	}
}

// stage_data refuses a path that goes through a junction or symlink out of
// the folder or into a private place (the resolver is swapped to act like
// GetFinalPathNameByHandle).
func TestStagePathsResolvesLinks(t *testing.T) {
	h := newHarness(t)
	root := filepath.Join(h.home, "data")
	os.MkdirAll(filepath.Join(root, "link"), 0o755)
	os.WriteFile(filepath.Join(root, "link", "notes.txt"), []byte("x"), 0o644)
	private := filepath.Join(h.home, ".ssh")
	os.MkdirAll(private, 0o700)
	outside := t.TempDir()

	old := evalLinks
	t.Cleanup(func() { evalLinks = old })
	target := ""
	evalLinks = func(p string) (string, error) {
		if target != "" && strings.HasPrefix(p, filepath.Join(root, "link")) {
			return filepath.Join(target, strings.TrimPrefix(p, filepath.Join(root, "link"))), nil
		}
		return old(p)
	}
	ts := &toolset{env: h.env}
	for _, tgt := range []string{private, outside} {
		target = tgt
		if _, _, err := ts.stagePaths(root, []string{"link/notes.txt"}); err == nil {
			t.Errorf("staged through a link to %s", tgt)
		}
	}
	target = ""
	if p, _, err := ts.stagePaths(root, []string{"link/notes.txt"}); err != nil || len(p) != 1 {
		t.Errorf("plain path: %v, %v", p, err)
	}
}
