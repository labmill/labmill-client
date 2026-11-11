package testrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"labmill-client/internal/agent"
	"labmill-client/internal/launch"
	"labmill-client/internal/site"
	"labmill-client/internal/sshx"
	"labmill-client/internal/store"
	"labmill-client/internal/testcase"
	"labmill-client/internal/transfer"
)

// QueueWaitMax is how long a job may stay queued before the case is an error.
const QueueWaitMax = 20 * time.Minute

// Upper-bound prices in USD per million tokens. The cap is a safety net, so it
// uses the highest tier on purpose.
const (
	priceInPerM  = 15.0
	priceOutPerM = 75.0
)

// ErrCap ends a case that used more than the cost cap.
var ErrCap = errors.New("cap")

// ErrQueue ends a case whose job did not leave the queue in QueueWaitMax.
var ErrQueue = errors.New("queue")

// ErrLogin ends a case when the cluster asks the user to sign in.
var ErrLogin = errors.New("login")

// scriptedUI is the launch.UI of a corpus run: it approves, answers questions
// with "skipped" (and counts them), and fails the case when a sign-in is asked.
type scriptedUI struct {
	mu        sync.Mutex
	final     strings.Builder
	asked     int
	login     bool
	submitErr string // text of the last failed submit_job
	turnErr   error
	cancel    context.CancelFunc
}

func (u *scriptedUI) Approve(_ context.Context, p agent.Proposal) (agent.Decision, error) {
	if p.Secret {
		return agent.Decision{Reason: "corpus runs never read secret files"}, nil
	}
	return agent.Decision{Approved: true}, nil
}

func (u *scriptedUI) Choose(_ context.Context, _ agent.Choice) (agent.ChoiceAnswer, error) {
	u.mu.Lock()
	u.asked++
	u.mu.Unlock()
	return agent.ChoiceAnswer{Skipped: true}, nil
}

func (u *scriptedUI) Ask(_ context.Context, _ sshx.Question) (string, error) {
	u.mu.Lock()
	u.login = true
	u.mu.Unlock()
	u.cancel()
	return "", ErrLogin
}

func (u *scriptedUI) Notify(string) {}

func (u *scriptedUI) Event(e agent.Event) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch e.Kind {
	case agent.EventText:
		u.final.WriteString(e.Text)
	case agent.EventToolResult:
		if e.Tool == "submit_job" && e.IsError {
			u.submitErr = e.Text
		}
	case agent.EventToolCall:
		u.final.Reset() // only the text after the last tool call is the final message
	}
}

func (u *scriptedUI) Done(err error) {
	u.mu.Lock()
	u.turnErr = err
	u.mu.Unlock()
}

func (u *scriptedUI) JobNotice(string)         {}
func (u *scriptedUI) Transfer(transfer.Update) {}

func (u *scriptedUI) snapshot() (final string, asked int, login bool, submitErr string, turnErr error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.final.String(), u.asked, u.login, u.submitErr, u.turnErr
}

// Options configure one Driver.
type Options struct {
	Home     string  // run folder: <home>/<run id>
	Cluster  string  // alias of the cluster to use
	CapUSD   float64 // cost cap per run
	ConfigIn string  // clusters.yaml to copy into each isolated session
	CasesDir string  // folder with the case folders
}

// Driver plays cases through a real launch.Runtime.
type Driver struct {
	Opt   Options
	spent float64 // USD over all cases of the run
}

