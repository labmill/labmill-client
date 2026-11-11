package testrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"labmill-client/internal/testcase"
)

type fake struct {
	obs map[string]Observation
	err map[string]error
	ran []string
}

func (f *fake) Run(_ context.Context, c testcase.Case, _ string) (Observation, float64, error) {
	f.ran = append(f.ran, c.ID)
	return f.obs[c.ID], 0, f.err[c.ID]
}

func mk(id, outcome string) testcase.Case {
	return testcase.Case{ID: id, Expect: testcase.Expect{Outcome: outcome, FailureClass: map[string]string{"failed": "exit_nonzero"}[outcome], AgentShould: "ask_user"}}
}

func TestRunAll(t *testing.T) {
	gpu := mk("gpu-one", "completed")
	gpu.Needs.GPU = true
	cases := []testcase.Case{mk("fail-one", "failed"), mk("ok-one", "completed"), gpu, mk("err-one", "completed"), mk("bad-one", "completed")}
	f := &fake{
		obs: map[string]Observation{
			"fail-one": {JobState: "FAILED", ExitCode: 1, AskedUser: true},
			"ok-one":   {JobState: "COMPLETED"},
			"bad-one":  {JobState: "COMPLETED", CheckExit: 1},
		},
		err: map[string]error{"err-one": ErrQueue},
	}
	rs := RunAll(context.Background(), f, cases, t.TempDir(), nil, time.Now)
	if f.ran[len(f.ran)-1] != "fail-one" {
		t.Errorf("failing case must run last: %v", f.ran)
	}
	got := map[string]Result{}
	for _, r := range rs {
		got[r.Case] = r
	}
	if got["gpu-one"].Outcome != Skipped || got["gpu-one"].Reason != "needs gpu" {
		t.Errorf("gpu: %+v", got["gpu-one"])
	}
	if got["err-one"].Outcome != Error || got["err-one"].Reason != "queue" {
		t.Errorf("err: %+v", got["err-one"])
	}
	if got["ok-one"].Outcome != Pass || got["fail-one"].Outcome != Pass || got["bad-one"].Outcome != Fail {
		t.Errorf("results: %+v", got)
	}
	if ExitCode(rs) != 1 || ExitCode(rs[:1]) != 0 {
		t.Errorf("exit codes %d", ExitCode(rs))
	}
	if ExitCode([]Result{{Outcome: Error}, {Outcome: Pass}}) != 3 {
		t.Error("error without fail must be 3")
	}
}

func TestSalt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "test-salt")
	n := 0
	rnd := func() (string, error) { n++; return "abc123", nil }
	a, err := Salt(p, rnd)
	if err != nil || a != "abc123" {
		t.Fatal(a, err)
	}
	b, _ := Salt(p, func() (string, error) { return "", errors.New("must not be called") })
	if b != a || n != 1 {
		t.Errorf("second read %q", b)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
}
