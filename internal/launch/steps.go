package launch

import (
	"sync"

	"labmill-client/internal/agent"
)

// Startup steps reported by BuildWithProgress, in order (the desktop app
// shows them under its hourglass while it starts).
const (
	StartLoadingSettings = "Loading your settings…"
	StartOpeningDatabase = "Opening the job database…"
	StartLoadingSkills   = "Loading skills…"
	StartReady           = "Ready"
)

// Progress receives startup steps (one of the Start* texts).
type Progress func(step string)

// stepTracker emits agent.EventStep events for the slow parts of a turn
// that happen below the agent (an SSH connect)
// and puts the previous step back when such a part ends, so the busy
// indicator always names what the turn is waiting for. It learns the
// agent's own steps by observing its events (observe). A nil tracker does
// nothing.
type stepTracker struct {
	ui UI

	mu  sync.Mutex
	cur string // the step on display ("" once the reply text started)
}

func newStepTracker(ui UI) *stepTracker { return &stepTracker{ui: ui} }

// observe follows the agent's events: its steps and tool calls set the
// current step, reply text clears it (the indicator is gone then).
func (s *stepTracker) observe(e agent.Event) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Kind {
	case agent.EventStep:
		s.cur = e.Text
	case agent.EventToolCall:
		s.cur = agent.StepRunning(e.Tool)
	case agent.EventText:
		s.cur = ""
	}
}

// begin shows text as the current step and returns the function that ends
// it: that puts the step shown before back, unless another step replaced
// text meanwhile or nothing was shown before.
func (s *stepTracker) begin(text string) (end func()) {
	if s == nil || s.ui == nil {
		return func() {}
	}
	s.mu.Lock()
	prev := s.cur
	s.cur = text
	s.mu.Unlock()
	s.ui.Event(agent.Event{Kind: agent.EventStep, Text: text})
	return func() {
		s.mu.Lock()
		restore := s.cur == text && prev != "" && prev != text
		if restore {
			s.cur = prev
		}
		s.mu.Unlock()
		if restore {
			s.ui.Event(agent.Event{Kind: agent.EventStep, Text: prev})
		}
	}
}
