package account

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/failclass"
	"labmill-client/internal/store"
)

// JobIn is one job report in the schema of the API's POST /api/jobs
// on app.labmill.org. BuildReport fills only the scheduler's facts: no job
// name, folder, command, environment value or file name. AddDetails adds the
// redacted script and output lines (not for a private job or a fully
// private account). Fields the client does not know are left out.
type JobIn struct {
	ClientJobRef    string          `json:"client_job_ref"`
	HPCSystemID     string          `json:"hpc_system_id"`
	Backend         string          `json:"backend"`
	State           string          `json:"state"`
	ExitCode        string          `json:"exit_code,omitempty"`
	FailureCategory string          `json:"failure_category,omitempty"`
	SubmittedAt     *time.Time      `json:"submitted_at,omitempty"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	EndedAt         *time.Time      `json:"ended_at,omitempty"`
	Req             json.RawMessage `json:"req,omitempty"`
	Used            json.RawMessage `json:"used,omitempty"`
	Partition       string          `json:"partition,omitempty"`
	QOS             string          `json:"qos,omitempty"`
	Resubmissions   int             `json:"resubmissions,omitempty"` // client resubmits plus scheduler requeues
	QueueWaitSec    *int64          `json:"queue_wait_sec,omitempty"`
	Software        []Software      `json:"software,omitempty"`
	Alloc           json.RawMessage `json:"alloc,omitempty"`  // what the job got (sacct AllocTRES)
	Usage           json.RawMessage `json:"usage,omitempty"`  // what it used, from the same facts
	Shared          *bool           `json:"shared,omitempty"` // false: a private job
	Script          string          `json:"script,omitempty"`
	StdoutLines     []string        `json:"stdout_lines,omitempty"`
	StderrLines     []string        `json:"stderr_lines,omitempty"`
}

// Software is one module the job loaded.
type Software struct {
	Name      string `json:"name"`
	Ecosystem string `json:"ecosystem"`
	Version   string `json:"version"`
}

// Final reports whether a job in state s has ended (completed counts: the
// results may still be on the cluster, but the job ran).
func Final(s store.JobState) bool {
	switch s {
	case store.StateCompleted, store.StateFetched, store.StateFailed, store.StateCanceled:
		return true
	}
	return false
}

var (
	slurmStateRE = regexp.MustCompile(`^[A-Z_]{3,20}$`)
	partitionRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	moduleRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+/-]{0,99}$`)
)

// apiState is the Slurm state the API expects: the raw Slurm state of a
// failed job (Reason, e.g. TIMEOUT, OUT_OF_MEMORY) when it has that form.
func apiState(j store.Job) (state, category string) {
	switch j.State {
	case store.StateSubmitted, store.StatePending:
		return "PENDING", ""
	case store.StateRunning:
		return "RUNNING", ""
	case store.StateCompleted, store.StateFetched:
		return "COMPLETED", ""
	case store.StateCanceled:
		return "CANCELLED", ""
	}
	st := "FAILED"
	if r := strings.TrimSpace(j.Message); slurmStateRE.MatchString(r) {
		st = r
	}
	switch st {
	case "TIMEOUT":
		return st, "timeout"
	case "OUT_OF_MEMORY":
		return st, "out_of_memory"
	}
	return st, ""
}

// Reported reports whether a job in state s gets a report: it was handed to
// the scheduler (waiting, running or ended).
func Reported(s store.JobState) bool {
	switch s {
	case store.StateSubmitted, store.StatePending, store.StateRunning:
		return true
	}
	return Final(s)
}

