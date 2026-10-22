package slurm

import (
	"context"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

func TestTaskCount(t *testing.T) {
	for id, want := range map[string]int{
		"123": 1, "123_4": 1, "": 1, "123_[5-9]": 5, "123_[5-9%2]": 5, "123_[1,3,5-7]": 5,
		"123_[0-10:2]": 6, "123_[7]": 1, "123_[9-5]": 1, "123_[x]": 1, "123_[1-3": 1, "123+1": 1,
	} {
		if got := taskCount(id); got != want {
			t.Errorf("taskCount(%q) = %d, want %d", id, got, want)
		}
	}
}

func TestAggregateTasks(t *testing.T) {
	row := func(raw string, n int, extra ...string) taskRow {
		js, ok := mapState(raw)
		r := taskRow{Raw: raw, State: js, Known: ok, N: n}
		if len(extra) > 0 {
			r.Reason = extra[0]
		}
		if len(extra) > 1 {
			r.Node = extra[1]
		}
		return r
	}
	cases := []struct {
		name   string
		rows   []taskRow
		state  store.JobState
		reason string // substring match
		node   string
	}{
		{"one running", []taskRow{row("RUNNING", 1, "", "n1"), row("PENDING", 4, "Resources")}, store.StateRunning, "0 of 5 tasks done", "n1"},
		{"running and done", []taskRow{row("COMPLETED", 2), row("RUNNING", 1, "", "n2"), row("PENDING", 2)}, store.StateRunning, "2 of 5 tasks done", "n2"},
		{"all pending", []taskRow{row("PENDING", 5, "Resources")}, store.StatePending, "0 of 5 tasks done (Resources)", ""},
		{"pending and completed", []taskRow{row("COMPLETED", 3), row("PENDING", 2, "Priority")}, store.StateRunning, "3 of 5 tasks done", ""},
		{"all completed", []taskRow{row("COMPLETED", 1), row("COMPLETED", 1), row("COMPLETED", 1)}, store.StateCompleted, "3 of 3 tasks done", ""},
		{"failed", []taskRow{row("COMPLETED", 2), row("TIMEOUT", 1)}, store.StateFailed, "3 of 3 tasks done, 1 failed (TIMEOUT)", ""},
		{"oom and node fail", []taskRow{row("OUT_OF_MEMORY", 1), row("NODE_FAIL", 1), row("COMPLETED", 1)}, store.StateFailed, "2 failed (OUT_OF_MEMORY)", ""},
		{"failed but still running", []taskRow{row("FAILED", 1), row("RUNNING", 1)}, store.StateRunning, "1 of 2 tasks done, 1 failed", ""},
		{"canceled", []taskRow{row("COMPLETED", 1), row("CANCELLED by 5", 1)}, store.StateCanceled, "2 of 2 tasks done, 1 canceled", ""},
		{"failed beats canceled", []taskRow{row("CANCELLED", 1), row("FAILED", 1)}, store.StateFailed, "1 failed, 1 canceled (FAILED)", ""},
		{"unknown state is waiting", []taskRow{{Raw: "WEIRD", State: store.StatePending, N: 2, Reason: "unknown state WEIRD"}}, store.StatePending, "unknown state WEIRD", ""},
		{"zero N counts one", []taskRow{{Raw: "COMPLETED", State: store.StateCompleted, Known: true}}, store.StateCompleted, "1 of 1 tasks done", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := aggregateTasks(tc.rows)
			if st.State != tc.state || !strings.Contains(st.Reason, tc.reason) || st.Node != tc.node {
				t.Errorf("got %+v, want state %s, reason ~%q, node %q", st, tc.state, tc.reason, tc.node)
			}
		})
	}
}

func TestAggregateTasksTimesAndExit(t *testing.T) {
	t0 := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	rows := []taskRow{
		{Raw: "COMPLETED", State: store.StateCompleted, Known: true, N: 1, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)},
		{Raw: "FAILED", State: store.StateFailed, Known: true, N: 1, Exit: 3, Start: t0, End: t0.Add(30 * time.Minute)},
	}
	st := aggregateTasks(rows)
	if st.ExitCode != 3 || !st.StartTime.Equal(t0) || !st.EndTime.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("st = %+v", st)
	}
	rows = append(rows, taskRow{Raw: "PENDING", State: store.StatePending, Known: true, N: 2})
	if st := aggregateTasks(rows); !st.EndTime.IsZero() {
		t.Errorf("EndTime set while tasks are waiting: %v", st.EndTime)
	}
}

