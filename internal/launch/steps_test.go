package launch

import (
	"context"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/agent"
	"labmill-client/internal/sshx"
)

// stepUI is a fakeUI that records the steps it is shown.
type stepUI struct {
	fakeUI
	mu    sync.Mutex
	steps []string
}

func (u *stepUI) Event(e agent.Event) {
	if e.Kind == agent.EventStep {
		u.mu.Lock()
		u.steps = append(u.steps, e.Text)
		u.mu.Unlock()
	}
}

func (u *stepUI) got() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.steps...)
}

func TestStepTrackerRestoresThePreviousStep(t *testing.T) {
	ui := &stepUI{}
	s := newStepTracker(ui)
	s.observe(agent.Event{Kind: agent.EventStep, Text: agent.StepWaitingForClaude})
	endRead := s.begin("Reading the sign-in from login…")
	endConn := s.begin("Connecting to login…")
	endConn()
	endRead()
	want := []string{"Reading the sign-in from login…", "Connecting to login…", "Reading the sign-in from login…", agent.StepWaitingForClaude}
	if got := ui.got(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("steps = %q, want %q", got, want)
	}

	// A tool call becomes the current step; once the reply text started
	// nothing is put back (the indicator is gone).
	ui.steps = nil
	s.observe(agent.Event{Kind: agent.EventToolCall, Tool: "cluster_info"})
	s.begin("Connecting to hpcone…")()
	s.observe(agent.Event{Kind: agent.EventText, Text: "hi"})
	s.begin("Connecting to hpcone…")()
	want = []string{"Connecting to hpcone…", "Running cluster_info…", "Connecting to hpcone…"}
	if got := ui.got(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("steps = %q, want %q", got, want)
	}

	// A newer step is not overwritten by an older part ending.
	ui.steps = nil
	s.observe(agent.Event{Kind: agent.EventStep, Text: "A…"})
	end := s.begin("B…")
	s.observe(agent.Event{Kind: agent.EventStep, Text: "C…"})
	end()
	if got := ui.got(); strings.Join(got, "|") != "B…" {
		t.Errorf("steps = %q, want only B…", got)
	}

	var nilTracker *stepTracker
	nilTracker.observe(agent.Event{Kind: agent.EventText})
	nilTracker.begin("x")() // no panic
}

// An SSH login shows "Connecting to <alias>…"; a cached client shows nothing.
func TestConnectShowsStep(t *testing.T) {
	ui := &stepUI{}
	cc := newConnCache(ui)
	cc.steps = newStepTracker(ui)
	cc.steps.observe(agent.Event{Kind: agent.EventToolCall, Tool: "cluster_info"})
	old := sshConnect
	t.Cleanup(func() { sshConnect = old })
	sshConnect = func(context.Context, string, sshx.Prompter, sshx.ConnectOptions) (*ssh.Client, error) {
		return nil, context.Canceled
	}
	if _, err := cc.client(context.Background(), "hpcone"); err == nil {
		t.Fatal("want the connect error")
	}
	want := []string{"Connecting to hpcone…", "Running cluster_info…"}
	if got := ui.got(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("steps = %q, want %q", got, want)
	}
}
