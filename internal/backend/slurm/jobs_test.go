package slurm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

const squeueJobs = `101|RUNNING|gpu|n01|None|2026-10-01T10:00:00|2026-10-01T10:05:00|train
102|PENDING|gpu||Resources|2026-10-01T11:00:00|2026-10-02T00:00:00|wait
103_[5-9]|PENDING|cpu|(null)|Priority|2026-10-01T11:30:00|N/A|arr
`

const sacctJobs = `101|RUNNING|gpu|n01|2026-10-01T10:00:00|2026-10-01T10:05:00|Unknown|train
99|COMPLETED|gpu|n02|2026-09-30T08:00:00|2026-09-30T08:01:00|2026-09-30T09:00:00|old
98|CANCELLED by 1000|cpu|None assigned|2026-09-29T08:00:00|Unknown|2026-09-29T08:10:00|gone
97|FAILED|cpu|n03|2026-09-30T20:00:00|2026-09-30T20:01:00|2026-09-30T21:00:00|a|b|c
96|PENDING|cpu||2026-10-01T11:00:00|Unknown|Unknown|queued
`

func TestJobsActiveOnly(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {Stdout: squeueJobs}}}
	jobs, err := (&Slurm{Runner: r}).Jobs(context.Background(), time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 || len(r.calls) != 1 {
		t.Fatalf("jobs=%d calls=%d", len(jobs), len(r.calls))
	}
	if got := strings.Join(r.calls[0][:3], " "); got != "env TZ=UTC squeue" {
		t.Errorf("argv = %v", r.calls[0])
	}
	if jobs[0].State != store.StateRunning || jobs[0].Node != "n01" || jobs[0].Start.IsZero() || jobs[0].Reason != "" {
		t.Errorf("running row: %+v", jobs[0])
	}
	if jobs[1].State != store.StatePending || jobs[1].Reason != "Resources" || jobs[1].Node != "" || !jobs[1].Start.IsZero() {
		t.Errorf("pending row: %+v", jobs[1])
	}
	if jobs[2].ID != "103_[5-9]" || jobs[2].Node != "" || jobs[2].Name != "arr" {
		t.Errorf("array row: %+v", jobs[2])
	}
}

func TestJobsWithAccounting(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {Stdout: squeueJobs}, "sacct": {Stdout: sacctJobs}}}
	loc := time.FixedZone("x", -7*3600)
	since := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	jobs, err := (&Slurm{Runner: r, Location: loc}).Jobs(context.Background(), since, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, j := range jobs {
		ids = append(ids, j.ID)
	}
	// active first (squeue order), then finished newest end first; 101 and 96 not duplicated.
	if got := strings.Join(ids, ","); got != "101,102,103_[5-9],97,99,98" {
		t.Fatalf("ids = %s", got)
	}
	argv := strings.Join(r.calls[1], " ")
	for _, want := range []string{"env TZ=UTC sacct", "-X -n -P", "-S 2026-09-27T12:00:00", "JobID,State"} {
		if !strings.Contains(argv, want) {
			t.Errorf("sacct argv %q lacks %q", argv, want)
		}
	}
	for _, j := range jobs {
		switch j.ID {
		case "97":
			if j.Name != "a|b|c" || j.State != store.StateFailed {
				t.Errorf("97: %+v", j)
			}
			if !j.End.Equal(time.Date(2026, 9, 30, 21, 0, 0, 0, loc)) {
				t.Errorf("97 end %v", j.End)
			}
		case "98":
			if j.State != store.StateCanceled || j.Node != "" || !j.Start.IsZero() || j.End.IsZero() {
				t.Errorf("98: %+v", j)
			}
		}
	}
}

func TestJobsLimit(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {Stdout: squeueJobs}, "sacct": {Stdout: sacctJobs}}}
	jobs, err := (&Slurm{Runner: r}).Jobs(context.Background(), time.Unix(1, 0), 4)
	if err != nil || len(jobs) != 4 {
		t.Fatalf("jobs=%d err=%v", len(jobs), err)
	}
}

