package gui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"labmill-client/internal/agent"
	"labmill-client/internal/sshx"
	"labmill-client/internal/store"
)

type recorder struct {
	mu     sync.Mutex
	events []struct {
		name string
		data any
	}
	ch chan struct{}
}

func newRecorder() *recorder { return &recorder{ch: make(chan struct{}, 1000)} }

func (r *recorder) emit(name string, data any) {
	r.mu.Lock()
	r.events = append(r.events, struct {
		name string
		data any
	}{name, data})
	r.mu.Unlock()
	r.ch <- struct{}{}
}

// waitFor returns the first event with that name (waiting up to 2s).
func (r *recorder) waitFor(t *testing.T, name string) any {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		r.mu.Lock()
		for _, e := range r.events {
			if e.name == name {
				r.mu.Unlock()
				return e.data
			}
		}
		r.mu.Unlock()
		select {
		case <-r.ch:
		case <-deadline:
			t.Fatalf("no %s event", name)
		}
	}
}

func TestApproveRoundTrip(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	got := make(chan agent.Decision, 1)
	go func() {
		d, _ := b.Approve(context.Background(), agent.Proposal{Tool: "upload_data", Summary: "Upload \x1b[8m3 files", Details: "a.csv"})
		got <- d
	}()
	ev := rec.waitFor(t, EvApproval).(ApprovalEvent)
	if strings.Contains(ev.Summary, "\x1b") || ev.Tool != "upload_data" || ev.ID == "" {
		t.Fatalf("approval event = %+v", ev)
	}
	if err := b.Decide(ev.ID, true, "", ""); err != nil {
		t.Fatal(err)
	}
	if d := <-got; !d.Approved {
		t.Errorf("decision = %+v", d)
	}
	// A second click on the same request is rejected.
	if err := b.Decide(ev.ID, true, "", ""); err == nil {
		t.Error("double decide accepted")
	}
	if err := b.Decide("nope", true, "", ""); err == nil {
		t.Error("unknown id accepted")
	}
}

func TestEditedOnlyForEditable(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	got := make(chan agent.Decision, 1)
	go func() {
		d, _ := b.Approve(context.Background(), agent.Proposal{Summary: "Cancel job", Details: "x", Editable: false})
		got <- d
	}()
	ev := rec.waitFor(t, EvApproval).(ApprovalEvent)
	b.Decide(ev.ID, true, "something else", "")
	if d := <-got; d.Edited != "" {
		t.Errorf("Edited set for a non-editable proposal: %+v", d)
	}
}

func TestDoneAnswersPendingAndDismisses(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	approved := make(chan agent.Decision, 1)
	asked := make(chan error, 1)
	go func() { d, _ := b.Approve(context.Background(), agent.Proposal{Summary: "s"}); approved <- d }()
	rec.waitFor(t, EvApproval)
	go func() {
		_, err := b.Ask(context.Background(), sshx.Question{Kind: sshx.AskPassword, Host: "h", Prompt: "Password: "})
		asked <- err
	}()
	rec.waitFor(t, EvPrompt)
	b.Done(nil)
	if d := <-approved; d.Approved {
		t.Error("pending approval approved by Done")
	}
	if err := <-asked; err == nil {
		t.Error("pending prompt not canceled by Done")
	}
	ev := rec.waitFor(t, EvDismiss).(DismissEvent)
	if len(ev.IDs) == 0 {
		t.Error("no dismissed IDs")
	}
	rec.waitFor(t, EvDone)
}

func TestCloseAnswersEverything(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	errs := make(chan error, 1)
	go func() { _, err := b.Ask(context.Background(), sshx.Question{Kind: sshx.AskPassword}); errs <- err }()
	rec.waitFor(t, EvPrompt)
	b.Close()
	if err := <-errs; err == nil {
		t.Error("Ask survived Close")
	}
	if _, err := b.Approve(context.Background(), agent.Proposal{}); err == nil {
		t.Error("Approve after Close succeeded")
	}
	b.Event(agent.Event{Kind: agent.EventText, Text: "late"}) // must not block or panic
	b.Close()                                                 // idempotent
}

