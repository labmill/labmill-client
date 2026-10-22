package slurm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

func TestRenderSubMinuteTime(t *testing.T) {
	s := spec()
	s.Time = 20 * time.Second
	script, err := (&Slurm{}).Render(s)
	if err != nil || !strings.Contains(script, "#SBATCH --time=00:01:00\n") {
		t.Errorf("script = %q, err = %v", script, err)
	}
}

func TestSubmitNormalizesCRLF(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"mkdir": {}, "sbatch": {Stdout: "7\n"}}}
	if _, err := (&Slurm{Runner: r}).Submit(context.Background(), "#!/bin/bash\r\necho hi\r\n", spec()); err != nil {
		t.Fatal(err)
	}
	if r.stdin[1] != "#!/bin/bash\necho hi\n" {
		t.Errorf("stdin = %q", r.stdin[1])
	}
}

func TestRenderRefusesNodeLocalWorkDir(t *testing.T) {
	for _, wd := range []string{"/tmp", "/tmp/x", "/var/tmp/run", "/dev/shm/a", "/tmp/../tmp/x"} {
		s := spec()
		s.WorkDir = wd
		_, err := (&Slurm{}).Render(s)
		if !errors.Is(err, backend.ErrInvalidSpec) || !strings.Contains(err.Error(), "node-local") {
			t.Errorf("%s: err = %v", wd, err)
		}
	}
	for _, wd := range []string{"/tmpfoo/x", "/home/a/tmp", "/storage/tmp/x"} {
		s := spec()
		s.WorkDir = wd
		if _, err := (&Slurm{}).Render(s); err != nil {
			t.Errorf("%s: %v", wd, err)
		}
	}
}

func TestSubmitCreatesWorkDirBeforeSbatch(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"mkdir": {ExitCode: 1, Stderr: "Permission denied"}}}
	if _, err := (&Slurm{Runner: r}).Submit(context.Background(), "#!/bin/bash\n", spec()); err == nil || len(r.calls) != 1 {
		t.Errorf("sbatch ran after failed mkdir: %v %v", err, r.calls)
	}
}

func TestStatusTerminalSqueueFallsThroughToSacct(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{
		"squeue": {Stdout: "FAILED|None|n1\n"},
		"sacct":  {Stdout: "FAILED|3:0|2026-10-03T10:00:00|2026-10-03T10:05:00|n1\n"},
	}}
	st, err := (&Slurm{Runner: r}).Status(context.Background(), "5")
	if err != nil || st.State != store.StateFailed || st.ExitCode != 3 || st.EndTime.IsZero() {
		t.Fatalf("st = %+v, err = %v", st, err)
	}
	// No sacct row: squeue's answer stands.
	r = &fakeRunner{replies: map[string]backend.Result{"squeue": {Stdout: "TIMEOUT|None|n1\n"}, "sacct": {}}}
	st, err = (&Slurm{Runner: r}).Status(context.Background(), "5")
	if err != nil || st.State != store.StateFailed || st.Reason != "TIMEOUT" {
		t.Fatalf("st = %+v, err = %v", st, err)
	}
}

func TestStatusStateMapping(t *testing.T) {
	cases := []struct {
		state string
		want  store.JobState
		note  string // expected Reason ("" = unchecked)
	}{
		{"REQUEUE_HOLD", store.StatePending, "REQUEUE_HOLD"},
		{"REQUEUE_FED", store.StatePending, "REQUEUE_FED"},
		{"RESV_DEL_HOLD", store.StatePending, "RESV_DEL_HOLD"},
		{"SPECIAL_EXIT", store.StatePending, "SPECIAL_EXIT"},
		{"STOPPED", store.StateRunning, "STOPPED"},
		{"SIGNALING", store.StateRunning, "SIGNALING"},
		{"STAGE_OUT", store.StateRunning, "STAGE_OUT"},
		{"REVOKED", store.StateCanceled, ""},
		{"WEIRD_NEW", store.StatePending, "unknown state WEIRD_NEW"},
	}
	for _, tc := range cases {
		// Via squeue (non-terminal ones return directly) ...
		r := &fakeRunner{replies: map[string]backend.Result{"squeue": {Stdout: tc.state + "|None|\n"}, "sacct": {}}}
		st, err := (&Slurm{Runner: r}).Status(context.Background(), "9")
		if err != nil || st.State != tc.want || (tc.note != "" && st.Reason != tc.note) {
			t.Errorf("squeue %s: %+v, %v", tc.state, st, err)
		}
		// ... and via sacct.
		r = &fakeRunner{replies: map[string]backend.Result{"squeue": {}, "sacct": {Stdout: tc.state + "|0:0|Unknown|Unknown|\n"}}}
		st, err = (&Slurm{Runner: r}).Status(context.Background(), "9")
		if err != nil || st.State != tc.want || (tc.note != "" && st.Reason != tc.note) {
			t.Errorf("sacct %s: %+v, %v", tc.state, st, err)
		}
	}
}

func TestSacctTimesLocation(t *testing.T) {
	out := "COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T11:00:00|n\n"
	mk := func(loc *time.Location) backend.Status {
		r := &fakeRunner{replies: map[string]backend.Result{"squeue": {}, "sacct": {Stdout: out}}}
		st, err := (&Slurm{Runner: r, Location: loc}).Status(context.Background(), "1")
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if got := mk(nil).StartTime; !got.Equal(time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("nil Location: %v", got)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	if got := mk(ny).StartTime; !got.Equal(time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("NY Location: %v", got)
	}
}
