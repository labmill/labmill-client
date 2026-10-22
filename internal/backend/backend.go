// Package backend defines the scheduler-neutral interface Labmill's tools use:
// Slurm (package slurm) implements it.
//
// Backends never run arbitrary model-written shell. They build argv vectors
// themselves from validated fields and run them through a Runner. The only
// free-form text is the job script/command the user reviewed and approved,
// and it is passed as data (stdin or an uploaded file), never spliced into a
// command line.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"labmill-client/internal/rules"
	"labmill-client/internal/store"
)

// Result is a finished command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner runs one command. Remote runners (SSH exec) execute through the
// remote login shell, so they must pass Quote(argv); local runners exec argv
// directly. A non-zero exit is not an error: it is reported in Result. err is
// for failing to run at all (connection lost, ctx canceled).
type Runner interface {
	Run(ctx context.Context, argv []string, stdin io.Reader) (Result, error)
}

// Quote renders argv as one POSIX shell command line in which every argument
// is a literal: each is single-quoted, with embedded single quotes written as
// '\” . Empty arguments become ”. Arguments made only of safe characters
// ([A-Za-z0-9@%_+=:,./-]) may be left unquoted for readability, except that
// an argument containing '=' and not starting with '-' is quoted (zsh
// equals-expansion, and NAME=value as argv[0] would be an assignment). It
// targets POSIX login shells (sh, bash, dash, ksh, zsh), not csh or fish.
func Quote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = quoteArg(a)
	}
	return strings.Join(parts, " ")
}

func quoteArg(a string) string {
	if a == "" {
		return "''"
	}
	safe := !(a[0] != '-' && strings.IndexByte(a, '=') >= 0)
	for i := 0; i < len(a); i++ {
		if !isSafeByte(a[i]) {
			safe = false
			break
		}
	}
	if safe {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}

func isSafeByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("@%_+=:,./-", c) >= 0
}

// Partition is one queue/node pool the user can submit to.
type Partition struct {
	Name      string
	Default   bool
	State     string        // scheduler's state, e.g. "up"
	MaxTime   time.Duration // 0 means unlimited or unknown
	GPUTypes  []string      // e.g. ["a100", "h100"], from GRES; empty if none
	Nodes     int           // node count
	IdleNodes int
}

// ClusterInfo summarizes what a cluster offers right now.
type ClusterInfo struct {
	Partitions []Partition
}

// JobSpec is what the agent asks for. Backends render it into a script.
type JobSpec struct {
	Name      string            // short job name; [A-Za-z0-9._-]{1,64}
	Partition string            // "" means the cluster default
	GPUs      int               // 0 means none
	GPUType   string            // "" means any
	CPUs      int               // 0 means scheduler default
	MemoryGB  int               // 0 means scheduler default
	Time      time.Duration     // 0 means partition default
	Command   string            // what to run, e.g. "python train.py --epochs 10"; shown to the user; no line may start with "#SBATCH" (directives belong in the header); no control characters except tab and newline, no DEL/C1 controls, no bidi/zero-width/format characters (U+200B-U+200F, U+202A-U+202E, U+2066-U+2069, U+FEFF)
	WorkDir   string            // remote working directory: absolute, ASCII, matching ^/[A-Za-z0-9._/+@,:=~-]*$
	Env       map[string]string // extra environment; names [A-Za-z_][A-Za-z0-9_]*; values follow the Command character rules
	// Nodes is the number of nodes (#SBATCH --nodes); 0 means the scheduler
	// default. Slurm only.
	Nodes int `json:",omitempty"`
	// QOS is the Slurm QOS (#SBATCH --qos); "" means none. Set by Labmill from
	// the site profile's preemptible resource (M61). Slurm only.
	QOS string `json:",omitempty"`
	// MailOnEnd adds #SBATCH --mail-type=END,FAIL (no --mail-user: Slurm
	// mails the site's default address). Set from the "Email me when a
	// Slurm job ends" setting (M64.3). Slurm only.
	MailOnEnd bool `json:",omitempty"`
	// Requeue adds #SBATCH --requeue, so Slurm puts a preempted job back in
	// the queue (M61). Slurm only.
	Requeue bool `json:",omitempty"`
	// LocalScratch, when set, makes the Slurm script stage the work folder
	// in to node-local scratch, run there, copy everything back to WorkDir
	// and remove its own local folder after a good copy (M61). It needs a
	// single node (Nodes 0 or 1); set by Labmill from the site profile.
	LocalScratch *rules.LocalScratch `json:",omitempty"`
	// Environment is the software the job needs: modules, a venv, a conda
	// environment or an Apptainer image (M67.1).
	Environment *Environment `json:",omitempty"`
}

