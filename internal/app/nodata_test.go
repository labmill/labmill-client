package app

import (
	"context"
	"io"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/backend/slurm"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

// argvRunner records commands and answers every one with exit 0 and out.
type argvRunner struct {
	calls [][]string
	out   string
}

func (r *argvRunner) Run(_ context.Context, argv []string, _ io.Reader) (backend.Result, error) {
	r.calls = append(r.calls, argv)
	return backend.Result{Stdout: r.out}, nil
}

// The same name again: an unsubmitted no-data job is redrafted in place; one
// that holds data is refused with the job_id hint.
func TestDraftNoDataSameName(t *testing.T) {
	h := newHarness(t)
	h.env.Backend = func(_ context.Context, c config.Cluster) (backend.Backend, error) {
		return &slurm.Slurm{Runner: &argvRunner{}}, nil
	}
	h.tools = map[string]agent.Tool{}
	for _, tool := range Tools(h.env) {
		h.tools[tool.Name] = tool
	}
	spec := func(cmd string) map[string]any {
		return map[string]any{"cluster": "hpcone", "spec": map[string]any{"name": "gpu-check", "command": cmd, "work_dir": "/scratch/alice/gpu-check"}}
	}
	if _, err := h.run(t, "draft_job", spec("nvidia-smi")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(t, "draft_job", spec("nvidia-smi -L")); err != nil {
		t.Fatal(err)
	}
	jobs, _ := h.env.Store.Jobs(context.Background(), store.JobFilter{})
	if len(jobs) != 1 || !strings.Contains(jobs[0].Script, "nvidia-smi -L") {
		t.Fatalf("redraft made duplicates or kept the old script: %+v", jobs)
	}
	if err := h.env.Store.UpdateJob(context.Background(), jobs[0].ID, func(j *store.Job) error { j.RemoteDir = "/scratch/x"; return nil }); err != nil {
		t.Fatal(err)
	}
	_, err := h.run(t, "draft_job", spec("nvidia-smi"))
	if err == nil || !strings.Contains(err.Error(), "already has this name and its data; pass job_id") {
		t.Errorf("same name with data: %v", err)
	}
}
