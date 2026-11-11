package testrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"labmill-client/internal/testcase"
)

// CaseRunner plays one case. *Driver is the real one; tests use a fake.
type CaseRunner interface {
	Run(ctx context.Context, c testcase.Case, caseDir string) (Observation, float64, error)
}

// Skip returns a reason when the cluster lacks something the case needs. have
// lists the capabilities the cluster has ("gpu", "mpi"). Modules are not probed yet.
func Skip(c testcase.Case, have map[string]bool) string {
	switch {
	case c.Needs.GPU && !have["gpu"]:
		return "needs gpu"
	case c.Needs.MPI && !have["mpi"]:
		return "needs mpi"
	}
	return ""
}

// Plan puts the cases that expect a failure last, keeping the order otherwise.
func Plan(cases []testcase.Case) []testcase.Case {
	out := append([]testcase.Case(nil), cases...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Expect.Outcome != "failed" && out[j].Expect.Outcome == "failed"
	})
	return out
}

// runErrorReason turns a runner error into the short reason stored with an error result.
func runErrorReason(err error) string {
	switch {
	case errors.Is(err, ErrCap), errors.Is(err, ErrQueue), errors.Is(err, ErrLogin):
		return err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	}
	return "runner"
}

// RunAll plays the cases in order and returns one Result per case.
func RunAll(ctx context.Context, r CaseRunner, cases []testcase.Case, runDir string, have map[string]bool, now func() time.Time) []Result {
	var out []Result
	for _, c := range Plan(cases) {
		if why := Skip(c, have); why != "" {
			out = append(out, Result{Case: c.ID, Outcome: Skipped, Reason: why})
			continue
		}
		start := now()
		obs, _, err := r.Run(ctx, c, filepath.Join(runDir, c.ID))
		res := Result{Case: c.ID, Seconds: int(now().Sub(start).Seconds())}
		if err != nil {
			res.Outcome, res.Reason = Error, runErrorReason(err)
			// The detail stays on this machine: it can hold paths and host names.
			_ = os.MkdirAll(filepath.Join(runDir, c.ID), 0o755)
			_ = os.WriteFile(filepath.Join(runDir, c.ID, "error.txt"), []byte(err.Error()+"\n"), 0o644)
		} else {
			v := Judge(c, obs)
			res.Outcome, res.FailureClass, res.Reason = v.Outcome, v.FailureClass, v.Reason
		}
		out = append(out, res)
		if ctx.Err() != nil {
			break
		}
	}
	return out
}

// ExitCode is 0 when nothing failed, 1 on any fail, 3 on an error and no fail.
func ExitCode(rs []Result) int {
	code := 0
	for _, r := range rs {
		switch r.Outcome {
		case Fail:
			return 1
		case Error:
			code = 3
		}
	}
	return code
}

// Table is the printed summary.
func Table(rs []Result) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, "%-28s %-8s %5ds  %s %s\n", r.Case, r.Outcome, r.Seconds, r.FailureClass, r.Reason)
	}
	return b.String()
}

// Salt reads the install salt at path, or makes one (mode 0600) when missing.
func Salt(path string, random func() (string, error)) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	s, err := random()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return s, os.WriteFile(path, []byte(s+"\n"), 0o600)
}
