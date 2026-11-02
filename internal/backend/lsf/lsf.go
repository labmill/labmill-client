// Package lsf implements backend.Backend for IBM Spectrum LSF over SSH:
// bqueues for queues, bsub (script on stdin) to submit, bjobs for status,
// bkill to cancel, and tail on the job's output file for logs.
//
// Same small scope as the pbs package: no accounting (bhist), no job arrays,
// no node-local scratch, no software environments, one node only. Render
// refuses what it cannot do.
package lsf

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

// LSF talks to one LSF cluster through a Runner (normally SSH exec on the
// login/submit host).
type LSF struct {
	Runner backend.Runner
	// Location is the zone of bjobs' timestamps, which have no zone. nil is UTC.
	Location *time.Location
}

// Type returns "lsf".
func (l *LSF) Type() string { return "lsf" }

// Info runs `bqueues -w` (columns QUEUE_NAME PRIO STATUS ...). State is "up"
// when STATUS starts with "Open:Active", else "down". LSF has no walltime
// or idle-node count in this table, so MaxTime, Nodes and IdleNodes stay 0.
// The default queue comes from `bqueues -d -w`; if that fails, none is marked.
func (l *LSF) Info(ctx context.Context) (backend.ClusterInfo, error) {
	res, err := l.Runner.Run(ctx, []string{"bqueues", "-w"}, nil)
	if err != nil {
		return backend.ClusterInfo{}, fmt.Errorf("bqueues: %w", err)
	}
	if res.ExitCode != 0 {
		return backend.ClusterInfo{}, cmdError("bqueues", res)
	}
	def := ""
	if r, err := l.Runner.Run(ctx, []string{"bqueues", "-d", "-w"}, nil); err == nil && r.ExitCode == 0 {
		if rows := queueRows(r.Stdout); len(rows) > 0 {
			def = rows[0][0]
		}
	}
	var info backend.ClusterInfo
	for _, f := range queueRows(res.Stdout) {
		q := backend.Partition{Name: f[0], Default: f[0] == def, State: "down"}
		if strings.HasPrefix(f[2], "Open:Active") {
			q.State = "up"
		}
		info.Partitions = append(info.Partitions, q)
	}
	return info, nil
}

// queueRows returns the data rows of a bqueues table (at least 3 fields).
func queueRows(out string) [][]string {
	var rows [][]string
	for i, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 3 { // header
			continue
		}
		rows = append(rows, f)
	}
	return rows
}

// Render turns spec into a bsub script with #BSUB directives. Memory is
// written in MB (rusage), the default unit of current LSF. It refuses
// options LSF does not support here: more than one node, QOS, node-local
// scratch and software environments.
func (l *LSF) Render(spec backend.JobSpec) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	if spec.Nodes > 1 || spec.QOS != "" || spec.LocalScratch != nil || spec.Environment != nil {
		return "", errors.New("lsf: more than one node, QOS, node-local scratch and software environments are not supported yet")
	}
	for _, line := range strings.Split(spec.Command, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#BSUB") {
			return "", errors.New("lsf: command must not contain #BSUB directives (they belong in the header)")
		}
	}
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	fmt.Fprintf(&b, "#BSUB -J %s\n", spec.Name)
	if spec.Partition != "" {
		fmt.Fprintf(&b, "#BSUB -q %s\n", spec.Partition)
	}
	if spec.CPUs > 0 {
		fmt.Fprintf(&b, "#BSUB -n %d\n#BSUB -R \"span[hosts=1]\"\n", spec.CPUs)
	}
	if spec.MemoryGB > 0 {
		fmt.Fprintf(&b, "#BSUB -R \"rusage[mem=%d]\"\n", spec.MemoryGB*1024)
	}
	if spec.GPUs > 0 {
		gpu := fmt.Sprintf("num=%d", spec.GPUs)
		if spec.GPUType != "" {
			gpu += ":gmodel=" + spec.GPUType
		}
		fmt.Fprintf(&b, "#BSUB -gpu \"%s\"\n", gpu)
	}
	if spec.Time > 0 {
		m := int((max(spec.Time, time.Minute) + time.Minute - 1) / time.Minute) // round up: LSF counts minutes
		fmt.Fprintf(&b, "#BSUB -W %d:%02d\n", m/60, m%60)
	}
	if spec.Requeue {
		b.WriteString("#BSUB -r\n")
	}
	if spec.MailOnEnd {
		b.WriteString("#BSUB -N\n")
	}
	fmt.Fprintf(&b, "#BSUB -cwd %s\n#BSUB -oo %s/%s.out\n", spec.WorkDir, strings.TrimRight(spec.WorkDir, "/"), spec.Name)
	for _, k := range slices.Sorted(maps.Keys(spec.Env)) {
		fmt.Fprintf(&b, "export %s='%s'\n", k, strings.ReplaceAll(spec.Env[k], "'", `'\''`))
	}
	b.WriteString(spec.Command)
	if !strings.HasSuffix(spec.Command, "\n") {
		b.WriteString("\n")
	}
	return b.String(), nil
}

