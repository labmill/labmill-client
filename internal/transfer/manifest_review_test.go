package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func manifestPaths(t *testing.T, root string, paths []string) (got []string, skipped []string) {
	t.Helper()
	entries, skipped, err := BuildManifest(context.Background(), root, paths, nil)
	if err != nil {
		t.Fatalf("BuildManifest(%v): %v", paths, err)
	}
	for _, e := range entries {
		got = append(got, e.Path)
	}
	return got, skipped
}

// A path equal to root (".", or root itself) stages everything under root.
func TestBuildManifestRootDot(t *testing.T) {
	root := t.TempDir()
	mwrite(t, filepath.Join(root, "a.txt"), "a")
	mwrite(t, filepath.Join(root, "d", "b.txt"), "bb")
	for _, p := range []string{".", root, "./"} {
		got, skipped := manifestPaths(t, root, []string{p})
		if strings.Join(got, ",") != "a.txt,d/b.txt" || len(skipped) != 0 {
			t.Errorf("path %q: got %v skipped %v", p, got, skipped)
		}
	}
}

func TestBuildManifestSymlinkedRootDot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	real := t.TempDir()
	mwrite(t, filepath.Join(real, "a.txt"), "a")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, _ := manifestPaths(t, link, []string{"."})
	if strings.Join(got, ",") != "a.txt" {
		t.Errorf("got %v", got)
	}
}

// Hidden entries are skipped and reported unless named explicitly.
func TestBuildManifestHidden(t *testing.T) {
	root := t.TempDir()
	mwrite(t, filepath.Join(root, "a.txt"), "a")
	mwrite(t, filepath.Join(root, ".env"), "SECRET")
	mwrite(t, filepath.Join(root, ".ssh", "id_rsa"), "key")
	mwrite(t, filepath.Join(root, "d", ".git", "config"), "c")
	mwrite(t, filepath.Join(root, "d", "ok.txt"), "ok")

	got, skipped := manifestPaths(t, root, []string{"."})
	if strings.Join(got, ",") != "a.txt,d/ok.txt" {
		t.Errorf("root: got %v", got)
	}
	if strings.Join(skipped, ",") != ".env,.ssh,d/.git" {
		t.Errorf("root: skipped = %v", skipped)
	}

	// Naming a hidden path explicitly stages it; hidden entries below it
	// are still skipped.
	mwrite(t, filepath.Join(root, ".cfg", "x", "y.txt"), "y")
	mwrite(t, filepath.Join(root, ".cfg", ".inner", "z.txt"), "z")
	got, skipped = manifestPaths(t, root, []string{".cfg"})
	if strings.Join(got, ",") != ".cfg/x/y.txt" || strings.Join(skipped, ",") != ".cfg/.inner" {
		t.Errorf("explicit hidden dir: got %v skipped %v", got, skipped)
	}
	got, _ = manifestPaths(t, root, []string{".env"})
	if strings.Join(got, ",") != ".env" {
		t.Errorf("explicit hidden file: got %v", got)
	}
	// A hidden parent is fine when the path under it is named.
	got, _ = manifestPaths(t, root, []string{".cfg/x"})
	if strings.Join(got, ",") != ".cfg/x/y.txt" {
		t.Errorf("explicit path under hidden dir: got %v", got)
	}
}

// With "." as the path, cancellation (not a bad path) is what stops the walk.
func TestBuildManifestDotCanceledIsCancellation(t *testing.T) {
	root := t.TempDir()
	mwrite(t, filepath.Join(root, "f"), "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := BuildManifest(ctx, root, []string{"."}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
