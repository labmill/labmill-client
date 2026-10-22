package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"labmill-client/internal/agent"
	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

// recSession records the texts it was sent; turns end only when the test
// sends TurnDoneMsg.
type recSession struct {
	mu   sync.Mutex
	got  []string
	ctxs []context.Context
	ch   chan string
}

func (s *recSession) Send(ctx context.Context, text string) {
	s.mu.Lock()
	s.got = append(s.got, text)
	s.ctxs = append(s.ctxs, ctx)
	s.mu.Unlock()
	s.ch <- text
}

func (s *recSession) wait(t *testing.T) string {
	t.Helper()
	select {
	case v := <-s.ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("no turn started")
	}
	return ""
}

func newQueued(t *testing.T) (*model, *recSession) {
	t.Helper()
	s := &recSession{ch: make(chan string, 8)}
	m := newModel(Options{Status: "hpcone (Slurm)", Session: s})
	m.now = advancingClock()
	step(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	return m, s
}

func TestTypingWhileBusyQueuesOneMessage(t *testing.T) {
	m, s := newQueued(t)
	typeText(t, m, "first\n")
	if got := s.wait(t); got != "first" || !m.running {
		t.Fatalf("turn 1 = %q running=%v", got, m.running)
	}
	// The input stays usable: typing and Enter queue the message.
	typeText(t, m, "second\n")
	if m.queued != "second" || m.input.Value() != "" {
		t.Fatalf("queued = %q, input = %q", m.queued, m.input.Value())
	}
	typeText(t, m, "third\n") // replaces it
	if m.queued != "third" {
		t.Fatalf("queued = %q", m.queued)
	}
	if !strings.Contains(content(m), "queued: third") {
		t.Errorf("the queued line isn't shown:\n%s", content(m))
	}
	select {
	case extra := <-s.ch:
		t.Fatalf("a queued message started early: %q", extra)
	default:
	}
	step(t, m, TurnDoneMsg{})
	if got := s.wait(t); got != "third" {
		t.Fatalf("turn 2 = %q", got)
	}
	if m.queued != "" || !m.running || strings.Contains(content(m), "queued:") {
		t.Errorf("after the queued turn started: queued=%q running=%v", m.queued, m.running)
	}
	step(t, m, TurnDoneMsg{})
	if m.running {
		t.Error("still running with nothing queued")
	}
}

func TestCtrlCKeepsQueuedMessage(t *testing.T) {
	m, s := newQueued(t)
	typeText(t, m, "slow\n")
	s.wait(t)
	typeText(t, m, "next\n")
	step(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	s.mu.Lock()
	ctx := s.ctxs[0]
	s.mu.Unlock()
	if ctx.Err() == nil {
		t.Fatal("Ctrl+C did not cancel the turn")
	}
	step(t, m, TurnDoneMsg{Err: context.Canceled})
	if got := s.wait(t); got != "next" {
		t.Fatalf("after cancel = %q", got)
	}
}

func TestJobNoticeInChat(t *testing.T) {
	var calls int
	var mu sync.Mutex
	m := newModel(Options{Status: "s", Jobs: func(context.Context) ([]store.Job, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil, nil
	}})
	m.now = advancingClock()
	step(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	mu.Lock()
	before := calls
	mu.Unlock()

	// A reply is streaming: the notice goes above it, the reply stays whole.
	step(t, m, AgentEventMsg{Event: agent.Event{Kind: agent.EventText, Text: "Hello "}})
	step(t, m, JobNoticeMsg{Text: "Job #12 (seg) on hpcone finished.\x1b[2J"})
	step(t, m, AgentEventMsg{Event: agent.Event{Kind: agent.EventText, Text: "world"}})
	n := len(m.entries)
	if n < 2 || m.entries[n-2].role != roleNotice || m.entries[n-1].text != "Hello world" {
		t.Fatalf("entries = %+v", m.entries)
	}
	if out := content(m); !strings.Contains(out, "Job #12 (seg) on hpcone finished.") || strings.Contains(out, "\x1b[2J") {
		t.Errorf("notice not shown cleanly:\n%q", out)
	}
	mu.Lock()
	after := calls
	mu.Unlock()
	if after <= before {
		t.Error("the jobs panel was not refreshed")
	}
}

func TestBridgeJobNoticeNeverBlocks(t *testing.T) {
	b := NewBridge()
	for i := 0; i < 300; i++ { // more than the buffer
		b.JobNotice("x")
	}
	b.Close()
	b.JobNotice("after close")
}

func TestTransferInStatusLine(t *testing.T) {
	m := newModel(Options{Status: "hpcone (Slurm)"})
	m.now = advancingClock()
	step(t, m, tea.WindowSizeMsg{Width: 160, Height: 30})
	const line = "Transferred: 1.0 MiB / 4.0 MiB, 25%, 45.0 MiB/s, ETA 1m02s · Files: 1 / 4 · now: a.csv"
	step(t, m, TransferMsg{Update: transfer.Update{Kind: "progress", Text: line + "\x1b[2J"}})
	if bar := m.statusBar(); !strings.Contains(bar, line) || strings.Contains(bar, "\x1b[2J") {
		t.Errorf("status bar = %q", bar)
	}
	step(t, m, TransferMsg{Update: transfer.Update{Kind: "end"}})
	if bar := m.statusBar(); strings.Contains(bar, "Transferred") || !strings.Contains(bar, "hpcone") {
		t.Errorf("status bar after the end = %q", bar)
	}
}

// The status bar names the turn's current step next to the spinner: the
// agent's steps, "Running <tool>…" for a tool call, "Working…" while the
// reply streams; the elapsed seconds show after 5 s; a notice stays visible.
func TestStatusShowsSteps(t *testing.T) {
	m, s := newQueued(t)
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	typeText(t, m, "go\n")
	s.wait(t)
	bar := func() string { return stripStyles(m.statusBar()) }
	if !strings.Contains(bar(), "Working…") {
		t.Errorf("before any step: %q", bar())
	}
	step(t, m, AgentEventMsg{Event: agent.Event{Kind: agent.EventStep, Text: "Reading the sign-in from login…"}})
	if !strings.Contains(bar(), "Reading the sign-in from login…") {
		t.Errorf("step: %q", bar())
	}
	step(t, m, AgentEventMsg{Event: agent.Event{Kind: agent.EventToolCall, Tool: "cluster_info"}})
	if !strings.Contains(bar(), "Running cluster_info…") || strings.Contains(bar(), " s ") {
		t.Errorf("tool call: %q", bar())
	}
	now = now.Add(7 * time.Second)
	if !strings.Contains(bar(), "Running cluster_info… 7 s") {
		t.Errorf("elapsed: %q", bar())
	}
	step(t, m, NotifyMsg{Text: "approve the Duo push"})
	if !strings.Contains(bar(), "Running cluster_info… 7 s · approve the Duo") || !strings.Contains(bar(), "Ctrl+C cancel") {
		t.Errorf("notice while running: %q", bar())
	}
	step(t, m, AgentEventMsg{Event: agent.Event{Kind: agent.EventText, Text: "Hi"}})
	if strings.Contains(bar(), "cluster_info") || !strings.Contains(bar(), "Working…") {
		t.Errorf("streaming: %q", bar())
	}
	step(t, m, TurnDoneMsg{})
	if strings.Contains(bar(), "Working…") || m.running {
		t.Errorf("after the turn: %q", bar())
	}
}
