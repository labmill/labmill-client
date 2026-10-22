package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func mwrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func msum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestBuildManifest(t *testing.T) {
	root := t.TempDir()
	mwrite(t, filepath.Join(root, "scans", "a.dcm"), "AAAA")
	mwrite(t, filepath.Join(root, "scans", "sub", "b.dcm"), "BB")
	mwrite(t, filepath.Join(root, "notes.txt"), "n")
	mwrite(t, filepath.Join(root, "other", "x.bin"), "not selected")
	if runtime.GOOS != "windows" {
		os.Symlink("/etc/passwd", filepath.Join(root, "scans", "link"))
	}
	var calls int
	entries, skipped, err := BuildManifest(context.Background(), root, []string{"scans", filepath.Join(root, "notes.txt"), "scans/sub"}, func(done, total int64) {
		calls++
		if total != 7 || done > total {
			t.Errorf("progress %d/%d", done, total)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	if strings.Join(paths, ",") != "notes.txt,scans/a.dcm,scans/sub/b.dcm" {
		t.Fatalf("paths = %v", paths)
	}
	if entries[1].Size != 4 || entries[1].SHA256 != msum("AAAA") {
		t.Errorf("a.dcm = %+v", entries[1])
	}
	if runtime.GOOS != "windows" && (len(skipped) != 1 || skipped[0] != "scans/link") {
		t.Errorf("skipped = %v", skipped)
	}
	if calls == 0 {
		t.Error("progress never called")
	}
	if f, b := Totals(entries); f != 3 || b != 7 {
		t.Errorf("Totals = %d, %d", f, b)
	}
}

func TestBuildManifestRefusesOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mwrite(t, filepath.Join(outside, "secret"), "s")
	for _, p := range []string{"../" + filepath.Base(outside), outside, filepath.Join(root, "..", filepath.Base(outside), "secret")} {
		if _, _, err := BuildManifest(context.Background(), root, []string{p}, nil); err == nil {
			t.Errorf("BuildManifest(%q) succeeded", p)
		}
	}
}

func TestBuildManifestCanceled(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 50; i++ {
		mwrite(t, filepath.Join(root, fmt.Sprintf("f%02d", i)), strings.Repeat("x", 1000))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := BuildManifest(ctx, root, []string{"."}, nil); err == nil {
		t.Error("canceled walk succeeded")
	}
}