// ErrInvalidSpec wraps every JobSpec validation failure.
var ErrInvalidSpec = errors.New("invalid job spec")

// Validate checks the fields' formats documented on JobSpec (name pattern,
// non-negative counts, absolute WorkDir, env names, partition/GPU type
// matching [A-Za-z0-9._-]*, non-empty Command). Errors wrap ErrInvalidSpec.
func (s JobSpec) Validate() error {
	if !nameRe.MatchString(s.Name) {
		return invalid("name %q must match [A-Za-z0-9._-]{1,64}", s.Name)
	}
	if !tokenRe.MatchString(s.Partition) {
		return invalid("partition %q must match [A-Za-z0-9._-]*", s.Partition)
	}
	if !tokenRe.MatchString(s.GPUType) {
		return invalid("GPU type %q must match [A-Za-z0-9._-]*", s.GPUType)
	}
	if !tokenRe.MatchString(s.QOS) {
		return invalid("qos %q must match [A-Za-z0-9._-]*", s.QOS)
	}
	if s.Nodes < 0 {
		return invalid("nodes must not be negative")
	}
	if s.LocalScratch != nil {
		if s.Nodes > 1 {
			return invalid("node-local scratch needs a single-node job (nodes is %d); use the shared scratch or the job folder", s.Nodes)
		}
		if err := (rules.SlurmHints{LocalScratch: s.LocalScratch}).Validate(); err != nil {
			return invalid("%v", err)
		}
	}
	if s.Environment != nil {
		if err := s.Environment.Validate(); err != nil {
			return err
		}
	}
	if s.GPUs < 0 || s.CPUs < 0 || s.MemoryGB < 0 {
		return invalid("GPUs, CPUs and memory must not be negative")
	}
	if s.Time < 0 {
		return invalid("time must not be negative")
	}
	if strings.TrimSpace(s.Command) == "" {
		return invalid("command is empty")
	}
	if strings.ContainsRune(s.Command, 0) {
		return invalid("command contains a NUL byte")
	}
	if err := checkText("command", s.Command, true); err != nil {
		return err
	}
	if !strings.HasPrefix(s.WorkDir, "/") {
		return invalid("work dir %q must be an absolute path", s.WorkDir)
	}
	// WorkDir is written into #SBATCH lines, where sbatch's parser treats
	// #, quotes, backslashes and % specially; keep it to a boring alphabet.
	if !workDirRe.MatchString(s.WorkDir) {
		return invalid("work dir %q must match ^/[A-Za-z0-9._/+@,:=~-]*$", s.WorkDir)
	}
	for _, line := range strings.Split(s.Command, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#SBATCH") {
			return invalid("command must not contain #SBATCH directives (they belong in the header)")
		}
	}
	for k, v := range s.Env {
		if !envNameRe.MatchString(k) {
			return invalid("env name %q must match [A-Za-z_][A-Za-z0-9_]*", k)
		}
		if strings.ContainsRune(v, 0) {
			return invalid("env %s: value contains a NUL byte", k)
		}
		if err := checkText("env "+k, v, true); err != nil {
			return err
		}
	}
	return nil
}

var (
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	imageRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)
	tokenRe   = regexp.MustCompile(`^([A-Za-z0-9._][A-Za-z0-9._-]*)?$`) // no leading '-': it would read as an option
	workDirRe = regexp.MustCompile(`^/[A-Za-z0-9._/+@,:=~-]*$`)
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// checkText rejects characters that can hide or rewrite text shown to the
// user: C0 controls (except tab, and newline when allowNewline), DEL, C1
// controls and Unicode format/bidi/zero-width characters.
func checkText(field, v string, allowNewline bool) error {
	// Invalid UTF-8 would let raw C1 bytes (e.g. 0x9B, the 8-bit CSI) reach
	// the approval view, since range turns them into U+FFFD.
	if !utf8.ValidString(v) {
		return invalid("%s is not valid UTF-8", field)
	}
	for _, r := range v {
		switch {
		case r == '\t', r == '\n' && allowNewline:
		case r == '\r':
			return invalid("%s contains a carriage return (use Unix line endings)", field)
		case unicode.Is(unicode.Cc, r), unicode.Is(unicode.Cf, r), unicode.In(r, unicode.Zl, unicode.Zp):
			// Controls, format characters (bidi, zero-width, soft hyphen, tag
			// characters, ZWJ) and line/paragraph separators could make the
			// approved text display differently from what runs.
			return invalid("%s contains a control or invisible character (U+%04X)", field, r)
		}
	}
	return nil
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSpec, fmt.Sprintf(format, a...))
}

