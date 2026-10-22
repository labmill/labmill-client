package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

type fakeBackend struct {
	typ       string
	submitted []string
	canceled  []string
	status    backend.Status
}

func (f *fakeBackend) Type() string { return f.typ }
func (f *fakeBackend) Info(context.Context) (backend.ClusterInfo, error) {
	return backend.ClusterInfo{Partitions: []backend.Partition{{Name: "gpu", Default: true, State: "up", GPUTypes: []string{"a100"}, Nodes: 4, IdleNodes: 2}}}, nil
}
func (f *fakeBackend) Render(s backend.JobSpec) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	return "#!/bin/bash\n#SBATCH --job-name=" + s.Name + "\n#SBATCH --chdir=" + s.WorkDir + "\n" + s.Command + "\n", nil
}
func (f *fakeBackend) Submit(_ context.Context, script string, _ backend.JobSpec) (string, error) {
	f.submitted = append(f.submitted, script)
	return "4242", nil
}
func (f *fakeBackend) Status(_ context.Context, id string) (backend.Status, error) {
	st := f.status
	st.RemoteID = id
	return st, nil
}
func (f *fakeBackend) Cancel(_ context.Context, id string) error {
	f.canceled = append(f.canceled, id)
	return nil
}
func (f *fakeBackend) Logs(context.Context, string, int) (string, error) {
	return "epoch 1\nepoch 2\n", nil
}

