package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

func TestPreviewRefusesSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	h := newHarness(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("secret"), 0o644)
	link := filepath.Join(h.home, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(t, "preview_file", map[string]any{"path": link}); err == nil {
		t.Error("preview followed a symlink out of LocalRoots")
	}
}

func TestStageDataValidation(t *testing.T) {
	h := newHarness(t)
	for _, in := range []map[string]any{
		{"root": h.home, "paths": []string{"data"}, "job_name": "bad name"},
		{"root": h.home, "paths": []string{}, "job_name": "ok"},
		{"root": h.home, "paths": []string{"../elsewhere"}, "job_name": "ok"},
	} {
		if _, err := h.run(t, "stage_data", in); err == nil {
			t.Errorf("stage_data(%v) succeeded", in)
		}
	}
}

func TestUploadAndDraftValidation(t *testing.T) {
	h := newHarness(t)
	if _, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data"}, "job_name": "j1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(t, "upload_data", map[string]any{"job_id": 1, "cluster": "hpcone", "remote_dir": "../x"}); err == nil {
		t.Error("remote_dir with .. accepted")
	}
	if _, err := h.run(t, "draft_job", map[string]any{"job_id": 1, "cluster": "hpcone", "spec": map[string]any{"command": "echo hi"}}); err == nil || !strings.Contains(err.Error(), "where to run") {
		t.Errorf("draft without work dir: %v", err)
	}
}

// What runs is what the user approved: an edited script is submitted as
// edited, and a script changed in the store after approval is refused.
func TestSubmitUsesApprovedText(t *testing.T) {
	h := newHarness(t)
	id := h.stageUploadDraft(t, "j1")
	ctx := context.Background()
	in := mustJSON(map[string]any{"job_id": id})

	sp, err := h.tools["submit_job"].Propose(in)
	if err != nil {
		t.Fatal(err)
	}
	// The stored script changes after the user saw the proposal.
	h.env.Store.UpdateJob(ctx, id, func(j *store.Job) error { j.Script = "#!/bin/bash\nrm -rf ~\n"; return nil })
	if _, err := h.tools["submit_job"].Run(ctx, in, agent.Decision{Approved: true, Proposal: sp}); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Errorf("Run after the script changed: %v", err)
	}
	if len(h.be.submitted) != 0 {
		t.Fatalf("submitted %q", h.be.submitted)
	}

	// Unchanged store: the user's edited text is what is submitted.
	h.env.Store.UpdateJob(ctx, id, func(j *store.Job) error { j.Script = sp.Details; return nil })
	edited := sp.Details + "echo edited\n"
	if _, err := h.tools["submit_job"].Run(ctx, in, agent.Decision{Approved: true, Proposal: sp, Edited: edited}); err != nil {
		t.Fatal(err)
	}
	if len(h.be.submitted) != 1 || h.be.submitted[0] != edited {
		t.Errorf("submitted = %q, want the edited text", h.be.submitted)
	}
}

// An absolute upload folder (group storage) is used as is on an SFTP cluster,
// without joining it to the home folder, and refused for an S3 cluster.
func TestUploadAbsoluteRemoteDir(t *testing.T) {
	h := s3Harness(t, "")
	homeCalls := 0
	h.env.RemoteHome = func(context.Context, config.Cluster) (string, error) { homeCalls++; return "/home/alice", nil }
	ctx := context.Background()
	if _, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data"}, "job_name": "j1"}); err != nil {
		t.Fatal(err)
	}
	const dir = "/storage/t3/lab/labmill-live/agent"
	p, err := h.tools["upload_data"].Propose(mustJSON(map[string]any{"job_id": 1, "cluster": "hpcone", "remote_dir": dir}))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !strings.Contains(p.Summary, "hpcone:"+dir+" (through hpclogin)") {
		t.Errorf("summary = %q", p.Summary)
	}
	out, err := h.run(t, "upload_data", map[string]any{"job_id": 1, "cluster": "hpcone", "remote_dir": dir})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if len(h.uploads) != 1 || h.uploads[0].Dir != dir {
		t.Errorf("uploads = %+v", h.uploads)
	}
	j, _ := h.env.Store.Job(ctx, 1)
	if j.RemoteDir != dir || !strings.Contains(out, "hpcone:"+dir) || homeCalls != 0 {
		t.Errorf("remote dir %q, out %q, home lookups %d", j.RemoteDir, out, homeCalls)
	}
	if _, err := h.run(t, "draft_job", map[string]any{"job_id": 1, "cluster": "hpcone", "spec": map[string]any{"command": "echo hi"}}); err != nil {
		t.Fatalf("draft: %v", err)
	}
	if j, _ := h.env.Store.Job(ctx, 1); !strings.Contains(j.Script, "--chdir="+dir+"\n") {
		t.Errorf("script = %q", j.Script)
	}

	if _, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data"}, "job_name": "j2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.tools["upload_data"].Propose(mustJSON(map[string]any{"job_id": 2, "cluster": "bucket", "remote_dir": dir})); err == nil {
		t.Error("absolute folder accepted for an S3 cluster")
	}
}
