package slurm

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

// fakeRunner answers by the command name (argv[0]) and records calls.
type fakeRunner struct {
	replies map[string]backend.Result // keyed by argv[0], or "argv0 argv1" for more specific matches
	calls   [][]string
	stdin   []string
}

func (f *fakeRunner) Run(_ context.Context, argv []string, stdin io.Reader) (backend.Result, error) {
	f.calls = append(f.calls, argv)
	// Like a shell: skip a leading `env VAR=value` prefix when matching.
	for len(argv) > 2 && argv[0] == "env" && strings.Contains(argv[1], "=") {
		argv = argv[2:]
	}
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	f.stdin = append(f.stdin, in)
	if len(argv) > 1 {
		if r, ok := f.replies[argv[0]+" "+argv[1]]; ok {
			return r, nil
		}
	}
	if r, ok := f.replies[argv[0]]; ok {
		return r, nil
	}
	return backend.Result{ExitCode: 127, Stderr: argv[0] + ": command not found"}, nil
}

const sinfoOut = `gpu*|up|2-00:00:00|gpu:a100:4|3|idle
gpu*|up|2-00:00:00|gpu:a100:4|2|mix
h100|up|1-00:00:00|gpu:h100:8(S:0-1)|1|alloc
cpu|up|infinite|(null)|10|idle
debug|down|30:00|gpu:2|1|idle
`

func TestInfo(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"sinfo": {Stdout: sinfoOut}}}
	info, err := (&Slurm{Runner: r}).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]backend.Partition{}
	for _, p := range info.Partitions {
		byName[p.Name] = p
	}
	gpu := byName["gpu"]
	if !gpu.Default || gpu.Nodes != 5 || gpu.IdleNodes != 3 || gpu.MaxTime != 48*time.Hour || strings.Join(gpu.GPUTypes, ",") != "a100" {
		t.Errorf("gpu = %+v", gpu)
	}
	if h := byName["h100"]; h.Default || strings.Join(h.GPUTypes, ",") != "h100" || h.MaxTime != 24*time.Hour {
		t.Errorf("h100 = %+v", h)
	}
	if c := byName["cpu"]; c.MaxTime != 0 || len(c.GPUTypes) != 0 || c.IdleNodes != 10 {
		t.Errorf("cpu = %+v", c)
	}
	if d := byName["debug"]; d.State != "down" || d.MaxTime != 30*time.Minute || len(d.GPUTypes) != 0 {
		t.Errorf("debug = %+v", d)
	}
	if len(info.Partitions) != 4 {
		t.Errorf("partitions = %d", len(info.Partitions))
	}
}

func TestInfoError(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"sinfo": {ExitCode: 1, Stderr: "slurm_load_partitions: Unable to contact slurm controller"}}}
	if _, err := (&Slurm{Runner: r}).Info(context.Background()); err == nil || !strings.Contains(err.Error(), "Unable to contact") {
		t.Fatalf("err = %v", err)
	}
}

func spec() backend.JobSpec {
	return backend.JobSpec{Name: "train", Partition: "gpu", GPUs: 2, GPUType: "a100", CPUs: 8, MemoryGB: 32,
		Time: 26*time.Hour + 30*time.Minute, Command: "python train.py --epochs 10", WorkDir: "/scratch/alice/run1",
		Env: map[string]string{"SEED": "42", "MSG": "it's ok"}}
}

func TestRender(t *testing.T) {
	script, err := (&Slurm{}).Render(spec())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#!/bin/bash\n", "#SBATCH --job-name=train\n", "#SBATCH --partition=gpu\n", "#SBATCH --gres=gpu:a100:2\n",
		"#SBATCH --cpus-per-task=8\n", "#SBATCH --mem=32G\n", "#SBATCH --time=1-02:30:00\n",
		"#SBATCH --chdir=/scratch/alice/run1\n", "#SBATCH --output=/scratch/alice/run1/%x-%j.out\n",
		"export MSG='it'\\''s ok'\n", "export SEED='42'\n", "python train.py --epochs 10",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	if strings.Index(script, "export MSG") > strings.Index(script, "export SEED") {
		t.Error("env not sorted")
	}
	s := spec()
	s.GPUType, s.Partition, s.CPUs, s.MemoryGB, s.Time = "", "", 0, 0, 0
	script, _ = (&Slurm{}).Render(s)
	if !strings.Contains(script, "#SBATCH --gres=gpu:2\n") || strings.Contains(script, "--partition") || strings.Contains(script, "--mem") || strings.Contains(script, "--time") {
		t.Errorf("minimal script:\n%s", script)
	}
	bad := spec()
	bad.Name = "x\n#SBATCH --uid=0"
	if _, err := (&Slurm{}).Render(bad); err == nil {
		t.Error("Render accepted an invalid spec")
	}
}