func TestJobsNoAccounting(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {Stdout: squeueJobs}, "sacct": {ExitCode: 1, Stderr: "Slurm accounting storage is disabled"}}}
	jobs, err := (&Slurm{Runner: r}).Jobs(context.Background(), time.Unix(1, 0), 0)
	if !errors.Is(err, backend.ErrNoAccounting) || len(jobs) != 3 {
		t.Fatalf("jobs=%d err=%v", len(jobs), err)
	}
}

func TestJobsSqueueFails(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {ExitCode: 1, Stderr: "boom"}}}
	if _, err := (&Slurm{Runner: r}).Jobs(context.Background(), time.Time{}, 0); err == nil {
		t.Fatal("want error")
	}
}

func TestJobsNameWithPipeInSqueue(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {Stdout: "5|RUNNING|gpu|n1|None|2026-10-01T10:00:00|2026-10-01T10:05:00|a|b\n"}}}
	jobs, err := (&Slurm{Runner: r}).Jobs(context.Background(), time.Time{}, 0)
	if err != nil || len(jobs) != 1 || jobs[0].Name != "a|b" {
		t.Fatalf("%+v %v", jobs, err)
	}
}

func TestJobsReasonCarriesRawState(t *testing.T) {
	sq := "7|SUSPENDED|gpu|n1|None|2026-10-01T10:00:00|2026-10-01T10:05:00|s\n8|REQUEUED|gpu||Priority|2026-10-01T10:00:00|N/A|q\n10|WEIRD|gpu||x|2026-10-01T10:00:00|N/A|w\n9|PENDING|gpu||Resources|2026-10-01T10:00:00|N/A|p\n"
	sa := "20|TIMEOUT|cpu|n1|2026-10-01T10:00:00|2026-10-01T10:01:00|2026-10-01T11:00:00|t\n" +
		"21|OUT_OF_MEMORY|cpu|n1|2026-10-01T10:00:00|2026-10-01T10:01:00|2026-10-01T11:00:00|o\n" +
		"23|COMPLETED|cpu|n1|2026-10-01T10:00:00|2026-10-01T10:01:00|2026-10-01T11:00:00|c\n"
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {Stdout: sq}, "sacct": {Stdout: sa}}}
	jobs, err := (&Slurm{Runner: r}).Jobs(context.Background(), time.Unix(1, 0), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]backend.RemoteJob{}
	for _, j := range jobs {
		got[j.ID] = j
	}
	for id, want := range map[string]string{"7": "SUSPENDED", "8": "REQUEUED", "9": "Resources", "20": "TIMEOUT", "21": "OUT_OF_MEMORY", "10": "unknown state WEIRD", "23": ""} {
		if got[id].Reason != want {
			t.Errorf("job %s reason = %q, want %q", id, got[id].Reason, want)
		}
	}
	if got["10"].State != store.StatePending {
		t.Errorf("unknown state mapped to %v", got["10"].State)
	}
}

func TestJobsGroupsFinishedArrayTasks(t *testing.T) {
	sa := "50_1|COMPLETED|cpu|n1|2026-10-01T09:00:00|2026-10-01T10:00:00|2026-10-01T10:30:00|arr\n" +
		"50_2|TIMEOUT|cpu|n2|2026-10-01T09:00:00|2026-10-01T09:50:00|2026-10-01T11:00:00|arr\n" +
		"50_3|COMPLETED|cpu|n3|2026-10-01T09:00:00|2026-10-01T10:10:00|2026-10-01T10:40:00|arr\n" +
		"60_1|COMPLETED|cpu|n1|2026-10-01T09:00:00|2026-10-01T10:00:00|2026-10-01T10:30:00|live\n" +
		"60_2|RUNNING|cpu|n1|2026-10-01T09:00:00|2026-10-01T10:00:00|Unknown|live\n" +
		"70|COMPLETED|cpu|n1|2026-10-01T09:00:00|2026-10-01T10:00:00|2026-10-01T10:30:00|plain\n"
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {}, "sacct": {Stdout: sa}}}
	jobs, err := (&Slurm{Runner: r}).Jobs(context.Background(), time.Unix(1, 0), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %+v", jobs)
	}
	var arr backend.RemoteJob
	for _, j := range jobs {
		if j.ID == "50" {
			arr = j
		}
	}
	if arr.ID != "50" || arr.Name != "arr" || arr.State != store.StateFailed ||
		arr.Reason != "3 tasks: 2 completed, 1 failed (e.g. TIMEOUT)" ||
		!arr.Start.Equal(time.Date(2026, 10, 1, 9, 50, 0, 0, time.UTC)) || !arr.End.Equal(time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("array group: %+v", arr)
	}
}

