// Package slurm implements backend.Backend for Slurm over SSH: sinfo for
// partitions, sbatch (script on stdin) to submit, squeue/sacct for status,
// scancel to cancel, and tail on the job's output file for logs.
package slurm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

// Slurm talks to one Slurm cluster through a Runner (normally SSH exec on the
// login/submit host).
type Slurm struct {
	Runner backend.Runner
	// Location overrides the time zone used to read sacct's zone-less
	// timestamps. sacct is run as `env TZ=UTC sacct ...`, so its times are
	// UTC and nil (UTC) is right; set it only for a cluster whose Slurm
	// ignores TZ.
	Location *time.Location
}

// Type returns "slurm".
func (s *Slurm) Type() string { return "slurm" }

// Info runs `sinfo -h -o "%P|%a|%l|%G|%D|%t"` (partition with "*" marking the
// default, availability, time limit, GRES, node count, node state), one line
// per partition/state group, and aggregates per partition: Nodes sums %D,
// IdleNodes sums %D where %t is exactly "idle" (see isIdleAvailable: "idle*", "idle~" etc. are excluded), GPU types come from GRES entries like
// "gpu:a100:4" or "gpu:h100:8(S:0-1)" (type "a100"/"h100"; untyped "gpu:4"
// gives no type), MaxTime parses Slurm time limits ("infinite" → 0,
// "1-00:00:00", "2:00:00", "30:00"). A non-zero exit is an error including
// stderr.
func (s *Slurm) Info(ctx context.Context) (backend.ClusterInfo, error) {
	res, err := s.Runner.Run(ctx, []string{"sinfo", "-h", "-o", "%P|%a|%l|%G|%D|%t"}, nil)
	if err != nil {
		return backend.ClusterInfo{}, fmt.Errorf("sinfo: %w", err)
	}
	if res.ExitCode != 0 {
		return backend.ClusterInfo{}, cmdError("sinfo", res)
	}
	var info backend.ClusterInfo
	idx := map[string]int{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, "|")
		if len(f) < 6 {
			return backend.ClusterInfo{}, fmt.Errorf("sinfo: unexpected line %q", line)
		}
		name := strings.TrimSpace(f[0])
		isDefault := strings.HasSuffix(name, "*")
		name = strings.TrimSuffix(name, "*")
		i, ok := idx[name]
		if !ok {
			maxTime, err := parseTimeLimit(f[2])
			if err != nil {
				return backend.ClusterInfo{}, fmt.Errorf("sinfo: partition %s: %w", name, err)
			}
			info.Partitions = append(info.Partitions, backend.Partition{Name: name, State: strings.TrimSpace(f[1]), MaxTime: maxTime})
			i = len(info.Partitions) - 1
			idx[name] = i
		}
		p := &info.Partitions[i]
		p.Default = p.Default || isDefault
		for _, t := range gresGPUTypes(f[3]) {
			if !slices.Contains(p.GPUTypes, t) {
				p.GPUTypes = append(p.GPUTypes, t)
			}
		}
		n, err := strconv.Atoi(strings.TrimSpace(f[4]))
		if err != nil {
			return backend.ClusterInfo{}, fmt.Errorf("sinfo: bad node count in %q", line)
		}
		p.Nodes += n
		if isIdleAvailable(f[5]) {
			p.IdleNodes += n
		}
	}
	return info, nil
}

// isIdleAvailable reports whether a sinfo %t node state counts as an idle,
// usable node. Only the plain state "idle" does. Slurm appends a flag
// character to a state that is not simply available:
//
//	suffix  meaning
//	*       not responding
//	~       powered down (cloud/power saving)
//	#       powering up
//	%       powering down
//	!       pending power down
//	-       planned for a job
//	$       maintenance
//	@       pending reboot
//	^       rebooting
//
// so "idle*", "idle~", "idle#", "idle%", "idle!", "idle-", "idle$", "idle@"
// and "idle^" are all excluded ("drain*", "down*" and the like are not idle
// to begin with). "idle~" nodes could start on demand, but backend.Partition
// has no field for powered-down capacity, so they are not counted.
func isIdleAvailable(state string) bool {
	return strings.TrimSpace(state) == "idle"
}

// Render validates spec and produces a bash batch script:
//
//	#!/bin/bash
//	#SBATCH --job-name=<Name>
//	#SBATCH --partition=<Partition>        (if set)
//	#SBATCH --gres=gpu[:<GPUType>]:<GPUs>  (if GPUs > 0)
//	#SBATCH --cpus-per-task=<CPUs>         (if set)
//	#SBATCH --mem=<MemoryGB>G              (if set)
//	#SBATCH --time=<D-HH:MM:SS>            (if set)
//	#SBATCH --chdir=<WorkDir>
//	#SBATCH --output=<WorkDir>/%x-%j.out
//	#SBATCH --qos=<QOS>                    (if set)
//	#SBATCH --requeue                      (if Requeue)
//	#SBATCH --mail-type=END,FAIL           (if MailOnEnd)
//	#SBATCH --nodes=<Nodes>                (if set; 1 with LocalScratch)
//	export NAME='value'                    (each Env entry, sorted, single-quoted)
//	<Command>
//
// With LocalScratch the command is wrapped in the stage-in / run / stage-out
// of localScratchBody.
//
// Command is written verbatim on its own line(s): the user sees and approves
// the whole script.
func (s *Slurm) Render(spec backend.JobSpec) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	if err := nodeLocalErr(spec.WorkDir); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	fmt.Fprintf(&b, "#SBATCH --job-name=%s\n", spec.Name)
	if spec.Partition != "" {
		fmt.Fprintf(&b, "#SBATCH --partition=%s\n", spec.Partition)
	}
	if spec.QOS != "" {
		fmt.Fprintf(&b, "#SBATCH --qos=%s\n", spec.QOS)
	}
	if spec.Requeue {
		b.WriteString("#SBATCH --requeue\n")
	}
	if spec.MailOnEnd {
		b.WriteString("#SBATCH --mail-type=END,FAIL\n")
	}
	switch {
	case spec.Nodes > 0:
		fmt.Fprintf(&b, "#SBATCH --nodes=%d\n", spec.Nodes)
	case spec.LocalScratch != nil:
		b.WriteString("#SBATCH --nodes=1\n") // node-local scratch exists on one node only
	}
	if spec.LocalScratch != nil {
		fmt.Fprintf(&b, "#SBATCH --signal=B:USR1@%d\n", signalLead(spec.Time)) // time to stage out
	}
	if spec.GPUs > 0 {
		if spec.GPUType != "" {
			fmt.Fprintf(&b, "#SBATCH --gres=gpu:%s:%d\n", spec.GPUType, spec.GPUs)
		} else {
			fmt.Fprintf(&b, "#SBATCH --gres=gpu:%d\n", spec.GPUs)
		}
	}
	if spec.CPUs > 0 {
		fmt.Fprintf(&b, "#SBATCH --cpus-per-task=%d\n", spec.CPUs)
	}
	if spec.MemoryGB > 0 {
		fmt.Fprintf(&b, "#SBATCH --mem=%dG\n", spec.MemoryGB)
	}
	if spec.Time > 0 {
		// Slurm's finest granularity is a minute; never render 00:00:00.
		d := max(spec.Time, time.Minute)
		fmt.Fprintf(&b, "#SBATCH --time=%s\n", formatTimeLimit(d))
	}
	fmt.Fprintf(&b, "#SBATCH --chdir=%s\n", spec.WorkDir)
	fmt.Fprintf(&b, "#SBATCH --output=%s/%%x-%%j.out\n", strings.TrimRight(spec.WorkDir, "/"))
	for _, k := range slices.Sorted(maps.Keys(spec.Env)) {
		fmt.Fprintf(&b, "export %s=%s\n", k, "'"+strings.ReplaceAll(spec.Env[k], "'", `'\''`)+"'")
	}
	// Environment setup runs before the command, inside the same subshell
	// with LocalScratch, so it happens where the command runs.
	spec.Command = environmentCommand(spec)
	if spec.LocalScratch != nil {
		b.WriteString(localScratchBody(spec))
		return b.String(), nil
	}
	b.WriteString(spec.Command)
	if !strings.HasSuffix(spec.Command, "\n") {
		b.WriteString("\n")
	}
	return b.String(), nil
}

