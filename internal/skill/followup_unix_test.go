//go:build !windows

package skill

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOverrideNonRegularSkillMD(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fifo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo", "SKILL.md"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "adir", "SKILL.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	set, warnings, err := LoadWithOverride(testFS(), dir) // must not block
	if err != nil {
		t.Fatal(err)
	}
	w := warnText(warnings)
	if len(warnings) != 2 || !strings.Contains(w, "not a regular file") || !strings.Contains(w, "fifo") || !strings.Contains(w, "adir") {
		t.Errorf("warnings = %q", w)
	}
	if _, ok := set.Get("fifo"); ok {
		t.Error("fifo skill loaded")
	}
}

func TestRootFSOpenRefusesFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "SKILL.md"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	rfs := newRootFS(root)
	done := make(chan error, 1)
	go func() {
		f, err := rfs.Open("SKILL.md") // no prior stat: must not block
		if err == nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("Open(FIFO) error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Open(FIFO) blocked")
	}
	if f, err := rfs.Open("ok.md"); err != nil {
		t.Errorf("Open(regular) = %v", err)
	} else {
		f.Close()
	}
	if f, err := rfs.Open("."); err != nil {
		t.Errorf("Open(dir) = %v", err)
	} else {
		f.Close()
	}
}
