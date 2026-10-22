package tui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"labmill-client/internal/agent"
	"labmill-client/internal/sshx"
)

func TestBridgeApproveRoundTrip(t *testing.T) {
	b := NewBridge()
	go func() {
		msg := b.Next()()
		req, ok := msg.(ApprovalRequestMsg)
		if !ok {
			t.Errorf("got %T", msg)
			return
		}
		req.Reply <- agent.Decision{Approved: true, Reason: "ok"}
	}()
	d, err := b.Approve(context.Background(), agent.Proposal{Summary: "Upload 3 files"})
	if err != nil || !d.Approved {
		t.Fatalf("Approve = %+v, %v", d, err)
	}
}

func TestBridgeApproveCanceled(t *testing.T) {
	b := NewBridge()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { b.Next()(); cancel() }() // UI receives the request but the turn is canceled
	if _, err := b.Approve(ctx, agent.Proposal{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestBridgeAskAndNotify(t *testing.T) {
	b := NewBridge()
	go func() {
		for {
			switch m := b.Next()().(type) {
			case PromptRequestMsg:
				m.Reply <- PromptAnswer{Text: "s3cret"}
				return
			case NotifyMsg:
			}
		}
	}()
	b.Notify("connecting")
	ans, err := b.Ask(context.Background(), sshx.Question{Kind: sshx.AskPassword, Host: "login.example.edu", Prompt: "Password: "})
	if err != nil || ans != "s3cret" {
		t.Fatalf("Ask = %q, %v", ans, err)
	}
	// Notify never blocks, even with nobody listening.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			b.Notify("spam")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked")
	}
}

// fakeSession plays a scripted turn using the bridge.
type fakeSession struct {
	b      *Bridge
	script func(ctx context.Context, b *Bridge, text string)
	mu     sync.Mutex
	got    []string
}

func (f *fakeSession) Send(ctx context.Context, text string) {
	f.mu.Lock()
	f.got = append(f.got, text)
	f.mu.Unlock()
	f.script(ctx, f.b, text)
}

func newTM(t *testing.T, s *fakeSession) *teatest.TestModel {
	t.Helper()
	m := NewModel(Options{Session: s, Bridge: s.b, Status: "hpcone (Slurm)", Welcome: "Hi! What would you like to run?"})
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(100, 30))
	t.Cleanup(func() { tm.Quit(); s.b.Close() })
	return tm
}

func waitFor(t *testing.T, tm *teatest.TestModel, s string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte(s)) },
		teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(20*time.Millisecond))
}

func typeLine(tm *teatest.TestModel, s string) {
	tm.Type(s)
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
}

func TestChatStreamsReply(t *testing.T) {
	b := NewBridge()
	s := &fakeSession{b: b, script: func(ctx context.Context, b *Bridge, text string) {
		b.Event(agent.Event{Kind: agent.EventText, Text: "Found 3 files "})
		b.Event(agent.Event{Kind: agent.EventText, Text: "in ~/data/mri-2026."})
		b.Done(nil)
	}}
	tm := newTM(t, s)
	waitFor(t, tm, "What would you like to run?")
	typeLine(tm, "train on my mri scans")
	waitFor(t, tm, "in ~/data/mri-2026.")
	if len(s.got) != 1 || s.got[0] != "train on my mri scans" {
		t.Errorf("session got %v", s.got)
	}
}

func TestApprovalModalApprove(t *testing.T) {
	b := NewBridge()
	decisions := make(chan agent.Decision, 1)
	s := &fakeSession{b: b, script: func(ctx context.Context, b *Bridge, text string) {
		d, err := b.Approve(ctx, agent.Proposal{Tool: "upload_data", Summary: "Upload 3 files (2 KB) to hpcone:labmill/seg", Details: "data/a.csv\ndata/b.csv\ndata/c.csv"})
		if err == nil {
			decisions <- d
		}
		b.Done(err)
	}}
	tm := newTM(t, s)
	typeLine(tm, "go")
	waitFor(t, tm, "Upload 3 files (2 KB) to hpcone:labmill/seg")
	// (the summary and the Approve button arrive in the same frame, and WaitFor
	// consumes the output it has read, so a second wait for "Approve" cannot match)
	time.Sleep(keyGrace + 100*time.Millisecond) // keys typed ahead of a new dialog are dropped
	// Decline is the default button, so move to Approve first: the approval
	// modal toggles its buttons with Left/Right/Tab.
	tm.Send(tea.KeyPressMsg{Code: tea.KeyLeft})
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case d := <-decisions:
		if !d.Approved {
			t.Errorf("decision = %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no decision")
	}
}

