package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

func TestValidateJobFiles(t *testing.T) {
	big := strings.Repeat("x", maxJobFileBytes+1)
	cases := []struct {
		name  string
		files []jobFile
		want  string // "" = valid
	}{
		{"ok", []jobFile{{"labmill-run.sh", "#!/bin/bash\necho hi"}}, ""},
		{"slash", []jobFile{{"sub/run.sh", "echo"}}, "isn't allowed"},
		{"dotdot", []jobFile{{"../run.sh", "echo"}}, "isn't allowed"},
		{"leading dot", []jobFile{{".bashrc", "echo"}}, "isn't allowed"},
		{"empty name", []jobFile{{"", "echo"}}, "isn't allowed"},
		{"space", []jobFile{{"my run.sh", "echo"}}, "isn't allowed"},
		{"duplicate", []jobFile{{"a.sh", "echo"}, {"a.sh", "echo"}}, "twice"},
		{"empty content", []jobFile{{"a.sh", " \n"}}, "empty"},
		{"too big", []jobFile{{"a.sh", big}}, "KiB"},
		{"control char", []jobFile{{"a.sh", "echo \x1b[31m"}}, "isn't valid"},
		{"bidi", []jobFile{{"a.sh", "echo ‮"}}, "isn't valid"},
		{"marker line", []jobFile{{"a.sh", "echo\n" + fileMarkerPrefix + "b.sh" + fileMarkerSuffix + "\n"}}, "marker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := validateJobFiles(tc.files)
			if tc.want == "" {
				if err != nil || !strings.HasSuffix(out[0].Content, "\n") {
					t.Fatalf("got %v, %v", out, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	many := make([]jobFile, maxJobFiles+1)
	for i := range many {
		many[i] = jobFile{"f" + string(rune('a'+i)) + ".sh", "echo"}
	}
	if _, err := validateJobFiles(many); err == nil {
		t.Error("too many files accepted")
	}
}

func TestReviewFilesRoundTrip(t *testing.T) {
	files := []jobFile{{"labmill-build.sh", "#!/bin/bash\nmake\n"}, {"labmill-run.sh", "#!/bin/bash\n./app\n"}}
	text := reviewWithFiles("#!/bin/bash\nbash labmill-build.sh\n", files)
	if !strings.Contains(text, fileMarkerPrefix+"labmill-run.sh") {
		t.Fatalf("review lacks the marker:\n%s", text)
	}
	script, got, err := splitReviewFiles(text)
	if err != nil || script != "#!/bin/bash\nbash labmill-build.sh\n" || len(got) != 2 || got[0] != files[0] || got[1] != files[1] {
		t.Fatalf("round trip: %q %v %v", script, got, err)
	}
	// No files: the text is untouched.
	if s, f, err := splitReviewFiles("a\nb"); s != "a\nb" || f != nil || err != nil {
		t.Errorf("no files: %q %v %v", s, f, err)
	}
	// A damaged marker is an error; a removed file is simply gone.
	if _, _, err := splitReviewFiles("x\n" + fileMarkerPrefix + "a.sh broken\necho\n"); err == nil {
		t.Error("damaged marker accepted")
	}
	// An edited file name is validated.
	if _, _, err := splitReviewFiles("x\n" + fileMarkerPrefix + "../a.sh" + fileMarkerSuffix + "\necho\n"); err == nil {
		t.Error("bad edited name accepted")
	}
}

// localRunner runs the argv on this machine (the sh program that writes
// the files is the code under test) and records the calls.
type localRunner struct{ calls [][]string }

func (l *localRunner) Run(ctx context.Context, argv []string, stdin io.Reader) (backend.Result, error) {
	l.calls = append(l.calls, argv)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdin
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	res := backend.Result{}
	err := cmd.Run()
	res.Stdout, res.Stderr = o.String(), e.String()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode, err = ee.ExitCode(), nil
	}
	return res, err
}

func filesHarness(t *testing.T) (*harness, *localRunner) {
	t.Helper()
	requireTools(t, "sh", "mktemp", "cmp", "chmod")
	h := newHarness(t)
	lr := &localRunner{}
	h.env.LoginRunner = func(context.Context, config.Cluster) (backend.Runner, error) { return lr, nil }
	return h, lr
}

func submitWith(t *testing.T, h *harness, id int64, edit func(string) string) (string, error) {
	t.Helper()
	in := mustJSON(map[string]any{"job_id": id})
	p, err := h.tools["submit_job"].Propose(in)
	if err != nil {
		t.Fatal(err)
	}
	d := agent.Decision{Approved: true, Proposal: p}
	if edit != nil {
		d.Edited = edit(p.Details)
	}
	return h.tools["submit_job"].Run(context.Background(), in, d)
}

// Slurm: the files are shown in the review below the script, edits come
// back, and submit writes them (mode 0755) before sbatch.
func TestDraftJobFilesSlurm(t *testing.T) {
	h, _ := filesHarness(t)
	dir := t.TempDir()
	out, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone",
		"files": []map[string]string{{"name": "labmill-run.sh", "content": "#!/bin/bash\necho one\n"}},
		"spec":  map[string]any{"name": "nolocal", "work_dir": dir, "command": "bash labmill-run.sh"}})
	if err != nil || !strings.Contains(out, "Extra files") || !strings.Contains(out, "labmill-run.sh") {
		t.Fatalf("draft: %q, %v", out, err)
	}
	jobs, _ := h.env.Store.Jobs(context.Background(), store.JobFilter{})
	id := jobs[0].ID
	p, err := h.tools["submit_job"].Propose(mustJSON(map[string]any{"job_id": id}))
	if err != nil || !strings.Contains(p.Details, fileMarkerPrefix+"labmill-run.sh") || !strings.Contains(p.Details, "echo one") ||
		!strings.Contains(p.Summary, "writes 1 file") {
		t.Fatalf("proposal: %+v, %v", p, err)
	}
	// The user edits the file in the review.
	_, err = submitWith(t, h, id, func(d string) string { return strings.Replace(d, "echo one", "echo two", 1) })
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "labmill-run.sh"))
	if err != nil || string(got) != "#!/bin/bash\necho two\n" {
		t.Fatalf("file = %q, %v", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "labmill-run.sh")); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", fi.Mode())
	}
	if len(h.be.submitted) != 1 || strings.Contains(h.be.submitted[0], "Labmill writes the file") || strings.Contains(h.be.submitted[0], "echo two") {
		t.Errorf("submitted script carries the files: %q", h.be.submitted)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover files in the job folder: %v", entries)
	}
}