var submittedRe = regexp.MustCompile(`Job <([0-9]+)> is submitted`)

// Submit creates the work folder, then runs `bsub` with script on stdin
// (bsub reads #BSUB lines from stdin) and returns the job ID from "Job <n> is
// submitted to queue <q>.". A failure that may have reached the scheduler
// wraps backend.ErrSubmitUncertain.
func (l *LSF) Submit(ctx context.Context, script string, spec backend.JobSpec) (string, error) {
	if strings.TrimSpace(script) == "" {
		return "", errors.New("bsub: empty script")
	}
	script = strings.ReplaceAll(script, "\r\n", "\n")
	if spec.WorkDir != "" {
		res, err := l.Runner.Run(ctx, []string{"mkdir", "-p", "--", spec.WorkDir}, nil)
		if err != nil {
			return "", fmt.Errorf("creating %s: %w", spec.WorkDir, err)
		}
		if res.ExitCode != 0 {
			return "", cmdError("mkdir", res)
		}
	}
	res, err := l.Runner.Run(ctx, []string{"bsub"}, strings.NewReader(script))
	if err != nil {
		return "", fmt.Errorf("bsub: %w: %w", backend.ErrSubmitUncertain, err)
	}
	if res.ExitCode != 0 {
		if strings.TrimSpace(res.Stderr) == "" || ctx.Err() != nil {
			return "", fmt.Errorf("%w: %w", backend.ErrSubmitUncertain, cmdError("bsub", res))
		}
		return "", cmdError("bsub", res)
	}
	m := submittedRe.FindStringSubmatch(res.Stdout)
	if m == nil {
		return "", fmt.Errorf("bsub: %w: unexpected output %q", backend.ErrSubmitUncertain, strings.TrimSpace(res.Stdout))
	}
	return m[1], nil
}

// jobFields is the bjobs -o list, '|' separated, in this order.
const jobFields = "stat exit_code exec_host start_time finish_time exit_reason output_file delimiter='|'"

// Status runs `bjobs -a -noheader -o "<jobFields>" <id>`. STAT maps PEND,
// PSUSP, WAIT → pending; RUN, USUSP, SSUSP, ZOMBI, UNKWN → running; DONE →
// completed; EXIT → canceled when the exit reason says the owner or an
// admin killed it (TERM_OWNER, TERM_ADMIN), else failed. LSF forgets
// finished jobs after CLEAN_PERIOD (one hour by default); then the job is
// reported as not found.
func (l *LSF) Status(ctx context.Context, remoteID string) (backend.Status, error) {
	f, err := l.jobRow(ctx, remoteID)
	if err != nil {
		return backend.Status{}, err
	}
	st := backend.Status{RemoteID: remoteID, Reason: f[5], Node: firstHost(f[2])}
	st.StartTime, st.EndTime = l.parseTime(f[3]), l.parseTime(f[4])
	switch f[0] {
	case "PEND", "PSUSP", "WAIT":
		st.State = store.StatePending
	case "RUN", "USUSP", "SSUSP", "ZOMBI", "UNKWN":
		st.State = store.StateRunning
	case "DONE":
		st.State = store.StateCompleted
	case "EXIT":
		st.ExitCode, _ = strconv.Atoi(f[1])
		st.State = store.StateFailed
		if strings.Contains(f[5], "TERM_OWNER") || strings.Contains(f[5], "TERM_ADMIN") {
			st.State = store.StateCanceled
		}
	default:
		st.State, st.Reason = store.StatePending, "unknown state "+f[0]
	}
	return st, nil
}

