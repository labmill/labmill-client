package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M38.1: a preview of a name that doesn't exist says so, lists the folder's
// real entries (never hidden or secret-looking ones), and after three in a
// row says to stop guessing.
func TestPreviewMissingFileListsFolder(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(h.home, "samples", "spai")
	os.MkdirAll(filepath.Join(dir, "testdata"), 0o755)
	for _, n := range []string{"README.md", "CMakeLists.txt", "main.cu", ".env", "id_rsa", "secrets.pem"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x\n"), 0o644)
	}
	_, err := h.run(t, "preview_file", map[string]any{"path": filepath.Join(dir, "run.sh")})
	if err == nil {
		t.Fatal("preview of a missing file succeeded")
	}
	msg := err.Error()
	for _, want := range []string{"run.sh doesn't exist in spai", "find_local_data", "instead of guessing", "README.md", "CMakeLists.txt", "main.cu", "testdata/"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %s", want, msg)
		}
	}
	for _, bad := range []string{".env", "id_rsa", "secrets.pem", "stop guessing"} {
		if strings.Contains(msg, bad) {
			t.Errorf("message contains %q: %s", bad, msg)
		}
	}
	// Second and third guess: the third says stop.
	h.run(t, "preview_file", map[string]any{"path": filepath.Join(dir, "build.sh")})
	_, err = h.run(t, "preview_file", map[string]any{"path": filepath.Join(dir, "reproduce.sh")})
	if err == nil || !strings.Contains(err.Error(), "stop guessing") {
		t.Errorf("third miss: %v", err)
	}
	// Listing the folder resets the count; so does a successful preview.
	if _, err := h.run(t, "find_local_data", map[string]any{"query": "spai"}); err != nil {
		t.Fatal(err)
	}
	_, err = h.run(t, "preview_file", map[string]any{"path": filepath.Join(dir, "run_spai.sh")})
	if err == nil || strings.Contains(err.Error(), "stop guessing") {
		t.Errorf("after find_local_data: %v", err)
	}
}

func TestPreviewMissingFileLimitsListing(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(h.home, "many")
	os.MkdirAll(dir, 0o755)
	for i := 0; i < 30; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.txt", i)), []byte("x"), 0o644)
	}
	_, err := h.run(t, "preview_file", map[string]any{"path": filepath.Join(dir, "nope.sh")})
	if err == nil || !strings.Contains(err.Error(), "f19.txt") || strings.Contains(err.Error(), "f20.txt") || !strings.Contains(err.Error(), "and 10 more") {
		t.Errorf("listing: %v", err)
	}
}

// A missing file outside the allowed folders gets no listing (and no hint
// about what exists there).
func TestPreviewMissingFileOutsideRoots(t *testing.T) {
	h := newHarness(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret-plan.txt"), []byte("x"), 0o644)
	_, err := h.run(t, "preview_file", map[string]any{"path": filepath.Join(outside, "run.sh")})
	if err == nil || strings.Contains(err.Error(), "secret-plan") {
		t.Errorf("err = %v", err)
	}
}

// A relative missing name resolves against the chosen folder.
func TestPreviewMissingRelativeName(t *testing.T) {
	h := newHarness(t)
	code := filepath.Join(h.home, "code", "proj")
	os.MkdirAll(code, 0o755)
	os.WriteFile(filepath.Join(code, "train.py"), []byte("x"), 0o644)
	h.env.Workspace = NewWorkspace(h.home, "")
	if _, err := h.env.Workspace.SetFolder(FolderCode, code); err != nil {
		t.Fatal(err)
	}
	_, err := h.run(t, "preview_file", map[string]any{"path": "run.sh"})
	if err == nil || !strings.Contains(err.Error(), "train.py") {
		t.Errorf("err = %v", err)
	}
}

// M38.1/M38.2: the prompt and the tool descriptions say it.
func TestPromptAndDescriptionsAgainstGuessing(t *testing.T) {
	h := newHarness(t)
	d := h.tools["preview_file"].Description
	for _, want := range []string{"Never guess file names", "find_local_data", "completely, page by page with start_line", "README"} {
		if !strings.Contains(d, want) {
			t.Errorf("preview_file description lacks %q", want)
		}
	}
	p := SystemPrompt(h.env)
	for _, want := range []string{"never guess file names", "page by page with start_line", "no build or run script", "draft_job's files", "do not create files on this computer"} {
		if !strings.Contains(strings.ToLower(p), strings.ToLower(want)) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if dj := h.tools["draft_job"].Description; !strings.Contains(dj, "files") || !strings.Contains(dj, "never on this computer") {
		t.Errorf("draft_job description: %s", dj)
	}
}

// M38.2: a long README can be read to its end by following the page notes.
func TestReadmeReadToTheEnd(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(h.home, "samples", "spai")
	os.MkdirAll(dir, 0o755)
	var b strings.Builder
	for i := 1; i <= 650; i++ {
		fmt.Fprintf(&b, "step %d\n", i)
	}
	readme := filepath.Join(dir, "README.md")
	os.WriteFile(readme, []byte(b.String()), 0o644)
	start, pages := 1, 0
	var last string
	for {
		out, err := h.run(t, "preview_file", map[string]any{"path": readme, "start_line": start, "lines": 200})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		last = out
		i := strings.Index(out, "more follows: use start_line ")
		if i < 0 {
			break
		}
		if _, err := fmt.Sscanf(out[i+len("more follows: use start_line "):], "%d", &start); err != nil {
			t.Fatal(err)
		}
	}
	if pages != 4 || !strings.Contains(last, "step 650") {
		t.Errorf("pages = %d, last page %q", pages, last)
	}
}