func TestSubmit(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"mkdir": {}, "sbatch": {Stdout: "12345;hpcone\n"}}}
	id, err := (&Slurm{Runner: r}).Submit(context.Background(), "#!/bin/bash\necho hi\n", spec())
	if err != nil || id != "12345" {
		t.Fatalf("id = %q, %v", id, err)
	}
	if strings.Join(r.calls[0], " ") != "mkdir -p -- /scratch/alice/run1" || strings.Join(r.calls[1], " ") != "sbatch --parsable" || r.stdin[1] != "#!/bin/bash\necho hi\n" {
		t.Errorf("calls = %v, stdin = %q", r.calls, r.stdin)
	}
	r = &fakeRunner{replies: map[string]backend.Result{"mkdir": {}, "sbatch": {ExitCode: 1, Stderr: "sbatch: error: Invalid partition name specified"}}}
	if _, err := (&Slurm{Runner: r}).Submit(context.Background(), "x", spec()); err == nil || !strings.Contains(err.Error(), "Invalid partition") {
		t.Fatalf("err = %v", err)
	}
	r = &fakeRunner{replies: map[string]backend.Result{"mkdir": {}, "sbatch": {Stdout: "Submitted batch job 7\n"}}}
	if _, err := (&Slurm{Runner: r}).Submit(context.Background(), "x", spec()); err == nil {
		t.Error("accepted non-parsable output")
	}
}

func TestStatus(t *testing.T) {
	cases := []struct {
		name    string
		replies map[string]backend.Result
		want    store.JobState
		reason  string
		exit    int
	}{
		{"pending", map[string]backend.Result{"squeue": {Stdout: "PENDING|Resources|\n"}}, store.StatePending, "Resources", 0},
		{"running", map[string]backend.Result{"squeue": {Stdout: "RUNNING|None|gpu-node-07\n"}}, store.StateRunning, "", 0},
		{"completed", map[string]backend.Result{"squeue": {}, "sacct": {Stdout: "COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T11:00:00|gpu-node-07\n"}}, store.StateCompleted, "", 0},
		{"oom", map[string]backend.Result{"squeue": {}, "sacct": {Stdout: "OUT_OF_MEMORY|0:125|2026-10-03T10:00:00|2026-10-03T10:05:00|n1\n"}}, store.StateFailed, "OUT_OF_MEMORY", 0},
		{"failed", map[string]backend.Result{"squeue": {}, "sacct": {Stdout: "FAILED|2:0|2026-10-03T10:00:00|2026-10-03T10:05:00|n1\n"}}, store.StateFailed, "FAILED", 2},
		{"cancelled", map[string]backend.Result{"squeue": {}, "sacct": {Stdout: "CANCELLED by 1234|0:15|2026-10-03T10:00:00|2026-10-03T10:05:00|n1\n"}}, store.StateCanceled, "", 0},
		{"timeout", map[string]backend.Result{"squeue": {}, "sacct": {Stdout: "TIMEOUT|0:0|2026-10-03T10:00:00|2026-10-04T10:00:00|n1\n"}}, store.StateFailed, "TIMEOUT", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{replies: tc.replies}
			st, err := (&Slurm{Runner: r}).Status(context.Background(), "12345")
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tc.want || st.ExitCode != tc.exit || (tc.reason != "" && st.Reason != tc.reason) || st.RemoteID != "12345" {
				t.Errorf("status = %+v", st)
			}
		})
	}
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {}, "sacct": {}}}
	if _, err := (&Slurm{Runner: r}).Status(context.Background(), "999"); err == nil {
		t.Error("unknown job: no error")
	}
}

func TestRemoteIDValidation(t *testing.T) {
	s := &Slurm{Runner: &fakeRunner{replies: map[string]backend.Result{"scancel": {}, "squeue": {Stdout: "RUNNING|None|n\n"}}}}
	for _, id := range []string{"", "abc", "1;rm -rf ~", "1 2", "-1", "12_x"} {
		if err := s.Cancel(context.Background(), id); err == nil {
			t.Errorf("Cancel(%q) accepted", id)
		}
		if _, err := s.Status(context.Background(), id); err == nil {
			t.Errorf("Status(%q) accepted", id)
		}
	}
	for _, id := range []string{"12345", "12345_7"} {
		if err := s.Cancel(context.Background(), id); err != nil {
			t.Errorf("Cancel(%q): %v", id, err)
		}
	}
}

func TestLogs(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{
		"scontrol": {Stdout: "JobId=12345 JobName=train\n   StdErr=/scratch/alice/run1/train-12345.out\n   StdOut=/scratch/alice/run1/train-12345.out\n"},
		"tail":     {Stdout: "epoch 9\nepoch 10\n"},
	}}
	out, err := (&Slurm{Runner: r}).Logs(context.Background(), "12345", 2)
	if err != nil || out != "epoch 9\nepoch 10\n" {
		t.Fatalf("logs = %q, %v", out, err)
	}
	last := r.calls[len(r.calls)-1]
	if strings.Join(last, " ") != "tail -n 2 -- /scratch/alice/run1/train-12345.out" {
		t.Errorf("tail call = %v", last)
	}
	// Finished job: scontrol fails, sacct gives WorkDir and JobName.
	r = &fakeRunner{replies: map[string]backend.Result{
		"scontrol": {ExitCode: 1, Stderr: "slurm_load_jobs error: Invalid job id specified"},
		"sacct":    {Stdout: "/scratch/alice/run1|train\n"},
		"tail":     {Stdout: "done\n"},
	}}
	if out, err := (&Slurm{Runner: r}).Logs(context.Background(), "12345", 0); err != nil || out != "done\n" {
		t.Fatalf("logs (finished) = %q, %v", out, err)
	}
	if last := r.calls[len(r.calls)-1]; strings.Join(last, " ") != "tail -n 100 -- /scratch/alice/run1/train-12345.out" {
		t.Errorf("tail call = %v", last)
	}
}
