package pbs

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

type fakeRunner struct {
	replies map[string]backend.Result // keyed by argv[0], or "argv0 argv1"
	calls   [][]string
	stdin   string
}

func (f *fakeRunner) Run(_ context.Context, argv []string, stdin io.Reader) (backend.Result, error) {
	f.calls = append(f.calls, argv)
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		f.stdin = string(b)
	}
	if len(argv) > 1 {
		if r, ok := f.replies[argv[0]+" "+argv[1]]; ok {
			return r, nil
		}
	}
	return f.replies[argv[0]], nil
}

func spec() backend.JobSpec {
	return backend.JobSpec{Name: "train", Partition: "gpuq", GPUs: 2, GPUType: "a100", CPUs: 8, MemoryGB: 32,
		Time: 26*time.Hour + 30*time.Minute, Command: "python train.py", WorkDir: "/scratch/alice/run1",
		Env: map[string]string{"MSG": "it's ok"}}
}

func TestRender(t *testing.T) {
	script, err := (&PBS{}).Render(spec())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#PBS -N train\n", "#PBS -q gpuq\n", "#PBS -l select=1:ncpus=8:mem=32gb:ngpus=2:gpu_type=a100\n",
		"#PBS -l walltime=26:30:00\n", "#PBS -o /scratch/alice/run1/train.out\n", "cd /scratch/alice/run1 || exit 1\n",
		"export MSG='it'\\''s ok'\n", "python train.py\n"} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %q in:\n%s", want, script)
		}
	}
	for name, mut := range map[string]func(*backend.JobSpec){
		"qos":    func(s *backend.JobSpec) { s.QOS = "low" },
		"pbsdir": func(s *backend.JobSpec) { s.Command = "x\n #PBS -l foo" },
		"empty":  func(s *backend.JobSpec) { s.Command = "" },
	} {
		s := spec()
		mut(&s)
		if _, err := (&PBS{}).Render(s); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestInfo(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{
		"qstat -Q": {Stdout: "Queue: workq\n    queue_type = Execution\n    enabled = True\n    started = True\n    resources_max.walltime = 48:00:00\n\nQueue: old\n    enabled = False\n    started = True\n"},
		"qstat -B": {Stdout: "Server: head\n    default_queue = workq\n"},
	}}
	info, err := (&PBS{Runner: r}).Info(context.Background())
	if err != nil || len(info.Partitions) != 2 {
		t.Fatalf("%v %+v", err, info)
	}
	if q := info.Partitions[0]; q.Name != "workq" || !q.Default || q.State != "up" || q.MaxTime != 48*time.Hour {
		t.Errorf("workq = %+v", q)
	}
	if q := info.Partitions[1]; q.Default || q.State != "down" {
		t.Errorf("old = %+v", q)
	}
}

func TestSubmit(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"qsub": {Stdout: "123.head\n"}}}
	id, err := (&PBS{Runner: r}).Submit(context.Background(), "#!/bin/bash\nid\n", spec())
	if err != nil || id != "123.head" || !strings.Contains(r.stdin, "id") {
		t.Fatalf("%q %v", id, err)
	}
	r = &fakeRunner{replies: map[string]backend.Result{"qsub": {Stdout: "weird"}}}
	if _, err := (&PBS{Runner: r}).Submit(context.Background(), "x", spec()); err == nil || !strings.Contains(err.Error(), "may or may not") {
		t.Errorf("err = %v", err)
	}
	r = &fakeRunner{replies: map[string]backend.Result{"qsub": {ExitCode: 1, Stderr: "qsub: Unknown queue"}}}
	if _, err := (&PBS{Runner: r}).Submit(context.Background(), "x", spec()); err == nil || strings.Contains(err.Error(), "may or may not") {
		t.Errorf("definite refusal must not be uncertain: %v", err)
	}
}

func TestStatus(t *testing.T) {
	q := func(out string) *PBS {
		return &PBS{Runner: &fakeRunner{replies: map[string]backend.Result{"qstat": {Stdout: out}}}}
	}
	st, err := q("Job Id: 1.h\n    job_state = Q\n    comment = Not Running: Insufficient resources\n").Status(context.Background(), "1.h")
	if err != nil || st.State != store.StatePending || !strings.Contains(st.Reason, "Insufficient") {
		t.Errorf("queued %+v %v", st, err)
	}
	st, _ = q("    job_state = R\n    exec_host = n01/0*8\n    stime = Mon Nov  2 10:00:00 2026\n").Status(context.Background(), "1.h")
	if st.State != store.StateRunning || st.Node != "n01" || st.StartTime.Day() != 2 {
		t.Errorf("running %+v", st)
	}
	done := "    job_state = F\n    Exit_status = %s\n    mtime = Tue Nov 10 11:00:00 2026\n"
	for code, want := range map[string]store.JobState{"0": store.StateCompleted, "271": store.StateCanceled, "1": store.StateFailed} {
		st, _ = q(strings.Replace(done, "%s", code, 1)).Status(context.Background(), "1.h")
		if st.State != want || st.EndTime.IsZero() {
			t.Errorf("exit %s: %+v", code, st)
		}
	}
	if _, err := q("").Status(context.Background(), "1.h"); err == nil {
		t.Error("unknown job must error")
	}
	if _, err := q("").Status(context.Background(), "1; rm"); err == nil {
		t.Error("bad id must error")
	}
}

func TestLogsAndCancel(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{
		"qstat": {Stdout: "    job_state = R\n    Output_Path = head:/scratch/a/very/long/\n\tpath/train.out\n"},
		"tail":  {Stdout: "hello\n"},
	}}
	out, err := (&PBS{Runner: r}).Logs(context.Background(), "5", 10)
	if err != nil || out != "hello\n" {
		t.Fatalf("%q %v", out, err)
	}
	if got := strings.Join(r.calls[len(r.calls)-1], " "); got != "tail -n 10 -- /scratch/a/very/long/path/train.out" {
		t.Errorf("tail = %s", got)
	}
	if err := (&PBS{Runner: &fakeRunner{replies: map[string]backend.Result{"qdel": {}}}}).Cancel(context.Background(), "5.h"); err != nil {
		t.Error(err)
	}
}

var _ backend.Backend = (*PBS)(nil)