// Nothing is submitted when a file can't be written; an existing file with
// other content is never overwritten, the same content is fine.
func TestJobFilesNeverOverwrite(t *testing.T) {
	h, _ := filesHarness(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "labmill-run.sh"), []byte("#!/bin/bash\nold\n"), 0o644)
	draft := func(content string) int64 {
		t.Helper()
		_, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone",
			"files": []map[string]string{{"name": "labmill-run.sh", "content": content}},
			"spec":  map[string]any{"name": "nolocal", "work_dir": dir, "command": "bash labmill-run.sh"}})
		if err != nil {
			t.Fatal(err)
		}
		jobs, _ := h.env.Store.Jobs(context.Background(), store.JobFilter{})
		return jobs[0].ID
	}
	id := draft("#!/bin/bash\nnew\n")
	if _, err := submitWith(t, h, id, nil); err == nil || !strings.Contains(err.Error(), "already exists") || len(h.be.submitted) != 0 {
		t.Fatalf("overwrite: %v, submitted %v", err, h.be.submitted)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "labmill-run.sh")); string(b) != "#!/bin/bash\nold\n" {
		t.Errorf("file overwritten: %q", b)
	}
	// Identical content: accepted.
	id = draft("#!/bin/bash\nold\n")
	if _, err := submitWith(t, h, id, nil); err != nil || len(h.be.submitted) != 1 {
		t.Errorf("identical file: %v", err)
	}
}

func TestJobFilesRejectedAtDraft(t *testing.T) {
	h, _ := filesHarness(t)
	spec := map[string]any{"name": "nolocal", "work_dir": t.TempDir(), "command": "true"}
	_, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone", "spec": spec,
		"files": []map[string]string{{"name": "../x.sh", "content": "echo"}}})
	if err == nil || !strings.Contains(err.Error(), "isn't allowed") {
		t.Errorf("bad name: %v", err)
	}
	h.env.LoginRunner = nil
	_, err = h.run(t, "draft_job", map[string]any{"cluster": "hpcone", "spec": spec,
		"files": []map[string]string{{"name": "x.sh", "content": "echo"}}})
	if err == nil {
		t.Error("files accepted without a login runner")
	}
	// A redraft without files clears an earlier set.
	h, _ = filesHarness(t)
	for _, files := range [][]map[string]string{{{"name": "x.sh", "content": "echo"}}, nil} {
		in := map[string]any{"cluster": "hpcone", "spec": spec}
		if files != nil {
			in["files"] = files
		}
		if _, err := h.run(t, "draft_job", in); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _ := h.env.Store.Jobs(context.Background(), store.JobFilter{})
	p, _ := h.tools["submit_job"].Propose(mustJSON(map[string]any{"job_id": jobs[0].ID}))
	if strings.Contains(p.Details, fileMarkerPrefix) {
		t.Errorf("cleared files still in the review:\n%s", p.Details)
	}
}