// BuildReport makes the report of job j from its events (the "spec" event
// of its draft and the "state" events) and the scheduler's facts of its last
// run (exit code, start and end, use), without hpc_system_id. ok is false
// when j was never submitted or is not in a reported state.
func BuildReport(j store.Job, evs []store.Event, ref string) (JobIn, bool) {
	if !Reported(j.State) || j.RemoteID == "" {
		return JobIn{}, false
	}
	in := JobIn{ClientJobRef: ref, Backend: cmp.Or(j.Backend, "slurm")}
	in.State, in.FailureCategory = apiState(j)
	var spec backend.JobSpec
	var haveSpec bool
	var submits int
	var submitted, started, ended time.Time
	for _, e := range evs {
		switch e.Kind {
		case "spec":
			var s backend.JobSpec
			if json.Unmarshal([]byte(e.Text), &s) == nil {
				spec, haveSpec = s, true
			}
		case "state":
			to := e.Text[strings.LastIndex(e.Text, " ")+1:]
			switch store.JobState(to) {
			case store.StateSubmitted:
				submits++
				submitted, started, ended = e.Time, time.Time{}, time.Time{} // a resubmission starts over
			case store.StateRunning:
				if started.IsZero() {
					started = e.Time
				}
			default:
				if Final(store.JobState(to)) && ended.IsZero() {
					ended = e.Time
				}
			}
		}
	}
	// Facts of an earlier run (the job was resubmitted) are left out.
	if f := j.Facts; f.RemoteID == j.RemoteID {
		in.ExitCode = fmt.Sprintf("%d:%d", f.ExitCode, f.Signal)
		in.Used = usedJSON(f)
		in.Alloc, in.Usage = allocUsage(f)
		// The scheduler's times win over the times this computer saw.
		if !f.Start.IsZero() {
			started = f.Start
		}
		if !f.End.IsZero() {
			ended = f.End
		}
	}
	at := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		u := t.UTC()
		return &u
	}
	in.SubmittedAt, in.StartedAt, in.EndedAt = at(submitted), at(started), at(ended)
	if !submitted.IsZero() && !started.IsZero() && !started.Before(submitted) {
		w := int64(started.Sub(submitted).Seconds())
		in.QueueWaitSec = &w
	}
	if Private(evs) {
		no := false
		in.Shared = &no
	}
	if haveSpec {
		in.Req = reqJSON(spec)
		if partitionRE.MatchString(spec.Partition) {
			in.Partition = spec.Partition
		}
		in.Software = modules(spec)
	}
	// Run facts from the scheduler; a private job sends none of them.
	if in.Shared == nil {
		f := j.Facts
		if f.RemoteID != j.RemoteID {
			f = store.Facts{} // an earlier run
		}
		if in.Partition == "" && partitionRE.MatchString(f.Partition) {
			in.Partition = f.Partition
		}
		if partitionRE.MatchString(f.QOS) {
			in.QOS = f.QOS
		}
		in.Resubmissions = f.Restarts + max(submits-1, 0)
		switch {
		case in.FailureCategory != "":
		case in.State == "NODE_FAIL":
			in.FailureCategory = "node_failure"
		case in.State == "PREEMPTED":
			in.FailureCategory = "preempted"
		case in.State == "FAILED":
			// From the exit code only; AddDetails refines it from the output
			// for an account that contributes details.
			in.FailureCategory = failclass.Of(failclass.Input{State: in.State, ExitCode: exitCode(j)})
		}
	}
	return in, true
}

// usedJSON is what the job used, in the API's form
// ({"elapsed": "07:42:10", "cpu_time": "50:01:02", "max_rss": "21.4G", "gpu": 1}).
func usedJSON(f store.Facts) json.RawMessage {
	r := map[string]any{}
	if f.ElapsedSec > 0 {
		r["elapsed"] = hms(f.ElapsedSec)
	}
	if f.CPUSec > 0 {
		r["cpu_time"] = hms(f.CPUSec)
	}
	if f.MaxRSSBytes > 0 {
		r["max_rss"] = fmt.Sprintf("%.1fG", float64(f.MaxRSSBytes)/(1<<30))
	}
	if f.GPUs > 0 {
		r["gpu"] = f.GPUs
	}
	if len(r) == 0 {
		return nil
	}
	b, _ := json.Marshal(r)
	return b
}

func hms(sec int64) string {
	return fmt.Sprintf("%02d:%02d:%02d", sec/3600, sec/60%60, sec%60)
}

// reqJSON is what the job asked for, in the API's form
// ({"cpu": 8, "mem": "32G", "gpu": 1, "gpu_type": "a100", "time": "12:00:00", "nodes": 1}).
func reqJSON(s backend.JobSpec) json.RawMessage {
	r := map[string]any{}
	if s.CPUs > 0 {
		r["cpu"] = s.CPUs
	}
	if s.MemoryGB > 0 {
		r["mem"] = fmt.Sprintf("%dG", s.MemoryGB)
	}
	if s.GPUs > 0 {
		r["gpu"] = s.GPUs
		if partitionRE.MatchString(s.GPUType) {
			r["gpu_type"] = s.GPUType
		}
	}
	if s.Time > 0 {
		r["time"] = hms(int64(s.Time.Seconds()))
	}
	if s.Nodes > 0 {
		r["nodes"] = s.Nodes
	}
	if len(r) == 0 {
		return nil
	}
	b, _ := json.Marshal(r)
	return b
}

// modules are the job's Lmod/Environment modules as name and version
// ("samtools/1.21"). A module named by a path (a private module folder) is
// left out; venv, conda and container names are never sent (they can name
// the user's own files).
func modules(s backend.JobSpec) []Software {
	if s.Environment == nil {
		return nil
	}
	var out []Software
	for _, m := range s.Environment.Modules {
		if !moduleRE.MatchString(m) || strings.Contains(m, "..") || len(out) == 200 {
			continue
		}
		name, ver := m, ""
		if i := strings.LastIndex(m, "/"); i > 0 {
			name, ver = m[:i], m[i+1:]
		}
		if len(ver) > 50 {
			continue
		}
		out = append(out, Software{Name: name, Ecosystem: "module", Version: ver})
	}
	return out
}

// EventPrivate is the event kind that marks a job private (draft_job
// "private"): its report holds only the scheduler's facts, and no output or
// problem report of it is collected or sent.
const EventPrivate = "private"

// EventOutput is the event kind that holds the redacted output lines of one
// run (JSON Output), collected when the job ended (CollectOutput).
const EventOutput = "output"

// Output is the text of an EventOutput event.
type Output struct {
	RemoteID string   `json:"remote_id"`
	Stdout   []string `json:"stdout,omitempty"`
	Stderr   []string `json:"stderr,omitempty"`
}