// NodeLocalDir returns the node-local root ("/tmp", "/var/tmp" or "/dev/shm")
// that dir is on or under, or "" when dir is somewhere shared.
func NodeLocalDir(dir string) string {
	d := path.Clean(dir)
	for _, root := range []string{"/tmp", "/var/tmp", "/dev/shm"} {
		if d == root || strings.HasPrefix(d, root+"/") {
			return root
		}
	}
	return ""
}

// nodeLocalErr is the refusal for a work or output folder on node-local disk,
// or nil when dir is shared (or empty).
func nodeLocalErr(dir string) error {
	d := NodeLocalDir(dir)
	if d == "" {
		return nil
	}
	return fmt.Errorf("%w: work_dir %s is node-local (%s): the job's output would land on the compute node's own disk, lost and unreachable from the login node. Use a shared folder (your home, a project or storage folder, or the uploaded data folder)", backend.ErrInvalidSpec, dir, d)
}

var scriptDirRe = regexp.MustCompile(`^/[A-Za-z0-9._/+@,:=~-]*$`)

// ScriptDirs returns the absolute folders a script's leading #SBATCH block
// names: the --chdir/-D folder, then the folder part of --output/-o and
// --error/-e (relative paths are resolved against --chdir; without one they
// are skipped, as the submit directory is unknown). Only plain paths are kept:
// anything with % or $ (job-name patterns, variables) or characters outside
// the work_dir alphabet is dropped. Duplicates are removed.
func ScriptDirs(script string) []string {
	dirs, _ := scriptDirs(script)
	return dirs
}

// scriptDirs is ScriptDirs plus whether the script has a usable --chdir.
func scriptDirs(script string) (dirs []string, hasChdir bool) {
	var chdir string
	var files []string
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || (strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#SBATCH")) {
			continue
		}
		rest, ok := strings.CutPrefix(line, "#SBATCH")
		if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
			break // the first command ends the block
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		opt, val := f[0], ""
		if k, v, ok := strings.Cut(opt, "="); ok {
			opt, val = k, v
		} else if len(f) > 1 {
			val = f[1]
		}
		switch opt {
		case "--chdir", "-D":
			chdir = val
		case "--output", "-o", "--error", "-e":
			files = append(files, val)
		}
	}
	add := func(d string) {
		if d != "" && !strings.ContainsAny(d, "%$") && scriptDirRe.MatchString(d) && !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	if chdir != "" && scriptDirRe.MatchString(chdir) && !strings.ContainsAny(chdir, "%$") && strings.HasPrefix(chdir, "/") {
		hasChdir = true
		add(path.Clean(chdir))
	}
	for _, f := range files {
		d := path.Dir(f)
		if !strings.Contains(f, "/") {
			d = "." // a bare file name lands in the working folder
		}
		if !strings.HasPrefix(d, "/") {
			if !hasChdir {
				continue
			}
			d = path.Join(chdir, d)
		}
		if strings.ContainsAny(f, "%$") && strings.ContainsAny(d, "%$") {
			continue
		}
		add(path.Clean(d))
	}
	return dirs, hasChdir
}

// Submit first runs `mkdir -p -- <dirs>` for the folders the script's #SBATCH
// block names (ScriptDirs; spec.WorkDir when the script has no --chdir),
// because sbatch fails when the --output folder is missing. Node-local
// folders are refused. It then runs `sbatch --parsable` with script on stdin
// and returns the job ID
// (the part of the output before any ';' cluster suffix, which must be
// digits, optionally "<digits>_<digits>"). A non-zero exit is an error with
// stderr.
func (s *Slurm) Submit(ctx context.Context, script string, spec backend.JobSpec) (string, error) {
	if strings.TrimSpace(script) == "" {
		return "", errors.New("sbatch: empty script")
	}
	script = strings.ReplaceAll(script, "\r\n", "\n") // e.g. edited on Windows
	dirs, hasChdir := scriptDirs(script)
	if !hasChdir && spec.WorkDir != "" && !slices.Contains(dirs, spec.WorkDir) {
		dirs = append([]string{spec.WorkDir}, dirs...)
	}
	for _, d := range dirs {
		if err := nodeLocalErr(d); err != nil {
			return "", err
		}
	}
	if len(dirs) > 0 {
		argv := append([]string{"mkdir", "-p", "--"}, dirs...)
		res, err := s.Runner.Run(ctx, argv, nil)
		if err != nil {
			return "", fmt.Errorf("creating %s: %w", strings.Join(dirs, " "), err)
		}
		if res.ExitCode != 0 {
			return "", cmdError(strings.Join(argv, " "), res)
		}
	}
	res, err := s.Runner.Run(ctx, []string{"sbatch", "--parsable"}, strings.NewReader(script))
	if err != nil {
		return "", fmt.Errorf("sbatch: %w: %w", backend.ErrSubmitUncertain, err)
	}
	if res.ExitCode != 0 {
		if strings.TrimSpace(res.Stderr) == "" || ctx.Err() != nil {
			return "", fmt.Errorf("%w: %w", backend.ErrSubmitUncertain, cmdError("sbatch", res))
		}
		return "", cmdError("sbatch", res)
	}
	id, _, _ := strings.Cut(firstLine(res.Stdout), ";")
	id = strings.TrimSpace(id)
	if !remoteIDRe.MatchString(id) {
		return "", fmt.Errorf("sbatch: %w: unexpected output %q", backend.ErrSubmitUncertain, strings.TrimSpace(res.Stdout))
	}
	return id, nil
}

