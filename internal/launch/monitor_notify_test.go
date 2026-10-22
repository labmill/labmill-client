package launch

import (
	"context"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

func TestEndedText(t *testing.T) {
	for _, c := range []struct {
		state  store.JobState
		reason string
		want   string
	}{
		{store.StateCompleted, "", "seg completed"},
		{store.StateFailed, "exit 1", "seg failed"},
		{store.StateFailed, "TIMEOUT", "seg timed out"},
		{store.StateCanceled, "", "seg was canceled"},
		{store.StateRunning, "", ""},
		{store.StatePending, "", ""},
	} {
		got, ok := endedText("seg", c.state, c.reason)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("endedText(%s, %q) = %q, %v; want %q", c.state, c.reason, got, ok, c.want)
		}
	}
}

func TestMonitorEndedHook(t *testing.T) {
	f := newMonitorFixture(t)
	var texts []string
	f.m.ended = func(s string) { texts = append(texts, s) }
	f.job(t, store.Job{Name: "a", Cluster: "hpcone", Backend: "slurm", RemoteID: "1", State: store.StatePending})
	f.job(t, store.Job{Name: "b", Cluster: "hpcone", Backend: "slurm", RemoteID: "2", State: store.StateRunning})
	f.be.set("1", backend.Status{State: store.StateRunning}) // not an end
	f.be.set("2", backend.Status{State: store.StateCompleted})
	f.m.check(context.Background())
	f.m.check(context.Background()) // no repeat
	if len(texts) != 1 || texts[0] != "b completed" {
		t.Errorf("ended texts = %q", texts)
	}
}

func TestAwaySummary(t *testing.T) {
	jobs := []awayEntry{{13, "train", store.StateFailed}, {12, "seg", store.StateCompleted}}
	want := "While you were away: job 12 (seg) completed, job 13 (train) failed."
	if got := awaySummary(jobs); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := awaySummary(nil); got != "" {
		t.Errorf("nothing changed, got %q", got)
	}
	var many []awayEntry
	for i := range 12 {
		many = append(many, awayEntry{int64(i + 1), "j", store.StateCompleted})
	}
	if got := awaySummary(many); !strings.HasSuffix(got, ", and 2 more.") {
		t.Errorf("long list: %q", got)
	}
}

// The first check holds back its per-job notices and sends one summary; the
// last-seen hook runs after it.
func TestMonitorWhileAway(t *testing.T) {
	f := newMonitorFixture(t)
	f.notices.ch = make(chan string, 4)
	f.job(t, store.Job{Name: "seg", Cluster: "hpcone", Backend: "slurm", RemoteID: "1", State: store.StateRunning})
	f.be.set("1", backend.Status{State: store.StateCompleted})
	f.m.awaySince = time.Now().Add(-time.Hour)
	var desktop []string
	f.m.ended = func(s string) { desktop = append(desktop, s) }
	alive := make(chan struct{}, 4)
	f.m.alive = func() { alive <- struct{}{} }
	clock := &fakeTicks{pauses: make(chan chan time.Time, 4)}
	f.m.start(clock.after) // checks at once, before the first pause
	select {
	case n := <-f.notices.ch:
		if n != "While you were away: job 1 (seg) completed." {
			t.Errorf("notice = %q", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no summary")
	}
	<-alive
	f.m.Close()
	if got := f.notices.all(); len(got) != 1 {
		t.Errorf("notices = %q, want only the summary", got)
	}
	if len(desktop) != 1 || !strings.HasPrefix(desktop[0], "While you were away:") {
		t.Errorf("desktop = %q, want one summary", desktop)
	}
}
