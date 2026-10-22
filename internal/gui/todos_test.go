package gui

import (
	"strings"
	"testing"

	"labmill-client/internal/app"
)

func TestBridgeTodos(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	b.Todos([]app.Todo{
		{Content: "Scan\x1b[8m the code", Status: app.TodoCompleted},
		{Content: "Upload", Status: app.TodoInProgress, ActiveForm: "Uploading"},
	})
	ev := rec.waitFor(t, EvTodos).(TodosEvent)
	if len(ev.Items) != 2 || ev.Items[1].Status != app.TodoInProgress || ev.Items[1].ActiveForm != "Uploading" {
		t.Fatalf("event = %+v", ev)
	}
	if strings.ContainsRune(ev.Items[0].Content, 0x1b) {
		t.Errorf("escape byte left in %q", ev.Items[0].Content)
	}
	rec2 := newRecorder()
	b = NewBridge(rec2.emit)
	b.Todos(nil)
	if ev := rec2.waitFor(t, EvTodos).(TodosEvent); len(ev.Items) != 0 {
		t.Errorf("empty list = %+v", ev)
	}
}