// Status validates remoteID (digits, optional _digits) and runs
// `squeue -h -j <id> -o "%T|%r|%N|%i"`. If squeue knows the job, its state maps
// as PENDING/CONFIGURING → pending; RUNNING/COMPLETING → running; others via
// the sacct mapping. If squeue prints nothing (job finished), it runs
// `env TZ=UTC sacct -n -P -X -j <id> --format=State,ExitCode,Start,End,NodeList,JobID,ElapsedRaw,AllocTRES`
// (TZ=UTC makes sacct's zone-less times UTC whatever the cluster's zone) and maps
// COMPLETED → completed; CANCELLED* → canceled; FAILED, TIMEOUT, OUT_OF_MEMORY,
// NODE_FAIL, PREEMPTED, BOOT_FAIL, DEADLINE → failed (Reason = the Slurm
// state); ExitCode is the part before ':'. Unknown job → error. A single job
// that ended also gets its use: elapsed time and allocated GPUs from that row,
// CPU time and the largest MaxRSS from `env TZ=UTC sacct -n -P -j <id> --format=JobID,TotalCPU,MaxRSS`
// (a failed query leaves them 0).
//
// Job arrays: for an array ID without a task suffix, squeue and sacct return
// one row per task (`123_4`) plus collapsed ranges of waiting tasks
// (`123_[5-9]`). The tasks are then aggregated by aggregateTasks into one
// state, with a "N of M tasks done" reason. A task ID such as "123_4" is a
// single job and reports just that task. A heterogeneous job (`100+0`,
// `100+1`) is one job: its components are folded the same way, with a reason
// like "2 components" (", K done", ", K failed (STATE)").
func (s *Slurm) Status(ctx context.Context, remoteID string) (backend.Status, error) {
	if err := checkRemoteID(remoteID); err != nil {
		return backend.Status{}, err
	}
	res, err := s.Runner.Run(ctx, []string{"squeue", "-h", "-j", remoteID, "-o", "%T|%r|%N|%i"}, nil)
	if err != nil {
		return backend.Status{}, fmt.Errorf("squeue: %w", err)
	}
	// Some sites make squeue fail for jobs that already left the queue.
	gone := res.ExitCode != 0 && strings.Contains(res.Stderr, "Invalid job id")
	if res.ExitCode != 0 && !gone {
		return backend.Status{}, cmdError("squeue", res)
	}
	var queued []taskRow
	if !gone {
		for _, line := range nonEmptyLines(res.Stdout) {
			queued = append(queued, parseSqueueRow(line))
		}
	}
	if isArray(remoteID, queued) {
		return s.arrayStatus(ctx, remoteID, queued)
	}
	var fromSqueue *backend.Status
	if len(queued) > 0 {
		q := queued[0]
		st := backend.Status{RemoteID: remoteID, State: q.State, Reason: q.Reason, Node: q.Node}
		if q.State != store.StatePending && q.State != store.StateRunning {
			// Terminal: sacct has the exit code and times.
			fromSqueue = &st
		} else {
			return st, nil
		}
	}
	rows, err := s.sacctRows(ctx, remoteID)
	switch {
	case errors.Is(err, errNoRows):
		if fromSqueue != nil {
			return *fromSqueue, nil
		}
		return backend.Status{}, fmt.Errorf("slurm job %s not found", remoteID)
	case err != nil:
		if fromSqueue != nil { // e.g. sites without slurmdbd accounting
			return *fromSqueue, nil
		}
		return backend.Status{}, err
	}
	if len(rows) > 1 || rows[0].N > 1 { // array whose tasks are all gone from squeue
		return aggregateWithID(remoteID, rows), nil
	}
	r := rows[0]
	st := backend.Status{RemoteID: remoteID, State: r.State, Node: r.Node, StartTime: r.Start, EndTime: r.End, ExitCode: r.Exit, Signal: r.Sig,
		ElapsedSec: r.Elapsed, GPUs: r.GPUs}
	if !r.End.IsZero() {
		st.CPUSec, st.MaxRSSBytes = s.stepUse(ctx, remoteID)
	}
	switch {
	case !r.Known:
		st.State, st.Reason = store.StatePending, "unknown state "+r.Raw
		if !r.End.IsZero() {
			st.State = store.StateFailed // ended in a state we don't know: finished, not waiting
		}
	case r.State == store.StateFailed || r.State == store.StatePending || r.State == store.StateRunning:
		st.Reason = r.Raw
	}
	return st, nil
}

// errNoRows means sacct succeeded but printed nothing.
var errNoRows = errors.New("sacct: no rows")