func TestJobsCapsSacctRows(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxSacctLines+10; i++ {
		fmt.Fprintf(&b, "%d|COMPLETED|cpu|n1|2026-10-01T09:00:00|2026-10-01T10:00:00|2026-10-01T10:30:00|j\n", 1000+i)
	}
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {}, "sacct": {Stdout: b.String()}}}
	jobs, err := (&Slurm{Runner: r}).Jobs(context.Background(), time.Unix(1, 0), 10000)
	if !errors.Is(err, backend.ErrTruncated) || len(jobs) != maxSacctLines {
		t.Fatalf("jobs=%d err=%v", len(jobs), err)
	}
	ids := map[string]bool{}
	for _, j := range jobs {
		ids[j.ID] = true
	}
	newest, oldest := fmt.Sprint(1000+maxSacctLines+9), "1000"
	if !ids[newest] || ids[oldest] {
		t.Errorf("cap kept the wrong end: newest %s kept=%v, oldest %s kept=%v", newest, ids[newest], oldest, ids[oldest])
	}
}

func TestJobsHeteroSqueueNoSacctDuplicate(t *testing.T) {
	sq := "100+0|RUNNING|cpu|n1|None|2026-10-01T10:00:00|2026-10-01T10:05:00|het\n" +
		"100+1|PENDING|gpu||Resources|2026-10-01T10:00:00|N/A|het\n"
	sa := "100+0|RUNNING|cpu|n1|2026-10-01T10:00:00|2026-10-01T10:05:00|Unknown|het\n" +
		"100+1|COMPLETED|gpu|n2|2026-10-01T10:00:00|2026-10-01T10:05:00|2026-10-01T10:30:00|het\n"
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {Stdout: sq}, "sacct": {Stdout: sa}}}
	jobs, err := (&Slurm{Runner: r}).Jobs(context.Background(), time.Unix(1, 0), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "100" || !strings.HasPrefix(jobs[0].Reason, "2 components") {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestJobsHeteroFinishedFailed(t *testing.T) {
	sa := "200+0|COMPLETED|cpu|n1|2026-10-01T09:00:00|2026-10-01T10:00:00|2026-10-01T10:30:00|h\n" +
		"200+1|TIMEOUT|gpu|n2|2026-10-01T09:00:00|2026-10-01T10:00:00|2026-10-01T11:00:00|h\n"
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {}, "sacct": {Stdout: sa}}}
	jobs, err := (&Slurm{Runner: r}).Jobs(context.Background(), time.Unix(1, 0), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "200" || jobs[0].State != store.StateFailed {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestJobsHeteroPartlyRunningInSacctOnlyHidden(t *testing.T) {
	sa := "300+0|COMPLETED|cpu|n1|2026-10-01T09:00:00|2026-10-01T10:00:00|2026-10-01T10:30:00|h\n" +
		"300+1|RUNNING|gpu|n2|2026-10-01T09:00:00|2026-10-01T10:00:00|Unknown|h\n"
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {}, "sacct": {Stdout: sa}}}
	jobs, err := (&Slurm{Runner: r}).Jobs(context.Background(), time.Unix(1, 0), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v", jobs)
	}
}
