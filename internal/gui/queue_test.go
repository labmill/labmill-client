package gui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

// waitIdle waits until no turn runs.
func waitIdle(t *testing.T, a *App) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.mu.Lock()
		running := a.running
		a.mu.Unlock()
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn never ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// names returns the recorded event names, in order, filtered to want.
func (r *recorder) names(want ...string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		for _, w := range want {
			if e.name == w {
				out = append(out, e.name)
			}
		}
	}
	return out
}

func (r *recorder) all(name string) []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []any
	for _, e := range r.events {
		if e.name == name {
			out = append(out, e.data)
		}
	}
	return out
}

// stepSession blocks each turn until the test releases it (or ctx ends).
type stepSession struct {
	b       *Bridge
	got     chan string
	release chan struct{}
}

func (s *stepSession) Send(ctx context.Context, text string) {
	s.got <- text
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	s.b.Done(ctx.Err())
}
func (s *stepSession) Wait(time.Duration) bool { return true }

func newStep() (*recorder, *App, *stepSession) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	s := &stepSession{b: b, got: make(chan string, 8), release: make(chan struct{})}
	return rec, NewApp(b, s, nil, ""), s
}

func recv(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("no turn started")
	}
	return ""
}

func TestSendWhileBusyQueuesOneMessage(t *testing.T) {
	rec, a, s := newStep()
	if err := a.Send("first"); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, s.got); got != "first" {
		t.Fatalf("turn 1 = %q", got)
	}
	if err := a.Send("second"); err != nil {
		t.Fatalf("send while busy: %v", err)
	}
	if err := a.Send("third\x1b[31m"); err != nil { // replaces "second"
		t.Fatal(err)
	}
	q := rec.all(EvQueued)
	if len(q) != 2 || q[1].(QueuedEvent).Text != "third" {
		t.Fatalf("queued events = %+v", q)
	}
	s.release <- struct{}{}
	if got := recv(t, s.got); got != "third\x1b[31m" {
		t.Fatalf("turn 2 = %q, want the latest queued message", got)
	}
	s.release <- struct{}{}
	waitIdle(t, a)
	select {
	case extra := <-s.got:
		t.Fatalf("an extra turn ran: %q", extra)
	default:
	}
	turns := rec.all(EvTurn)
	if len(turns) != 2 || turns[0].(TurnEvent).Text != "first" || turns[1].(TurnEvent).Text != "third" {
		t.Fatalf("turn events = %+v", turns)
	}
	// Order: the queue is cleared before the turn that sends it starts, and
	// that turn starts after the previous one's done.
	want := []string{EvTurn, EvQueued, EvQueued, EvDone, EvQueued, EvTurn, EvDone}
	if got := rec.names(EvTurn, EvQueued, EvDone); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if last := rec.all(EvQueued); last[len(last)-1].(QueuedEvent).Text != "" {
		t.Errorf("the queue wasn't cleared: %+v", last)
	}
}

func TestCancelKeepsQueuedMessage(t *testing.T) {
	_, a, s := newStep()
	a.Send("slow")
	recv(t, s.got)
	a.Send("after cancel")
	a.Cancel()
	if got := recv(t, s.got); got != "after cancel" {
		t.Fatalf("next turn = %q", got)
	}
	s.release <- struct{}{}
	waitIdle(t, a)
}

func TestUnqueue(t *testing.T) {
	rec, a, s := newStep()
	a.Send("one")
	recv(t, s.got)
	a.Unqueue() // nothing queued: no event
	a.Send("two")
	a.Unqueue()
	q := rec.all(EvQueued)
	if len(q) != 2 || q[1].(QueuedEvent).Text != "" {
		t.Fatalf("queued events = %+v", q)
	}
	s.release <- struct{}{}
	waitIdle(t, a)
	select {
	case extra := <-s.got:
		t.Fatalf("a dropped message was sent: %q", extra)
	default:
	}
}

func TestShutdownDropsQueue(t *testing.T) {
	_, a, s := newStep()
	a.Send("one")
	recv(t, s.got)
	a.Send("two")
	a.Shutdown() // cancels "one"
	waitIdle(t, a)
	select {
	case extra := <-s.got:
		t.Fatalf("a queued message ran after shutdown: %q", extra)
	default:
	}
	if err := a.Send("late"); err == nil {
		t.Error("send after shutdown accepted")
	}
}

func TestJobActions(t *testing.T) {
	cases := []struct {
		j    store.Job
		want []string
	}{
		{store.Job{State: store.StateDraft}, []string{}},
		{store.Job{State: store.StateSubmitted, RemoteID: "1"}, []string{ActStatus, ActLogs, ActCancel}},
		{store.Job{State: store.StatePending, RemoteID: "1"}, []string{ActStatus, ActLogs, ActCancel}},
		{store.Job{State: store.StateRunning, RemoteID: "1", RemoteDir: "/r"}, []string{ActStatus, ActLogs, ActCancel}},
		{store.Job{State: store.StateCompleted, RemoteID: "1", RemoteDir: "/r"}, []string{ActStatus, ActLogs, ActFetch}},
		{store.Job{State: store.StateFailed, RemoteID: "1", RemoteDir: "/r"}, []string{ActStatus, ActLogs, ActFetch}},
		{store.Job{State: store.StateCompleted, RemoteID: "1"}, []string{ActStatus, ActLogs}},
		{store.Job{State: store.StateFetched, RemoteID: "1", RemoteDir: "/r"}, []string{ActStatus, ActLogs}},
		{store.Job{State: store.StateFetched, RemoteID: "1", RemoteDir: "/r", LocalDir: "/l"}, []string{ActStatus, ActLogs, ActOpen}},
		{store.Job{State: store.StateCanceled, RemoteID: "1"}, []string{ActStatus, ActLogs}},
	}
	for _, c := range cases {
		if got := jobActions(c.j); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s (remote %q, dir %q): %v, want %v", c.j.State, c.j.RemoteID, c.j.RemoteDir, got, c.want)
		}
	}
}

