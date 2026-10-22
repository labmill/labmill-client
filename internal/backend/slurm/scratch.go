package slurm

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"labmill-client/internal/backend"
)

// localScratchBody is the end of the batch script for a single-node job on
// node-local scratch (M61; placement-and-preemption.md): copy the work
// folder to the node's local disk, run the command there, copy everything
// back to the work folder on shared storage, and remove the job's own local
// folder only after that copy worked. The trap runs on success, failure,
// timeout and scancel (not on a node crash or kill -9); the command runs in
// the background and the script waits for it, so a signal's trap fires at
// once. The current output file is excluded from both copies: Slurm writes
// it in the work folder while the job runs. Nothing is deleted on a failed
// copy: the job then fails and says where the results were left.
func localScratchBody(spec backend.JobSpec) string {
	l := spec.LocalScratch
	base := ""
	switch {
	case l.EnvVar != "" && l.Path != "":
		base = "${" + l.EnvVar + ":-" + l.Path + "}"
	case l.EnvVar != "":
		base = "${" + l.EnvVar + `:?no node-local folder: ` + l.EnvVar + ` is not set}`
	case strings.HasPrefix(l.Path, "$"):
		// A variable path fails at once when the variable is unset.
		name, rest, _ := strings.Cut(l.Path[1:], "/")
		base = "${" + name + `:?no node-local folder: ` + name + ` is not set}`
		if rest != "" {
			base += "/" + rest
		}
	default:
		base = l.Path
	}
	var b strings.Builder
	b.WriteString("\n# Labmill: node-local scratch. Stage in, run, stage out; the trap removes the\n")
	b.WriteString("# local folder only after the copy back worked. Results end up in the work folder.\n")
	b.WriteString("set -eo pipefail\n")
	fmt.Fprintf(&b, "LABMILL_JOB=%s\n", backend.Quote([]string{strings.TrimRight(spec.WorkDir, "/")}))
	fmt.Fprintf(&b, "LABMILL_WORK=\"%s/labmill_${SLURM_JOB_ID}\"\n", base)
	b.WriteString("LABMILL_LOG=\"${SLURM_JOB_NAME}-${SLURM_JOB_ID}.out\"\n")
	// A requeued run may land on the node of the killed run: start clean.
	b.WriteString("[ -z \"${SLURM_RESTART_COUNT:-}\" ] || rm -rf -- \"$LABMILL_WORK\"\n")
	b.WriteString("mkdir -p \"$LABMILL_WORK/job\"\n")
	// --update: a file the program changed straight in the shared folder
	// (an absolute path) is newer there and is not replaced by the stale
	// staged-in copy.
	b.WriteString(`trap 'if rsync -a --update --exclude="/$LABMILL_LOG" "$LABMILL_WORK/job/" "$LABMILL_JOB/"; then rm -rf -- "$LABMILL_WORK"; else echo "Labmill: stage-out failed; results kept in $LABMILL_WORK on $(hostname)" >&2; exit 1; fi' EXIT` + "\n")
	b.WriteString("rsync -a --exclude=\"/$LABMILL_LOG\" \"$LABMILL_JOB/\" \"$LABMILL_WORK/job/\"\n")
	b.WriteString("cd \"$LABMILL_WORK/job\"\n")
	// USR1 arrives signalLead seconds before the time limit (#SBATCH
	// --signal): TERM the program's whole process group (set -m gives the
	// background command its own group; $! alone would hit only the
	// subshell), wait for it at most half the lead (then KILL), and leave;
	// the EXIT trap stages out. "|| true" keeps set -e from aborting the trap.
	lead := signalLead(spec.Time)
	fmt.Fprintf(&b, `trap 'kill -TERM -- "-$LABMILL_PID" 2>/dev/null || true; ( sleep %d; kill -KILL -- "-$LABMILL_PID" ) 2>/dev/null & LABMILL_DOG=$!; wait "$LABMILL_PID" 2>/dev/null || true; pkill -P "$LABMILL_DOG" 2>/dev/null || true; kill "$LABMILL_DOG" 2>/dev/null || true; exit 143' USR1`+"\n", lead/2)
	b.WriteString("set -m\n")
	b.WriteString("(\n")
	// The command keeps the error behaviour it always had.
	b.WriteString("set +e +o pipefail\n")
	b.WriteString(spec.Command)
	if !strings.HasSuffix(spec.Command, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(") &\n")
	b.WriteString("LABMILL_PID=$!\n")
	b.WriteString("set +m\n")
	b.WriteString("wait \"$LABMILL_PID\"\n")
	return b.String()
}

// signalLead is how long before the time limit the script gets USR1 so it
// can stage out: 10% of the limit, between 60 and 600 seconds (120 when the
// job has no explicit limit).
func signalLead(limit time.Duration) int {
	if limit <= 0 {
		return 120
	}
	return int(min(max(limit/10, time.Minute), 10*time.Minute) / time.Second)
}

var startAtRe = regexp.MustCompile(`to start at (\S+)(?: using \d+ processors? on nodes? (\S+))?(?: in partition (\S+))?`)

// EstimateStart runs `sbatch --test-only` with script on stdin: Slurm
// validates the script and says when and where it would start, and submits
// nothing. The estimate is read from sbatch's message ("Job N to start at
// <time> using C processors on nodes X in partition P", printed on stderr).
// A non-zero exit or a message without a start time is an error; callers
// treat that as "no estimate".
func (s *Slurm) EstimateStart(ctx context.Context, script string) (backend.StartEstimate, error) {
	if strings.TrimSpace(script) == "" {
		return backend.StartEstimate{}, errors.New("sbatch --test-only: empty script")
	}
	script = strings.ReplaceAll(script, "\r\n", "\n")
	res, err := s.Runner.Run(ctx, []string{"sbatch", "--test-only"}, strings.NewReader(script))
	if err != nil {
		return backend.StartEstimate{}, fmt.Errorf("sbatch --test-only: %w", err)
	}
	if res.ExitCode != 0 {
		return backend.StartEstimate{}, cmdError("sbatch --test-only", res)
	}
	m := startAtRe.FindStringSubmatch(res.Stderr + "\n" + res.Stdout)
	if m == nil {
		return backend.StartEstimate{}, errors.New("sbatch --test-only gave no start time")
	}
	return backend.StartEstimate{Start: m[1], Nodes: m[2], Partition: m[3]}, nil
}
