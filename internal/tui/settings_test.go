package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestSettingsCommand(t *testing.T) {
	s := &countSession{}
	calls := 0
	m := newModel(Options{Session: s, Settings: func() string { calls++; return "## Settings\n\n**Clusters**\n\n- hpcone (Slurm)\n" }})
	m.now = advancingClock()
	step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	enter(t, m, "/settings")
	if e := lastEntry(m); e.role != roleAssistant || !strings.Contains(e.text, "hpcone (Slurm)") || calls != 1 {
		t.Errorf("/settings: %+v (calls=%d)", e, calls)
	}
	if len(s.texts) != 0 || m.running || m.input.Value() != "" {
		t.Errorf("/settings started a turn: %q running=%v", s.texts, m.running)
	}
	if !strings.Contains(content(m), "hpcone") {
		t.Errorf("the view lacks the settings:\n%s", content(m))
	}
	// Other text starting with /settings goes to Labmill.
	enter(t, m, "/settings please")
	if !m.running {
		t.Error("a non-command was not sent")
	}
}

func TestSettingsCommandUnavailable(t *testing.T) {
	m := newDirect(t, 100, 30)
	enter(t, m, "/settings")
	if e := lastEntry(m); e.role != roleError || m.running {
		t.Errorf("without Settings: %+v running=%v", e, m.running)
	}
}
