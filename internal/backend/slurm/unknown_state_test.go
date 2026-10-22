package slurm

import (
	"context"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

func TestJobsFinishedArrayWithUnknownState(t *testing.T) {
	sa := "80_1|COMPLETED|cpu|n1|2026-10-01T09:00:00|2026-10-01T10:00:00|2026-10-01T10:30:00|arr\n" +
		"80_2|WEIRD|cpu|n2|2026-10-01T09:00:00|2026-10-01T10:00:00|2026-10-01T10:40:00|arr\n"
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {}, "sacct": {Stdout: sa}}}
	jobs, err := (&Slurm{Runner: r}).Jobs(context.Background(), time.Unix(1, 0), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "80" || jobs[0].State != store.StateFailed ||
		jobs[0].Reason != "2 tasks: 1 completed, 1 unknown (WEIRD)" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestStatusArrayUnknownFinishedState(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{
		"squeue": {},
		"sacct": {Stdout: "COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T10:10:00|n1|123_1\n" +
			"WEIRD|0:0|2026-10-03T10:00:00|2026-10-03T10:20:00|n2|123_2\n"}}}
	st, err := (&Slurm{Runner: r}).Status(context.Background(), "123")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != store.StateFailed || !strings.Contains(st.Reason, "2 of 2 tasks done, 1 unknown (WEIRD)") || st.EndTime.IsZero() {
		t.Errorf("st = %+v", st)
	}
	// Without an end time the same state is still treated as waiting.
	r = &fakeRunner{replies: map[string]backend.Result{
		"squeue": {Stdout: "WEIRD|None||123_2\n"},
		"sacct":  {Stdout: "WEIRD|0:0|2026-10-03T10:00:00|Unknown|n2|123_2\n"}}}
	if st, _ := (&Slurm{Runner: r}).Status(context.Background(), "123"); st.State != store.StatePending {
		t.Errorf("no end time: st = %+v", st)
	}
}

func TestStatusPlainJobUnknownStateWithEnd(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {},
		"sacct": {Stdout: "WEIRD|0:0|2026-10-03T10:00:00|2026-10-03T10:20:00|n2\n"}}}
	st, err := (&Slurm{Runner: r}).Status(context.Background(), "9")
	if err != nil || st.State != store.StateFailed || st.Reason != "unknown state WEIRD" {
		t.Errorf("st = %+v, %v", st, err)
	}
}

func TestAggregateUnknownKeepsRealFailureSuffix(t *testing.T) {
	end := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	st := aggregateTasks([]taskRow{
		{Raw: "TIMEOUT", State: store.StateFailed, Known: true, N: 1, End: end},
		{Raw: "WEIRD", N: 1, End: end}})
	if st.State != store.StateFailed || st.Reason != "2 of 2 tasks done, 1 failed, 1 unknown (WEIRD) (TIMEOUT)" {
		t.Errorf("st = %+v", st)
	}
}
