// Package queueplan chooses the Slurm queue for a job and what to try next when it fails. It is pure Go on probe
// results (no network, no shell, only the standard library), so the client can copy it. Design: docs/preemptible-qos.md.
// Owner rules (2026-10-26): a preemptible queue goes first when the probe allows it and the job qualifies; the normal
// queue (Q1) is always the last step, so a queue is always obtained.
package queueplan

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Technique ids of docs/preemptible-qos.md, section 2a.
const (
	Q1 = "Q1" // default partition and QOS
	Q2 = "Q2" // other partition
	Q3 = "Q3" // preemptible partition or QOS
	Q4 = "Q4" // private owner QOS or partition
	Q5 = "Q5" // per-lab QOS
	Q6 = "Q6" // priority QOS
)

// Candidate is a queue the probe saw.
type Candidate struct {
	Name        string // partition or QOS name
	IsQOS       bool
	Technique   string // Q2 to Q6
	MaxWall     time.Duration
	Allowed     bool // the association lists it for the user. Set only from a readable sacctmgr, never from a hidden one.
	ByRequest   bool // the profile says access is by request
	StartsSoon  bool // the probe shows an earlier start than the normal queue
	CancelMode  bool // preemption cancels instead of requeueing
	UsageFactor float64
	IsDefault   bool // the user's default QOS (Q5)
}

// Probe is the result of the read-only probe.
type Probe struct {
	Default    Candidate // Q1; always present
	Candidates []Candidate
	Hidden     bool // sacctmgr was not readable: low confidence
}

// Job is what the job needs.
type Job struct {
	Restartable bool // checkpoints, or the user says so. A short job without a checkpoint is not restartable.
	ArrayShort  bool // an array of independent short tasks (each task restarts alone)
	Estimate    time.Duration
	TimeLimit   time.Duration // the requested --time
	NeedOther   bool          // the job states a need (time, memory, GPU) that the default partition cannot meet
	MultiNode   bool
	NamedQOS    string // a QOS the user named
	WantsHigh   bool   // the user asked for a priority QOS
}

// Prefs are the user settings.
type Prefs struct {
	Use      string // "auto" (default) | "ask" | "off"
	MaxStops int    // default 3
	Approved map[string]bool
}

func (p Prefs) maxStops() int {
	if p.MaxStops < 1 {
		return 3
	}
	return p.MaxStops
}

// Option is one queue request.
type Option struct {
	Tech       string
	Name       string
	IsQOS      bool
	Preemptive bool
	Requeue    bool // add --requeue and the TERM trap
	NeedsAsk   bool // the user must approve before submit
}

// Qualifies is rule 2 of the design and the owner answer: restartable (or an array of short tasks), the estimate and
// the requested time under 4 h and under one third of maxwall, and the requested time within maxwall.
func Qualifies(j Job, c Candidate) bool {
	if !j.Restartable && !j.ArrayShort {
		return false
	}
	if j.MultiNode && c.CancelMode {
		return false
	}
	if j.Estimate <= 0 {
		return false
	}
	limit := max(j.Estimate, j.TimeLimit)
	if limit >= 4*time.Hour {
		return false
	}
	if c.MaxWall > 0 && limit >= c.MaxWall/3 {
		return false
	}
	return true
}

// ChooseQueue returns the ladder in the order to try. The normal queue (Q1) is always last, so it is never empty.
// A QOS that is not allowed is never in it.
func ChooseQueue(pr Probe, j Job, p Prefs) []Option {
	var out []Option
	for _, c := range pr.Candidates {
		if !c.Allowed || (c.ByRequest && !p.Approved[c.Name]) {
			continue
		}
		o := Option{Tech: c.Technique, Name: c.Name, IsQOS: c.IsQOS}
		switch c.Technique {
		case Q3:
			if p.Use == "off" || !c.StartsSoon || !Qualifies(j, c) {
				continue
			}
			o.Preemptive, o.Requeue = true, true
			o.NeedsAsk = p.Use == "ask" || pr.Hidden
		case Q6:
			if !j.WantsHigh && j.NamedQOS != c.Name {
				continue
			}
			o.NeedsAsk = c.UsageFactor > 1
		case Q5:
			if !c.IsDefault && j.NamedQOS != c.Name {
				continue
			}
		case Q2, Q4:
			// Another partition only for a stated need, never as a default (owner rule, review 2026-10-26).
			if !j.NeedOther || (c.MaxWall > 0 && max(j.Estimate, j.TimeLimit) > c.MaxWall) {
				continue
			}
			o.NeedsAsk = true
		default:
			continue
		}
		out = append(out, o)
	}
	// A QOS the user named or asked for first, then preemptible (owner rule), then the others in probe order, Q1 last.
	var first, rest []Option
	for _, o := range out {
		if o.Tech == Q6 || (j.NamedQOS != "" && o.Name == j.NamedQOS) {
			first = append([]Option{o}, first...)
		} else if o.Preemptive {
			first = append(first, o)
		} else {
			rest = append(rest, o)
		}
	}
	q1 := Option{Tech: Q1, Name: pr.Default.Name, IsQOS: pr.Default.IsQOS}
	return append(append(first, rest...), q1)
}

