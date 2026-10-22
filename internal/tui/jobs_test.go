package tui

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"labmill-client/internal/store"
)

// fakeJobs is a job lister whose result tests can swap.
type fakeJobs struct {
	jobs  atomic.Pointer[[]store.Job]
	err   atomic.Pointer[error]
	calls atomic.Int32
}

func (f *fakeJobs) set(j ...store.Job) { f.jobs.Store(&j) }

func (f *fakeJobs) list(context.Context) ([]store.Job, error) {
	f.calls.Add(1)
	if e := f.err.Load(); e != nil {
		return nil, *e
	}
	if j := f.jobs.Load(); j != nil {
		return *j, nil
	}
	return nil, nil
}

// jobsModel returns a model with the jobs panel open at the given size.
func jobsModel(t *testing.T, f *fakeJobs, w, h int) *model {
	t.Helper()
	m := newModel(Options{Jobs: f.list})
	step(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	step(t, m, kp(tea.KeyTab))
	if !m.showJobs {
		t.Fatal("jobs panel not shown")
	}
	return m
}

// fetch runs the model's fetch command and delivers its result.
func fetch(t *testing.T, m *model) {
	t.Helper()
	msg := m.fetchJobs()()
	if _, ok := msg.(jobsMsg); !ok {
		t.Fatalf("fetchJobs returned %T", msg)
	}
	step(t, m, msg)
}

func TestJobsPanelRendersRows(t *testing.T) {
	f := &fakeJobs{}
	f.set(
		store.Job{Name: "train-seg", Cluster: "hpcone", State: store.JobState("RUNNING"), UpdatedAt: time.Now().Add(-5 * time.Minute)},
		store.Job{Name: "eval", Cluster: "gpu-two", State: store.JobState("COMPLETED"), UpdatedAt: time.Now().Add(-3 * time.Hour)},
	)
	m := jobsModel(t, f, 100, 24)
	fetch(t, m)
	v := stripStyles(content(m))
	for _, want := range []string{"Jobs", "train-seg", "hpcone · RUNNING · 5m ago", "eval", "gpu-two · COMPLETED · 3h ago"} {
		if !strings.Contains(v, want) {
			t.Errorf("%q missing from view:\n%s", want, v)
		}
	}
	if strings.Contains(v, "no jobs yet") {
		t.Errorf("empty-state message shown with jobs present:\n%s", v)
	}
}

func TestJobsPanelEmptyState(t *testing.T) {
	f := &fakeJobs{}
	m := jobsModel(t, f, 100, 24)
	fetch(t, m)
	if v := stripStyles(content(m)); !strings.Contains(v, "no jobs yet") {
		t.Errorf("no empty-state message:\n%s", v)
	}
}

func TestJobsPanelShowsListerError(t *testing.T) {
	f := &fakeJobs{}
	e := errors.New("db locked\x1b[31m\nsecond line")
	f.err.Store(&e)
	m := jobsModel(t, f, 100, 24)
	fetch(t, m)
	v := content(m)
	if !strings.Contains(stripStyles(v), "db locked second line") || strings.Contains(stripStyles(v), "[31m") {
		t.Errorf("error not shown as one clean line:\n%s", v)
	}
}

func TestJobsPanelRefreshTick(t *testing.T) {
	f := &fakeJobs{}
	f.set(store.Job{Name: "first-job", Cluster: "c", State: store.JobState("PENDING"), UpdatedAt: time.Now()})
	m := jobsModel(t, f, 100, 24)
	fetch(t, m)
	if v := content(m); !strings.Contains(v, "first-job") || !strings.Contains(v, "PENDING") {
		t.Fatalf("first fetch not shown:\n%s", v)
	}
	f.set(store.Job{Name: "first-job", Cluster: "c", State: store.JobState("RUNNING"), UpdatedAt: time.Now()},
		store.Job{Name: "second-job", Cluster: "c", State: store.JobState("PENDING"), UpdatedAt: time.Now()})
	before := f.calls.Load()
	// A tick re-arms itself and fetches again: run the returned command and
	// step the model with the jobsMsg it produces (the re-armed tick
	// sub-command sleeps, so it is not waited for).
	_, cmd := m.Update(jobsTickMsg{})
	if cmd == nil {
		t.Fatal("tick returned no command")
	}
	msg := firstJobsMsg(t, cmd)
	step(t, m, msg)
	if f.calls.Load() <= before {
		t.Error("tick did not fetch")
	}
	v := content(m)
	if !strings.Contains(v, "second-job") || !strings.Contains(v, "RUNNING") {
		t.Errorf("refresh not shown:\n%s", v)
	}
	// Without a lister the tick does nothing.
	m2 := newModel(Options{})
	if _, cmd := m2.Update(jobsTickMsg{}); cmd != nil {
		t.Error("tick without a lister returned a command")
	}
}

// firstJobsMsg runs cmd (flattening tea.BatchMsg, each sub-command in its own
// goroutine) and returns the first jobsMsg produced within a second.
func firstJobsMsg(t *testing.T, cmd tea.Cmd) jobsMsg {
	t.Helper()
	ch := make(chan tea.Msg, 16)
	var run func(c tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		go func() {
			msg := c()
			if b, ok := msg.(tea.BatchMsg); ok {
				for _, sub := range b {
					run(sub)
				}
				return
			}
			ch <- msg
		}()
	}
	run(cmd)
	timeout := time.After(time.Second)
	for {
		select {
		case msg := <-ch:
			if jm, ok := msg.(jobsMsg); ok {
				return jm
			}
		case <-timeout:
			t.Fatal("no jobsMsg from the tick command within 1s")
		}
	}
}

func TestJobsPanelLongNameTruncated(t *testing.T) {
	f := &fakeJobs{}
	long := "train-" + strings.Repeat("x", 120) + "-END"
	f.set(store.Job{Name: long, Cluster: "hpcone", State: store.JobState("RUNNING"), UpdatedAt: time.Now()})
	m := jobsModel(t, f, 100, 24)
	fetch(t, m)
	v := content(m)
	if strings.Contains(v, "-END") {
		t.Errorf("long name not clipped:\n%s", v)
	}
	for _, l := range strings.Split(v, "\n") {
		if lipgloss.Width(l) > 100 {
			t.Fatalf("row too wide (%d): %q", lipgloss.Width(l), l)
		}
	}
}

func TestJobsPanelCleansText(t *testing.T) {
	f := &fakeJobs{}
	f.set(store.Job{
		Name:      "evil\x1b]8;;http://evil.example\x07click\x1b]8;;\x07\x1b[2Jjob",
		Cluster:   "hpc\x1b[2J\x1b[31mone",
		State:     store.JobState("RUN\u009b31m\x1b[2JNING"),
		UpdatedAt: time.Now(),
	})
	m := jobsModel(t, f, 100, 24)
	fetch(t, m)
	raw := content(m)
	for _, bad := range []string{"hpc\x1b", "\x1b[2J", "evil.example", "\x1b]8", "\x07"} {
		if strings.Contains(raw, bad) {
			t.Errorf("raw output contains %q:\n%q", bad, raw)
		}
	}
	if strings.ContainsRune(raw, 0x9b) {
		t.Errorf("raw output contains U+009B:\n%q", raw)
	}
	v := stripStyles(raw)
	for _, want := range []string{"clickjob", "hpcone", "RUNNING"} {
		if !strings.Contains(v, want) {
			t.Errorf("%q missing:\n%s", want, v)
		}
	}
}