// Run plays one case and returns the observation, the cost in USD, and an
// error when the runner failed (as opposed to the case failing).
func (d *Driver) Run(ctx context.Context, c testcase.Case, caseDir string) (Observation, float64, error) {
	obs := Observation{CheckExit: -1}
	work := filepath.Join(caseDir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return obs, 0, err
	}
	if out, err := exec.CommandContext(ctx, "sh", filepath.Join(d.caseSrc(c), "gen.sh"), work).CombinedOutput(); err != nil {
		return obs, 0, fmt.Errorf("gen.sh: %v: %s", err, tail(string(out), 200))
	}
	if err := d.isolate(caseDir); err != nil {
		return obs, 0, err
	}

	limit := time.Duration(c.TimeBudget)*time.Second + QueueWaitMax
	runCtx, cancel := context.WithTimeout(ctx, limit+time.Duration(c.TimeBudget)*time.Second)
	defer cancel()
	ui := &scriptedUI{cancel: cancel}
	rt, err := launch.Build(runCtx, ui, func(context.Context, site.Site) (launch.SetupAnswers, error) {
		return launch.SetupAnswers{}, errors.New("no cluster configuration: run labmill once, or pass -config")
	})
	if err != nil {
		return obs, 0, fmt.Errorf("build: %w", err)
	}
	defer rt.Close()
	// Results and input stay inside the case folder, never in the person's own folders.
	rt.Env.ResultsDir = filepath.Join(caseDir, "results")
	rt.Env.LocalRoots = append(rt.Env.LocalRoots, work)

	before := d.spent
	var capped bool
	rt.Agent.OnReply = func(m *anthropic.BetaMessage) {
		u := m.Usage
		d.spent += (float64(u.InputTokens+u.CacheCreationInputTokens)*priceInPerM + float64(u.CacheReadInputTokens)*priceInPerM/10 +
			float64(u.OutputTokens)*priceOutPerM) / 1e6
		if d.spent > d.Opt.CapUSD && !capped {
			capped = true
			cancel()
		}
	}
	prompt := c.Prompt + "\n\nThe input files are in " + work + ". Use cluster " + d.Opt.Cluster + "."
	rt.Session.Send(runCtx, prompt)

	// The session is one turn at a time. When a job ends, tell the agent, as the
	// desktop app would on a job notice, so it can explain or fix and resubmit.
	var waitErr error
	for seen, i := 0, 0; i < 3 && waitErr == nil && runCtx.Err() == nil; i++ {
		if waitErr = d.waitJobs(runCtx, rt.Store, limit); waitErr != nil {
			break
		}
		n := d.endedJobs(runCtx, rt.Store)
		if n == 0 || n <= seen {
			break
		}
		seen = n
		rt.Session.Send(runCtx, "The job has ended. Check how it went and tell me. If it failed and you can fix it, fix it and submit again.")
	}
	final, asked, login, submitErr, turnErr := ui.snapshot()
	obs.FinalText, obs.AskedUser = final, asked > 0
	cost := d.spent - before
	switch {
	case capped:
		return obs, cost, ErrCap
	case login:
		return obs, cost, ErrLogin
	case waitErr == errWaitQueue:
		return obs, cost, ErrQueue
	case ctx.Err() != nil:
		return obs, cost, ctx.Err()
	case runCtx.Err() != nil:
		return obs, cost, context.DeadlineExceeded
	}
	o, err := d.observe(ctx, rt.Store, c, work, submitErr, turnErr, &obs)
	return o, cost, err
}

var errWaitQueue = errors.New("queue wait")

func (d *Driver) caseSrc(c testcase.Case) string { return filepath.Join(d.Opt.CasesDir, c.Dir) }

// isolate points the process at fresh XDG folders under caseDir and writes the
// session settings. The runner runs one case at a time, so the process
// environment is safe to change.
func (d *Driver) isolate(caseDir string) error {
	cfg := filepath.Join(caseDir, "config")
	state := filepath.Join(caseDir, "state")
	for _, p := range []string{filepath.Join(cfg, "labmill"), state} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
	}
	if d.Opt.ConfigIn != "" {
		b, err := os.ReadFile(d.Opt.ConfigIn)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(cfg, "labmill", "clusters.yaml"), b, 0o600); err != nil {
			return err
		}
	}
	prefs, _ := json.Marshal(map[string]bool{"approveAll": true, "autoDownload": true})
	if err := os.WriteFile(filepath.Join(cfg, "labmill", "gui.json"), prefs, 0o600); err != nil {
		return err
	}
	os.Setenv("XDG_CONFIG_HOME", cfg)
	os.Setenv("XDG_STATE_HOME", state)
	return nil
}