// ErrNoOption means every option failed or the stop limit is reached.
var ErrNoOption = errors.New("queueplan: no option left")

// Attempt is one try and its result.
type Attempt struct {
	Option Option
	Code   string // P1..P7
}

// Next picks the option after failures. Rules:
//   - 3 attempts at most (the M61 limit; preemptions P4 are bounded by MaxStops instead); then ErrNoOption (stop and ask).
//   - P1, P2, P4 (after MaxStops stops) and P5 go straight to Q1. The doc settles P4: a preemptible option is
//     tried again until preemptible.max_stops stops are used, then Q1.
//   - A failed Q1 is never tried again: ErrNoOption.
//   - ask is true when the plan changes: P6 and P7 need a longer time or a new class, any partition other than Q1,
//     an option that needs approval (use=ask, hidden sacctmgr, usage factor above 1), also on the first attempt.
func Next(ladder []Option, history []Attempt, p Prefs) (Option, bool, error) {
	// P4 stops are bounded by MaxStops, not by the 3-attempt limit; every other failure counts.
	failed := 0
	for _, a := range history {
		if a.Code != "P4" {
			failed++
		}
	}
	if failed >= 3 || len(history) >= 3+p.maxStops() {
		return Option{}, false, ErrNoOption
	}
	stops, toQ1, bad := 0, false, map[string]bool{}
	askPlan := false
	for _, a := range history {
		switch a.Code {
		case "P4":
			stops++
		case "P5", "P1", "P2":
			toQ1 = true
			bad[a.Option.Name] = true
		case "P6", "P7":
			askPlan = true
			bad[a.Option.Name] = true
		default:
			bad[a.Option.Name] = true
		}
	}
	if stops >= p.maxStops() {
		toQ1 = true
	}
	for _, o := range ladder {
		if bad[o.Name] {
			if o.Tech == Q1 {
				return Option{}, false, ErrNoOption
			}
			continue
		}
		if toQ1 && o.Tech != Q1 {
			continue
		}
		ask := o.NeedsAsk || askPlan || (o.Tech != Q1 && !o.Preemptive && len(history) > 0)
		return o, ask, nil
	}
	return Option{}, false, ErrNoOption
}

// Failure is one record for the site profile (qos.failed).
type Failure struct {
	Name, Code, Date, Detail, Confidence string
}

// Record: high for scheduler messages (P1 to P3), medium for sacct states (P4, P6), low for a guess (P7, P5).
func Record(a Attempt, detail, date string) Failure {
	conf := "low"
	switch a.Code {
	case "P1", "P2", "P3":
		conf = "high"
	case "P4", "P6":
		conf = "medium"
	}
	if len(detail) > 200 {
		detail = detail[:200]
	}
	return Failure{a.Option.Name, a.Code, date, strings.Join(strings.Fields(detail), " "), conf}
}

var rules = []struct {
	code string
	re   *regexp.Regexp
}{
	{"P2", regexp.MustCompile(`(?i)invalid qos|invalid account|job violates accounting/qos policy|not (allowed|permitted) to use`)},
	{"P3", regexp.MustCompile(`(?i)requested time limit is invalid|PartitionTimeLimit`)},
	// These Reasons only mean waiting (low confidence, never a 180-day removal).
	{"P7", regexp.MustCompile(`AssocGrp|ReqNodeNotAvail|QOSMaxJobsPerUserLimit`)},
	{"P1", regexp.MustCompile(`(?i)unrecognized option|unknown option|invalid.*(--requeue|--signal)`)},
}

// Classify maps sbatch stderr, a pending Reason or a sacct State to P1..P7, or "" when nothing matches.
// restarts is the SLURM_RESTART_COUNT / Restarts value; pendingLong is true when the job waited too long.
func Classify(text, state string, restarts int, pendingLong bool) string {
	switch {
	case restarts >= 3:
		return "P5"
	case state == "PREEMPTED":
		return "P4"
	case state == "TIMEOUT":
		return "P6"
	}
	for _, r := range rules {
		if r.re.MatchString(text) {
			return r.code
		}
	}
	if pendingLong {
		return "P7"
	}
	return ""
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)

// SbatchLines returns the #SBATCH lines for an option. A preemptible option adds --requeue and a TERM signal 120 s
// before the end, so the script can save its state; any other option adds --no-requeue. Names come from probe output,
// so a bad one gives an error (a newline would inject a shell line).
func SbatchLines(o Option) ([]string, error) {
	var l []string
	if o.Tech != Q1 && o.Name != "" {
		if !safeName.MatchString(o.Name) {
			return nil, fmt.Errorf("queueplan: unsafe queue name %q", o.Name)
		}
		if o.IsQOS {
			l = append(l, "#SBATCH --qos="+o.Name)
		} else {
			l = append(l, "#SBATCH --partition="+o.Name)
		}
	}
	if o.Requeue {
		return append(l, "#SBATCH --requeue", "#SBATCH --signal=B:TERM@120"), nil
	}
	return append(l, "#SBATCH --no-requeue"), nil
}