// Status is the scheduler's view of one job, mapped onto store states.
type Status struct {
	RemoteID  string
	State     store.JobState // pending/running/completed/failed/canceled
	Reason    string         // scheduler's reason or detail, e.g. "Resources", "OutOfMemory"
	ExitCode  int
	Signal    int // the signal part of Slurm's "exit:signal", 0 when none
	Node      string
	StartTime time.Time // zero if unknown
	EndTime   time.Time // zero if unknown
}

// StartEstimate is the scheduler's expected start of a job that is not
// submitted (Slurm `sbatch --test-only`). Start is the scheduler's own
// zone-less local time text (for example 2026-10-15T14:20:00).
type StartEstimate struct {
	Start     string
	Nodes     string // the nodes it would run on, "" when not reported
	Partition string // "" when not reported
}

// StartEstimator is implemented by backends that can estimate when a script
// would start without submitting it (Slurm). It only reads. An error means
// no estimate (callers treat it as "unknown", not as a failure).
type StartEstimator interface {
	EstimateStart(ctx context.Context, script string) (StartEstimate, error)
}

// ErrNoAccounting is returned by Lister.Jobs together with the active jobs
// when the scheduler's accounting (sacct) isn't available, so finished jobs
// can't be listed. Callers treat it as a note, not a failure.
var ErrNoAccounting = errors.New("finished jobs aren't available on this cluster")

// ErrTruncated is returned by Lister.Jobs together with the jobs found when
// the scheduler's history was too long to read completely, so older finished
// jobs may be missing. Callers treat it as a note, not a failure.
var ErrTruncated = errors.New("the job history is long, so some older jobs may be missing")

// Lister is implemented by backends that can list the user's own jobs on
// the scheduler itself, including jobs not submitted through Labmill.
type Lister interface {
	// Jobs returns the user's active jobs and, when since is non-zero, also
	// jobs that finished after since; at most limit entries (limit <= 0
	// means 50), active first, then finished newest first. With
	// ErrNoAccounting or ErrTruncated the result still holds the jobs found.
	Jobs(ctx context.Context, since time.Time, limit int) ([]RemoteJob, error)
}

// RemoteJob is one row of Lister.Jobs. Times are zero when unknown.
type RemoteJob struct {
	ID, Name           string
	State              store.JobState // same mapping as Status
	Partition          string         // Slurm partition
	Node               string         // empty when none assigned (placeholders dropped)
	Reason             string         // e.g. squeue's pending reason; empty otherwise
	Submit, Start, End time.Time
}

// Backend is one scheduler.
type Backend interface {
	// Type is "slurm".
	Type() string
	// Info reports partitions/node pools and their state.
	Info(ctx context.Context) (ClusterInfo, error)
	// Render turns spec into the exact script or command that will be
	// submitted. It is shown to the user for approval (and editing).
	Render(spec JobSpec) (string, error)
	// Submit submits script (as approved, possibly edited by the user) and
	// returns the scheduler's job ID.
	Submit(ctx context.Context, script string, spec JobSpec) (remoteID string, err error)
	// Status reports the job's current state, also after it has finished.
	Status(ctx context.Context, remoteID string) (Status, error)
	// Cancel cancels the job.
	Cancel(ctx context.Context, remoteID string) error
	// Logs returns the last tailLines lines of the job's output.
	Logs(ctx context.Context, remoteID string, tailLines int) (string, error)
}

