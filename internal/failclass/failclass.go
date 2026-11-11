// Package failclass names why a job did not complete. The job report to the
// Labmill account (internal/account) and the test runner (internal/testrun)
// use the same names, so the server counts one vocabulary.
package failclass

import (
	"regexp"
	"strings"
)

// The classes of a job that did not complete. The scheduler states NODE_FAIL
// and PREEMPTED have their own names in the report (node_failure, preempted).
const (
	Timeout         = "timeout"
	OutOfMemory     = "out_of_memory"
	ExitNonzero     = "exit_nonzero"
	SubmitRejected  = "submit_rejected"
	MissingSoftware = "missing_software"
	DataMissing     = "data_missing"
)

// Input is what is known of one job. The text is redacted output of the job.
type Input struct {
	SubmitErr string // scheduler error when no job was made
	State     string // scheduler state, first word, for example FAILED
	ExitCode  int
	Output    string // last lines of the job's output
}

var (
	oomRE     = regexp.MustCompile(`(?i)oom-kill|out of memory|MemoryError|Cannot allocate memory`)
	missingRE = regexp.MustCompile(`(?i)command not found|No module named|ModuleNotFoundError|module: command|Lmod has detected`)
	dataRE    = regexp.MustCompile(`(?i)No such file or directory|FileNotFoundError`)
)

// Of works out the failure class. It returns "" for a completed job and for a
// job whose cause is not one of the classes. Order matters: the most specific
// cause first.
func Of(in Input) string {
	state := strings.ToUpper(in.State)
	switch {
	case in.State == "" && in.SubmitErr != "":
		return SubmitRejected
	case state == "COMPLETED":
		return ""
	case state == "TIMEOUT":
		return Timeout
	case state == "OUT_OF_MEMORY" || oomRE.MatchString(in.Output):
		return OutOfMemory
	case missingRE.MatchString(in.Output), in.ExitCode == 127 && state == "FAILED":
		return MissingSoftware
	case dataRE.MatchString(in.Output):
		return DataMissing
	case state != "" && in.ExitCode != 0, state == "FAILED":
		return ExitNonzero
	}
	return ""
}
