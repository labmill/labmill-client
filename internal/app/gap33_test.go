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

func TestDraftDefaultWorkDir(t *testing.T) {
	h := gapHarness(t, &gapRunner{})
	in := draftSpec(nil, nil)
	delete(in["spec"].(map[string]any), "work_dir")
	out, err := h.run(t, "draft_job", in)
	if err != nil || !strings.Contains(out, "#SBATCH --chdir=/home/alice/labmill/seg\n") {
		t.Fatalf("draft: %v\n%s", err, out)
	}
}

// A rejected pre-flight check
// queues one problem report; a private job queues none.
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
}

// ask_user queues one "question" event, cut to 60 characters after redaction.
func TestQuestionIsCounted(t *testing.T) {
	h := gapHarness(t, &gapRunner{})
	link := filepath.Join(t.TempDir(), "account.json")
	if err := account.Save(link, account.Link{URL: "https://app.example.org", Token: "lmt_abcdefghijklmnopqrstuvwxyz012345", LinkedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h.env.Problem = (&account.Problems{Store: h.env.Store, LinkPath: link, Version: "1.0", OS: "linux"}).Record
	h.env.Choose = func(context.Context, agent.Choice) (agent.ChoiceAnswer, error) { return agent.ChoiceAnswer{}, nil }
	q := strings.Repeat("Which partition should the long running analysis use? ", 3)
	if _, err := h.run(t, "ask_user", map[string]any{"question": q, "options": []map[string]any{{"label": "a"}, {"label": "b"}}}); err != nil {
		t.Fatal(err)
	}
	evs, _ := h.env.Store.DueClientEvents(context.Background(), time.Now().Add(time.Hour), 50)
	if len(evs) != 1 || !strings.Contains(evs[0].Payload, `"kind":"question"`) || !strings.Contains(evs[0].Payload, `"message":"`+q[:60]+`"`) {
		t.Errorf("events = %+v", evs)
	}
}