func TestApprovalModalEscDeclines(t *testing.T) {
	b := NewBridge()
	decisions := make(chan agent.Decision, 1)
	s := &fakeSession{b: b, script: func(ctx context.Context, b *Bridge, text string) {
		d, _ := b.Approve(ctx, agent.Proposal{Summary: "Cancel job train (4242) on hpcone"})
		decisions <- d
		b.Done(nil)
	}}
	tm := newTM(t, s)
	typeLine(tm, "cancel it")
	waitFor(t, tm, "Cancel job train (4242)")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	select {
	case d := <-decisions:
		if d.Approved {
			t.Error("Esc approved")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no decision")
	}
}

func TestLoginModalPassword(t *testing.T) {
	b := NewBridge()
	answers := make(chan string, 1)
	s := &fakeSession{b: b, script: func(ctx context.Context, b *Bridge, text string) {
		a, err := b.Ask(ctx, sshx.Question{Kind: sshx.AskPassword, Host: "login.example.edu", Prompt: "Password: "})
		if err == nil {
			answers <- a
		}
		b.Done(err)
	}}
	tm := newTM(t, s)
	typeLine(tm, "status")
	waitFor(t, tm, "login.example.edu")
	time.Sleep(keyGrace + 100*time.Millisecond)
	tm.Type("hunter2")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case a := <-answers:
		if a != "hunter2" {
			t.Errorf("answer = %q", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}
	// The password must never appear on screen.
	out := tm.Output()
	buf := new(bytes.Buffer)
	buf.ReadFrom(out)
	if strings.Contains(buf.String(), "hunter2") {
		t.Error("password echoed on screen")
	}
}

func TestCtrlCCancelsTurn(t *testing.T) {
	b := NewBridge()
	canceled := make(chan struct{})
	s := &fakeSession{b: b, script: func(ctx context.Context, b *Bridge, text string) {
		<-ctx.Done()
		close(canceled)
		b.Done(ctx.Err())
	}}
	tm := newTM(t, s)
	typeLine(tm, "long job")
	tm.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl+C did not cancel the turn")
	}
}

// startDecline opens an approval, moves past the grace period and presses
// Enter on the default Decline button, which opens the "Why?" prompt.
func startDecline(t *testing.T) (*teatest.TestModel, chan agent.Decision, chan struct{}) {
	t.Helper()
	b := NewBridge()
	decisions := make(chan agent.Decision, 2)
	release := make(chan struct{})
	s := &fakeSession{b: b, script: func(ctx context.Context, b *Bridge, text string) {
		d, _ := b.Approve(ctx, agent.Proposal{Summary: "Submit job train on hpcone"})
		decisions <- d
		<-release
		b.Done(nil)
	}}
	tm := newTM(t, s)
	typeLine(tm, "go")
	waitFor(t, tm, "Submit job train on hpcone")
	time.Sleep(keyGrace + 100*time.Millisecond)
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	waitFor(t, tm, "Why? (optional")
	return tm, decisions, release
}

func TestDeclineReasonTyped(t *testing.T) {
	tm, decisions, release := startDecline(t)
	defer close(release)
	tm.Type("use the gpu\tpartition")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case d := <-decisions:
		if d.Approved || d.Reason != "use the gpu partition" {
			t.Errorf("decision = %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no decision")
	}
}

func TestDeclineReasonEscSkips(t *testing.T) {
	tm, decisions, release := startDecline(t)
	defer close(release)
	tm.Type("typed but skipped")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	select {
	case d := <-decisions:
		if d.Approved || d.Reason != "declined in the dialog" {
			t.Errorf("decision = %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no decision")
	}
}

func TestDeclineReasonTurnEndAnswersOnce(t *testing.T) {
	tm, decisions, release := startDecline(t)
	tm.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}) // cancel the turn during the prompt
	select {
	case d := <-decisions:
		if d.Approved || d.Reason != "the turn was canceled" {
			t.Errorf("decision = %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no decision")
	}
	close(release)
	select {
	case d := <-decisions:
		t.Errorf("a second decision was sent: %+v", d)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestCleanReason(t *testing.T) {
	if got := cleanReason("  a\x1b[31m b\nc\t d "); got != "a b c d" {
		t.Errorf("cleanReason = %q", got)
	}
	if n := len([]rune(cleanReason(strings.Repeat("é", 800)))); n != maxReasonRunes {
		t.Errorf("runes = %d", n)
	}
}
