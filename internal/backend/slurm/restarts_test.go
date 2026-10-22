package slurm

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/backend"
)

func TestQueuedJobs(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {Stdout: "100|train|/w/a|2026-01-01T10:00:00\n102|other|/w/b|2026-01-01T11:30:00\nbad line\n"}}}
	// A cluster zone far from UTC must not shift the times: they are asked
	// for in UTC (TZ=UTC) and read as UTC.
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no zone data")
	}
	got, err := (&Slurm{Runner: r, Location: la}).QueuedJobs(context.Background())
	if err != nil || len(got) != 2 || got[0].ID != "100" || got[0].Name != "train" || got[0].WorkDir != "/w/a" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if want := time.Date(2026, 1, 1, 11, 30, 0, 0, time.UTC); !got[1].Submitted.Equal(want) {
		t.Errorf("submitted = %v, want %v", got[1].Submitted, want)
	}
	if strings.Join(r.calls[0], " ") != "env TZ=UTC squeue --me -h -o %i|%j|%Z|%V" {
		t.Errorf("call = %q", r.calls[0])
	}
}

func TestJobsSince(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"sacct": {Stdout: "300|train|/w|2026-01-01T12:00:00\n"}}}
	got, err := (&Slurm{Runner: r}).JobsSince(context.Background(), time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC))
	if err != nil || len(got) != 1 || got[0].ID != "300" || got[0].Submitted.Hour() != 12 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if strings.Join(r.calls[0], " ") != "env TZ=UTC sacct -X -n -P -S 2026-01-01T11:00:00 --format=JobID,JobName,WorkDir,Submit" {
		t.Errorf("call = %q", r.calls[0])
	}
}

func TestSubmitUncertain(t *testing.T) {
	for _, tt := range []struct {
		name      string
		reply     backend.Result
		uncertain bool
	}{
		{"refused", backend.Result{ExitCode: 1, Stderr: "sbatch: error: invalid partition"}, false},
		{"exit without message", backend.Result{ExitCode: 255}, true},
		{"garbled answer", backend.Result{Stdout: "hello\n"}, true},
		{"ok", backend.Result{Stdout: "5\n"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRunner{replies: map[string]backend.Result{"mkdir": {}, "sbatch": tt.reply}}
			_, err := (&Slurm{Runner: r}).Submit(context.Background(), "#!/bin/bash\n#SBATCH --chdir=/w\necho\n", backend.JobSpec{})
			if got := errors.Is(err, backend.ErrSubmitUncertain); got != tt.uncertain {
				t.Errorf("uncertain = %v, want %v (err %v)", got, tt.uncertain, err)
			}
		})
	}
	if _, err := (&Slurm{Runner: errRunner{}}).Submit(context.Background(), "#!/bin/bash\n#SBATCH --chdir=/w\necho\n", backend.JobSpec{}); !errors.Is(err, backend.ErrSubmitUncertain) {
		t.Errorf("transport error at sbatch = %v, want uncertain", err)
	}
}

// errRunner fails sbatch with a transport error, everything else succeeds.
type errRunner struct{}

func (errRunner) Run(_ context.Context, argv []string, _ io.Reader) (backend.Result, error) {
	if argv[0] == "sbatch" {
		return backend.Result{}, errors.New("connection reset")
	}
	return backend.Result{}, nil
}

func TestStatusKeepsSignal(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {}, "sacct": {Stdout: "FAILED|0:9|2026-01-01T10:00:00|2026-01-01T11:00:00|n1|42\n"}}}
	st, err := (&Slurm{Runner: r}).Status(context.Background(), "42")
	if err != nil || st.ExitCode != 0 || st.Signal != 9 {
		t.Fatalf("status = %+v, %v", st, err)
	}
}

func TestRestarts(t *testing.T) {
	tests := []struct {
		name    string
		reply   backend.Result
		id      string
		want    int
		wantErr string
	}{
		{"none", backend.Result{Stdout: "0\n"}, "42", 0, ""},
		{"requeued twice", backend.Result{Stdout: "2\n"}, "42", 2, ""},
		{"largest row", backend.Result{Stdout: "1\n3\n"}, "42", 3, ""},
		{"no rows", backend.Result{}, "42", 0, ""},
		{"sacct fails", backend.Result{ExitCode: 1, Stderr: "no accounting"}, "42", 0, "no accounting"},
		{"bad id", backend.Result{}, "4; rm", 0, "job"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRunner{replies: map[string]backend.Result{"sacct": tt.reply}}
			got, err := (&Slurm{Runner: r}).Restarts(context.Background(), tt.id)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Restarts = %d, %v; want %d", got, err, tt.want)
			}
			if len(r.calls) != 1 || strings.Join(r.calls[0], " ") != "sacct -n -P -X -j 42 --format=Restarts" {
				t.Errorf("calls = %q", r.calls)
			}
		})
	}
}