// sacctRows runs sacct for remoteID and parses every row. It returns
// errNoRows if there are none, and a command error on a non-zero exit.
func (s *Slurm) sacctRows(ctx context.Context, remoteID string) ([]taskRow, error) {
	res, err := s.Runner.Run(ctx, []string{"env", "TZ=UTC", "sacct", "-n", "-P", "-X", "-j", remoteID, "--format=State,ExitCode,Start,End,NodeList,JobID,ElapsedRaw,AllocTRES"}, nil)
	if err != nil {
		return nil, fmt.Errorf("sacct: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, cmdError("sacct", res)
	}
	var rows []taskRow
	for _, line := range nonEmptyLines(res.Stdout) {
		r, ok := s.parseSacctRow(line)
		if !ok {
			return nil, fmt.Errorf("sacct: unexpected line %q", line)
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return nil, errNoRows
	}
	return rows, nil
}

// arrayStatus reports a job array. sacct sees every task (finished ones
// too), so it is preferred; squeue's rows supply the reason of waiting tasks
// and, when sacct lags behind (its rows still PENDING), the fact that a task
// is already RUNNING. Without accounting only squeue's rows exist: see
// aggregateQueued.
func (s *Slurm) arrayStatus(ctx context.Context, remoteID string, queued []taskRow) (backend.Status, error) {
	rows, err := s.sacctRows(ctx, remoteID)
	if err != nil || len(rows) == 0 {
		return aggregateQueued(remoteID, queued), nil
	}
	var waitReason, runNode string
	running := false
	for _, q := range queued {
		if q.State == store.StatePending && q.Known && q.Reason != "" && waitReason == "" {
			waitReason = q.Reason
		}
		if q.State == store.StateRunning && q.Known {
			if !running {
				runNode = q.Node
			}
			running = true
		}
	}
	for i := range rows {
		if rows[i].State == store.StatePending && rows[i].Known {
			rows[i].Reason = waitReason
		}
	}
	st := aggregateWithID(remoteID, rows)
	if running && st.State != store.StateRunning {
		st.State, st.Node, st.EndTime = store.StateRunning, runNode, time.Time{}
		// The wait reason came from the lagging sacct view; a running array
		// isn't waiting, so don't show it.
		if waitReason != "" {
			st.Reason = strings.TrimSuffix(st.Reason, " ("+waitReason+")")
		}
	}
	return st, nil
}

// aggregateQueued folds squeue-only rows (a site without accounting, so no
// finished tasks are visible) into a Status. It cannot know how many tasks
// already finished, so it reports "N tasks left (K running, J pending)", or
// "N tasks left, all waiting (reason)" when none runs, never "0 of M done".
// A waiting array is reported pending even if earlier tasks finished: that
// cannot be told apart without sacct.
func aggregateQueued(remoteID string, rows []taskRow) backend.Status {
	var running, pending int
	var node, waitReason string
	for _, r := range rows {
		n := max(r.N, 1)
		if r.Known && r.State == store.StateRunning {
			running += n
			if node == "" {
				node = r.Node
			}
		} else {
			pending += n
			if waitReason == "" {
				waitReason = r.Reason
			}
		}
	}
	st := backend.Status{RemoteID: remoteID}
	if running > 0 {
		st.State, st.Node = store.StateRunning, node
		st.Reason = fmt.Sprintf("%d tasks left (%d running, %d pending)", running+pending, running, pending)
		return st
	}
	st.State = store.StatePending
	st.Reason = fmt.Sprintf("%d tasks left, all waiting", pending)
	if waitReason != "" {
		st.Reason += " (" + waitReason + ")"
	}
	return st
}

func aggregateWithID(remoteID string, rows []taskRow) backend.Status {
	st := aggregateTasks(rows)
	st.RemoteID = remoteID
	return st
}

// taskRow is one squeue or sacct row: a job, one array task, or a collapsed
// range of waiting tasks (N > 1).
type taskRow struct {
	ID     string
	Raw    string         // Slurm state word, e.g. "TIMEOUT"
	State  store.JobState // mapped state
	Known  bool           // Raw is a state mapState knows
	Reason string         // squeue's reason for waiting rows
	Node   string
	Exit   int
	Sig    int
	Start  time.Time
	End    time.Time
	N      int // number of tasks the row stands for, at least 1
	// sacct only: ElapsedRaw seconds and the GPUs in AllocTRES (0: none reported)
	Elapsed int64
	GPUs    int
}

// parseSqueueRow parses "%T|%r|%N|%i" (the last field is optional).
func parseSqueueRow(line string) taskRow {
	f := strings.Split(line, "|")
	f = append(f, "", "", "", "")
	state := strings.TrimSpace(f[0])
	reason := strings.TrimSpace(f[1])
	if reason == "None" {
		reason = ""
	}
	id := strings.TrimSpace(f[3])
	r := taskRow{ID: id, Raw: state, Node: cleanNode(f[2]), N: taskCount(id)}
	js, ok := mapState(state)
	r.State, r.Known = js, ok
	switch {
	case !ok:
		r.State, r.Reason = store.StatePending, "unknown state "+state
	case state == "PENDING" || state == "CONFIGURING":
		r.Reason = reason // keep squeue's reason
	case state == "RUNNING" || state == "COMPLETING":
	case js == store.StateCanceled || js == store.StateCompleted:
	default:
		r.Reason = state
	}
	return r
}

// parseSacctRow parses "State|ExitCode|Start|End|NodeList[|JobID[|ElapsedRaw|AllocTRES]]". ok is
// false if the line has no state.
func (s *Slurm) parseSacctRow(line string) (taskRow, bool) {
	f := strings.Split(line, "|")
	f = append(f, "", "", "", "", "", "", "", "")
	state := strings.Fields(f[0])
	if len(state) == 0 {
		return taskRow{}, false
	}
	js, ok := mapState(state[0])
	id := strings.TrimSpace(f[5])
	r := taskRow{ID: id, Raw: state[0], State: js, Known: ok, Node: cleanNode(f[4]), N: taskCount(id),
		Start: parseSlurmTime(f[2], s.Location), End: parseSlurmTime(f[3], s.Location)}
	code, sig, _ := strings.Cut(strings.TrimSpace(f[1]), ":")
	r.Sig, _ = strconv.Atoi(sig)
	r.Exit, _ = strconv.Atoi(code)
	r.Elapsed, _ = strconv.ParseInt(strings.TrimSpace(f[6]), 10, 64)
	for _, t := range strings.Split(f[7], ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(t), "gres/gpu="); ok {
			r.GPUs, _ = strconv.Atoi(v)
		}
	}
	return r, true
}

// stepUse runs `env TZ=UTC sacct -n -P -j <id> --format=JobID,TotalCPU,MaxRSS` and returns
// the job line's TotalCPU (the sum over its steps) in seconds and the largest
// MaxRSS of its steps in bytes. Read only; 0 for anything not reported.
func (s *Slurm) stepUse(ctx context.Context, remoteID string) (cpuSec, maxRSS int64) {
	res, err := s.Runner.Run(ctx, []string{"env", "TZ=UTC", "sacct", "-n", "-P", "-j", remoteID, "--format=JobID,TotalCPU,MaxRSS"}, nil)
	if err != nil || res.ExitCode != 0 {
		return 0, 0
	}
	for _, l := range nonEmptyLines(res.Stdout) {
		f := strings.Split(l, "|")
		if len(f) != 3 {
			continue
		}
		id := strings.TrimSpace(f[0])
		switch {
		case id == remoteID:
			cpu, _, _ := strings.Cut(strings.TrimSpace(f[1]), ".") // drop the milliseconds
			if d, err := parseTimeLimit(cpu); err == nil {
				cpuSec = int64(d.Seconds())
			}
		case strings.HasPrefix(id, remoteID+"."):
			maxRSS = max(maxRSS, parseSlurmMem(f[2]))
		}
	}
	return cpuSec, maxRSS
}

// isArray reports whether remoteID, an ID without a task suffix, is a job
// array according to its squeue rows: several rows, a collapsed range, or a
// task row such as "123_4". The components of a heterogeneous job ("123+0")
// count too: they are folded into one Status by aggregateTasks.
func isArray(remoteID string, rows []taskRow) bool {
	if strings.Contains(remoteID, "_") || len(rows) == 0 {
		return false
	}
	if len(rows) > 1 {
		return true
	}
	return rows[0].N > 1 || strings.Contains(rows[0].ID, "_") || hetIDRe.MatchString(rows[0].ID)
}

// taskCount returns how many array tasks a squeue/sacct job ID stands for:
// "123" and "123_4" are 1, "123_[5-9]" is 5, "123_[1,3,5-7%2]" is 5 (a "%N"
// throttle is ignored, "a-b:s" has a step). Anything unparseable is 1.
func taskCount(id string) int {
	_, rest, ok := strings.Cut(id, "_[")
	if !ok {
		return 1
	}
	rest, _, ok = strings.Cut(rest, "]")
	if !ok {
		return 1
	}
	rest, _, _ = strings.Cut(rest, "%")
	total := 0
	for _, part := range strings.Split(rest, ",") {
		part, stepStr, hasStep := strings.Cut(part, ":")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return 1
			}
			step = n
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 {
			return 1
		}
		if !isRange {
			total++
			continue
		}
		b, err := strconv.Atoi(hi)
		if err != nil || b < a {
			return 1
		}
		total += (b-a)/step + 1
	}
	if total < 1 {
		return 1
	}
	return total
}

