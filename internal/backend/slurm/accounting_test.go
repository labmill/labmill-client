package slurm

import (
	"context"
	"io"
	"testing"

	"labmill-client/internal/backend"
)

func TestAccounting(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"sacct": {Stdout: "77|OUT_OF_MEMORY|0:125|None|00:10:00|01:00:00|4000Mn||n1\n"}}}
	got, err := (&Slurm{Runner: &stepRunner{fakeRunner: r}}).Accounting(context.Background(), "77")
	if err != nil || got.State != "OUT_OF_MEMORY" || got.ReqMemBytes != 4000<<20 || got.MaxRSSBytes != 4198400<<10 || got.Timelimit != "01:00:00" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := (&Slurm{Runner: r}).Accounting(context.Background(), "x;y"); err == nil {
		t.Error("bad id accepted")
	}
}

// stepRunner answers the step query (no -X) with MaxRSS lines.
type stepRunner struct{ *fakeRunner }

func (s *stepRunner) Run(ctx context.Context, argv []string, in io.Reader) (backend.Result, error) {
	for _, a := range argv {
		if a == "JobID,MaxRSS" {
			return backend.Result{Stdout: "77.batch|4198400K\n77.0|1024K\n77|\n"}, nil
		}
	}
	return s.fakeRunner.Run(ctx, argv, in)
}
