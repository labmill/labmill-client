package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreviewFilePaging(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(h.home, "data", "pipe")
	os.MkdirAll(dir, 0o755)
	var b strings.Builder
	for i := 1; i <= 450; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	readme := filepath.Join(dir, "README.md")
	os.WriteFile(readme, []byte(b.String()), 0o644)
	csv := filepath.Join(dir, "table.csv")
	os.WriteFile(csv, []byte(b.String()), 0o644)

	// Default page of a doc file: 100 lines, with a pointer to the next page.
	out, err := h.run(t, "preview_file", map[string]any{"path": readme})
	if err != nil || !strings.Contains(out, "line 100\n") || strings.Contains(out, "line 101\n") || !strings.Contains(out, "use start_line 101") {
		t.Fatalf("default page: %q, %v", out, err)
	}
	// Line cap of 200.
	out, _ = h.run(t, "preview_file", map[string]any{"path": readme, "max_lines": 1000})
	if !strings.Contains(out, "line 200\n") || strings.Contains(out, "line 201\n") {
		t.Errorf("200-line cap failed: %d bytes", len(out))
	}
	// Page two.
	out, err = h.run(t, "preview_file", map[string]any{"path": readme, "start_line": 201, "max_lines": 5})
	if err != nil || !strings.HasPrefix(out, "line 201\n") || !strings.Contains(out, "line 205\n") || strings.Contains(out, "line 206\n") || !strings.Contains(out, "start_line 206") {
		t.Errorf("page 2: %q, %v", out, err)
	}
	// Last page has no continuation note.
	out, err = h.run(t, "preview_file", map[string]any{"path": readme, "start_line": 440, "lines": 100})
	if err != nil || !strings.Contains(out, "line 450") || strings.Contains(out, "use start_line") {
		t.Errorf("last page: %q, %v", out, err)
	}
	// Past the end, and negative.
	if _, err := h.run(t, "preview_file", map[string]any{"path": readme, "start_line": 5000}); err == nil {
		t.Error("start_line past the end succeeded")
	}
	if _, err := h.run(t, "preview_file", map[string]any{"path": readme, "start_line": -3}); err == nil {
		t.Error("negative start_line succeeded")
	}
	// Byte cap: 190 lines of 100 bytes stop at 16 KiB.
	long := filepath.Join(dir, "notes.txt")
	os.WriteFile(long, []byte(strings.Repeat(strings.Repeat("x", 99)+"\n", 190)), 0o644)
	out, err = h.run(t, "preview_file", map[string]any{"path": long, "lines": 200})
	if err != nil || len(out) > maxTextBytes+100 || !strings.Contains(out, "use start_line") {
		t.Errorf("byte cap: len %d, %v", len(out), err)
	}
	// Data files keep the sample cap and can't be paged.
	out, err = h.run(t, "preview_file", map[string]any{"path": csv, "lines": 100})
	if err != nil || strings.Contains(out, "line 21\n") || !strings.Contains(out, "line 20\n") {
		t.Errorf("data cap: %q, %v", out, err)
	}
	if _, err := h.run(t, "preview_file", map[string]any{"path": csv, "start_line": 5}); err == nil {
		t.Error("data file paging succeeded")
	}
	// Privacy unchanged for doc files.
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "README.md"), []byte("x\n"), 0o644)
	if _, err := h.run(t, "preview_file", map[string]any{"path": filepath.Join(dir, ".git", "README.md")}); err == nil {
		t.Error("hidden-folder doc previewed")
	}
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("x\n"), 0o644)
	if _, err := h.run(t, "preview_file", map[string]any{"path": filepath.Join(dir, "secret.txt")}); err == nil {
		t.Error("secret-looking doc previewed")
	}
	// A NUL deep in a page is refused.
	os.WriteFile(filepath.Join(dir, "bad.py"), []byte(strings.Repeat("a\n", 3000)+"b\x00c\n"), 0o644)
	if _, err := h.run(t, "preview_file", map[string]any{"path": filepath.Join(dir, "bad.py"), "start_line": 2999, "lines": 5}); err == nil {
		t.Error("NUL in a page shown")
	}
}
