package launch

import (
	"context"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"labmill-client/internal/agent"
)

// Session runs chat turns with an agent.Agent, keeping the
// conversation history across turns.
type Session struct {
	agent *agent.Agent
	ui    UI
	// onTurn, when set, runs at the start of every turn.
	onTurn func()
	// firstTurn, when set, runs once, at the start of the first turn (before
	// the agent), where dialogs can be shown: one-time offers.
	firstTurn func(ctx context.Context)

	mu      sync.Mutex
	history []anthropic.BetaMessageParam
}

// Send runs one turn (agent.Send with the current history, Origin Typed),
// stores the returned history (also on error, so the conversation can
// continue), and always finishes with ui.Done(err). Turns are
// serialized.
func (s *Session) Send(ctx context.Context, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.onTurn != nil {
		s.onTurn()
	}
	if f := s.firstTurn; f != nil {
		s.firstTurn = nil
		f(ctx)
	}
	h, err := s.agent.Send(ctx, s.history, text, agent.Typed)
	s.history = h
	s.ui.Done(err)
}

// Wait blocks until no turn is running, or timeout passes; it reports
// whether the session went idle. A quit during a submit uses it so the job
// gets recorded before the store is closed.
func (s *Session) Wait(timeout time.Duration) bool {
	idle := make(chan struct{})
	go func() {
		s.mu.Lock()
		s.mu.Unlock() //nolint:staticcheck // waiting for the running turn only
		close(idle)
	}()
	select {
	case <-idle:
		return true
	case <-time.After(timeout):
		return false
	}
}
