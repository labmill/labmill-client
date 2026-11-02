// Package pbs implements backend.Backend for PBS (OpenPBS, PBS Pro) over SSH:
// qstat -Q for queues, qsub (script on stdin) to submit, qstat -x -f for
// status, qdel to cancel, and tail on the job's output file for logs.
//
// It is the small first step (M-more-schedulers): no accounting, no array
// jobs, no node-local scratch, no software environments. Render refuses what
// it cannot do, so a job never runs with fewer options than the user approved.
package pbs

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

// PBS talks to one PBS cluster through a Runner (normally SSH exec on the
// login/submit host).
type PBS struct {
	Runner backend.Runner
	// Location is the zone of qstat's timestamps, which have no zone. nil is UTC.
	Location *time.Location
}

// Type returns "pbs".
func (p *PBS) Type() string { return "pbs" }

// Info runs `qstat -Q -f` and reads one block per queue: "Queue: <name>",
// then "key = value" lines. State is "up" when enabled and started are both
// True, else "down". MaxTime comes from resources_max.walltime. PBS gives no
// idle-node count per queue, so Nodes and IdleNodes stay 0. The default queue
// comes from `qstat -B -f` (default_queue); if that fails, no queue is marked.
func (p *PBS) Info(ctx context.Context) (backend.ClusterInfo, error) {
	res, err := p.Runner.Run(ctx, []string{"qstat", "-Q", "-f"}, nil)
	if err != nil {
		return backend.ClusterInfo{}, fmt.Errorf("qstat: %w", err)
	}
	if res.ExitCode != 0 {
		return backend.ClusterInfo{}, cmdError("qstat -Q", res)
	}
	def := ""
	if r, err := p.Runner.Run(ctx, []string{"qstat", "-B", "-f"}, nil); err == nil && r.ExitCode == 0 {
		def = attrs(r.Stdout)["default_queue"]
	}
	var info backend.ClusterInfo
	for _, block := range strings.Split("\n"+res.Stdout, "\nQueue:")[1:] {
		name, rest, _ := strings.Cut(block, "\n")
		name = strings.TrimSpace(name)
		a := attrs(rest)
		q := backend.Partition{Name: name, Default: name == def, State: "down"}
		if a["enabled"] == "True" && a["started"] == "True" {
			q.State = "up"
		}
		if w, ok := a["resources_max.walltime"]; ok {
			q.MaxTime, _ = parseWalltime(w)
		}
		info.Partitions = append(info.Partitions, q)
	}
	return info, nil
}

// Render turns spec into a qsub script with #PBS directives. It refuses
// options PBS does not support here: QOS, node-local scratch and software
// environments.
func (p *PBS) Render(spec backend.JobSpec) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	if spec.QOS != "" || spec.LocalScratch != nil || spec.Environment != nil {
		return "", errors.New("pbs: QOS, node-local scratch and software environments are not supported yet")
	}
	for _, line := range strings.Split(spec.Command, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#PBS") {
			return "", errors.New("pbs: command must not contain #PBS directives (they belong in the header)")
		}
	}
	sel := fmt.Sprintf("select=%d", max(spec.Nodes, 1))
	if spec.CPUs > 0 {
		sel += fmt.Sprintf(":ncpus=%d", spec.CPUs)
	}
	if spec.MemoryGB > 0 {
		sel += fmt.Sprintf(":mem=%dgb", spec.MemoryGB)
	}
	if spec.GPUs > 0 {
		sel += fmt.Sprintf(":ngpus=%d", spec.GPUs)
		if spec.GPUType != "" {
			sel += ":gpu_type=" + spec.GPUType
		}
	}
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	fmt.Fprintf(&b, "#PBS -N %s\n", spec.Name)
	if spec.Partition != "" {
		fmt.Fprintf(&b, "#PBS -q %s\n", spec.Partition)
	}
	fmt.Fprintf(&b, "#PBS -l %s\n", sel)
	if spec.Time > 0 {
		fmt.Fprintf(&b, "#PBS -l walltime=%s\n", formatWalltime(max(spec.Time, time.Second)))
	}
	if spec.Requeue {
		b.WriteString("#PBS -r y\n")
	}
	if spec.MailOnEnd {
		b.WriteString("#PBS -m ae\n")
	}
	fmt.Fprintf(&b, "#PBS -j oe\n#PBS -o %s/%s.out\n", strings.TrimRight(spec.WorkDir, "/"), spec.Name)
	fmt.Fprintf(&b, "cd %s || exit 1\n", spec.WorkDir)
	for _, k := range slices.Sorted(maps.Keys(spec.Env)) {
		fmt.Fprintf(&b, "export %s='%s'\n", k, strings.ReplaceAll(spec.Env[k], "'", `'\''`))
	}
	b.WriteString(spec.Command)
	if !strings.HasSuffix(spec.Command, "\n") {
		b.WriteString("\n")
	}
	return b.String(), nil
}

