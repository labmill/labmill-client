package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"labmill-client/internal/agent"
)

// To-do list statuses and limits of the update_todos tool.
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"

	maxTodos        = 20
	maxTodoRunes    = 200
	maxTodoActiveFm = 200
)

// Todo is one item of the visible to-do list (Claude Code's TodoWrite).
type Todo struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"active_form,omitempty"` // shown while in progress, e.g. "Uploading the data"
}

// parseTodos validates update_todos' input: 0 to 20 items (none clears the list), each with content
// of 1 to 200 runes and a valid status; at most one in_progress. Text is
// cleaned and collapsed to one line. Errors are sentences Claude can act on.
func parseTodos(input json.RawMessage) ([]Todo, error) {
	var in struct {
		Todos []Todo `json:"todos"`
	}
	if err := decode(input, &in); err != nil {
		return nil, err
	}
	if n := len(in.Todos); n > maxTodos {
		return nil, fmt.Errorf("the to-do list has at most %d items, not %d", maxTodos, n)
	}
	out := make([]Todo, 0, len(in.Todos))
	active := 0
	for i, t := range in.Todos {
		c := singleLine(t.Content)
		if c == "" {
			return nil, fmt.Errorf("item %d has no content", i+1)
		}
		if n := runeCount(c); n > maxTodoRunes {
			return nil, fmt.Errorf("item %d is too long (%d characters, at most %d)", i+1, n, maxTodoRunes)
		}
		st := strings.TrimSpace(t.Status)
		switch st {
		case TodoPending, TodoCompleted:
		case TodoInProgress:
			active++
		default:
			return nil, fmt.Errorf("item %d has status %q; use pending, in_progress or completed", i+1, t.Status)
		}
		af := singleLine(t.ActiveForm)
		if runeCount(af) > maxTodoActiveFm {
			return nil, fmt.Errorf("item %d's active_form is too long (at most %d characters)", i+1, maxTodoActiveFm)
		}
		out = append(out, Todo{Content: c, Status: st, ActiveForm: af})
	}
	if active > 1 {
		return nil, fmt.Errorf("%d items are in_progress; keep at most one", active)
	}
	return out, nil
}

// updateTodos is the update_todos tool: it replaces the whole visible to-do
// list and shows it through Env.Todos. It does no I/O and needs no approval.
func (t *toolset) updateTodos() agent.Tool {
	return agent.Tool{
		Name: "update_todos",
		Description: "Show the user a to-do list for a request with 3 or more steps (for example scan code, choose an image, upload data, submit, monitor, fetch results), and keep it current. " +
			"Each call replaces the whole list. Keep exactly one item in_progress while you work, and mark an item completed right away when it is done. " +
			"Call it with an empty list when you abandon the plan, so no stale list stays on screen. Do not use it for a one-step question. It changes nothing else.",
		Schema: objSchema(map[string]any{
			"todos": map[string]any{
				"type":        "array",
				"description": "the full list, up to 20 items, in order; an empty list clears it",
				"maxItems":    maxTodos,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content":     strProp("the step in the imperative, e.g. \"Upload the data\" (at most 200 characters)"),
						"status":      map[string]any{"type": "string", "enum": []string{TodoPending, TodoInProgress, TodoCompleted}},
						"active_form": strProp("optional present-tense form shown while it runs, e.g. \"Uploading the data\""),
					},
					"required": []string{"content", "status"},
				},
			},
		}, "todos"),
		Run: func(_ context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			todos, err := parseTodos(input)
			if err != nil {
				return "", err
			}
			if t.env.Todos != nil {
				t.env.Todos(todos)
			}
			if len(todos) == 0 {
				return "To-do list cleared.", nil
			}
			done := 0
			for _, td := range todos {
				if td.Status == TodoCompleted {
					done++
				}
			}
			return fmt.Sprintf("To-do list updated: %d of %d done.", done, len(todos)), nil
		},
	}
}
