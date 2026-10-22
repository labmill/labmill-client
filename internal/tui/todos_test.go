package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"labmill-client/internal/app"
)

func TestTodoSection(t *testing.T) {
	m, _ := newQueued(t)
	h0 := m.vp.Height()
	step(t, m, TodosMsg{Todos: []app.Todo{
		{Content: "Scan the code", Status: app.TodoCompleted},
		{Content: "Upload the data", Status: app.TodoInProgress, ActiveForm: "Uploading the data"},
		{Content: "Submit the job", Status: app.TodoPending},
	}})
	c := content(m)
	for _, want := range []string{"☑ Scan the code", "◐ Uploading the data", "☐ Submit the job", "To-do 1/3"} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
	if m.vp.Height() != h0-4 {
		t.Errorf("chat height %d, want %d", m.vp.Height(), h0-4)
	}
	// Updated in place.
	step(t, m, TodosMsg{Todos: []app.Todo{
		{Content: "Scan the code", Status: app.TodoCompleted},
		{Content: "Upload the data", Status: app.TodoCompleted},
		{Content: "Submit the job", Status: app.TodoInProgress},
	}})
	c = content(m)
	if strings.Contains(c, "Uploading the data") || !strings.Contains(c, "◐ Submit the job") || strings.Count(c, "☑") != 2 {
		t.Errorf("not updated:\n%s", c)
	}
	// All done: still shown at the end of the turn that finished it, gone
	// after the next one.
	step(t, m, TodosMsg{Todos: []app.Todo{{Content: "Scan the code", Status: app.TodoCompleted}, {Content: "Submit the job", Status: app.TodoCompleted}}})
	step(t, m, TurnDoneMsg{})
	if !strings.Contains(content(m), "☑ Submit the job") {
		t.Error("hidden too early")
	}
	step(t, m, TurnDoneMsg{})
	if len(m.todos) != 0 || strings.Contains(content(m), "☑") || m.vp.Height() != h0 {
		t.Errorf("not hidden: todos=%v height=%d", m.todos, m.vp.Height())
	}
	// An empty list hides it at once.
	step(t, m, TodosMsg{Todos: []app.Todo{{Content: "x", Status: app.TodoPending}}})
	step(t, m, TodosMsg{})
	if m.todoRows() != 0 {
		t.Error("empty list still shown")
	}
}

func TestTodoSectionLong(t *testing.T) {
	m, _ := newQueued(t)
	var l []app.Todo
	for i := 1; i <= 15; i++ {
		st := app.TodoPending
		if i == 10 {
			st = app.TodoInProgress
		}
		l = append(l, app.Todo{Content: fmt.Sprintf("Step %d", i), Status: st})
	}
	step(t, m, TodosMsg{Todos: l})
	if m.todoRows() != maxTodoRows || len(m.todoLines()) != maxTodoRows {
		t.Fatalf("rows %d lines %d", m.todoRows(), len(m.todoLines()))
	}
	if c := content(m); !strings.Contains(c, "◐ Step 10") || !strings.Contains(c, "more") {
		t.Errorf("in-progress item or the more line missing:\n%s", c)
	}
}

func oneTodo() TodosMsg {
	return TodosMsg{Todos: []app.Todo{{Content: "Upload", Status: app.TodoInProgress, ActiveForm: "Uploading"}, {Content: "Submit", Status: app.TodoPending}}}
}

func TestTodoHiddenAfterCancelOrError(t *testing.T) {
	for name, err := range map[string]error{"canceled": context.Canceled, "error": errors.New("boom")} {
		m, _ := newQueued(t)
		h0 := m.vp.Height()
		step(t, m, oneTodo())
		step(t, m, TurnDoneMsg{Err: err})
		if len(m.todos) != 0 || strings.Contains(content(m), "◐") || m.vp.Height() != h0 {
			t.Errorf("%s: list still shown: %v", name, m.todos)
		}
	}
}

func TestTodoHiddenWhenStale(t *testing.T) {
	m, _ := newQueued(t)
	step(t, m, oneTodo())
	step(t, m, TurnDoneMsg{}) // the turn that updated it
	step(t, m, TurnDoneMsg{}) // one turn without an update
	if len(m.todos) != 2 {
		t.Fatal("hidden after one turn without update_todos")
	}
	step(t, m, TurnDoneMsg{}) // two
	if len(m.todos) != 0 {
		t.Error("a list not updated for two turns is still shown")
	}
	// An update resets the count.
	step(t, m, oneTodo())
	step(t, m, TurnDoneMsg{})
	step(t, m, TurnDoneMsg{})
	step(t, m, oneTodo())
	step(t, m, TurnDoneMsg{})
	step(t, m, TurnDoneMsg{})
	if len(m.todos) != 2 {
		t.Error("update_todos did not reset the stale count")
	}
}
