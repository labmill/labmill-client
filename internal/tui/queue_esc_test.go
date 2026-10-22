package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestEscRemovesQueuedMessage(t *testing.T) {
	m, s := newQueued(t)
	typeText(t, m, "slow\n")
	s.wait(t)
	typeText(t, m, "one\n")
	typeText(t, m, "two\n")
	if m.queued != "two" || m.notice != "replaced the queued message" {
		t.Fatalf("queued=%q notice=%q", m.queued, m.notice)
	}
	step(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.queued != "" || m.notice != "queued message removed" {
		t.Fatalf("after Esc: queued=%q notice=%q", m.queued, m.notice)
	}
	step(t, m, TurnDoneMsg{})
	select {
	case got := <-s.ch:
		t.Fatalf("a removed message ran: %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}
