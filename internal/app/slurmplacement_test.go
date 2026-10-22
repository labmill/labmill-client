package app

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/backend/slurm"
	"labmill-client/internal/config"
	"labmill-client/internal/rules"
	"labmill-client/internal/store"
)

// placeRunner is a Slurm login node for the M61 tests: sinfo, a
// `sbatch --test-only` estimate (or none) and sbatch itself.
type placeRunner struct {
	calls      [][]string
	noEstimate bool
	noDefault  bool   // sinfo shows no default partition
	rejectMsg  string // sbatch --test-only fails with this message
	reject     bool   // sbatch --test-only fails with a Slurm error
}

func (r *placeRunner) Run(_ context.Context, argv []string, _ io.Reader) (backend.Result, error) {
	r.calls = append(r.calls, argv)
	switch {
	case argv[0] == "sinfo":
		if r.noDefault {
			return backend.Result{Stdout: "preempt|up|2:00:00|(null)|10|idle\ncompute|up|7-00:00:00|(null)|10|idle\n"}, nil
		}
		return backend.Result{Stdout: "preempt|up|2:00:00|(null)|10|idle\ncompute*|up|7-00:00:00|(null)|10|idle\n"}, nil
	case argv[0] == "sbatch" && len(argv) > 1 && argv[1] == "--test-only":
		if r.rejectMsg != "" {
			return backend.Result{ExitCode: 1, Stderr: r.rejectMsg}, nil
		}
		if r.reject {
			return backend.Result{ExitCode: 1, Stderr: "sbatch: error: Invalid account\n"}, nil
		}
		if r.noEstimate {
			return backend.Result{ExitCode: 1, Stderr: "sbatch: error: Batch job submission failed"}, nil
		}
		return backend.Result{Stderr: "sbatch: Job 99 to start at 2026-10-15T14:20:00 using 4 processors on nodes n01 in partition preempt\n"}, nil
	case argv[0] == "sbatch":
		return backend.Result{Stdout: "4242\n"}, nil
	}
	return backend.Result{}, nil
}

var (
	testPreempt = &rules.Preemptible{Partition: "preempt", QOS: "low", Requeue: true, FallbackPartition: "compute"}
	testLocal   = &rules.LocalScratch{Path: "/tmp", EnvVar: "TMPDIR", SizeGB: 400}
	testGlobal  = &rules.GlobalScratch{Path: "/scratch/<group>", PurgeDays: 90}
)

// slurmHarness is a harness whose hpcone cluster is a real Slurm backend
// over a placeRunner and has the given placement facts.
func slurmHarness(t *testing.T, hints rules.SlurmHints) (*harness, *placeRunner) {
	t.Helper()
	h := newHarness(t)
	rr := &placeRunner{}
	h.env.Backend = func(_ context.Context, c config.Cluster) (backend.Backend, error) {
		return &slurm.Slurm{Runner: rr}, nil
	}
	h.env.Site = SiteInfo{Clusters: map[string]ClusterHints{"hpcone": {Slurm: hints}}}
	h.tools = map[string]agent.Tool{}
	for _, tool := range Tools(h.env) {
		h.tools[tool.Name] = tool
	}
	return h, rr
}

func draftSpec(extra map[string]any, spec map[string]any) map[string]any {
	s := map[string]any{"name": "seg", "command": "python train.py", "work_dir": "/storage/me/labmill/seg"}
	for k, v := range spec {
		s[k] = v
	}
	in := map[string]any{"cluster": "hpcone", "spec": s}
	for k, v := range extra {
		in[k] = v
	}
	return in
}

func jobPlanOf(t *testing.T, h *harness) (store.Job, placementPlan) {
	t.Helper()
	jobs, _ := h.env.Store.Jobs(context.Background(), store.JobFilter{})
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v", jobs)
	}
	evs, _ := h.env.Store.Events(context.Background(), jobs[0].ID, 0)
	var plan placementPlan
	found := false
	for _, e := range evs {
		if e.Kind == eventPlan {
			found = json.Unmarshal([]byte(e.Text), &plan) == nil
		}
	}
	if !found {
		t.Fatalf("no plan event: %+v", evs)
	}
	return jobs[0], plan
}

