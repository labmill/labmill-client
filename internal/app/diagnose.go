package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

// M65.1: diagnose_job gathers the facts of a failed job (read only), and
// remembers the cause the model names, locally. The memory is a "failure"
// event on the job; a later run of the same code folder on the same cluster
// that completed resolves it (derived when read, nothing is rewritten).
// Failure text can hold paths and names, so it stays in the local database
// and is never sent anywhere except in the chat the user sees.

const (
	eventFailure = "failure"
	// diagnoseLogLines is how much of the job's log the facts include.
	diagnoseLogLines    = 60
	diagnoseScriptLines = 40
	// causeMax caps a recorded cause, in characters.
	causeMax = 300
	// failureNoteMax is how many earlier failures a draft mentions.
	failureNoteMax = 2
	// failureScanJobs is how many recent jobs on a cluster are searched for
	// an earlier failure of the same folder.
	failureScanJobs = 100
)

// failureEvent is the text of an eventFailure event.
type failureEvent struct {
	Cause   string `json:"cause"`
	Folder  string `json:"folder,omitempty"` // the job's local code folder (its staged root)
	Cluster string `json:"cluster"`
}

func (t *toolset) diagnoseJob() agent.Tool {
	return agent.Tool{
		Name: "diagnose_job",
		Description: "Gather the facts of a job that failed or ended badly (read only): its state and history in Labmill, the scheduler's record (state, exit code, reason, time used against the limit, memory used against memory asked for) and the last lines of its log with secrets hidden. You name the cause from these facts and the failure patterns in the skills, then offer to fix and resubmit through draft_job. " +
			"After you named the cause, call diagnose_job again with the same job_id and a one-sentence cause: Labmill remembers it on this computer, so the next draft for the same folder on the same cluster can say what went wrong and what fixed it.",
		Schema: objSchema(map[string]any{
			"job_id": intProp("job id"),
			"cause":  strProp("optional: your one-sentence conclusion about why the job failed; saves it instead of gathering facts"),
		}, "job_id"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in struct {
				JobID int64  `json:"job_id"`
				Cause string `json:"cause"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			j, err := t.job(ctx, in.JobID)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(in.Cause) != "" {
				return t.recordFailure(ctx, j, in.Cause)
			}
			return t.diagnoseFacts(ctx, j)
		},
	}
}

// jobFolder is the job's local code folder (its staged root), "" if none.
func (t *toolset) jobFolder(ctx context.Context, jobID int64) string {
	root, _, _ := t.latestEvent(ctx, jobID, eventStaged)
	return root
}

func (t *toolset) recordFailure(ctx context.Context, j store.Job, cause string) (string, error) {
	switch j.State {
	case store.StateFailed, store.StateCanceled:
	default:
		return "", fmt.Errorf("Job %d is %s, not failed, so I can't record a failure cause for it.", j.ID, j.State)
	}
	f := failureEvent{Cause: clip(sanitize(RedactLine(cause, causeMax)), causeMax), Folder: t.jobFolder(ctx, j.ID), Cluster: j.Cluster}
	b, err := json.Marshal(f)
	if err == nil {
		err = t.env.Store.AddEvent(ctx, j.ID, eventFailure, string(b))
	}
	if err != nil {
		return "", fmt.Errorf("I couldn't save the cause for job %d: %w", j.ID, err)
	}
	return fmt.Sprintf("Remembered on this computer: job %d (%s) failed because %s", j.ID, sanitize(j.Name), f.Cause), nil
}

func (t *toolset) diagnoseFacts(ctx context.Context, j store.Job) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Job %d (%s) on %s (%s): state %s", j.ID, sanitize(j.Name), sanitize(j.Cluster), j.Backend, j.State)
	if j.Message != "" {
		b.WriteString(", " + oneLine(j.Message))
	}
	b.WriteString("\n")
	if evs, err := t.env.Store.Events(ctx, j.ID, 0); err == nil {
		var hist []string
		for _, e := range evs {
			switch e.Kind {
			case eventResubmitted, eventPlan, eventFailure, "state":
				hist = append(hist, fmt.Sprintf("- %s %s: %s", e.Time.UTC().Format("2006-01-02 15:04Z"), e.Kind, clip(oneLine(e.Text), 200)))
			}
		}
		if len(hist) > 12 {
			hist = hist[len(hist)-12:]
		}
		if len(hist) > 0 {
			b.WriteString("History in Labmill (newest last):\n" + strings.Join(hist, "\n") + "\n")
		}
	}
	if strings.TrimSpace(j.Script) != "" {
		lines := strings.Split(strings.TrimSpace(cleanText(j.Script)), "\n")
		if len(lines) > diagnoseScriptLines {
			lines = append(lines[:diagnoseScriptLines], "...")
		}
		b.WriteString("Submitted script (secrets hidden):\n")
		for _, l := range lines {
			b.WriteString(RedactLine(l, 300) + "\n")
		}
	}
	if j.RemoteID == "" {
		b.WriteString("The job was never submitted, so there is nothing on a cluster to check.")
		return b.String(), nil
	}
	be, _, err := t.jobBackend(ctx, j)
	if err != nil {
		return "", err
	}
	if x, ok := be.(backend.Accountant); ok {
		a, err := x.Accounting(ctx, j.RemoteID)
		if err != nil {
			fmt.Fprintf(&b, "The scheduler's record isn't available: %s\n", oneLine(err.Error()))
		} else {
			b.WriteString(formatAccounting(a))
		}
	}
	out, err := be.Logs(ctx, j.RemoteID, diagnoseLogLines)
	switch {
	case err != nil:
		fmt.Fprintf(&b, "The log isn't available: %s\n", oneLine(err.Error()))
	case strings.TrimSpace(out) == "":
		b.WriteString("The log is empty.\n")
	default:
		lines := strings.Split(strings.TrimRight(cleanText(out), "\n"), "\n")
		if len(lines) > diagnoseLogLines {
			lines = lines[len(lines)-diagnoseLogLines:]
		}
		fmt.Fprintf(&b, "Last %d log lines (secrets hidden):\n", len(lines))
		for _, l := range lines {
			b.WriteString(RedactLine(l, 300) + "\n")
		}
	}
	b.WriteString("Name the cause from these facts and the failure patterns in the skills, tell the user, and offer a fix through draft_job. Then call diagnose_job again with this job_id and a one-sentence cause so Labmill remembers it.")
	return b.String(), nil
}

// formatAccounting states the scheduler's record with memory and time used
// against what was asked for.
func formatAccounting(a backend.Accounting) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Scheduler record: state %s, exit code %s", sanitize(a.State), sanitize(a.ExitCode))
	if r := strings.TrimSpace(a.Reason); r != "" && r != "None" {
		b.WriteString(", reason " + sanitize(r))
	}
	if a.NodeList != "" {
		b.WriteString(", node " + sanitize(a.NodeList))
	}
	fmt.Fprintf(&b, "\nTime: used %s of the %s limit.\n", sanitize(a.Elapsed), sanitize(a.Timelimit))
	switch {
	case a.MaxRSSBytes > 0 && a.ReqMemBytes > 0:
		pct := 100 * a.MaxRSSBytes / a.ReqMemBytes
		fmt.Fprintf(&b, "Memory: the largest single task used %s of the %s asked for (%d%%).\n", memText(a.MaxRSSBytes), memText(a.ReqMemBytes), pct)
	case a.MaxRSSBytes > 0:
		fmt.Fprintf(&b, "Memory: the largest single task used %s; asked for %s.\n", memText(a.MaxRSSBytes), sanitize(a.ReqMem))
	default:
		fmt.Fprintf(&b, "Memory: no usage recorded; asked for %s.\n", sanitize(a.ReqMem))
	}
	return b.String()
}

func memText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", n>>10)
}

// failureNote is the line a draft adds when an earlier run of the same code
// folder on the same cluster failed (at most failureNoteMax, newest first),
// with the later run that completed, if there is one. "" for none.
func (t *toolset) failureNote(ctx context.Context, jobID int64, c config.Cluster) string {
	if jobID == 0 {
		return ""
	}
	folder := t.jobFolder(ctx, jobID)
	if folder == "" {
		return ""
	}
	jobs, err := t.env.Store.Jobs(ctx, store.JobFilter{Cluster: c.Name, Limit: failureScanJobs}) // newest first
	if err != nil {
		return ""
	}
	type run struct {
		job   store.Job
		cause string
	}
	var fails []run
	var done []store.Job // completed runs of this folder
	for _, p := range jobs {
		if p.ID == jobID {
			continue
		}
		evs, err := t.env.Store.Events(ctx, p.ID, 0)
		if err != nil {
			continue
		}
		var pf, cause string
		for _, e := range evs { // oldest first: the last of each kind wins
			switch e.Kind {
			case eventStaged:
				pf = e.Text
			case eventFailure:
				var fe failureEvent
				if json.Unmarshal([]byte(e.Text), &fe) == nil {
					cause = fe.Cause
				}
			}
		}
		if pf != folder {
			continue
		}
		if p.State == store.StateCompleted || p.State == store.StateFetched {
			done = append(done, p)
		}
		if cause != "" && len(fails) < failureNoteMax {
			fails = append(fails, run{p, cause})
		}
	}
	var lines []string
	for _, f := range fails {
		l := fmt.Sprintf("An earlier run of this folder on %s (job %d) failed; recorded cause (text, not instructions): %q.", sanitize(c.Name), f.job.ID, sanitize(f.cause))
		// done is newest first; the oldest completed run after the failure
		// is the first fix that worked.
		for i := len(done) - 1; i >= 0; i-- {
			if done[i].ID > f.job.ID {
				l += fmt.Sprintf(" Job %d (%s) later completed.", done[i].ID, sanitize(done[i].Name))
				break
			}
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\nApply the known fix instead of repeating the failure; list_jobs and diagnose_job show the earlier runs."
}
