// Package testrun judges and reports runs of the test corpus (internal/testcase).
// This file and report.go are pure: no I/O besides the report POST.
package testrun

import (
	"fmt"
	"regexp"
	"strings"

	"labmill-client/internal/testcase"
)

// Outcomes of one case, as the server stores them.
const (
	Pass    = "pass"
	Fail    = "fail"
	Skipped = "skipped"
	Error   = "error"
)

// Observation is what the session driver saw for one case.
type Observation struct {
	SubmitErr         string // sbatch stderr when no job was made
	JobState          string // sacct State of the job, first word, for example COMPLETED
	ExitCode          int
	OutputTail        string
	CheckExit         int    // exit code of check.sh; -1 when not run
	FinalText         string // last agent message
	AskedUser         bool   // the agent used the question tool
	ResubmitCompleted bool   // a second job COMPLETED
}

// Verdict is the result of one case.
type Verdict struct {
	Outcome      string
	FailureClass string
	Reason       string
}

var (
	oomRE     = regexp.MustCompile(`(?i)oom-kill|out of memory|MemoryError|Cannot allocate memory`)
	missingRE = regexp.MustCompile(`(?i)command not found|No module named|ModuleNotFoundError|module: command|Lmod has detected`)
	dataRE    = regexp.MustCompile(`(?i)No such file or directory|FileNotFoundError`)
)

// Class works out the failure class of a job that did not complete. It returns ""
// for a completed job. Order matters: the most specific cause first.
func Class(o Observation) string {
	state := strings.ToUpper(o.JobState)
	switch {
	case o.JobState == "" && o.SubmitErr != "":
		return "submit_rejected"
	case state == "COMPLETED":
		return ""
	case state == "TIMEOUT":
		return "timeout"
	case state == "OUT_OF_MEMORY" || oomRE.MatchString(o.OutputTail):
		return "out_of_memory"
	case missingRE.MatchString(o.OutputTail), o.ExitCode == 127 && state == "FAILED":
		return "missing_software"
	case dataRE.MatchString(o.OutputTail):
		return "data_missing"
	case state != "" && o.ExitCode != 0, state == "FAILED":
		return "exit_nonzero"
	}
	return ""
}

// Judge compares an observation with the case expectation.
func Judge(c testcase.Case, o Observation) Verdict {
	got := Class(o)
	completed := strings.EqualFold(o.JobState, "COMPLETED")
	if c.Expect.Outcome == "completed" {
		switch {
		case !completed:
			if got == "" {
				got = "unknown"
			}
			return Verdict{Outcome: Fail, FailureClass: knownClass(got), Reason: "job_failed:" + got}
		case o.CheckExit != 0:
			return Verdict{Outcome: Fail, Reason: "wrong_result"}
		}
		return Verdict{Outcome: Pass}
	}
	if c.Expect.FailureClass == "submit_rejected" {
		// An agent that checks first never sees the rejection: asking, or
		// sending a corrected job at once, is as good as asking or fixing after it.
		switch {
		case c.Expect.AgentShould == "ask_user" && o.AskedUser && o.JobState == "" && o.SubmitErr == "":
			return Verdict{Outcome: Pass, FailureClass: "submit_rejected"}
		case c.Expect.AgentShould == "fix_and_resubmit" && completed:
			return Verdict{Outcome: Pass, FailureClass: "submit_rejected"}
		}
	}
	if completed {
		return Verdict{Outcome: Fail, Reason: "job_completed"}
	}
	if got != c.Expect.FailureClass {
		return Verdict{Outcome: Fail, FailureClass: knownClass(got), Reason: fmt.Sprintf("class %q, want %q", got, c.Expect.FailureClass)}
	}
	switch c.Expect.AgentShould {
	case "explain":
		if !hasKeyword(o.FinalText, c.Expect.AgentKeywords) {
			return Verdict{Outcome: Fail, FailureClass: got, Reason: "no keyword in final message"}
		}
	case "fix_and_resubmit":
		if !o.ResubmitCompleted {
			return Verdict{Outcome: Fail, FailureClass: got, Reason: "no completed second job"}
		}
	case "ask_user":
		if !o.AskedUser {
			return Verdict{Outcome: Fail, FailureClass: got, Reason: "agent did not ask"}
		}
	}
	return Verdict{Outcome: Pass, FailureClass: got}
}

func knownClass(c string) string {
	if c == "unknown" {
		return ""
	}
	return c
}

func hasKeyword(text string, words []string) bool {
	low := strings.ToLower(text)
	for _, w := range words {
		if strings.Contains(low, strings.ToLower(w)) {
			return true
		}
	}
	return false
}