// Private reports whether the job of evs is marked private.
func Private(evs []store.Event) bool {
	for _, e := range evs {
		if e.Kind == EventPrivate {
			return true
		}
	}
	return false
}

// AddDetails adds the redacted script and, when collected, the output lines
// of j's current run to in. Callers leave it out for a private job and for
// a fully private account.
func AddDetails(in *JobIn, j store.Job, evs []store.Event, red Redactor) {
	if strings.TrimSpace(j.Script) != "" {
		in.Script = red.Script(j.Script)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		var o Output
		if evs[i].Kind == EventOutput && json.Unmarshal([]byte(evs[i].Text), &o) == nil && o.RemoteID == j.RemoteID {
			// Already redacted when collected; again here, in case the words grew since.
			in.StdoutLines, in.StderrLines = red.Lines(o.Stdout), red.Lines(o.Stderr)
			break
		}
	}
	// The output tells more about a plain failure; only a contributing account gets here.
	if in.State == "FAILED" && (in.FailureCategory == "" || in.FailureCategory == failclass.ExitNonzero) {
		in.FailureCategory = failclass.Of(failclass.Input{State: in.State, ExitCode: failedExit(j), Output: outputText(evs, j.RemoteID)})
	}
}

// tresTextRE is what the API accepts in a text of alloc or usage.
var tresTextRE = regexp.MustCompile(`^[A-Za-z0-9_=,./:+-]{1,200}$`)

// allocUsage is what the job got and used, in the API's form
// (alloc: cpus, mem_mb, gpus, gpu_type, nodes, tres; usage: cpu_eff,
// max_rss_mb, elapsed_s, source).
func allocUsage(f store.Facts) (alloc, usage json.RawMessage) {
	a := map[string]any{}
	if tresTextRE.MatchString(f.AllocTRES) {
		a["tres"] = f.AllocTRES
	}
	for _, kv := range strings.Split(f.AllocTRES, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseFloat(v, 64)
		switch {
		case k == "cpu" && err == nil:
			a["cpus"] = n
		case k == "node" && err == nil:
			a["nodes"] = n
		case k == "gres/gpu" && err == nil:
			a["gpus"] = n
		case k == "mem":
			if mb := memMB(v); mb > 0 {
				a["mem_mb"] = mb
			}
		case strings.HasPrefix(k, "gres/gpu:") && partitionRE.MatchString(k[len("gres/gpu:"):]):
			a["gpu_type"] = k[len("gres/gpu:"):]
		}
	}
	if f.GPUs > 0 && a["gpus"] == nil {
		a["gpus"] = f.GPUs
	}
	u := map[string]any{"source": "sacct"}
	if f.ElapsedSec > 0 {
		u["elapsed_s"] = f.ElapsedSec
	}
	if f.MaxRSSBytes > 0 {
		u["max_rss_mb"] = float64(f.MaxRSSBytes) / (1 << 20)
	}
	if cpus, _ := a["cpus"].(float64); cpus > 0 && f.ElapsedSec > 0 && f.CPUSec > 0 {
		u["cpu_eff"] = float64(f.CPUSec) / (float64(f.ElapsedSec) * cpus)
	}
	if len(a) > 0 {
		alloc, _ = json.Marshal(a)
	}
	if len(u) > 1 {
		usage, _ = json.Marshal(u)
	}
	return alloc, usage
}

// memMB parses a Slurm memory value such as "32G", "4000M" or "1.5T" (no
// unit: megabytes). 0 when it does not parse.
func memMB(v string) float64 {
	mult := 1.0
	if v == "" {
		return 0
	}
	switch v[len(v)-1] {
	case 'K', 'k':
		mult, v = 1.0/1024, v[:len(v)-1]
	case 'M', 'm':
		v = v[:len(v)-1]
	case 'G', 'g':
		mult, v = 1024, v[:len(v)-1]
	case 'T', 't':
		mult, v = 1024*1024, v[:len(v)-1]
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n * mult
}

var (
	toolNameRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	versionRE  = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)
)

// outputText is the collected output (stderr first, then stdout) of the
// current run of a job (stderr and the last 20 lines of stdout), or "" when none was collected.
func outputText(evs []store.Event, remoteID string) string {
	for i := len(evs) - 1; i >= 0; i-- {
		var o Output
		if evs[i].Kind == EventOutput && json.Unmarshal([]byte(evs[i].Text), &o) == nil && o.RemoteID == remoteID {
			out := o.Stdout
			if len(out) > 20 {
				out = out[len(out)-20:]
			}
			return strings.Join(append(append([]string{}, o.Stderr...), out...), "\n")
		}
	}
	return ""
}

// exitCode is the exit code of the current run of j, or -1 when unknown.
func exitCode(j store.Job) int {
	if j.Facts.RemoteID != "" && j.Facts.RemoteID == j.RemoteID {
		return j.Facts.ExitCode
	}
	return -1
}

// failedExit is the exit code of a failed job for the class: 1 when unknown.
func failedExit(j store.Job) int {
	if c := exitCode(j); c >= 0 {
		return c
	}
	return 1
}