func TestJobActionSendsMessage(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	s := &fakeSession{b: b}
	jobs := func(context.Context) ([]store.Job, error) {
		return []store.Job{
			{ID: 12, Name: "seg", State: store.StateRunning, RemoteID: "900"},
			{ID: 13, Name: "old", State: store.StateCompleted, RemoteID: "901", RemoteDir: "/r"},
		}, nil
	}
	a := NewApp(b, s, jobs, "")
	for _, c := range []struct {
		id   int64
		act  string
		want string
	}{
		{12, ActStatus, "Show the status of job #12"},
		{12, ActLogs, "Show the last lines of the log of job #12"},
		{12, ActCancel, "Cancel job #12"},
		{13, ActFetch, "Fetch the results of job #13"},
	} {
		waitIdle(t, a)
		if err := a.JobAction(c.id, c.act); err != nil {
			t.Fatalf("%d %s: %v", c.id, c.act, err)
		}
		waitIdle(t, a)
		s.mu.Lock()
		got := s.got[len(s.got)-1]
		s.mu.Unlock()
		if got != c.want {
			t.Errorf("%d %s sent %q, want %q", c.id, c.act, got, c.want)
		}
	}
	for _, bad := range []struct {
		id  int64
		act string
	}{{12, ActFetch}, {13, ActCancel}, {99, ActStatus}, {12, "rm"}} {
		if err := a.JobAction(bad.id, bad.act); err == nil {
			t.Errorf("JobAction(%d, %q) accepted", bad.id, bad.act)
		}
	}
	js, _ := a.Jobs()
	if len(js) != 2 || !reflect.DeepEqual(js[0].Actions, []string{ActStatus, ActLogs, ActCancel}) {
		t.Errorf("job views = %+v", js)
	}
}

func TestJobNoticeCleaned(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	b.JobNotice("Job #12 (seg\x1b[2J) on hpcone finished.")
	ev := rec.waitFor(t, EvJob).(JobNoticeEvent)
	if strings.Contains(ev.Text, "\x1b") || !strings.Contains(ev.Text, "Job #12") {
		t.Errorf("notice = %q", ev.Text)
	}
	b.Close()
	b.JobNotice("after close") // must not emit or block
	if n := len(rec.all(EvJob)); n != 1 {
		t.Errorf("notices after close = %d", n)
	}
}

// M60.1: a job whose results came down by themselves is a job notice that
// carries the job's ID (the page offers "Open folder").
func TestJobFetchedCarriesJobID(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	b.JobFetched(12, "Job #12 (seg\x1b[2J): downloaded 2 files.")
	ev := rec.waitFor(t, EvJob).(JobNoticeEvent)
	if ev.JobID != 12 || strings.Contains(ev.Text, "\x1b") || !strings.Contains(ev.Text, "downloaded 2 files") {
		t.Errorf("event = %+v", ev)
	}
	b.Close()
}

func TestTransferEventCleaned(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	b.Transfer(transfer.Update{Tool: "upload_data", Kind: "progress", Text: "Transferred: 1 B / 2 B\x1b[2J", Current: "a\x1b[2Jb", Bytes: 1, TotalBytes: 2, ETASeconds: -1})
	ev := rec.waitFor(t, EvProgress).(ProgressEvent)
	if strings.Contains(ev.Text+ev.Current, "\x1b") || ev.Kind != "progress" || ev.Bytes != 1 || ev.TotalBytes != 2 || ev.ETASeconds != -1 {
		t.Errorf("event = %+v", ev)
	}
	b.Close()
	b.Transfer(transfer.Update{Kind: "end"}) // must not emit or block
	if n := len(rec.all(EvProgress)); n != 1 {
		t.Errorf("events after close = %d", n)
	}
}

// A job button never replaces the user's queued message; with the queue
// empty it queues like a typed message.
func TestJobActionKeepsQueuedMessage(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	s := &fakeSession{b: b, block: make(chan struct{})}
	jobs := func(context.Context) ([]store.Job, error) {
		return []store.Job{{ID: 12, Name: "seg", State: store.StateRunning, RemoteID: "900"}}, nil
	}
	a := NewApp(b, s, jobs, "")
	if err := a.Send("first"); err != nil {
		t.Fatal(err)
	}
	if err := a.JobAction(12, ActStatus); err != nil { // queue empty: queued
		t.Fatalf("button with empty queue: %v", err)
	}
	a.mu.Lock()
	q := a.queued
	a.mu.Unlock()
	if q != "Show the status of job #12" {
		t.Fatalf("queued = %q", q)
	}
	a.Unqueue()
	if err := a.Send("mine"); err != nil {
		t.Fatal(err)
	}
	if err := a.JobAction(12, ActLogs); !errors.Is(err, ErrQueueBusy) {
		t.Fatalf("err = %v, want ErrQueueBusy", err)
	}
	a.mu.Lock()
	q = a.queued
	a.mu.Unlock()
	if q != "mine" {
		t.Errorf("the user's queued message was replaced: %q", q)
	}
	close(s.block)
	waitIdle(t, a)
}
