package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"labmill-client/internal/app"
)

var _ interface{ Todos([]app.Todo) } = (*Bridge)(nil)

// TodosMsg carries the whole to-do list (update_todos).
type TodosMsg struct{ Todos []app.Todo }

// Todos shows the to-do list above the input (the optional launch.UI hook).
// It replaces the previous list; it never blocks past the Bridge's close.
func (b *Bridge) Todos(todos []app.Todo) { b.send(TodosMsg{Todos: todos}) }

// maxTodoRows bounds the to-do section so the chat keeps its room.
const maxTodoRows = 7

// setTodos replaces the list and relayouts when its height changes.
func (m *model) setTodos(todos []app.Todo) {
	before := m.todoRows()
	m.todos = todos
	m.todoStale = 0
	m.todoUpdated = true
	if m.width > 0 && before != m.todoRows() {
		m.layout()
	}
}

// todoAllDone reports whether the list is non-empty and every item is done.
func (m *model) todoAllDone() bool {
	for _, t := range m.todos {
		if t.Status != app.TodoCompleted {
			return false
		}
	}
	return len(m.todos) > 0
}

// todoTurnDone is called at the end of every turn. The list is hidden when
// the turn was canceled or failed (a plan that stopped must not look like
// work in progress), when it is all done and a later turn ended (it stays
// through the turn that finished it), and after two turns that did not call
// update_todos.
func (m *model) todoTurnDone(err error) {
	if len(m.todos) == 0 {
		return
	}
	if err != nil {
		m.setTodos(nil)
		return
	}
	if m.todoUpdated {
		m.todoUpdated = false
		m.todoStale = 0
		return
	}
	m.todoStale++
	if m.todoStale >= 2 || (m.todoAllDone() && m.todoStale >= 1) {
		m.setTodos(nil)
	}
}

// todoLines renders the section: a dim "To-do 1/3" header, then one line per item (☐ pending, ◐ in progress
// with its active form, ☑ completed), at most maxTodoRows lines in all.
func (m *model) todoLines() []string {
	if len(m.todos) == 0 {
		return nil
	}
	done := 0
	for _, t := range m.todos {
		if t.Status == app.TodoCompleted {
			done++
		}
	}
	items := m.todos
	more := 0
	if len(items) > maxTodoRows-1 {
		// Keep a window of maxTodoRows-2 items around the item in progress,
		// plus the "more" line.
		shown := maxTodoRows - 2
		start := 0
		for i, t := range items {
			if t.Status == app.TodoInProgress {
				start = max(min(i-2, len(items)-shown), 0)
				break
			}
		}
		more = len(items) - shown
		items = items[start : start+shown]
	}
	w := m.chatWidth()
	out := []string{dimStyle.MaxWidth(w).Render(fmt.Sprintf("To-do %d/%d", done, len(m.todos)))}
	for _, t := range items {
		var s string
		switch t.Status {
		case app.TodoCompleted:
			s = dimStyle.MaxWidth(w).Render("☑ " + oneLine(t.Content))
		case app.TodoInProgress:
			txt := t.ActiveForm
			if txt == "" {
				txt = t.Content
			}
			s = lipgloss.NewStyle().Bold(true).MaxWidth(w).Render("◐ " + oneLine(txt))
		default:
			s = lipgloss.NewStyle().MaxWidth(w).Render("☐ " + oneLine(t.Content))
		}
		out = append(out, s)
	}
	if more > 0 {
		out = append(out, dimStyle.MaxWidth(w).Render(fmt.Sprintf("… and %d more", more)))
	}
	return out
}

// todoRows is the height of the to-do section.
func (m *model) todoRows() int {
	if len(m.todos) == 0 {
		return 0
	}
	return min(len(m.todos)+1, maxTodoRows)
}

// todoView is the section as one string ("" when empty).
func (m *model) todoView() string {
	return strings.Join(m.todoLines(), "\n")
}