// Cancel runs `bkill <id>`.
func (l *LSF) Cancel(ctx context.Context, remoteID string) error {
	if err := checkRemoteID(remoteID); err != nil {
		return err
	}
	res, err := l.Runner.Run(ctx, []string{"bkill", remoteID}, nil)
	if err != nil {
		return fmt.Errorf("bkill: %w", err)
	}
	if res.ExitCode != 0 {
		return cmdError("bkill", res)
	}
	return nil
}

// Logs finds the output file from bjobs (output_file) and runs
// `tail -n <tailLines> -- <path>`. tailLines <= 0 means 100.
func (l *LSF) Logs(ctx context.Context, remoteID string, tailLines int) (string, error) {
	f, err := l.jobRow(ctx, remoteID)
	if err != nil {
		return "", err
	}
	if tailLines <= 0 {
		tailLines = 100
	}
	if !strings.HasPrefix(f[6], "/") {
		return "", fmt.Errorf("cannot locate the output file of lsf job %s", remoteID)
	}
	res, err := l.Runner.Run(ctx, []string{"tail", "-n", strconv.Itoa(tailLines), "--", f[6]}, nil)
	if err != nil {
		return "", fmt.Errorf("tail: %w", err)
	}
	if res.ExitCode != 0 {
		return "", cmdError("tail "+f[6], res)
	}
	return res.Stdout, nil
}

// jobRow returns the 7 fields of jobFields for one job.
func (l *LSF) jobRow(ctx context.Context, remoteID string) ([]string, error) {
	if err := checkRemoteID(remoteID); err != nil {
		return nil, err
	}
	res, err := l.Runner.Run(ctx, []string{"bjobs", "-a", "-noheader", "-o", jobFields, remoteID}, nil)
	if err != nil {
		return nil, fmt.Errorf("bjobs: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, cmdError("bjobs", res)
	}
	f := strings.Split(strings.TrimSpace(res.Stdout), "|")
	if len(f) != 7 || strings.TrimSpace(f[0]) == "" {
		return nil, fmt.Errorf("lsf job %s not found", remoteID)
	}
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	return f, nil
}

func firstHost(execHost string) string {
	h, _, _ := strings.Cut(execHost, ":") // "8*n01:8*n02"
	if _, name, ok := strings.Cut(h, "*"); ok {
		return name
	}
	return h
}

// parseTime reads bjobs' "Nov  2 10:00:00 2026" (a trailing " L" marks an
// estimate and is dropped). Anything else, such as "-", gives the zero time.
func (l *LSF) parseTime(v string) time.Time {
	loc := l.Location
	if loc == nil {
		loc = time.UTC
	}
	v = strings.TrimSuffix(strings.Join(strings.Fields(v), " "), " L")
	t, err := time.ParseInLocation("Jan 2 15:04:05 2006", v, loc)
	if err != nil {
		return time.Time{}
	}
	return t
}

var remoteIDRe = regexp.MustCompile(`^[0-9]+(\[[0-9]+\])?$`)

func checkRemoteID(id string) error {
	if !remoteIDRe.MatchString(id) {
		return fmt.Errorf("invalid lsf job id %q", id)
	}
	return nil
}

func cmdError(what string, res backend.Result) error {
	msg := strings.TrimSpace(res.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(res.Stdout)
	}
	return fmt.Errorf("%s failed (exit %d): %s", what, res.ExitCode, msg)
}
