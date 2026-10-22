package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"labmill-client/internal/agent"
)

// M43.1: an empty NotifyMsg (the SSH layer's "the sign-in wait is over")
// clears the status-line notice.
func TestEmptyNotifyClearsTheNotice(t *testing.T) {
	m := newDirect(t, 100, 30)
	step(t, m, NotifyMsg{Text: "Approve the sign-in request for shell on your phone…"})
	if !strings.Contains(m.notice, "Approve the sign-in request") {
		t.Fatalf("notice not set: %q", m.notice)
	}
	step(t, m, NotifyMsg{Text: ""})
	if m.notice != "" || strings.Contains(stripStyles(m.statusBar()), "Approve the") {
		t.Errorf("notice left: %q / %q", m.notice, stripStyles(m.statusBar()))
	}
}

func autoModel(t *testing.T, on *bool, saveErr error) *model {
	t.Helper()
	m := newModel(Options{
		Session:     &countSession{},
		AutoApprove: func() bool { return *on },
		SetAutoApprove: func(v bool) error {
			if saveErr == nil {
				*on = v
			}
			return saveErr
		},
	})
	m.now = advancingClock()
	step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

// M43.2: /autoapprove on|off saves the setting, reports it and shows it in
// the status line; no argument just reports; other text is a chat message.
func TestAutoApproveCommand(t *testing.T) {
	on := false
	m := autoModel(t, &on, nil)
	if strings.Contains(stripStyles(m.statusBar()), "AUTO-APPROVE") {
		t.Error("status line shows it while off")
	}
	enter(t, m, "/autoapprove on")
	if !on || m.running {
		t.Fatalf("on=%v running=%v", on, m.running)
	}
	if e := lastEntry(m); e.role != roleNotice || !strings.Contains(e.text, AutoApproveBanner) {
		t.Errorf("entry = %+v", e)
	}
	if !strings.Contains(stripStyles(m.statusBar()), "AUTO-APPROVE ON") {
		t.Errorf("status line: %q", stripStyles(m.statusBar()))
	}
	enter(t, m, "/autoapprove")
	if !on || !strings.Contains(lastEntry(m).text, "is on") {
		t.Errorf("report: on=%v %+v", on, lastEntry(m))
	}
	enter(t, m, "/autoapprove off")
	if on || !strings.Contains(lastEntry(m).text, "is off") || strings.Contains(stripStyles(m.statusBar()), "AUTO-APPROVE") {
		t.Errorf("off: on=%v %+v", on, lastEntry(m))
	}
	// An unknown argument is a usage hint and never goes to Claude.
	enter(t, m, "/autoapprove maybe")
	if m.running || lastEntry(m).role != roleError || !strings.Contains(lastEntry(m).text, "on or /autoapprove off") {
		t.Errorf("/autoapprove maybe: running=%v %+v", m.running, lastEntry(m))
	}
	enter(t, m, "/autoapprove on now")
	if m.running || on {
		t.Errorf("/autoapprove on now: running=%v on=%v", m.running, on)
	}
}

func TestAutoApproveCommandErrorsAndUnavailable(t *testing.T) {
	on := false
	m := autoModel(t, &on, errors.New("disk full\nbad"))
	enter(t, m, "/autoapprove on")
	if e := lastEntry(m); e.role != roleError || !strings.Contains(e.text, "disk full") || on {
		t.Errorf("entry = %+v on=%v", e, on)
	}
	m2 := newDirect(t, 100, 30)
	enter(t, m2, "/autoapprove on")
	if e := lastEntry(m2); e.role != roleError || m2.running {
		t.Errorf("without hooks: %+v running=%v", e, m2.running)
	}
}

// The chat line for an automatic approval.
func TestAutoApprovedEventIsAChatLine(t *testing.T) {
	m := newDirect(t, 100, 30)
	step(t, m, AgentEventMsg{Event: agent.Event{Kind: agent.EventAutoApproved, Tool: "upload_data", Text: "Upload 2 files\nto x"}})
	if e := lastEntry(m); e.role != roleNotice || e.text != "Auto-approved: Upload 2 files to x" {
		t.Errorf("entry = %+v", e)
	}
}