// aggregateTasks folds the rows of one job array into a single Status
// (RemoteID is left for the caller). A row with N > 1 counts N tasks.
//
//   - any task running → running
//   - tasks waiting, none running: pending if no task has finished yet, else
//     running (the array is in progress)
//   - nothing waiting or running: failed if any task failed (including
//     TIMEOUT, OUT_OF_MEMORY, NODE_FAIL...), else canceled if any was
//     canceled, else completed
//
// Reason is "N of M tasks done", where done counts completed, failed and
// canceled tasks, plus ", K failed" / ", K canceled" and, for failed arrays,
// the first failure's Slurm state in parentheses (for waiting arrays with
// nothing done yet, squeue's reason). Node is a running task's node (empty when none runs). ExitCode
// is the first non-zero exit code. StartTime is the earliest start; EndTime is
// the latest end, only once nothing is waiting or running.
func aggregateTasks(rows []taskRow) backend.Status {
	var total, pending, running, completed, failed, canceled, unknown int
	var firstFail, firstUnknown, waitReason, runNode string
	var exit int
	var start, end time.Time
	for _, r := range rows {
		n := max(r.N, 1)
		total += n
		switch {
		case !r.Known && !r.End.IsZero():
			// sacct row with an end time but a state we don't know: finished.
			unknown += n
			if firstUnknown == "" {
				firstUnknown = r.Raw
			}
		case !r.Known:
			pending += n
			if waitReason == "" {
				waitReason = r.Reason
			}
		case r.State == store.StatePending:
			pending += n
			if waitReason == "" {
				waitReason = r.Reason
			}
		case r.State == store.StateRunning:
			running += n
			if runNode == "" {
				runNode = r.Node
			}
		case r.State == store.StateCompleted:
			completed += n
		case r.State == store.StateCanceled:
			canceled += n
		default:
			failed += n
			if firstFail == "" {
				firstFail = r.Raw
			}
		}
		if exit == 0 && r.State == store.StateFailed {
			exit = r.Exit
		}
		if !r.Start.IsZero() && (start.IsZero() || r.Start.Before(start)) {
			start = r.Start
		}
		if r.End.After(end) {
			end = r.End
		}
	}
	done := completed + failed + canceled + unknown
	st := backend.Status{ExitCode: exit, StartTime: start}
	reason := fmt.Sprintf("%d of %d tasks done", done, total)
	if isHetero(rows) {
		// A heterogeneous job ("100+0", "100+1", ...) is one job whose
		// components are scheduled together, not an array of tasks.
		reason = fmt.Sprintf("%d components", total)
		if done > 0 && done < total {
			reason += fmt.Sprintf(", %d done", done)
		}
	}
	if failed > 0 {
		reason += fmt.Sprintf(", %d failed", failed)
	}
	if canceled > 0 {
		reason += fmt.Sprintf(", %d canceled", canceled)
	}
	if unknown > 0 {
		reason += fmt.Sprintf(", %d unknown (%s)", unknown, firstUnknown)
	}
	switch {
	case running > 0:
		st.State, st.Node = store.StateRunning, runNode
	case pending > 0 && done > 0:
		st.State = store.StateRunning
	case pending > 0:
		st.State = store.StatePending
		if waitReason != "" {
			reason += " (" + waitReason + ")"
		}
	case failed > 0:
		st.State = store.StateFailed
		reason += " (" + firstFail + ")"
	case canceled > 0:
		st.State = store.StateCanceled
	case unknown > 0:
		st.State = store.StateFailed // finished in a state we can't classify: don't call it completed
	default:
		st.State = store.StateCompleted
	}
	if running == 0 && pending == 0 {
		st.EndTime = end
	}
	st.Reason = reason
	return st
}

// hetIDRe matches the job ID of one component of a heterogeneous job.
var hetIDRe = regexp.MustCompile(`^\d+\+\d+$`)

// isHetero reports whether every row is a component ("100+0") of a
// heterogeneous job.
func isHetero(rows []taskRow) bool {
	if len(rows) == 0 {
		return false
	}
	for _, r := range rows {
		if !hetIDRe.MatchString(r.ID) {
			return false
		}
	}
	return true
}

// cleanNode trims a node-list value and maps Slurm's placeholders for "no
// node" ("None assigned", "(null)", "None", "n/a") to "".
func cleanNode(v string) string {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "none assigned", "(null)", "none", "n/a":
		return ""
	}
	return v
}

func nonEmptyLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// Restarts validates remoteID and runs
// `sacct -n -P -X -j <id> --format=Restarts`: how often Slurm requeued the
// job under the same ID (the largest value of its rows). No accounting rows
// give 0.
func (s *Slurm) Restarts(ctx context.Context, remoteID string) (int, error) {
	if err := checkRemoteID(remoteID); err != nil {
		return 0, err
	}
	res, err := s.Runner.Run(ctx, []string{"sacct", "-n", "-P", "-X", "-j", remoteID, "--format=Restarts"}, nil)
	if err != nil {
		return 0, fmt.Errorf("sacct: %w", err)
	}
	if res.ExitCode != 0 {
		return 0, cmdError("sacct", res)
	}
	n := 0
	for _, line := range nonEmptyLines(res.Stdout) {
		if v, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && v > n {
			n = v
		}
	}
	return n, nil
}

// Accounting runs `sacct -n -P -X -j <id> -o JobID,State,ExitCode,Reason,Elapsed,Timelimit,ReqMem,MaxRSS,NodeList`
// and, because MaxRSS is only on the step lines, `sacct -n -P -j <id> -o JobID,MaxRSS`
// for the task reported (the largest value wins). Read only. A failed step query leaves MaxRSS 0.
func (s *Slurm) Accounting(ctx context.Context, remoteID string) (backend.Accounting, error) {
	if err := checkRemoteID(remoteID); err != nil {
		return backend.Accounting{}, err
	}
	res, err := s.Runner.Run(ctx, []string{"sacct", "-n", "-P", "-X", "-j", remoteID, "-o", "JobID,State,ExitCode,Reason,Elapsed,Timelimit,ReqMem,MaxRSS,NodeList"}, nil)
	if err != nil {
		return backend.Accounting{}, fmt.Errorf("sacct: %w", err)
	}
	if res.ExitCode != 0 {
		return backend.Accounting{}, cmdError("sacct", res)
	}
	lines := nonEmptyLines(res.Stdout)
	if len(lines) == 0 {
		return backend.Accounting{}, fmt.Errorf("slurm accounting has no record of job %s", remoteID)
	}
	// An array job has one line per task: report the first task that did
	// not complete, and count the states of all of them.
	rows := make([][]string, len(lines))
	pick, counts, order := 0, map[string]int{}, []string{}
	for i, l := range lines {
		f := strings.Split(l, "|")
		for len(f) < 9 {
			f = append(f, "")
		}
		rows[i] = f
		if counts[f[1]] == 0 {
			order = append(order, f[1])
		}
		counts[f[1]]++
		if rows[pick][1] == "COMPLETED" && f[1] != "COMPLETED" {
			pick = i
		}
	}
	f := rows[pick]
	a := backend.Accounting{State: f[1], ExitCode: f[2], Reason: f[3], Elapsed: f[4], Timelimit: f[5], ReqMem: f[6], NodeList: f[8]}
	if len(lines) > 1 {
		var parts []string
		for _, st := range order {
			parts = append(parts, fmt.Sprintf("%d %s", counts[st], st))
		}
		a.State = strings.Join(parts, ", ") + " (details of task " + f[0] + ": " + f[1] + ")"
	}
	a.ReqMemBytes = parseSlurmMem(f[6])
	if st, err := s.Runner.Run(ctx, []string{"sacct", "-n", "-P", "-j", f[0], "-o", "JobID,MaxRSS"}, nil); err == nil && st.ExitCode == 0 {
		for _, l := range nonEmptyLines(st.Stdout) {
			if _, v, ok := strings.Cut(l, "|"); ok {
				if b := parseSlurmMem(v); b > a.MaxRSSBytes {
					a.MaxRSSBytes = b
				}
			}
		}
	}
	return a, nil
}

