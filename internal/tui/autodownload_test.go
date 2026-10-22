package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// M60.1: /autodownload on|off saves the setting and reports it; no argument
// just reports; bad arguments give a hint; none of it is sent to the model.
func TestAutoDownloadCommand(t *testing.T) {
	on := false
	m := newModel(Options{
		Session:         &countSession{},
		AutoDownload:    func() bool { return on },
		SetAutoDownload: func(v bool) error { on = v; return nil },
	})
	m.now = advancingClock()
	step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	enter(t, m, "/autodownload on")
	if !on || m.running {
		t.Fatalf("on=%v running=%v", on, m.running)
	}
	if e := lastEntry(m); e.role != roleNotice || !strings.Contains(e.text, "Download results automatically is on") {
		t.Errorf("entry = %+v", e)
	}
	enter(t, m, "/autodownload")
	if !on || !strings.Contains(lastEntry(m).text, "is on") {
		t.Errorf("report: on=%v %+v", on, lastEntry(m))
	}
	enter(t, m, "/autodownload off")
	if on || !strings.Contains(lastEntry(m).text, "is off") {
		t.Errorf("off: on=%v %+v", on, lastEntry(m))
	}
	enter(t, m, "/autodownload maybe")
	if m.running || lastEntry(m).role != roleError || !strings.Contains(lastEntry(m).text, "/autodownload on or /autodownload off") {
		t.Errorf("maybe: running=%v %+v", m.running, lastEntry(m))
	}
}

func TestAutoDownloadCommandNotAvailable(t *testing.T) {
	m := newModel(Options{Session: &countSession{}})
	m.now = advancingClock()
	step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	enter(t, m, "/autodownload on")
	if m.running || lastEntry(m).role != roleError {
		t.Errorf("running=%v %+v", m.running, lastEntry(m))
	}
}