func TestPromptPasswordAnswer(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	got := make(chan string, 1)
	go func() {
		a, _ := b.Ask(context.Background(), sshx.Question{Kind: sshx.AskPassword, Host: "login.example.edu", Prompt: "Password: "})
		got <- a
	}()
	ev := rec.waitFor(t, EvPrompt).(PromptEvent)
	if ev.Kind != "password" || ev.Echo || ev.Host != "login.example.edu" {
		t.Fatalf("prompt = %+v", ev)
	}
	b.Answer(ev.ID, "hunter2", false)
	if a := <-got; a != "hunter2" {
		t.Errorf("answer = %q", a)
	}
	// The password never appears in any emitted event.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, e := range rec.events {
		if strings.Contains(strings.ToLower(stringify(e.data)), "hunter2") {
			t.Errorf("password emitted in %s", e.name)
		}
	}
}

// TestPromptKeySetupKinds: the SSH key setup questions reach the page with
// their own kinds, and the public key travels in Data (cleaned); a passphrase
// answer is never emitted.
func TestPromptKeySetupKinds(t *testing.T) {
	for _, tc := range []struct {
		kind sshx.QuestionKind
		want string
	}{
		{sshx.AskKeyPassphrase, "keypassphrase"}, {sshx.AskKeyCreate, "keycreate"},
		{sshx.AskNewPassphrase, "newpassphrase"}, {sshx.AskKeyUpload, "keyupload"},
		{sshx.AskKeyCopy, "keycopy"},
	} {
		rec := newRecorder()
		b := NewBridge(rec.emit)
		got := make(chan string, 1)
		go func() {
			a, _ := b.Ask(context.Background(), sshx.Question{Kind: tc.kind, Host: "login.example.edu", Prompt: "p", Data: "ssh-ed25519 AAAA labmill-x\x1b[31m"})
			got <- a
		}()
		ev := rec.waitFor(t, EvPrompt).(PromptEvent)
		if ev.Kind != tc.want || ev.Data != "ssh-ed25519 AAAA labmill-x[31m" && ev.Data != "ssh-ed25519 AAAA labmill-x" {
			t.Fatalf("%s: prompt = %+v", tc.want, ev)
		}
		b.Answer(ev.ID, "s3cretphrase", false)
		if a := <-got; a != "s3cretphrase" {
			t.Errorf("answer = %q", a)
		}
		rec.mu.Lock()
		for _, e := range rec.events {
			if strings.Contains(stringify(e.data), "s3cretphrase") {
				t.Errorf("answer emitted in %s", e.name)
			}
		}
		rec.mu.Unlock()
	}
}

func stringify(v any) string {
	switch x := v.(type) {
	case TextEvent:
		return x.Text
	case PromptEvent:
		return x.Prompt + x.Host
	case NotifyEvent:
		return x.Text
	case ApprovalEvent:
		return x.Summary + x.Details
	case ToolEvent:
		return x.Tool + x.Text
	case DoneEvent:
		return x.Error
	}
	return ""
}

