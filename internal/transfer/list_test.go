package transfer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestListFiles: the walk returns every file with its size, sorted, and no
// directories.
func TestListFiles(t *testing.T) {
	dir := t.TempDir()
	for rel, n := range map[string]int{"b.txt": 3, "a/x.csv": 10, "a/deep/y.bin": 1} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, done, err := rcloneContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	fsrc, err := newLocalFs(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := listFiles(ctx, fsrc)
	if err != nil {
		t.Fatal(err)
	}
	want := []RemoteFile{{"a/deep/y.bin", 1}, {"a/x.csv", 10}, {"b.txt", 3}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