func terminal(s store.JobState) bool {
	switch s {
	case store.StateCompleted, store.StateFetched, store.StateFailed, store.StateCanceled:
		return true
	}
	return false
}

// submittedJobs returns the jobs the scheduler accepted, oldest first. A draft
// row without a scheduler id (a rejected sbatch) is not one of them.
func submittedJobs(ctx context.Context, st *store.Store) []store.Job {
	jobs, err := st.Jobs(ctx, store.JobFilter{})
	if err != nil {
		return nil
	}
	var out []store.Job
	for _, j := range jobs {
		if j.RemoteID != "" {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out
}

// endedJobs counts the accepted jobs in a final state.
func (d *Driver) endedJobs(ctx context.Context, st *store.Store) int {
	n := 0
	for _, j := range submittedJobs(ctx, st) {
		if terminal(j.State) {
			n++
		}
	}
	return n
}

// waitJobs polls until no accepted job is active, or limit passes. A job that
// is still queued at the limit makes errWaitQueue.
func (d *Driver) waitJobs(ctx context.Context, st *store.Store, limit time.Duration) error {
	start := time.Now()
	for {
		var active []store.Job
		for _, j := range submittedJobs(ctx, st) {
			if !terminal(j.State) {
				active = append(active, j)
			}
		}
		if len(active) == 0 {
			return nil
		}
		if time.Since(start) > limit {
			for _, j := range active {
				if j.State == store.StatePending || j.State == store.StateSubmitted {
					return errWaitQueue
				}
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

// observe builds the Observation from the job store and runs check.sh.
func (d *Driver) observe(ctx context.Context, st *store.Store, c testcase.Case, work, submitErr string, turnErr error, obs *Observation) (Observation, error) {
	jobs := submittedJobs(ctx, st)
	if len(jobs) == 0 {
		if submitErr == "" && turnErr != nil {
			return *obs, fmt.Errorf("turn: %w", turnErr)
		}
		obs.SubmitErr = submitErr
		return *obs, nil
	}
	first := jobs[0]
	rest := jobs[1:]
	if submitErr != "" {
		// The first sbatch was refused and a later one went through.
		obs.SubmitErr = submitErr
		rest = jobs
	} else {
		obs.ExitCode = first.Facts.ExitCode
		evs, _ := st.Events(ctx, first.ID, 0)
		var sb strings.Builder
		sb.WriteString(first.Message)
		for _, e := range evs {
			sb.WriteString("\n" + e.Text)
		}
		obs.OutputTail = tail(sb.String(), 4000)
		obs.JobState = slurmState(first, obs.OutputTail)
	}
	for _, j := range rest {
		if j.State == store.StateCompleted || j.State == store.StateFetched {
			obs.ResubmitCompleted = true
		}
	}
	if obs.JobState == "COMPLETED" && c.Expect.Outcome == "completed" {
		if first.Downloaded {
			obs.CheckExit = d.runCheck(ctx, c, first.LocalDir, work)
		} else {
			obs.CheckExit = 1 // the results never came back
		}
	}
	return *obs, nil
}

// slurmState maps the store state back to the scheduler word the judge uses.
func slurmState(j store.Job, tailText string) string {
	switch j.State {
	case store.StateCompleted, store.StateFetched:
		return "COMPLETED"
	case store.StateFailed:
		up := strings.ToUpper(j.Message + " " + tailText)
		switch {
		case strings.Contains(up, "TIMEOUT") || strings.Contains(up, "TIME LIMIT"):
			return "TIMEOUT"
		case strings.Contains(up, "OUT_OF_MEMORY"):
			return "OUT_OF_MEMORY"
		}
		return "FAILED"
	case store.StateCanceled:
		return "CANCELLED"
	}
	return ""
}

func (d *Driver) runCheck(ctx context.Context, c testcase.Case, resultDir, work string) int {
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(d.caseSrc(c), "check.sh"), resultDir, work)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 2
	}
	return 0
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