// Submit creates the work folder, then runs `qsub` with script on stdin and
// returns the job ID (for example "123.server"). A failure that may have
// reached the scheduler wraps backend.ErrSubmitUncertain.
func (p *PBS) Submit(ctx context.Context, script string, spec backend.JobSpec) (string, error) {
	if strings.TrimSpace(script) == "" {
		return "", errors.New("qsub: empty script")
	}
	script = strings.ReplaceAll(script, "\r\n", "\n")
	if spec.WorkDir != "" {
		res, err := p.Runner.Run(ctx, []string{"mkdir", "-p", "--", spec.WorkDir}, nil)
		if err != nil {
			return "", fmt.Errorf("creating %s: %w", spec.WorkDir, err)
		}
		if res.ExitCode != 0 {
			return "", cmdError("mkdir", res)
		}
	}
	res, err := p.Runner.Run(ctx, []string{"qsub"}, strings.NewReader(script))
	if err != nil {
		return "", fmt.Errorf("qsub: %w: %w", backend.ErrSubmitUncertain, err)
	}
	if res.ExitCode != 0 {
		if strings.TrimSpace(res.Stderr) == "" || ctx.Err() != nil {
			return "", fmt.Errorf("%w: %w", backend.ErrSubmitUncertain, cmdError("qsub", res))
		}
		return "", cmdError("qsub", res)
	}
	id := firstLine(res.Stdout)
	if !remoteIDRe.MatchString(id) {
		return "", fmt.Errorf("qsub: %w: unexpected output %q", backend.ErrSubmitUncertain, strings.TrimSpace(res.Stdout))
	}
	return id, nil
}

// Status runs `qstat -x -f <id>` (-x includes finished jobs). job_state maps
// Q, H, W, T → pending; R, E, B, S, U, M → running; F and X → completed when
// Exit_status is 0, canceled when it is 271 (PBS Pro: killed by qdel), else
// failed. Times come from stime and mtime.
func (p *PBS) Status(ctx context.Context, remoteID string) (backend.Status, error) {
	if err := checkRemoteID(remoteID); err != nil {
		return backend.Status{}, err
	}
	a, err := p.jobAttrs(ctx, remoteID)
	if err != nil {
		return backend.Status{}, err
	}
	st := backend.Status{RemoteID: remoteID, Reason: a["comment"], Node: firstHost(a["exec_host"])}
	st.StartTime = p.parseTime(a["stime"])
	switch js := a["job_state"]; js {
	case "Q", "H", "W", "T":
		st.State = store.StatePending
	case "R", "E", "B", "S", "U", "M":
		st.State = store.StateRunning
	case "F", "X":
		code, _ := strconv.Atoi(a["Exit_status"])
		st.ExitCode = code
		st.EndTime = p.parseTime(a["mtime"])
		switch code {
		case 0:
			st.State = store.StateCompleted
		case 271:
			st.State = store.StateCanceled
		default:
			st.State = store.StateFailed
		}
	default:
		st.State, st.Reason = store.StatePending, "unknown state "+js
	}
	return st, nil
}