// parseSlurmMem reads a sacct size such as "123456K", "4000Mn" or "16G" into
// bytes. A per-CPU size (suffix "c") and anything unreadable give 0.
func parseSlurmMem(v string) int64 {
	v = strings.TrimSpace(v)
	if strings.HasSuffix(v, "c") {
		return 0
	}
	v = strings.TrimSuffix(v, "n")
	if v == "" {
		return 0
	}
	mult := int64(1)
	switch v[len(v)-1] {
	case 'K':
		mult = 1 << 10
	case 'M':
		mult = 1 << 20
	case 'G':
		mult = 1 << 30
	case 'T':
		mult = 1 << 40
	}
	if mult > 1 {
		v = v[:len(v)-1]
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0
	}
	return int64(f * float64(mult))
}

// QueuedJobs runs `env TZ=UTC squeue --me -h -o "%i|%j|%Z|%V"` (read only;
// TZ=UTC makes the zone-less submit times UTC whatever the cluster's zone)
// and returns the user's queued or running jobs with name, work folder and
// submit time.
func (s *Slurm) QueuedJobs(ctx context.Context) ([]backend.QueuedJob, error) {
	res, err := s.Runner.Run(ctx, []string{"env", "TZ=UTC", "squeue", "--me", "-h", "-o", "%i|%j|%Z|%V"}, nil)
	if err != nil {
		return nil, fmt.Errorf("squeue: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, cmdError("squeue", res)
	}
	return parseJobList(res.Stdout), nil
}

// JobsSince runs `env TZ=UTC sacct -X -n -P -S <since, UTC> --format=JobID,JobName,WorkDir,Submit`
// (read only) and returns the user's jobs submitted since then, finished
// ones included.
func (s *Slurm) JobsSince(ctx context.Context, since time.Time) ([]backend.QueuedJob, error) {
	res, err := s.Runner.Run(ctx, []string{"env", "TZ=UTC", "sacct", "-X", "-n", "-P", "-S", since.UTC().Format("2006-01-02T15:04:05"), "--format=JobID,JobName,WorkDir,Submit"}, nil)
	if err != nil {
		return nil, fmt.Errorf("sacct: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, cmdError("sacct", res)
	}
	return parseJobList(res.Stdout), nil
}

// parseJobList parses "id|name|workdir|submit" lines (submit in UTC).
func parseJobList(out string) []backend.QueuedJob {
	var jobs []backend.QueuedJob
	for _, line := range nonEmptyLines(out) {
		f := strings.Split(line, "|")
		if len(f) < 4 || !remoteIDRe.MatchString(strings.TrimSpace(f[0])) {
			continue
		}
		jobs = append(jobs, backend.QueuedJob{ID: strings.TrimSpace(f[0]), Name: f[1], WorkDir: f[2], Submitted: parseSlurmTime(f[3], time.UTC)})
	}
	return jobs
}

// Cancel validates remoteID and runs `scancel <id>`.
func (s *Slurm) Cancel(ctx context.Context, remoteID string) error {
	if err := checkRemoteID(remoteID); err != nil {
		return err
	}
	res, err := s.Runner.Run(ctx, []string{"scancel", remoteID}, nil)
	if err != nil {
		return fmt.Errorf("scancel: %w", err)
	}
	if res.ExitCode != 0 {
		return cmdError("scancel", res)
	}
	return nil
}

// Logs validates remoteID, finds the job's output file from
// `scontrol show job <id>` (StdOut=...) or, if the job is gone, from
// `sacct -n -P -X -j <id> --format=WorkDir,JobName` as <WorkDir>/<JobName>-<id>.out,
// and runs `tail -n <tailLines> -- <path>`. tailLines <= 0 means 100.
func (s *Slurm) Logs(ctx context.Context, remoteID string, tailLines int) (string, error) {
	if err := checkRemoteID(remoteID); err != nil {
		return "", err
	}
	if tailLines <= 0 {
		tailLines = 100
	}
	path, err := s.outputPath(ctx, remoteID)
	if err != nil {
		return "", err
	}
	res, err := s.Runner.Run(ctx, []string{"tail", "-n", strconv.Itoa(tailLines), "--", path}, nil)
	if err != nil {
		return "", fmt.Errorf("tail: %w", err)
	}
	if res.ExitCode != 0 {
		return "", cmdError("tail "+path, res)
	}
	return res.Stdout, nil
}

// outputPath finds the job's stdout file.
func (s *Slurm) outputPath(ctx context.Context, remoteID string) (string, error) {
	res, err := s.Runner.Run(ctx, []string{"scontrol", "show", "job", remoteID}, nil)
	if err != nil {
		return "", fmt.Errorf("scontrol: %w", err)
	}
	if res.ExitCode == 0 {
		for _, tok := range strings.Fields(res.Stdout) {
			if p, ok := strings.CutPrefix(tok, "StdOut="); ok && p != "" {
				return p, nil
			}
		}
	}
	res, err = s.Runner.Run(ctx, []string{"sacct", "-n", "-P", "-X", "-j", remoteID, "--format=WorkDir,JobName"}, nil)
	if err != nil {
		return "", fmt.Errorf("sacct: %w", err)
	}
	if res.ExitCode != 0 {
		return "", cmdError("sacct", res)
	}
	line := firstLine(res.Stdout)
	i := strings.LastIndex(line, "|")
	if i <= 0 || i == len(line)-1 {
		return "", fmt.Errorf("cannot locate the output file of slurm job %s", remoteID)
	}
	return strings.TrimRight(line[:i], "/") + "/" + line[i+1:] + "-" + remoteID + ".out", nil
}

var remoteIDRe = regexp.MustCompile(`^[0-9]+(_[0-9]+)?$`)

func checkRemoteID(id string) error {
	if !remoteIDRe.MatchString(id) {
		return fmt.Errorf("invalid slurm job id %q", id)
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

// firstLine returns the first non-empty line of out, trimmed.
func firstLine(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// mapState maps a Slurm job state (suffixes such as "by 1234" already
// removed) onto a store state.
func mapState(state string) (store.JobState, bool) {
	switch {
	case state == "COMPLETED":
		return store.StateCompleted, true
	case strings.HasPrefix(state, "CANCELLED"), state == "REVOKED":
		return store.StateCanceled, true
	case state == "PENDING", state == "REQUEUED", state == "CONFIGURING",
		state == "REQUEUE_HOLD", state == "REQUEUE_FED", state == "RESV_DEL_HOLD", state == "SPECIAL_EXIT":
		return store.StatePending, true
	case state == "RUNNING", state == "COMPLETING", state == "SUSPENDED", state == "RESIZING",
		state == "STOPPED", state == "SIGNALING", state == "STAGE_OUT":
		return store.StateRunning, true
	}
	switch state {
	case "FAILED", "TIMEOUT", "OUT_OF_MEMORY", "NODE_FAIL", "PREEMPTED", "BOOT_FAIL", "DEADLINE":
		return store.StateFailed, true
	}
	return "", false
}

// parseSlurmTime parses Slurm's zone-less timestamps in loc (nil = UTC);
// "Unknown"/"None"/garbage yield the zero time.
func parseSlurmTime(v string, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05", strings.TrimSpace(v), loc)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseTimeLimit parses sinfo/Slurm limits: "infinite"/"UNLIMITED" → 0,
// "D-HH:MM:SS", "HH:MM:SS", "MM:SS", "MM".
func parseTimeLimit(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "infinite", "unlimited", "n/a", "":
		return 0, nil
	}
	days := 0
	rest := v
	if d, r, ok := strings.Cut(v, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad time limit %q", v)
		}
		days, rest = n, r
	}
	parts := strings.Split(rest, ":")
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad time limit %q", v)
		}
		nums[i] = n
	}
	var h, m, sec int
	switch {
	case len(nums) == 3:
		h, m, sec = nums[0], nums[1], nums[2]
	case len(nums) == 2 && days == 0:
		m, sec = nums[0], nums[1]
	case len(nums) == 1 && days == 0:
		m = nums[0]
	case len(nums) == 2: // "D-HH:MM"
		h, m = nums[0], nums[1]
	default: // "D-HH"
		h = nums[0]
	}
	return time.Duration(days)*24*time.Hour + time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second, nil
}

// formatTimeLimit renders d as "HH:MM:SS", or "D-HH:MM:SS" from one day up.
func formatTimeLimit(d time.Duration) string {
	secs := int(d / time.Second)
	days, secs := secs/86400, secs%86400
	if days == 0 {
		return fmt.Sprintf("%02d:%02d:%02d", secs/3600, secs%3600/60, secs%60)
	}
	return fmt.Sprintf("%d-%02d:%02d:%02d", days, secs/3600, secs%3600/60, secs%60)
}

var parenRe = regexp.MustCompile(`\([^)]*\)`)

// gresGPUTypes extracts GPU types from a GRES list such as
// "gpu:a100:4,lscratch:100" or "gpu:h100:8(S:0-1)".
func gresGPUTypes(gres string) []string {
	var out []string
	for _, e := range strings.Split(parenRe.ReplaceAllString(gres, ""), ",") {
		f := strings.Split(strings.TrimSpace(e), ":")
		if len(f) < 3 || f[0] != "gpu" || f[1] == "" {
			continue
		}
		if _, err := strconv.Atoi(f[1]); err == nil {
			continue
		}
		out = append(out, f[1])
	}
	return out
}

var _ backend.Backend = (*Slurm)(nil)

// Jobs implements backend.Lister. Active jobs come from
// `env TZ=UTC squeue --me -h -o "%i|%T|%P|%N|%r|%V|%S|%j"`; when since is
// non-zero, finished jobs come from
// `env TZ=UTC sacct -X -n -P -S <since UTC> --format=JobID,State,Partition,NodeList,Submit,Start,End,JobName`
// (rows still PENDING/RUNNING or already seen in squeue are dropped). The job
// name is the last field, so a '|' inside it is tolerated. If sacct exits
// non-zero the active jobs are returned with backend.ErrNoAccounting.
func (s *Slurm) Jobs(ctx context.Context, since time.Time, limit int) ([]backend.RemoteJob, error) {
	if limit <= 0 {
		limit = 50
	}
	res, err := s.Runner.Run(ctx, []string{"env", "TZ=UTC", "squeue", "--me", "-h", "-o", "%i|%T|%P|%N|%r|%V|%S|%j"}, nil)
	if err != nil {
		return nil, fmt.Errorf("squeue: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, cmdError("squeue", res)
	}
	var active []backend.RemoteJob
	seen := map[string]bool{}
	for _, line := range nonEmptyLines(res.Stdout) {
		f := strings.SplitN(line, "|", 8)
		if len(f) < 8 {
			continue
		}
		j := s.remoteJob(f, false)
		if j.ID == "" {
			continue
		}
		seen[j.ID] = true
		if parent, ok := arrayParent(j.ID); ok {
			seen[parent] = true // finished tasks of a live array stay out of the finished list
		}
		active = append(active, j)
	}
	active = foldHetero(active, seen)
	var noAcct error
	var done []backend.RemoteJob
	if !since.IsZero() {
		argv := []string{"env", "TZ=UTC", "sacct", "-X", "-n", "-P",
			"-S", since.UTC().Format("2006-01-02T15:04:05"),
			"--format=JobID,State,Partition,NodeList,Submit,Start,End,JobName"}
		sres, err := s.Runner.Run(ctx, argv, nil)
		if err != nil {
			return nil, fmt.Errorf("sacct: %w", err)
		}
		if sres.ExitCode != 0 {
			noAcct = backend.ErrNoAccounting
		} else {
			lines := nonEmptyLines(sres.Stdout)
			if len(lines) > maxSacctLines {
				// sacct lists oldest first: keep the newest rows (the tail).
				lines, noAcct = lines[len(lines)-maxSacctLines:], backend.ErrTruncated
			}
			groups := map[string]*backend.RemoteJob{}
			counts := map[string]*taskTally{}
			var order []string
			hetGroups := map[string][]backend.RemoteJob{}
			hetBusy := map[string]bool{}
			var hetOrder []string
			busy := map[string]bool{} // parents of arrays that still have waiting or running tasks
			for _, line := range lines {
				f := strings.SplitN(line, "|", 8)
				if len(f) < 8 {
					continue
				}
				j := s.remoteJob(f, true)
				if j.ID == "" {
					continue
				}
				if hp, ok := hetParent(j.ID); ok {
					// A component of a heterogeneous job: collect, fold below.
					if j.State == store.StatePending || j.State == store.StateRunning {
						hetBusy[hp] = true
					}
					if _, ok := hetGroups[hp]; !ok {
						hetOrder = append(hetOrder, hp)
					}
					hetGroups[hp] = append(hetGroups[hp], j)
					continue
				}
				parent, isTask := arrayParent(j.ID)
				if j.State == store.StatePending || j.State == store.StateRunning {
					if isTask {
						busy[parent] = true
					}
					continue
				}
				if !isTask {
					if !seen[j.ID] {
						seen[j.ID] = true
						done = append(done, j)
					}
					continue
				}
				g, ok := groups[parent]
				if !ok {
					first := j
					first.ID = parent
					groups[parent], counts[parent] = &first, &taskTally{}
					g = &first
					order = append(order, parent)
				}
				counts[parent].add(j)
				mergeTask(g, j)
			}
			for _, hp := range hetOrder {
				if seen[hp] || hetBusy[hp] {
					continue
				}
				seen[hp] = true
				done = append(done, foldComponents(hp, hetGroups[hp]))
			}
			for _, parent := range order {
				if seen[parent] || busy[parent] {
					continue
				}
				seen[parent] = true
				g := groups[parent]
				g.State, g.Reason = counts[parent].summary()
				g.Node = ""
				done = append(done, *g)
			}
		}
	}
	key := func(j backend.RemoteJob) time.Time {
		for _, t := range []time.Time{j.End, j.Start, j.Submit} {
			if !t.IsZero() {
				return t
			}
		}
		return time.Time{}
	}
	slices.SortStableFunc(done, func(a, b backend.RemoteJob) int { return key(b).Compare(key(a)) })
	out := append(active, done...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, noAcct
}

// remoteJob builds a RemoteJob from split fields. Both formats start with
// "id|state|partition|node" and end with the name; squeue (accounting false)
// has "reason|submit|start" between, sacct "submit|start|end".
func (s *Slurm) remoteJob(f []string, accounting bool) backend.RemoteJob {
	j := backend.RemoteJob{ID: strings.TrimSpace(f[0]), Name: strings.TrimSpace(f[7]),
		Partition: strings.TrimSpace(f[2]), Node: cleanNode(f[3])}
	var raw string
	if st := strings.Fields(f[1]); len(st) > 0 {
		raw = st[0]
		js, ok := mapState(raw)
		j.State = js
		switch {
		case !ok:
			j.State, j.Reason = store.StatePending, "unknown state "+raw
			if accounting && !parseSlurmTime(f[6], s.Location).IsZero() {
				j.State = store.StateFailed // ended in a state we don't know: finished, not waiting
			}
		case reasonIsState(raw, js):
			j.Reason = raw
		}
	}
	if accounting {
		j.Submit, j.Start, j.End = parseSlurmTime(f[4], s.Location), parseSlurmTime(f[5], s.Location), parseSlurmTime(f[6], s.Location)
		return j
	}
	if r := strings.TrimSpace(f[4]); r != "None" && j.State == store.StatePending && j.Reason == "" {
		j.Reason = r
	}
	j.Submit = parseSlurmTime(f[5], s.Location)
	if j.State != store.StatePending {
		j.Start = parseSlurmTime(f[6], s.Location)
	}
	return j
}

// maxSacctLines caps how many sacct rows Jobs reads.
const maxSacctLines = 5000

// reasonIsState reports whether a job's raw Slurm state is worth showing as
// the reason: any failed state, and any state other than the plain
// RUNNING, PENDING, COMPLETED, CANCELLED*, COMPLETING and CONFIGURING (so
// SUSPENDED, STOPPED, SIGNALING, REQUEUED and so on show up).
func reasonIsState(raw string, js store.JobState) bool {
	switch {
	case js == store.StateFailed:
		return true
	case raw == "RUNNING", raw == "PENDING", raw == "COMPLETED", raw == "COMPLETING", raw == "CONFIGURING",
		strings.HasPrefix(raw, "CANCELLED"):
		return false
	}
	return true
}

// arrayParent splits a job ID such as "123_4" into its array parent "123".
// ok is false for plain jobs. A collapsed range "123_[5-9]" counts too.
func arrayParent(id string) (string, bool) {
	p, _, ok := strings.Cut(id, "_")
	return p, ok && p != ""
}

// taskTally counts the finished tasks of one array.
type taskTally struct {
	total, completed, failed, canceled, unknown int
	firstFail, firstUnknown                     string
}

func (t *taskTally) add(j backend.RemoteJob) {
	n := 1
	t.total += n
	if strings.HasPrefix(j.Reason, "unknown state ") {
		t.unknown++
		if t.firstUnknown == "" {
			t.firstUnknown = strings.TrimPrefix(j.Reason, "unknown state ")
		}
		return
	}
	switch j.State {
	case store.StateCompleted:
		t.completed++
	case store.StateCanceled:
		t.canceled++
	default:
		t.failed++
		if t.firstFail == "" {
			t.firstFail = j.Reason
		}
	}
}

// summary returns the array's state (failed if any task failed, else
// canceled if any was, else completed) and a "N tasks: ..." reason.
func (t *taskTally) summary() (store.JobState, string) {
	st := store.StateCompleted
	switch {
	case t.failed > 0, t.unknown > 0:
		st = store.StateFailed
	case t.canceled > 0:
		st = store.StateCanceled
	}
	parts := []string{fmt.Sprintf("%d completed", t.completed)}
	if t.failed > 0 {
		f := fmt.Sprintf("%d failed", t.failed)
		if t.firstFail != "" {
			f += " (e.g. " + t.firstFail + ")"
		}
		parts = append(parts, f)
	}
	if t.canceled > 0 {
		parts = append(parts, fmt.Sprintf("%d canceled", t.canceled))
	}
	if t.unknown > 0 {
		parts = append(parts, fmt.Sprintf("%d unknown (%s)", t.unknown, t.firstUnknown))
	}
	word := "tasks"
	if t.total == 1 {
		word = "task"
	}
	return st, fmt.Sprintf("%d %s: %s", t.total, word, strings.Join(parts, ", "))
}

// mergeTask widens group g (an array's first task) with task j: earliest
// submit and start, latest end.
func mergeTask(g *backend.RemoteJob, j backend.RemoteJob) {
	if !j.Submit.IsZero() && (g.Submit.IsZero() || j.Submit.Before(g.Submit)) {
		g.Submit = j.Submit
	}
	if !j.Start.IsZero() && (g.Start.IsZero() || j.Start.Before(g.Start)) {
		g.Start = j.Start
	}
	if j.End.After(g.End) {
		g.End = j.End
	}
}

// hetParent splits a heterogeneous-job component ID "100+1" into "100".
func hetParent(id string) (string, bool) {
	if !hetIDRe.MatchString(id) {
		return "", false
	}
	p, _, _ := strings.Cut(id, "+")
	return p, true
}

// foldHetero replaces the component rows ("100+0", "100+1") of each
// heterogeneous job in jobs by one row, in the position of the first
// component, and marks the job ID in seen.
func foldHetero(jobs []backend.RemoteJob, seen map[string]bool) []backend.RemoteJob {
	groups := map[string][]backend.RemoteJob{}
	for _, j := range jobs {
		if hp, ok := hetParent(j.ID); ok {
			groups[hp] = append(groups[hp], j)
		}
	}
	if len(groups) == 0 {
		return jobs
	}
	var out []backend.RemoteJob
	for _, j := range jobs {
		hp, ok := hetParent(j.ID)
		if !ok {
			out = append(out, j)
			continue
		}
		if seen[hp] {
			continue
		}
		seen[hp] = true
		out = append(out, foldComponents(hp, groups[hp]))
	}
	return out
}

// foldComponents merges the components of one heterogeneous job into a
// single row. The state and the "N components" reason come from
// aggregateTasks, the same folding Status uses.
func foldComponents(id string, parts []backend.RemoteJob) backend.RemoteJob {
	g := parts[0]
	g.ID = id
	rows := make([]taskRow, 0, len(parts))
	for _, p := range parts {
		raw := p.Reason
		if raw == "" {
			raw = string(p.State)
		}
		rows = append(rows, taskRow{ID: p.ID, Raw: raw, State: p.State, Known: true,
			Reason: p.Reason, Node: p.Node, Start: p.Start, End: p.End, N: 1})
		mergeTask(&g, p)
	}
	st := aggregateTasks(rows)
	g.State, g.Reason, g.Node = st.State, st.Reason, st.Node
	return g
}