func TestStatusJobArray(t *testing.T) {
	const sacctMixed = "COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T10:10:00|n1|123_1\n" +
		"COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T10:20:00|n2|123_2\n" +
		"RUNNING|0:0|2026-10-03T10:05:00|Unknown|n3|123_3\n" +
		"PENDING|0:0|Unknown|Unknown|None assigned|123_[4-9]\n"
	cases := []struct {
		name    string
		replies map[string]backend.Result
		id      string
		state   store.JobState
		reason  string
	}{
		{"squeue and sacct", map[string]backend.Result{
			"squeue": {Stdout: "RUNNING|None|n3|123_3\nPENDING|Resources||123_[4-9]\n"},
			"sacct":  {Stdout: sacctMixed}}, "123", store.StateRunning, "2 of 9 tasks done"},
		{"squeue only (no accounting)", map[string]backend.Result{
			"squeue": {Stdout: "PENDING|Resources||123_[4-9%2]\n"},
			"sacct":  {ExitCode: 1, Stderr: "Slurm accounting storage is disabled"}}, "123", store.StatePending, "6 tasks left, all waiting (Resources)"},
		{"pending reason copied from squeue", map[string]backend.Result{
			"squeue": {Stdout: "PENDING|Priority||123_[1-3]\n"},
			"sacct":  {Stdout: "PENDING|0:0|Unknown|Unknown|None assigned|123_[1-3]\n"}}, "123", store.StatePending, "(Priority)"},
		{"finished array, completed", map[string]backend.Result{
			"squeue": {},
			"sacct": {Stdout: "COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T10:10:00|n1|123_1\n" +
				"COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T10:20:00|n2|123_2\n"}}, "123", store.StateCompleted, "2 of 2 tasks done"},
		{"finished array, one failed", map[string]backend.Result{
			"squeue": {},
			"sacct": {Stdout: "COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T10:10:00|n1|123_1\n" +
				"TIMEOUT|0:0|2026-10-03T10:00:00|2026-10-03T11:10:00|n2|123_2\n"}}, "123", store.StateFailed, "1 failed (TIMEOUT)"},
		{"last running task, others finished", map[string]backend.Result{
			"squeue": {Stdout: "RUNNING|None|n3|123_3\n"},
			"sacct": {Stdout: "COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T10:10:00|n1|123_1\n" +
				"RUNNING|0:0|2026-10-03T10:05:00|Unknown|n3|123_3\n"}}, "123", store.StateRunning, "1 of 2 tasks done"},
		{"single task id is not aggregated", map[string]backend.Result{
			"squeue": {Stdout: "RUNNING|None|n3|123_3\n"}}, "123_3", store.StateRunning, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := (&Slurm{Runner: &fakeRunner{replies: tc.replies}}).Status(context.Background(), tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tc.state || !strings.Contains(st.Reason, tc.reason) || st.RemoteID != tc.id {
				t.Errorf("st = %+v", st)
			}
		})
	}
}

func TestStatusArrayTimesUseLocation(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {},
		"sacct": {Stdout: "COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T11:00:00|n1|7_1\nCOMPLETED|0:0|2026-10-03T10:30:00|2026-10-03T11:30:00|n1|7_2\n"}}}
	st, err := (&Slurm{Runner: r, Location: ny}).Status(context.Background(), "7")
	if err != nil || !st.StartTime.Equal(time.Date(2026, 10, 3, 10, 0, 0, 0, ny)) || !st.EndTime.Equal(time.Date(2026, 10, 3, 11, 30, 0, 0, ny)) {
		t.Errorf("st = %+v, err = %v", st, err)
	}
}

func TestInfoIdleSuffixes(t *testing.T) {
	out := strings.Join([]string{
		"p*|up|1:00:00|(null)|4|idle",
		"p*|up|1:00:00|(null)|1|idle*",
		"p*|up|1:00:00|(null)|2|idle~",
		"p*|up|1:00:00|(null)|3|idle#",
		"p*|up|1:00:00|(null)|5|idle%",
		"p*|up|1:00:00|(null)|6|idle!",
		"p*|up|1:00:00|(null)|7|idle-",
		"p*|up|1:00:00|(null)|8|drain*",
		"p*|up|1:00:00|(null)|9|mix",
	}, "\n") + "\n"
	info, err := (&Slurm{Runner: &fakeRunner{replies: map[string]backend.Result{"sinfo": {Stdout: out}}}}).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := info.Partitions[0]; p.IdleNodes != 4 || p.Nodes != 45 {
		t.Errorf("partition = %+v", p)
	}
}

func TestSacctRunsWithTZUTC(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {},
		"sacct": {Stdout: "COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T11:00:00|n1\n"}}}
	if _, err := (&Slurm{Runner: r}).Status(context.Background(), "5"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), "sacct") {
			found = true
			if len(c) < 3 || c[0] != "env" || c[1] != "TZ=UTC" || c[2] != "sacct" {
				t.Errorf("sacct argv = %q, want env TZ=UTC sacct ...", c)
			}
		}
	}
	if !found {
		t.Fatal("sacct not called")
	}
	// The quoted form is a plain env word, no shell injection surface.
	if got := backend.Quote([]string{"env", "TZ=UTC", "sacct"}); got != "env 'TZ=UTC' sacct" {
		t.Errorf("quote = %q", got)
	}
}