type harness struct {
	env      *Env
	be       *fakeBackend
	tools    map[string]agent.Tool
	uploads  []transfer.Remote
	uploaded [][]store.ManifestEntry
	home     string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	home := t.TempDir()
	mk := func(rel, content string) {
		p := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	mk("data/mri-2026/scan001.dcm", strings.Repeat("d", 300))
	mk("data/mri-2026/scan002.dcm", strings.Repeat("d", 300))
	mk("data/mri-2026/labels.csv", "id,label\n1,tumor\n2,clear\n")
	mk("Documents/thesis.docx", "x")
	mk(".hidden/mri-secret/x.dcm", "x")
	st, err := store.Open(filepath.Join(t.TempDir(), "y.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &harness{be: &fakeBackend{typ: "slurm", status: backend.Status{State: store.StateRunning}}, home: home}
	h.env = &Env{
		Config: &config.Config{Clusters: []config.Cluster{
			{Name: "hpcone", Type: config.TypeSlurm, SSHHost: "hpclogin"},
			{Name: "second", Type: config.TypeSlurm, SSHHost: "hpclogin2"},
		}},
		Store: st,
		Backend: func(_ context.Context, c config.Cluster) (backend.Backend, error) {
			if c.Name == "hpcone" {
				return h.be, nil
			}
			return nil, errors.New("not in test")
		},
		RemoteHome: func(context.Context, config.Cluster) (string, error) { return "/home/alice", nil },
		Upload: func(_ context.Context, root string, entries []store.ManifestEntry, dst transfer.Remote, progress func(transfer.Progress)) error {
			h.uploads = append(h.uploads, dst)
			h.uploaded = append(h.uploaded, entries)
			if progress != nil {
				progress(transfer.Progress{Bytes: 1, TotalBytes: 1, Files: 1, TotalFiles: 1})
			}
			return nil
		},
		Download: func(_ context.Context, src transfer.Remote, paths []string, localDir string, _ func(transfer.Progress)) ([]string, error) {
			return []string{filepath.Join(localDir, paths[0])}, nil
		},
		SSHCommand: func(alias string) []string { return []string{"labmill", "ssh-exec", alias} },
		LocalRoots: []string{home},
		ResultsDir: filepath.Join(home, "labmill-results"),
		Now:        func() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) },
	}
	h.tools = map[string]agent.Tool{}
	for _, tool := range Tools(h.env) {
		h.tools[tool.Name] = tool
	}
	return h
}

func (h *harness) run(t *testing.T, name string, input any) (string, error) {
	t.Helper()
	tool, ok := h.tools[name]
	if !ok {
		t.Fatalf("no tool %q", name)
	}
	b, _ := json.Marshal(input)
	var d agent.Decision
	if tool.NeedsApproval {
		p, err := tool.Propose(b)
		switch {
		case errors.Is(err, agent.ErrNoApproval):
		case err != nil:
			return "", err
		default:
			d = agent.Decision{Approved: true, Proposal: p}
		}
	}
	return tool.Run(context.Background(), b, d)
}

func TestToolSetAndApprovalGates(t *testing.T) {
	h := newHarness(t)
	want := map[string]bool{
		"list_clusters": false, "cluster_info": false, "cluster_jobs": false, "find_local_data": false, "preview_file": false, "stage_data": false,
		"draft_job": false, "job_status": false, "list_jobs": false, "job_logs": false, "diagnose_job": false, "ask_user": false,
		"update_todos": false,
		"upload_data":  true, "submit_job": true, "cancel_job": true, "fetch_results": true, "cleanup_job": true,
		"add_cluster": true, "set_cluster_account": true,
		"s3_credentials_to_cluster": true,
		"remote_command":            true, // gated, except read-only commands (Propose returns agent.ErrNoApproval)
	}
	if len(h.tools) != len(want) {
		t.Errorf("got %d tools, want %d", len(h.tools), len(want))
	}
	for name, gated := range want {
		tool, ok := h.tools[name]
		if !ok {
			t.Errorf("missing tool %s", name)
			continue
		}
		if tool.NeedsApproval != gated {
			t.Errorf("%s NeedsApproval = %v, want %v", name, tool.NeedsApproval, gated)
		}
	}
	if err := agent.NewRegistry().Register(h.tools["submit_job"]); err != nil {
		t.Errorf("submit_job not registrable: %v", err)
	}
}

func TestListClusters(t *testing.T) {
	h := newHarness(t)
	out, err := h.run(t, "list_clusters", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hpcone", "slurm", "second", "transfer sftp"} {
		if !strings.Contains(out, want) {
			t.Errorf("list_clusters missing %q:\n%s", want, out)
		}
	}
}

func TestFindLocalDataAndPreview(t *testing.T) {
	h := newHarness(t)
	out, err := h.run(t, "find_local_data", map[string]any{"query": "mri 2026"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "mri-2026") || !strings.Contains(out, "scan001.dcm") || strings.Contains(out, "mri-secret") {
		t.Errorf("find_local_data:\n%s", out)
	}
	prev, err := h.run(t, "preview_file", map[string]any{"path": filepath.Join(h.home, "data/mri-2026/labels.csv"), "lines": 2})
	if err != nil || !strings.Contains(prev, "id,label") || strings.Contains(prev, "clear") {
		t.Errorf("preview = %q, %v", prev, err)
	}
	if _, err := h.run(t, "preview_file", map[string]any{"path": "/etc/passwd"}); err == nil {
		t.Error("preview outside LocalRoots succeeded")
	}
	if _, err := h.run(t, "preview_file", map[string]any{"path": filepath.Join(h.home, "data/mri-2026/scan001.dcm")}); err != nil {
		// "ddd..." is text; a NUL-containing file must be refused instead.
		t.Logf("dcm preview: %v", err)
	}
	os.WriteFile(filepath.Join(h.home, "bin.dat"), []byte{0, 1, 2, 3}, 0o644)
	if _, err := h.run(t, "preview_file", map[string]any{"path": filepath.Join(h.home, "bin.dat")}); err == nil {
		t.Error("binary preview succeeded")
	}
}

func TestFullJobFlow(t *testing.T) {
	h := newHarness(t)
	out, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data/mri-2026"}, "job_name": "seg-train"})
	if err != nil {
		t.Fatalf("stage_data: %v", err)
	}
	var staged struct {
		JobID int64 `json:"job_id"`
		Files int   `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &staged); err != nil || staged.JobID == 0 || staged.Files != 3 {
		t.Fatalf("stage_data result %q (%v)", out, err)
	}
	id := staged.JobID

	// Upload: proposal lists files; remote dir defaults under labmill/<job name>.
	up := h.tools["upload_data"]
	p, err := up.Propose(mustJSON(map[string]any{"job_id": id, "cluster": "hpcone"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Summary, "3 files") || !strings.Contains(p.Details, "labels.csv") || p.Editable {
		t.Errorf("upload proposal = %+v", p)
	}
	if _, err := h.run(t, "upload_data", map[string]any{"job_id": id, "cluster": "hpcone"}); err != nil {
		t.Fatalf("upload_data: %v", err)
	}
	if len(h.uploads) != 1 || h.uploads[0].Dir != "labmill/seg-train" || strings.Join(h.uploads[0].SSHCommand, " ") != "labmill ssh-exec hpclogin" {
		t.Errorf("upload dst = %+v", h.uploads)
	}
	j, _ := h.env.Store.Job(context.Background(), id)
	if j.RemoteDir != "/home/alice/labmill/seg-train" || j.Cluster != "hpcone" {
		t.Errorf("job after upload = %+v", j)
	}

	// Draft: work dir defaults to the remote dir.
	out, err = h.run(t, "draft_job", map[string]any{"job_id": id, "cluster": "hpcone",
		"spec": map[string]any{"name": "seg-train", "gpus": 1, "command": "python train.py --data data/mri-2026"}})
	if err != nil {
		t.Fatalf("draft_job: %v", err)
	}
	if !strings.Contains(out, "--chdir=/home/alice/labmill/seg-train") {
		t.Errorf("draft = %s", out)
	}

	// Submit: proposal shows the exact script, editable; the edited script is what's submitted.
	sp, err := h.tools["submit_job"].Propose(mustJSON(map[string]any{"job_id": id}))
	if err != nil || !sp.Editable || !strings.Contains(sp.Details, "python train.py") {
		t.Fatalf("submit proposal = %+v, %v", sp, err)
	}
	edited := strings.Replace(sp.Details, "python train.py", "python train.py --epochs 5", 1)
	res, err := h.tools["submit_job"].Run(context.Background(), mustJSON(map[string]any{"job_id": id}), agent.Decision{Approved: true, Proposal: sp, Edited: edited})
	if err != nil || !strings.Contains(res, "4242") {
		t.Fatalf("submit = %q, %v", res, err)
	}
	if len(h.be.submitted) != 1 || !strings.Contains(h.be.submitted[0], "--epochs 5") {
		t.Errorf("submitted = %q", h.be.submitted)
	}
	j, _ = h.env.Store.Job(context.Background(), id)
	if j.State != store.StateSubmitted || j.RemoteID != "4242" || !strings.Contains(j.Script, "--epochs 5") {
		t.Errorf("job after submit = %+v", j)
	}

	// Status and logs.
	if out, err := h.run(t, "job_status", map[string]any{"job_id": id}); err != nil || !strings.Contains(out, "running") {
		t.Errorf("status = %q, %v", out, err)
	}
	if j, _ := h.env.Store.Job(context.Background(), id); j.State != store.StateRunning {
		t.Errorf("state after status = %s", j.State)
	}
	if out, _ := h.run(t, "job_logs", map[string]any{"job_id": id, "lines": 2}); !strings.Contains(out, "epoch 2") {
		t.Errorf("logs = %q", out)
	}
	if out, _ := h.run(t, "list_jobs", map[string]any{"active_only": true}); !strings.Contains(out, "seg-train") {
		t.Errorf("list_jobs = %q", out)
	}

	// Fetch results into the results dir.
	fp, err := h.tools["fetch_results"].Propose(mustJSON(map[string]any{"job_id": id, "paths": []string{"results"}}))
	if err != nil || !strings.Contains(fp.Summary, filepath.Join(h.home, "labmill-results")) {
		t.Errorf("fetch proposal = %+v, %v", fp, err)
	}
	if _, err := h.run(t, "fetch_results", map[string]any{"job_id": id, "paths": []string{"results"}}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if j, _ := h.env.Store.Job(context.Background(), id); j.State != store.StateFetched {
		t.Errorf("state after fetch = %s", j.State)
	}
}

func TestCancelAndErrors(t *testing.T) {
	h := newHarness(t)
	id, _ := h.env.Store.CreateJob(context.Background(), store.Job{Name: "x", Cluster: "hpcone", Backend: "slurm", RemoteID: "77", State: store.StateRunning})
	cp, err := h.tools["cancel_job"].Propose(mustJSON(map[string]any{"job_id": id}))
	if err != nil || !strings.Contains(cp.Summary, "77") {
		t.Fatalf("cancel proposal = %+v %v", cp, err)
	}
	if _, err := h.run(t, "cancel_job", map[string]any{"job_id": id}); err != nil || len(h.be.canceled) != 1 {
		t.Fatalf("cancel: %v %v", err, h.be.canceled)
	}
	for name, in := range map[string]any{
		"job_status":   map[string]any{"job_id": 99999},
		"cluster_info": map[string]any{"cluster": "nope"},
		"draft_job":    map[string]any{"job_id": id, "cluster": "hpcone", "spec": map[string]any{"name": "bad name", "command": "x"}},
		"stage_data":   map[string]any{"root": "/etc", "paths": []string{"passwd"}, "job_name": "x"},
	} {
		if _, err := h.run(t, name, in); err == nil {
			t.Errorf("%s(%v) succeeded", name, in)
		}
	}
	if _, err := h.tools["submit_job"].Propose(mustJSON(map[string]any{"job_id": id})); err == nil {
		// job has no drafted script in this setup? it has none: must refuse
		t.Error("submit without a drafted script proposed")
	}
}

func TestSystemPrompt(t *testing.T) {
	h := newHarness(t)
	sp := SystemPrompt(h.env)
	for _, want := range []string{"hpcone", "second", "approve", "login node", "find_local_data", "ask_user"} {
		if !strings.Contains(sp, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if SystemPrompt(h.env) != sp {
		t.Error("system prompt not deterministic")
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
