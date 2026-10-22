package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOpenTightensExistingPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "labmill.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, c := range []struct {
		p    string
		want os.FileMode
	}{{dir, 0o700}, {path, 0o600}} {
		fi, err := os.Stat(c.p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != c.want {
			t.Errorf("%s mode = %o, want %o", c.p, got, c.want)
		}
	}
}

func TestMissingJobWrapsNotFound(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.AddEvent(ctx, 999, "k", "t"); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddEvent: %v, want ErrNotFound", err)
	}
	if err := s.SetManifest(ctx, 999, []ManifestEntry{{Path: "a"}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetManifest: %v, want ErrNotFound", err)
	}
}

func TestActiveOnlyUsesTerminal(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	want := 0
	for _, st := range allStates {
		if _, err := s.CreateJob(ctx, Job{Name: string(st), State: st}); err != nil {
			t.Fatal(err)
		}
		if !st.Terminal() {
			want++
		}
	}
	jobs, err := s.Jobs(ctx, JobFilter{ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != want {
		t.Errorf("active jobs = %d, want %d", len(jobs), want)
	}
	for _, j := range jobs {
		if j.State.Terminal() {
			t.Errorf("terminal job %s listed", j.State)
		}
	}
}

func TestReopenUpToDateSkipsMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestOpenTightensExistingWalAndShm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "labmill.db")
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f, 0o644); err != nil { // beat the umask
			t.Fatal(err)
		}
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %v, want 0600", filepath.Base(f), got)
		}
	}
}