func TestDraftScratchLocal(t *testing.T) {
	h, rr := slurmHarness(t, rules.SlurmHints{LocalScratch: testLocal})
	out, err := h.run(t, "draft_job", draftSpec(map[string]any{"scratch": "local"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#SBATCH --nodes=1",
		`LABMILL_WORK="${TMPDIR:-/tmp}/labmill_${SLURM_JOB_ID}"`,
		`mkdir -p "$LABMILL_WORK/job"`,
		`if rsync -a --update --exclude="/$LABMILL_LOG" "$LABMILL_WORK/job/" "$LABMILL_JOB/"; then rm -rf -- "$LABMILL_WORK"; else`,
		"python train.py", `wait "$LABMILL_PID"`, "#SBATCH --signal=B:USR1@120",
		"Storage: node-local scratch $TMPDIR (else /tmp)", "removes its local folder only after a good copy",
		"Estimated start: 2026-10-15T14:20:00 (cluster time), partition preempt, nodes n01",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("draft output lacks %q:\n%s", want, out)
		}
	}
	job, plan := jobPlanOf(t, h)
	if plan.Scratch != scratchLocal || plan.Preemptible || plan.Estimate == "" {
		t.Errorf("plan = %+v", plan)
	}
	// The estimate ran sbatch --test-only and submitted nothing.
	for _, c := range rr.calls {
		if c[0] == "sbatch" && (len(c) < 2 || c[1] != "--test-only") {
			t.Errorf("a draft submitted something: %v", c)
		}
	}
	// The review the user approves carries the plan.
	p, err := h.tools["submit_job"].Propose(mustJSON(map[string]any{"job_id": job.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Summary, "Storage: node-local scratch") || !strings.Contains(p.Summary, "Estimated start: 2026-10-15T14:20:00") {
		t.Errorf("summary = %q", p.Summary)
	}
}

func TestDraftScratchLocalRefusals(t *testing.T) {
	tests := []struct {
		name  string
		hints rules.SlurmHints
		spec  map[string]any
		want  string
	}{
		{"multi node", rules.SlurmHints{LocalScratch: testLocal}, map[string]any{"nodes": 2}, "single-node"},
		{"no local scratch known", rules.SlurmHints{GlobalScratch: testGlobal}, nil, "No node-local scratch is known"},
		{"nothing known", rules.SlurmHints{}, nil, "No node-local scratch is known"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := slurmHarness(t, tc.hints)
			_, err := h.run(t, "draft_job", draftSpec(map[string]any{"scratch": "local"}, tc.spec))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
			if jobs, _ := h.env.Store.Jobs(context.Background(), store.JobFilter{}); len(jobs) != 0 {
				t.Errorf("a refused draft created a job: %+v", jobs)
			}
		})
	}
}

func TestDraftScratchGlobal(t *testing.T) {
	h, _ := slurmHarness(t, rules.SlurmHints{GlobalScratch: testGlobal})
	out, err := h.run(t, "draft_job", draftSpec(map[string]any{"scratch": "global"}, map[string]any{"work_dir": "/scratch/lab/labmill/seg"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "trap ") || !strings.Contains(out, "#SBATCH --chdir=/scratch/lab/labmill/seg") || !strings.Contains(out, "Storage: the job runs in its folder on the shared scratch /scratch/<group>; nothing is removed by the job.") || !strings.Contains(out, "purged after 90 days") {
		t.Errorf("output:\n%s", out)
	}
	// A work folder outside the scratch is refused with what to do.
	_, err = h.run(t, "draft_job", draftSpec(map[string]any{"scratch": "global"}, map[string]any{"name": "other", "work_dir": "/storage/me/other"}))
	if err == nil || !strings.Contains(err.Error(), "not under the shared scratch /scratch/<group>") {
		t.Errorf("err = %v", err)
	}
}

// A job without data on the shared scratch needs a work_dir of the user's
// own: Labmill does not invent a folder shared with other users.
func TestDraftScratchGlobalNeedsWorkDir(t *testing.T) {
	for _, g := range []*rules.GlobalScratch{{Path: "/storage/scratch"}, testGlobal} {
		h, _ := slurmHarness(t, rules.SlurmHints{GlobalScratch: g})
		in := draftSpec(map[string]any{"scratch": "global"}, nil)
		delete(in["spec"].(map[string]any), "work_dir")
		if _, err := h.run(t, "draft_job", in); err == nil || !strings.Contains(err.Error(), "needs a work_dir") {
			t.Errorf("%s: err = %v", g.Path, err)
		}
	}
}

func TestDraftPreemptible(t *testing.T) {
	h, _ := slurmHarness(t, rules.SlurmHints{Preemptible: testPreempt})
	out, err := h.run(t, "draft_job", draftSpec(nil, map[string]any{"time_minutes": 60}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#SBATCH --partition=preempt\n", "#SBATCH --qos=low\n", "#SBATCH --requeue\n",
		"Partition: preempt (preemptible, qos low, --requeue).",
		"resubmits it up to 2 more times, then once on compute; your approval of this job covers those resubmissions.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	job, plan := jobPlanOf(t, h)
	if !plan.Preemptible || plan.FallbackPartition != "compute" || plan.MaxResubmits != 2 || plan.Partition != "preempt" || plan.QOS != "low" || !plan.Requeue {
		t.Errorf("plan = %+v", plan)
	}
	// The spec event carries what M61.3 needs to resubmit.
	var spec backend.JobSpec
	text, _, _ := (&toolset{env: h.env}).latestEvent(context.Background(), job.ID, eventSpec)
	if err := json.Unmarshal([]byte(text), &spec); err != nil || spec.Partition != "preempt" || spec.QOS != "low" || !spec.Requeue {
		t.Errorf("spec = %+v (%v)", spec, err)
	}
	p, err := h.tools["submit_job"].Propose(mustJSON(map[string]any{"job_id": job.ID}))
	if err != nil || !strings.Contains(p.Summary, "then once on compute") {
		t.Errorf("summary = %q (%v)", p.Summary, err)
	}
}

func TestDraftPreemptibleDefaultFallback(t *testing.T) {
	h, _ := slurmHarness(t, rules.SlurmHints{Preemptible: &rules.Preemptible{Partition: "preempt"}})
	out, err := h.run(t, "draft_job", draftSpec(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "then once on the cluster's default partition") || strings.Contains(out, "--requeue") || strings.Contains(out, "--qos") {
		t.Errorf("output:\n%s", out)
	}
}

func TestDraftPreemptibleChoices(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name        string
		hints       rules.SlurmHints
		preemptible *bool
		spec        map[string]any
		wantScript  []string
		notScript   []string
		wantErr     string
	}{
		{"false", rules.SlurmHints{Preemptible: testPreempt}, &no, nil, nil, []string{"preempt", "--requeue", "--qos"}, ""},
		{"too long for the partition", rules.SlurmHints{Preemptible: testPreempt}, nil, map[string]any{"time_minutes": 600}, nil, []string{"--partition=preempt", "--requeue"}, ""},
		{"true but too long", rules.SlurmHints{Preemptible: testPreempt}, &yes, map[string]any{"time_minutes": 600}, nil, nil, "longer than the preemptible partition"},
		{"true without a profile", rules.SlurmHints{}, &yes, nil, nil, nil, "No preemptible resource is known"},
		{"no profile, default", rules.SlurmHints{}, nil, nil, nil, []string{"--partition", "--requeue", "--qos"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := slurmHarness(t, tc.hints)
			extra := map[string]any{}
			if tc.preemptible != nil {
				extra["preemptible"] = *tc.preemptible
			}
			out, err := h.run(t, "draft_job", draftSpec(extra, tc.spec))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			script, _, _ := strings.Cut(out, "Partition:")
			for _, w := range tc.wantScript {
				if !strings.Contains(script, w) {
					t.Errorf("script lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tc.notScript {
				if strings.Contains(script, w) {
					t.Errorf("script has %q:\n%s", w, out)
				}
			}
			if _, plan := jobPlanOf(t, h); plan.Preemptible {
				t.Errorf("plan is preemptible: %+v", plan)
			}
		})
	}
}

// A failed estimate is not an error: the draft works and says so.
func TestDraftEstimateUnavailable(t *testing.T) {
	h, rr := slurmHarness(t, rules.SlurmHints{})
	rr.noEstimate = true
	out, err := h.run(t, "draft_job", draftSpec(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Estimated start: not available.") {
		t.Errorf("output:\n%s", out)
	}
}

// Existing inputs (no scratch, no preemptible) keep working on a cluster
// without any placement facts: the script is the plain one.
func TestDraftBackwardCompatible(t *testing.T) {
	h, _ := slurmHarness(t, rules.SlurmHints{})
	out, err := h.run(t, "draft_job", draftSpec(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := h.env.Store.Jobs(context.Background(), store.JobFilter{})
	want := "#!/bin/bash\n#SBATCH --job-name=seg\n#SBATCH --chdir=/storage/me/labmill/seg\n#SBATCH --output=/storage/me/labmill/seg/%x-%j.out\npython train.py\n"
	if len(jobs) != 1 || jobs[0].Script != want {
		t.Errorf("script = %q", jobs[0].Script)
	}
	if !strings.Contains(out, "Storage: the job runs in its own folder on shared storage.") {
		t.Errorf("output:\n%s", out)
	}
	if _, err := h.run(t, "draft_job", draftSpec(map[string]any{"scratch": "somewhere"}, map[string]any{"name": "x"})); err == nil || !strings.Contains(err.Error(), "scratch must be") {
		t.Errorf("bad scratch: %v", err)
	}
}

func TestClusterInfoPlacementNote(t *testing.T) {
	h, _ := slurmHarness(t, rules.SlurmHints{Preemptible: testPreempt, LocalScratch: testLocal, GlobalScratch: testGlobal})
	out, err := h.run(t, "cluster_info", map[string]any{"cluster": "hpcone"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Preemptible: partition preempt, qos low; submitted with --requeue.",
		"then once on compute",
		"Node-local scratch: $TMPDIR (else /tmp), about 400 GB per node.",
		"Shared scratch: /scratch/<group>, files purged after 90 days.",
		"smallest node that fits",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cluster_info lacks %q:\n%s", want, out)
		}
	}
	h, _ = slurmHarness(t, rules.SlurmHints{})
	if out, _ := h.run(t, "cluster_info", map[string]any{"cluster": "hpcone"}); strings.Contains(out, "Placement rules") {
		t.Errorf("cluster_info without facts mentions rules:\n%s", out)
	}
}

// The partition the model names becomes the fallback; the preemptible one is
// tried first. The profile's fallback is used only when none was named.
func TestDraftPartitionBecomesFallback(t *testing.T) {
	tests := []struct {
		name     string
		hints    rules.Preemptible
		spec     map[string]any
		wantPart string
		wantFall string
	}{
		{"named partition wins over the profile's", *testPreempt, map[string]any{"partition": "gpu"}, "preempt", "gpu"},
		{"profile fallback when none named", *testPreempt, nil, "preempt", "compute"},
		{"named equals the preemptible one", *testPreempt, map[string]any{"partition": "preempt"}, "preempt", "compute"},
		{"qos only keeps the partition", rules.Preemptible{QOS: "lowprio", Requeue: true}, map[string]any{"partition": "gpu"}, "gpu", "gpu"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := slurmHarness(t, rules.SlurmHints{Preemptible: &tc.hints})
			if _, err := h.run(t, "draft_job", draftSpec(nil, tc.spec)); err != nil {
				t.Fatal(err)
			}
			_, plan := jobPlanOf(t, h)
			if !plan.Preemptible || plan.Partition != tc.wantPart || plan.FallbackPartition != tc.wantFall || plan.FallbackNeedsPartition {
				t.Errorf("plan = %+v", plan)
			}
		})
	}
}

// Without any fallback and without a default partition the plan says that
// the last step needs a partition, instead of promising the default one.
func TestDraftFallbackNeedsPartition(t *testing.T) {
	h, rr := slurmHarness(t, rules.SlurmHints{Preemptible: &rules.Preemptible{Partition: "preempt"}})
	rr.noDefault = true
	out, err := h.run(t, "draft_job", draftSpec(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	_, plan := jobPlanOf(t, h)
	if !plan.FallbackNeedsPartition || !strings.Contains(out, "this cluster has no default partition and none was named") || strings.Contains(out, "the cluster's default partition") {
		t.Errorf("plan = %+v\n%s", plan, out)
	}
}

// The plan text keeps its sentences: no "?" from flattened newlines, no
// doubled full stops, in the draft's answer and the approval summary.
func TestPlanTextExact(t *testing.T) {
	p := placementPlan{
		Scratch: scratchLocal, ScratchPath: "/tmp", ScratchWhy: "Why: fast.",
		Preemptible: true, Partition: "preempt", QOS: "lowprio", Requeue: true, FallbackPartition: "compute", MaxResubmits: 2,
		PartitionWhy: "Why: first.", Estimate: "2026-10-15T14:20:00 (cluster time)",
	}
	wantLines := []string{
		"Storage: node-local scratch /tmp (single-node job). The job copies its folder there, runs on the fast local disk, copies the results back to its folder and removes its local folder only after a good copy. Why: fast.",
		"Partition: preempt (preemptible, qos lowprio, --requeue). Why: first. If it is preempted or a node fails, Labmill resubmits it up to 2 more times, then once on compute; your approval of this job covers those resubmissions.",
		"Estimated start: 2026-10-15T14:20:00 (cluster time) (the scheduler's estimate, it can change).",
	}
	if got := p.answer(); got != strings.Join(wantLines, "\n") {
		t.Errorf("answer = %q", got)
	}
	want := strings.Join(wantLines, " ")
	want = strings.TrimSuffix(want, ".")
	if got := p.summary(); got != want || strings.Contains(got, "?") || strings.Contains(got, "..") {
		t.Errorf("summary = %q", got)
	}
	j := store.Job{Name: "seg", Cluster: "hpcone", Backend: "slurm", Script: "#!/bin/bash\n"}
	if sum := submitProposal(j, nil, "/storage/x", p.summary()).Summary; strings.Contains(sum, "..") || strings.Contains(sum, "?") || !strings.HasPrefix(sum, "Submit job seg to hpcone. Storage: node-local scratch /tmp") {
		t.Errorf("summary = %q", sum)
	}
}