// A change to the stored files after the user saw the review is caught.
func TestJobFilesChangedAfterApproval(t *testing.T) {
	h, _ := filesHarness(t)
	spec := map[string]any{"name": "nolocal", "work_dir": t.TempDir(), "command": "true"}
	draft := func(c string) {
		if _, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone", "spec": spec,
			"files": []map[string]string{{"name": "x.sh", "content": c}}}); err != nil {
			t.Fatal(err)
		}
	}
	draft("echo a")
	jobs, _ := h.env.Store.Jobs(context.Background(), store.JobFilter{})
	in := mustJSON(map[string]any{"job_id": jobs[0].ID})
	p, _ := h.tools["submit_job"].Propose(in)
	draft("echo b")
	if _, err := h.tools["submit_job"].Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p}); !errors.Is(err, errChanged) {
		t.Errorf("err = %v, want errChanged", err)
	}
}

type recordingRunner struct {
	calls [][]string
	stdin []string
}

func (r *recordingRunner) Run(_ context.Context, argv []string, stdin io.Reader) (backend.Result, error) {
	r.calls = append(r.calls, argv)
	b, _ := io.ReadAll(stdin)
	r.stdin = append(r.stdin, string(b))
	return backend.Result{}, nil
}

// The writer program refuses a symlinked job folder.
func TestWriteJobFilesSymlinkFolder(t *testing.T) {
	h, _ := filesHarness(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	ts := &toolset{env: h.env}
	err := ts.writeJobFiles(context.Background(), config.Cluster{Name: "hpcone"}, link, []jobFile{{"a.sh", "echo\n"}})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("err = %v", err)
	}
}

// The writer program is one line without '!' (csh/tcsh login shells).
func TestWriteScriptOneLine(t *testing.T) {
	if strings.ContainsAny(writeScript, "\n\r!") {
		t.Errorf("writeScript has a newline or '!': %q", writeScript)
	}
}

// A short write (the expected byte count differs from what arrived) leaves
// no file behind and fails with exit 4.
func TestWriteScriptShortWrite(t *testing.T) {
	requireTools(t, "sh", "mktemp", "cmp", "chmod", "ln", "wc")
	dir := t.TempDir()
	run := func(content string, n string) (int, string) {
		cmd := exec.Command("sh", "-c", writeScript, "sh", dir, "a.sh", n)
		cmd.Stdin = strings.NewReader(content)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), string(out)
		}
		return 0, string(out)
	}
	if code, out := run("echo hi\n", "100"); code != 4 || !strings.Contains(out, "short write") {
		t.Errorf("short write: exit %d, %q", code, out)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("leftovers after a short write: %v", ents)
	}
	if code, out := run("echo hi\n", "8"); code != 0 {
		t.Fatalf("full write: exit %d, %q", code, out)
	}
	if code, _ := run("echo other\n", "11"); code != 3 {
		t.Errorf("existing different file: exit %d, want 3", code)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.sh")); string(b) != "echo hi\n" {
		t.Errorf("file = %q", b)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("leftover temp files: %v", ents)
	}
}

// A job script that looks like the review's file marker is refused at
// draft, and the approval names the destination folder.
func TestJobScriptMarkerRefusedAndSummaryFolder(t *testing.T) {
	h, _ := filesHarness(t)
	dir := t.TempDir()
	files := []map[string]string{{"name": "x.sh", "content": "echo\n"}}
	_, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone", "files": files,
		"spec": map[string]any{"name": "nolocal", "work_dir": dir, "command": "true\n" + fileMarkerPrefix + "evil.sh"}})
	if err == nil {
		t.Error("script with a marker line accepted")
	}
	// The same without files: submit splits every review text, so the
	// marker is refused either way.
	if _, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone",
		"spec": map[string]any{"name": "nolocal", "work_dir": dir, "command": "true\n" + fileMarkerPrefix + "evil.sh"}}); err == nil {
		t.Error("script with a marker line and no files accepted")
	}
	if _, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone", "files": files,
		"spec": map[string]any{"name": "nolocal", "work_dir": dir, "command": "bash x.sh"}}); err != nil {
		t.Fatal(err)
	}
	jobs, _ := h.env.Store.Jobs(context.Background(), store.JobFilter{})
	p, err := h.tools["submit_job"].Propose(mustJSON(map[string]any{"job_id": jobs[0].ID}))
	if err != nil || !strings.Contains(p.Summary, dir) {
		t.Errorf("summary %q (%v) lacks %s", p.Summary, err, dir)
	}
}