func TestEventsCleaned(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	b.Event(agent.Event{Kind: agent.EventText, Text: "hi\x1b[2Jthere"})
	ev := rec.waitFor(t, EvText).(TextEvent)
	if ev.Text != "hithere" {
		t.Errorf("text = %q", ev.Text)
	}
	b.Event(agent.Event{Kind: agent.EventToolResult, Tool: "job_status", Text: "running", IsError: false})
	te := rec.waitFor(t, EvTool).(ToolEvent)
	if te.Kind != "result" || te.Tool != "job_status" {
		t.Errorf("tool = %+v", te)
	}
	b.Event(agent.Event{Kind: agent.EventStep, Text: "Connecting to \x1b]8;;x\x07hpcone…"})
	se := rec.waitFor(t, EvStep).(StepEvent)
	if strings.ContainsAny(se.Text, "\x1b\x07") || !strings.HasPrefix(se.Text, "Connecting to ") || !strings.HasSuffix(se.Text, "hpcone…") {
		t.Errorf("step = %q", se.Text)
	}
	// A conversation note (e.g. after compaction) is a system line.
	b.Event(agent.Event{Kind: agent.EventNote, Text: agent.NoteCompacted + "\x1b[2J"})
	if ne := rec.waitFor(t, EvNotice).(NoticeEvent); ne.Text != agent.NoteCompacted {
		t.Errorf("note = %q", ne.Text)
	}
}

type fakeSession struct {
	b       *Bridge
	mu      sync.Mutex
	running bool
	got     []string
	block   chan struct{}
}

func (f *fakeSession) Send(ctx context.Context, text string) {
	f.mu.Lock()
	f.got = append(f.got, text)
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
		}
	}
	f.b.Done(ctx.Err())
}
func (f *fakeSession) Wait(time.Duration) bool { return true }

func TestAppSendCancelJobs(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	s := &fakeSession{b: b, block: make(chan struct{})}
	jobs := func(context.Context) ([]store.Job, error) {
		return []store.Job{{ID: 1, Name: "seg\x1b[31m", Cluster: "hpcone", State: store.StateRunning, UpdatedAt: time.Unix(0, 0)}}, nil
	}
	a := NewApp(b, s, jobs, "hpcone (Slurm)")
	if err := a.Send("  "); err == nil {
		t.Error("empty send accepted")
	}
	if err := a.Send("train it"); err != nil {
		t.Fatal(err)
	}
	a.Cancel()
	ev := rec.waitFor(t, EvDone).(DoneEvent)
	if !strings.Contains(ev.Error, "cancel") {
		t.Errorf("done = %+v", ev)
	}
	// After the turn ended, sending works again.
	waitIdle(t, a)
	if err := a.Send("next"); err != nil {
		t.Fatal(err)
	}
	close(s.block)
	js, err := a.Jobs()
	if err != nil || len(js) != 1 || strings.Contains(js[0].Name, "\x1b") || js[0].State != "running" {
		t.Errorf("jobs = %+v %v", js, err)
	}
	if a.Status() != "hpcone (Slurm)" {
		t.Errorf("status = %q", a.Status())
	}
}

func decideWith(t *testing.T, approve bool, reason string) agent.Decision {
	t.Helper()
	rec := newRecorder()
	b := NewBridge(rec.emit)
	got := make(chan agent.Decision, 1)
	go func() {
		d, _ := b.Approve(context.Background(), agent.Proposal{Summary: "s", Details: "x"})
		got <- d
	}()
	ev := rec.waitFor(t, EvApproval).(ApprovalEvent)
	if err := b.Decide(ev.ID, approve, "", reason); err != nil {
		t.Fatal(err)
	}
	return <-got
}

func TestDeclineReason(t *testing.T) {
	d := decideWith(t, false, "  use the gpu\x1b[31m partition\nplease\r\n ")
	if d.Approved || d.Reason != "use the gpu partition please" {
		t.Errorf("decision = %+v", d)
	}
	long := strings.Repeat("é", 700)
	d = decideWith(t, false, long)
	if n := len([]rune(d.Reason)); n != maxReasonRunes {
		t.Errorf("reason runes = %d, want %d", n, maxReasonRunes)
	}
	if d := decideWith(t, false, "  \n "); d.Reason != "declined in the dialog" {
		t.Errorf("empty reason = %q", d.Reason)
	}
}

func TestApproveIgnoresReason(t *testing.T) {
	d := decideWith(t, true, "use the gpu partition")
	if !d.Approved || d.Reason != "" {
		t.Errorf("decision = %+v", d)
	}
}
