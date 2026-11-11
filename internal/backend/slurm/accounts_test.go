package slurm

import (
	"context"
	"slices"
	"strings"
	"testing"

	"labmill-client/internal/backend"
)

func TestAccounts(t *testing.T) {
	run := func(r backend.Result) ([]string, string) {
		s := &Slurm{Runner: &fakeRunner{replies: map[string]backend.Result{"sh": r}}}
		return s.Accounts(context.Background())
	}
	a, d := run(backend.Result{Stdout: "lab2|lab1\nlab1|lab1\nlab1|lab1\nbad acct|x\n\n"})
	if !slices.Equal(a, []string{"lab1", "lab2"}) || d != "lab1" {
		t.Errorf("got %v %q", a, d)
	}
	if a, _ := run(backend.Result{ExitCode: 1}); a != nil {
		t.Errorf("failure gave %v", a)
	}
}

func TestRenderAccount(t *testing.T) {
	s := &Slurm{}
	out, err := s.Render(backend.JobSpec{Name: "j", Account: "lab1", Command: "true", WorkDir: "/home/a/w"})
	if err != nil || !strings.Contains(out, "#SBATCH --account=lab1\n") {
		t.Errorf("%q %v", out, err)
	}
	if _, err := s.Render(backend.JobSpec{Name: "j", Account: "a b", Command: "true", WorkDir: "/home/a/w"}); err == nil {
		t.Error("bad account accepted")
	}
}
