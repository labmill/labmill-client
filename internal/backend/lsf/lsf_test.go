package lsf

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
		Time: 90*time.Minute + time.Second, Command: "python train.py", WorkDir: "/scratch/alice/run1",
		Env: map[string]string{"MSG": "it's ok"}}
}

func TestRender(t *testing.T) {
	script, err := (&LSF{}).Render(spec())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#BSUB -J train\n", "#BSUB -q gpuq\n", "#BSUB -n 8\n", "#BSUB -R \"rusage[mem=32768]\"\n",
		"#BSUB -gpu \"num=2:gmodel=a100\"\n", "#BSUB -W 1:31\n", "#BSUB -cwd /scratch/alice/run1\n",
		"#BSUB -oo /scratch/alice/run1/train.out\n", "export MSG='it'\\''s ok'\n", "python train.py\n"} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %q in:\n%s", want, script)
		}
	}
	for name, mut := range map[string]func(*backend.JobSpec){
		"nodes": func(s *backend.JobSpec) { s.Nodes = 2 },
		"qos":   func(s *backend.JobSpec) { s.QOS = "low" },
		"dir":   func(s *backend.JobSpec) { s.Command = "x\n #BSUB -q evil" },
	} {
		s := spec()
		mut(&s)
		if _, err := (&LSF{}).Render(s); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestInfo(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{
		"bqueues -w": {Stdout: "QUEUE_NAME PRIO STATUS MAX JL/U JL/P JL/H NJOBS PEND RUN SUSP\nnormal 30 Open:Active - - - - 5 1 4 0\nold 10 Closed:Inact - - - - 0 0 0 0\n"},
		"bqueues -d": {Stdout: "QUEUE_NAME PRIO STATUS MAX\nnormal 30 Open:Active -\n"},
	}}
	info, err := (&LSF{Runner: r}).Info(context.Background())
	if err != nil || len(info.Partitions) != 2 {
		t.Fatalf("%v %+v", err, info)
	}
	if q := info.Partitions[0]; q.Name != "normal" || !q.Default || q.State != "up" {
		t.Errorf("normal = %+v", q)
	}
	if q := info.Partitions[1]; q.Default || q.State != "down" {
		t.Errorf("old = %+v", q)
	}
}

func TestSubmit(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"bsub": {Stdout: "Job <4711> is submitted to queue <normal>.\n"}}}
	id, err := (&LSF{Runner: r}).Submit(context.Background(), "#!/bin/bash\nid\n", spec())
	if err != nil || id != "4711" || !strings.Contains(r.stdin, "id") {
		t.Fatalf("%q %v", id, err)
	}
	r = &fakeRunner{replies: map[string]backend.Result{"bsub": {Stdout: "weird"}}}
	if _, err := (&LSF{Runner: r}).Submit(context.Background(), "x", spec()); err == nil || !strings.Contains(err.Error(), "may or may not") {
		t.Errorf("err = %v", err)
	}
	r = &fakeRunner{replies: map[string]backend.Result{"bsub": {ExitCode: 255, Stderr: "No such queue"}}}
	if _, err := (&LSF{Runner: r}).Submit(context.Background(), "x", spec()); err == nil || strings.Contains(err.Error(), "may or may not") {
		t.Errorf("definite refusal must not be uncertain: %v", err)
	}
}

func TestStatus(t *testing.T) {
	q := func(out string) *LSF {
		return &LSF{Runner: &fakeRunner{replies: map[string]backend.Result{"bjobs": {Stdout: out}}}}
	}
	ctx := context.Background()
	st, err := q("PEND|-|-|-|-|-|-\n").Status(ctx, "7")
	if err != nil || st.State != store.StatePending {
		t.Errorf("pend %+v %v", st, err)
	}
	st, _ = q("RUN|-|8*n01:8*n02|Nov  2 10:00:00 2026|-|-|/s/a.out\n").Status(ctx, "7")
	if st.State != store.StateRunning || st.Node != "n01" || st.StartTime.Day() != 2 || !st.EndTime.IsZero() {
		t.Errorf("run %+v", st)
	}
	cases := map[string]store.JobState{
		"DONE|0|n01|Nov  2 10:00:00 2026|Nov  2 11:00:00 2026|-|-":                                 store.StateCompleted,
		"EXIT|3|n01|Nov  2 10:00:00 2026|Nov  2 11:00:00 2026|TERM_MEMLIMIT: job killed|-":         store.StateFailed,
		"EXIT|130|n01|Nov  2 10:00:00 2026|Nov  2 11:00:00 2026|TERM_OWNER: job killed by owner|-": store.StateCanceled,
	}
	for row, want := range cases {
		st, _ = q(row+"\n").Status(ctx, "7")
		if st.State != want || st.EndTime.IsZero() {
			t.Errorf("%s: %+v", row, st)
		}
	}
	if _, err := q("").Status(ctx, "7"); err == nil {
		t.Error("unknown job must error")
	}
	if _, err := q("").Status(ctx, "7; rm"); err == nil {
		t.Error("bad id must error")
	}
}

func TestLogsAndCancel(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{
		"bjobs": {Stdout: "RUN|-|n01|-|-|-|/scratch/a/train.out\n"},
		"tail":  {Stdout: "hello\n"},
	}}
	out, err := (&LSF{Runner: r}).Logs(context.Background(), "5", 10)
	if err != nil || out != "hello\n" {
		t.Fatalf("%q %v", out, err)
	}
	if got := strings.Join(r.calls[len(r.calls)-1], " "); got != "tail -n 10 -- /scratch/a/train.out" {
		t.Errorf("tail = %s", got)
	}
	if err := (&LSF{Runner: &fakeRunner{replies: map[string]backend.Result{"bkill": {}}}}).Cancel(context.Background(), "5"); err != nil {
		t.Error(err)
	}
}

var _ backend.Backend = (*LSF)(nil)
