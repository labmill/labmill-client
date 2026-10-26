package account

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

// JobIn is one job report in the schema of the API's POST /api/jobs
// on app.labmill.org. It holds no job name, folder,
// script, command, environment value, log line or file name: only the
// scheduler's facts. Fields the client does not know are left out.
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
	QueueWaitSec    *int64          `json:"queue_wait_sec,omitempty"`
	Software        []Software      `json:"software,omitempty"`
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

// BuildReport makes the report of job j from its events (the "spec" event
// of its draft and the "state" events) and the scheduler's facts of its last
// run (exit code, start and end, use), without hpc_system_id. ok is false
// when j has not ended or was never submitted.
func BuildReport(j store.Job, evs []store.Event, ref string) (JobIn, bool) {
	if !Final(j.State) || j.RemoteID == "" {
		return JobIn{}, false
	}
	in := JobIn{ClientJobRef: ref, Backend: "slurm"}
	in.State, in.FailureCategory = apiState(j)
	var spec backend.JobSpec
	var haveSpec bool
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
	if haveSpec {
		in.Req = reqJSON(spec)
		if partitionRE.MatchString(spec.Partition) {
			in.Partition = spec.Partition
		}
		in.Software = modules(spec)
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
