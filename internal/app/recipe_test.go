package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"labmill-client/internal/account"
	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

func TestRecipeSoftware(t *testing.T) {
	spec := backend.JobSpec{Environment: &backend.Environment{Modules: []string{"python/3.11", "gcc"}, Apptainer: &backend.ApptainerEnv{Image: "docker://ubuntu:24.04"}}}
	got := strings.Join(recipeSoftware(spec,
		"#!/bin/bash\nmodule load R/4.3.1 samtools --quiet\nml python/3.11\n# module load hidden\nconda activate myenv\napptainer exec --nv img.sif run.sh\n"), "\n")
	for _, want := range []string{"module python/3.11\n", "module gcc (no version pinned)", "container docker://ubuntu:24.04",
		"module R/4.3.1", "module samtools (no version pinned)", "conda environment myenv", "container img.sif"} {
		if !strings.Contains(got+"\n", want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "hidden") || strings.Contains(got, "quiet") || strings.Count(got, "python/3.11") != 1 {
		t.Errorf("wrong list:\n%s", got)
	}
}

// submit saves the recipe as an event and as a file; job_recipe shows it; rerun_job drafts the same job again.
func TestRecipeSaveShowRerun(t *testing.T) {
	h, _ := filesHarness(t)
	dir := t.TempDir()
	h.env.ResultsDir = t.TempDir()
	h.env.ClientVersion = "v9.9"
	if _, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone",
		"files": []map[string]string{{"name": "labmill-run.sh", "content": "#!/bin/bash\nmodule load tool\necho one\n"}},
		"spec":  map[string]any{"name": "rr", "work_dir": dir, "command": "bash labmill-run.sh"}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	jobs, _ := h.env.Store.Jobs(ctx, store.JobFilter{})
	id := jobs[0].ID
	if _, err := h.run(t, "rerun_job", map[string]any{"job_id": id}); err == nil || !strings.Contains(err.Error(), "never submitted") {
		t.Fatalf("rerun of a draft: %v", err)
	}
	if _, err := submitWith(t, h, id, nil); err != nil {
		t.Fatalf("submit: %v", err)
	}
	j, _ := h.env.Store.Job(ctx, id)
	fi, err := os.Stat(filepath.Join(j.LocalDir, "labmill-recipe.json"))
	if j.LocalDir == "" || err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("recipe file: %q %v", j.LocalDir, err)
	}
	if _, err := h.run(t, "rerun_job", map[string]any{"job_id": id}); err == nil || !strings.Contains(err.Error(), "may still be running") {
		t.Fatalf("rerun while running: %v", err)
	}
	h.env.Store.UpdateJob(ctx, id, func(j *store.Job) error { j.State = store.StateCompleted; return nil })
	evs, _ := h.env.Store.Events(ctx, id, 0)
	saved := false
	for _, e := range evs {
		saved = saved || e.Kind == eventRecipe
	}
	if !saved {
		t.Fatal("no recipe event")
	}
	out, err := h.run(t, "job_recipe", map[string]any{"job_id": id})
	if err != nil || !strings.Contains(out, "module tool (no version pinned)") || !strings.Contains(out, "labmill-run.sh") || !strings.Contains(out, "v9.9") {
		t.Fatalf("job_recipe: %q %v", out, err)
	}
	out, err = h.run(t, "rerun_job", map[string]any{"job_id": id})
	if err != nil || !strings.Contains(out, "submit_job") {
		t.Fatalf("rerun: %q %v", out, err)
	}
	jobs, _ = h.env.Store.Jobs(ctx, store.JobFilter{})
	if len(jobs) != 2 {
		t.Fatalf("jobs: %d", len(jobs))
	}
	var nj store.Job
	for _, x := range jobs {
		if x.ID != id {
			nj = x
		}
	}
	if nj.Name != "rr-rerun" || nj.State != store.StateDraft || nj.Script != j.Script || nj.Cluster != j.Cluster {
		t.Errorf("new job: %+v", nj)
	}
	if _, err := h.run(t, "rerun_job", map[string]any{"job_id": id, "name": "bad name!"}); err == nil {
		t.Error("bad name accepted")
	}
	// The recipe never goes to the account.
	evs, _ = h.env.Store.Events(ctx, nj.ID, 0)
	if account.Private(evs) {
		t.Error("new job private without reason")
	}
}

func TestRecipeSoftwareNoFalseHits(t *testing.T) {
	got := recipeSoftware(backend.JobSpec{}, "module purge\nml purge\nml list\nmodule use /my/modules\n# module load hidden\necho apptainer exec x y\n"+
		"source env.sh && module load tool/1.0\nif true; then ml gcc/12; fi\napptainer exec --bind /a:/b --nv img.sif cmd\n")
	want := []string{"module tool/1.0", "module gcc/12", "container img.sif"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q want %q", got, want)
	}
}

// A second rerun gets its own name and folder; the recipe never reaches the account report.
func TestRecipeRerunTwiceAndAccount(t *testing.T) {
	h, _ := filesHarness(t)
	h.env.ResultsDir = t.TempDir()
	ctx := context.Background()
	if _, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone",
		"spec": map[string]any{"name": "tw", "work_dir": t.TempDir(), "command": "echo SECRETWORD"}}); err != nil {
		t.Fatal(err)
	}
	jobs, _ := h.env.Store.Jobs(ctx, store.JobFilter{})
	id := jobs[0].ID
	if _, err := submitWith(t, h, id, nil); err != nil {
		t.Fatal(err)
	}
	h.env.Store.AddEvent(ctx, id, account.EventPrivate, "")
	h.env.Store.UpdateJob(ctx, id, func(j *store.Job) error { j.State = store.StateFailed; return nil })
	for range 2 {
		if _, err := h.run(t, "rerun_job", map[string]any{"job_id": id}); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _ = h.env.Store.Jobs(ctx, store.JobFilter{})
	names := map[string]bool{}
	for _, j := range jobs {
		names[j.Name] = true
		if j.ID != id {
			evs, _ := h.env.Store.Events(ctx, j.ID, 0)
			if !account.Private(evs) {
				t.Errorf("job %s lost the private mark", j.Name)
			}
		}
	}
	if len(names) != 3 {
		t.Errorf("names: %v", names)
	}
	// A recipe event on a job is not part of its account record.
	evs, _ := h.env.Store.Events(ctx, id, 0)
	j, _ := h.env.Store.Job(ctx, id)
	in, _ := account.BuildReport(j, evs, "ref")
	if b, _ := json.Marshal(in); strings.Contains(string(b), "recipe") {
		t.Errorf("report carries the recipe: %s", b)
	}
}

// A job from before recipes gets a rebuilt recipe.
func TestRecipeRebuilt(t *testing.T) {
	h, _ := filesHarness(t)
	h.env.ResultsDir = t.TempDir()
	ctx := context.Background()
	if _, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone",
		"spec": map[string]any{"name": "old", "work_dir": t.TempDir(), "command": "echo hi"}}); err != nil {
		t.Fatal(err)
	}
	jobs, _ := h.env.Store.Jobs(ctx, store.JobFilter{})
	h.env.Store.UpdateJob(ctx, jobs[0].ID, func(j *store.Job) error { j.RemoteID, j.State = "77", store.StateFailed; return nil })
	out, err := h.run(t, "job_recipe", map[string]any{"job_id": jobs[0].ID})
	if err != nil || !strings.Contains(out, "Recipe of job") || strings.Contains(out, "submitted 0001") {
		t.Fatalf("%q %v", out, err)
	}
}