// Cancel runs `qdel <id>`.
func (p *PBS) Cancel(ctx context.Context, remoteID string) error {
	if err := checkRemoteID(remoteID); err != nil {
		return err
	}
	res, err := p.Runner.Run(ctx, []string{"qdel", remoteID}, nil)
	if err != nil {
		return fmt.Errorf("qdel: %w", err)
	}
	if res.ExitCode != 0 {
		return cmdError("qdel", res)
	}
	return nil
}

// Logs finds the output file in Output_Path of `qstat -x -f` and runs
// `tail -n <tailLines> -- <path>`. tailLines <= 0 means 100.
func (p *PBS) Logs(ctx context.Context, remoteID string, tailLines int) (string, error) {
	if err := checkRemoteID(remoteID); err != nil {
		return "", err
	}
	if tailLines <= 0 {
		tailLines = 100
	}
	a, err := p.jobAttrs(ctx, remoteID)
	if err != nil {
		return "", err
	}
	_, path, ok := strings.Cut(a["Output_Path"], ":") // host:/path
	if !ok {
		path = a["Output_Path"]
	}
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("cannot locate the output file of pbs job %s", remoteID)
	}
	res, err := p.Runner.Run(ctx, []string{"tail", "-n", strconv.Itoa(tailLines), "--", path}, nil)
	if err != nil {
		return "", fmt.Errorf("tail: %w", err)
	}
	if res.ExitCode != 0 {
		return "", cmdError("tail "+path, res)
	}
	return res.Stdout, nil
}

func (p *PBS) jobAttrs(ctx context.Context, remoteID string) (map[string]string, error) {
	res, err := p.Runner.Run(ctx, []string{"qstat", "-x", "-f", remoteID}, nil)
	if err != nil {
		return nil, fmt.Errorf("qstat: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, cmdError("qstat", res)
	}
	a := attrs(res.Stdout)
	if a["job_state"] == "" {
		return nil, fmt.Errorf("pbs job %s not found", remoteID)
	}
	return a, nil
}

// attrs reads "key = value" lines. A line that starts with a tab continues
// the value of the line before it (qstat -f wraps long values).
func attrs(out string) map[string]string {
	m := map[string]string{}
	last := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "\t") && last != "" {
			m[last] += strings.TrimSpace(line)
			continue
		}
		k, v, ok := strings.Cut(line, " = ")
		if !ok {
			last = ""
			continue
		}
		last = strings.TrimSpace(k)
		m[last] = strings.TrimSpace(v)
	}
	return m
}

func firstHost(execHost string) string {
	h, _, _ := strings.Cut(execHost, "/")
	return h
}

func (p *PBS) parseTime(v string) time.Time {
	loc := p.Location
	if loc == nil {
		loc = time.UTC
	}
	t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.Join(strings.Fields(v), " "), loc)
	if err != nil {
		// Fields collapsed the padding of single-digit days; retry unpadded.
		t, err = time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(v), " "), loc)
	}
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseWalltime reads [[HH:]MM:]SS or HH:MM:SS.
func parseWalltime(v string) (time.Duration, error) {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("bad walltime %q", v)
	}
	var sec int
	for _, s := range parts {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad walltime %q", v)
		}
		sec = sec*60 + n
	}
	return time.Duration(sec) * time.Second, nil
}

func formatWalltime(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
}

var remoteIDRe = regexp.MustCompile(`^[0-9]+(\[[0-9]*\])?(\.[A-Za-z0-9._-]+)?$`)

func checkRemoteID(id string) error {
	if !remoteIDRe.MatchString(id) {
		return fmt.Errorf("invalid pbs job id %q", id)
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

func firstLine(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
