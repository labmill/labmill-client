package app

import (
	"context"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/backend/slurm"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

// On Slurm a node-local work_dir is refused (the output would be lost), and
// the approved submit creates the work dir before sbatch runs; the approval
// says so.
func TestSlurmWorkDirNodeLocalAndMkdir(t *testing.T) {
	h := newHarness(t)
	rr := &argvRunner{out: "42\n"}
	h.env.Backend = func(_ context.Context, c config.Cluster) (backend.Backend, error) {
		return &slurm.Slurm{Runner: rr}, nil
	}
	h.tools = map[string]agent.Tool{}
	for _, tool := range Tools(h.env) {
		h.tools[tool.Name] = tool
	}
	for _, wd := range []string{"/tmp/run", "/var/tmp/run", "/dev/shm/x"} {
		_, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone", "spec": map[string]any{"name": "cpu-job", "command": "echo hi", "work_dir": wd}})
		if err == nil || !strings.Contains(err.Error(), "node-local") {
			t.Errorf("%s: err = %v", wd, err)
		}
	}
	if _, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone", "spec": map[string]any{"name": "cpu-job", "command": "echo hi", "work_dir": "/storage/me/run"}}); err != nil {
		t.Fatal(err)
	}
	// The draft asked the scheduler for a start estimate (M61): a read-only
	// sbatch --test-only that submits nothing.
	if len(rr.calls) != 1 || strings.Join(rr.calls[0], " ") != "sbatch --test-only" {
		t.Errorf("draft calls = %v", rr.calls)
	}
	rr.calls = nil
	jobs, _ := h.env.Store.Jobs(context.Background(), store.JobFilter{})
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v", jobs)
	}
	in := mustJSON(map[string]any{"job_id": jobs[0].ID})
	p, err := h.tools["submit_job"].Propose(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Summary, "`mkdir -p -- /storage/me/run`") || !strings.Contains(p.Summary, "submitted script") {
		t.Errorf("summary = %q", p.Summary)
	}
	if len(rr.calls) != 1 || strings.Join(rr.calls[0], " ") != "sbatch --test-only" {
		t.Errorf("review calls = %v", rr.calls) // the pre-flight check reads only
	}
	rr.calls = nil
	if _, err := h.tools["submit_job"].Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p}); err != nil {
		t.Fatal(err)
	}
	if len(rr.calls) != 2 || strings.Join(rr.calls[0], " ") != "mkdir -p -- /storage/me/run" || rr.calls[1][0] != "sbatch" {
		t.Errorf("calls = %v", rr.calls)
	}
}
