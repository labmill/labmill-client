package tui

import (
	"testing"

	"labmill-client/internal/agent"
)

// A conversation note from the agent (e.g. after compaction) is a notice line.
func TestAgentNoteIsNotice(t *testing.T) {
	m, _, _ := newFolderModel(t, "/home/ann")
	step(t, m, AgentEventMsg{Event: agent.Event{Kind: agent.EventNote, Text: agent.NoteCompacted}})
	if e := lastEntry(m); e.role != roleNotice || e.text != agent.NoteCompacted {
		t.Errorf("note entry = %+v", e)
	}
}