func TestArraySacctLagsSqueueRunning(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{
		"squeue": {Stdout: "RUNNING|None|n3|123_1\nPENDING|Resources||123_[2-4]\n"},
		"sacct":  {Stdout: "PENDING|0:0|Unknown|Unknown|None assigned|123_[1-4]\n"}}}
	st, err := (&Slurm{Runner: r}).Status(context.Background(), "123")
	if err != nil || st.State != store.StateRunning || st.Node != "n3" {
		t.Errorf("st = %+v, err = %v", st, err)
	}
	if strings.Contains(st.Reason, "Resources") {
		t.Errorf("running array still shows a wait reason: %q", st.Reason)
	}
}

func TestArraySqueueOnlyPhrasing(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{
		"squeue": {Stdout: "RUNNING|None|n3|123_1\nPENDING|Resources||123_[2-4]\n"},
		"sacct":  {ExitCode: 1, Stderr: "accounting disabled"}}}
	st, err := (&Slurm{Runner: r}).Status(context.Background(), "123")
	if err != nil || st.State != store.StateRunning || st.Reason != "4 tasks left (1 running, 3 pending)" || st.Node != "n3" {
		t.Errorf("st = %+v, err = %v", st, err)
	}
	if strings.Contains(st.Reason, "done") {
		t.Errorf("reason claims done count: %q", st.Reason)
	}
}

func TestNodePlaceholdersDropped(t *testing.T) {
	for _, v := range []string{"None assigned", "(null)", "", "  None  ", "n/a"} {
		if got := cleanNode(v); got != "" {
			t.Errorf("cleanNode(%q) = %q", v, got)
		}
	}
	if cleanNode(" gpu-01 ") != "gpu-01" {
		t.Error("real node lost")
	}
	// No task running: the node stays empty even if finished tasks had one.
	r := &fakeRunner{replies: map[string]backend.Result{"squeue": {},
		"sacct": {Stdout: "COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T10:10:00|n1|9_1\nCOMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T10:10:00|n2|9_2\n"}}}
	st, err := (&Slurm{Runner: r}).Status(context.Background(), "9")
	if err != nil || st.Node != "" {
		t.Errorf("st = %+v, err = %v", st, err)
	}
	r = &fakeRunner{replies: map[string]backend.Result{"squeue": {Stdout: "PENDING|Priority|(null)|77\n"}}}
	st, err = (&Slurm{Runner: r}).Status(context.Background(), "77")
	if err != nil || st.Node != "" {
		t.Errorf("pending st = %+v, err = %v", st, err)
	}
}

// A heterogeneous job (components 100+0, 100+1) is one job with components,
// not an array of tasks.
func TestStatusHeterogeneousJob(t *testing.T) {
	const sacct = "RUNNING|0:0|2026-10-03T10:00:00|Unknown|n1|100+0\nRUNNING|0:0|2026-10-03T10:00:00|Unknown|n2|100+1\n"
	cases := []struct {
		name    string
		replies map[string]backend.Result
		state   store.JobState
		reason  string
	}{
		{"both running", map[string]backend.Result{
			"squeue": {Stdout: "RUNNING|None|n1|100+0\nRUNNING|None|n2|100+1\n"}, "sacct": {Stdout: sacct}}, store.StateRunning, "2 components"},
		{"one component left in squeue", map[string]backend.Result{
			"squeue": {Stdout: "RUNNING|None|n2|100+1\n"},
			"sacct":  {Stdout: "COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T10:10:00|n1|100+0\nRUNNING|0:0|2026-10-03T10:00:00|Unknown|n2|100+1\n"}}, store.StateRunning, "2 components, 1 done"},
		{"finished", map[string]backend.Result{
			"squeue": {},
			"sacct":  {Stdout: "COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T10:10:00|n1|100+0\nCOMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T10:12:00|n2|100+1\n"}}, store.StateCompleted, "2 components"},
		{"failed", map[string]backend.Result{
			"squeue": {},
			"sacct":  {Stdout: "COMPLETED|0:0|2026-10-03T10:00:00|2026-10-03T10:10:00|n1|100+0\nTIMEOUT|0:0|2026-10-03T10:00:00|2026-10-03T10:12:00|n2|100+1\n"}}, store.StateFailed, "2 components, 1 failed (TIMEOUT)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := (&Slurm{Runner: &fakeRunner{replies: tc.replies}}).Status(context.Background(), "100")
			if err != nil || st.State != tc.state || !strings.Contains(st.Reason, tc.reason) || strings.Contains(st.Reason, "tasks") {
				t.Errorf("st = %+v, err = %v; want %s ~%q", st, err, tc.state, tc.reason)
			}
		})
	}
}
