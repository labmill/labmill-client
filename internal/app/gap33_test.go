package app

import (
	"fmt"
	"path/filepath"
	"time"

	"context"
	"io"
	"labmill-client/internal/account"
	"labmill-client/internal/store"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/backend/slurm"
	"labmill-client/internal/config"
)

// gapRunner is a Slurm login node whose sacctmgr, scontrol and sinfo answers each test sets.
type gapRunner struct {
	sinfo, assoc, scontrol string
}

func (r *gapRunner) Run(_ context.Context, argv []string, _ io.Reader) (backend.Result, error) {
	switch {
	case argv[0] == "sinfo":
		return backend.Result{Stdout: r.sinfo}, nil
	case argv[0] == "scontrol":
		return backend.Result{Stdout: r.scontrol}, nil
	case argv[0] == "sh" && len(argv) > 2 && strings.Contains(argv[2], "sacctmgr"):
		return backend.Result{Stdout: r.assoc}, nil
	case argv[0] == "sbatch":
		return backend.Result{ExitCode: 1, Stderr: "sbatch: error: invalid account"}, nil
	case strings.Contains(strings.Join(argv, " "), "mkdir"):
		return backend.Result{ExitCode: 1, Stderr: "mkdir: cannot create directory"}, nil
	}
	return backend.Result{}, nil
}

func gapHarness(t *testing.T, r *gapRunner) *harness {
	t.Helper()
	if r.sinfo == "" {
		r.sinfo = "std*|up|2-00:00:00|(null)|10|idle|32|128000\n"
	}
	h := newHarness(t)
	h.env.Backend = func(context.Context, config.Cluster) (backend.Backend, error) {
		return &slurm.Slurm{Runner: r}, nil
	}
	h.tools = map[string]agent.Tool{}
	for _, tool := range Tools(h.env) {
		h.tools[tool.Name] = tool
	}
	return h
}

func TestDraftAccount(t *testing.T) {
	h := gapHarness(t, &gapRunner{assoc: "lab1|lab1|normal,low|normal\nlab1|lab1|normal,low|normal\n"})
	out, err := h.run(t, "draft_job", draftSpec(nil, nil))
	if err != nil || !strings.Contains(out, "#SBATCH --account=lab1\n") {
		t.Fatalf("one account: %v\n%s", err, out)
	}
	h = gapHarness(t, &gapRunner{assoc: "lab1|lab2|normal|normal\nlab2|lab2|normal,low|low\n"})
	out, err = h.run(t, "draft_job", draftSpec(nil, nil))
	if err != nil || strings.Contains(out, "--account") {
		t.Fatalf("two accounts: %v\n%s", err, out)
	}
	info, err := h.run(t, "cluster_info", map[string]any{"cluster": "hpcone"})
	if err != nil || !strings.Contains(info, "account lab1") || !strings.Contains(info, "account lab2 (default), QOS: normal, low") {
		t.Errorf("cluster_info: %v\n%s", err, info)
	}
}

func TestDraftPartitionDefaults(t *testing.T) {
	h := gapHarness(t, &gapRunner{scontrol: "PartitionName=std AllowAccounts=lab1,lab2 DefaultTime=NONE MaxTime=2-00:00:00 DefMemPerCPU=4000\n"})
	out, err := h.run(t, "draft_job", draftSpec(nil, nil))
	if err != nil || !strings.Contains(out, "time limit: 48h0m0s (partition default)") || !strings.Contains(out, "memory: 4000M per CPU") {
		t.Fatalf("draft: %v\n%s", err, out)
	}
	info, _ := h.run(t, "cluster_info", map[string]any{"cluster": "hpcone"})
	if !strings.Contains(info, "32 cores and 125 GB per node") || !strings.Contains(info, "default memory 4000M per CPU") || !strings.Contains(info, "only accounts: lab1, lab2") {
		t.Errorf("cluster_info:\n%s", info)
	}
}

func TestDraftDefaultWorkDir(t *testing.T) {
	h := gapHarness(t, &gapRunner{})
	in := draftSpec(nil, nil)
	delete(in["spec"].(map[string]any), "work_dir")
	out, err := h.run(t, "draft_job", in)
	if err != nil || !strings.Contains(out, "#SBATCH --chdir=/home/alice/labmill/seg\n") {
		t.Fatalf("draft: %v\n%s", err, out)
	}
}

// A rejected pre-flight check and a failed non-read-only remote command each
// queue one problem report; a private job queues none.
func TestFailuresAreReported(t *testing.T) {
	r := &gapRunner{}
	h := gapHarness(t, r)
	h.env.LoginRunner = func(context.Context, config.Cluster) (backend.Runner, error) { return r, nil }
	link := filepath.Join(t.TempDir(), "account.json")
	if err := account.Save(link, account.Link{URL: "https://app.example.org", Token: "lmt_abcdefghijklmnopqrstuvwxyz012345", LinkedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pr := &account.Problems{Store: h.env.Store, LinkPath: link, Version: "1.0", OS: "linux"}
	h.env.Problem = pr.Record
	queued := func() int {
		evs, _ := h.env.Store.DueClientEvents(context.Background(), time.Now().Add(time.Hour), 50)
		return len(evs)
	}
	for _, private := range []bool{true, false} {
		before := queued()
		if _, err := h.run(t, "draft_job", draftSpec(map[string]any{"private": private}, map[string]any{"name": fmt.Sprint("seg", private)})); err != nil {
			t.Fatal(err)
		}
		jobs, _ := h.env.Store.Jobs(context.Background(), store.JobFilter{})
		in := mustJSON(map[string]any{"job_id": jobs[0].ID})
		_, _ = h.tools["submit_job"].Propose(in)
		want := 1
		if private {
			want = 0
		}
		if got := queued() - before; got != want {
			t.Errorf("private %v: %d problems after the rejected check, want %d", private, got, want)
		}
	}
	before := queued()
	if _, err := h.run(t, "remote_command", map[string]any{"cluster": "hpcone", "argv": []string{"mkdir", "out"}}); err != nil {
		t.Fatal(err)
	}
	if got := queued() - before; got != 1 {
		t.Errorf("failed remote command: %d problems, want 1", got)
	}
}