// SplitWords splits a command line the user approved (and may have edited)
// back into argv, POSIX-style but with no expansions: single quotes are
// literal, double quotes allow \" \\ \$ \` escapes, a backslash outside quotes
// escapes the next character, and unquoted whitespace separates words. It
// refuses (error) unquoted shell metacharacters ; & | < > ( ) ` $ and #
// (a shell would start a comment), newlines outside quotes, a backslash-newline
// inside double quotes, NUL bytes anywhere, unterminated quotes, and a
// trailing backslash, so an approved command can never become a pipeline, a
// second command or hide text in a comment. The characters * ? [ ~ { are
// passed through literally: there is no globbing, tilde or brace expansion.
// It scans bytes (every metacharacter is ASCII and never part of a UTF-8
// multi-byte sequence), so SplitWords(Quote(argv)) == argv for every argv
// without NUL bytes, valid UTF-8 or not.
func SplitWords(line string) ([]string, error) {
	if strings.IndexByte(line, 0) >= 0 {
		return nil, errors.New("NUL byte is not allowed")
	}
	var (
		words   []string
		cur     []byte
		inWord  bool
		bad     = func(c byte) error { return fmt.Errorf("unquoted shell metacharacter %q is not allowed", rune(c)) }
		flushed = func() {
			if inWord {
				words = append(words, string(cur))
				cur = cur[:0]
				inWord = false
			}
		}
	)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch c {
		case ' ', '\t':
			flushed()
		case '\n', '\r':
			return nil, fmt.Errorf("newline outside quotes is not allowed")
		case ';', '&', '|', '<', '>', '(', ')', '`', '$', '#':
			return nil, bad(c)
		case '\\':
			i++
			if i >= len(line) {
				return nil, errors.New("trailing backslash")
			}
			if line[i] == '\n' || line[i] == '\r' {
				return nil, errors.New("newline outside quotes is not allowed")
			}
			cur = append(cur, line[i])
			inWord = true
		case '\'':
			inWord = true
			i++
			for ; i < len(line) && line[i] != '\''; i++ {
				cur = append(cur, line[i])
			}
			if i >= len(line) {
				return nil, errors.New("unterminated single quote")
			}
		case '"':
			inWord = true
			i++
			closed := false
			for ; i < len(line); i++ {
				d := line[i]
				if d == '"' {
					closed = true
					break
				}
				if d == '$' || d == '`' {
					return nil, fmt.Errorf("unescaped %q inside double quotes is not allowed", rune(d))
				}
				if d == '\\' {
					if i+1 >= len(line) {
						break
					}
					switch n := line[i+1]; n {
					case '"', '\\', '$', '`':
						cur = append(cur, n)
						i++
						continue
					case '\n':
						return nil, errors.New("backslash-newline inside double quotes is not allowed")
					}
				}
				cur = append(cur, d)
			}
			if !closed {
				return nil, errors.New("unterminated double quote")
			}
		default:
			cur = append(cur, c)
			inWord = true
		}
	}
	flushed()
	return words, nil
}

// ValidImage reports whether s is a usable container image reference: the
// JobSpec.Image rule (^[A-Za-z0-9][A-Za-z0-9._/:@-]*$, so no leading '-',
// spaces or control characters). The empty string is not valid.
func ValidImage(s string) bool { return imageRe.MatchString(s) }

// CheckText applies the character rules of a JobSpec command to any text that
// is shown to the user for approval and then run or stored (for example the
// extra script files of a job): valid UTF-8, no carriage returns, controls,
// DEL, C1 controls or invisible/bidi characters; newlines only when
// allowNewline. field names the text in the error, which wraps ErrInvalidSpec.
func CheckText(field, v string, allowNewline bool) error {
	return checkText(field, v, allowNewline)
}

// RestartCounter is implemented by backends whose scheduler requeues a job
// under the same ID (Slurm: sacct's Restarts). The job monitor counts those
// requeues as attempts before it resubmits a preempted job (M61.3).
type RestartCounter interface {
	// Restarts is how often the scheduler requeued the job itself.
	Restarts(ctx context.Context, remoteID string) (int, error)
}

// Accounting is what the scheduler's accounting records about a finished
// job (Slurm: sacct), for diagnosing a failure.
type Accounting struct {
	State, ExitCode, Reason string
	Elapsed, Timelimit      string
	NodeList                string
	ReqMem                  string // as sacct prints it, e.g. "4000Mn" or "16G"
	// ReqMemBytes is ReqMem in bytes as sacct gives it (per node for "n");
	// 0 when unknown or per CPU. MaxRSSBytes is the largest MaxRSS of any
	// single task of any step, not a total; 0 when none is recorded.
	ReqMemBytes, MaxRSSBytes int64
}

// Accountant is implemented by backends that can read the accounting
// record of a job (Slurm). It only reads.
type Accountant interface {
	Accounting(ctx context.Context, remoteID string) (Accounting, error)
}

// QueuedJob is one of the user's queued or running scheduler jobs.
type QueuedJob struct {
	ID        string
	Name      string
	WorkDir   string
	Submitted time.Time // zero when unknown
}

// QueueFinder is implemented by backends that can list the user's queued or
// running jobs (Slurm: squeue --me). The job monitor uses it, read only, to
// find a resubmission that a crash left unrecorded (M61.3).
type QueueFinder interface {
	QueuedJobs(ctx context.Context) ([]QueuedJob, error)
}

// HistoryFinder is implemented by backends that can also list jobs that have
// already finished (Slurm: sacct), used after the queue showed nothing.
type HistoryFinder interface {
	JobsSince(ctx context.Context, since time.Time) ([]QueuedJob, error)
}

// ErrSubmitUncertain wraps the error of a Submit that may have reached the
// scheduler (the connection dropped or timed out while sbatch ran, or its
// answer was unreadable): the job may exist, so the caller must not submit
// again. A definite refusal (sbatch exits non-zero with a message, the work
// folder can't be created) does not wrap it.
var ErrSubmitUncertain = errors.New("the submit may or may not have reached the scheduler")
