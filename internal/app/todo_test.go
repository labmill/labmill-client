package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"labmill-client/internal/agent"
)

func runTodos(t *testing.T, env *Env, js string) (string, error) {
	t.Helper()
	for _, tl := range Tools(env) {
		if tl.Name == "update_todos" {
			return tl.Run(context.Background(), json.RawMessage(js), agent.Decision{})
		}
	}
	t.Fatal("update_todos not registered")
	return "", nil
}

func TestUpdateTodos(t *testing.T) {
	var got []Todo
	env := &Env{Todos: func(l []Todo) { got = l }}
	out, err := runTodos(t, env, `{"todos":[{"content":"Scan\u0007 the\ncode","status":"completed"},{"content":"Upload","status":"in_progress","active_form":"Uploading"},{"content":"Submit","status":"pending"}]}`)
	if err != nil || !strings.Contains(out, "1 of 3") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(got) != 3 || got[0].Content != "Scan the code" || got[1].ActiveForm != "Uploading" || got[1].Status != TodoInProgress {
		t.Fatalf("got %+v", got)
	}
	// An empty list clears.
	got = []Todo{{}}
	if out, err := runTodos(t, env, `{"todos":[]}`); err != nil || len(got) != 0 || !strings.Contains(out, "cleared") {
		t.Fatalf("clear: out=%q err=%v got=%v", out, err, got)
	}
	// No callback: still fine.
	if _, err := runTodos(t, &Env{}, `{"todos":[{"content":"a","status":"pending"}]}`); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateTodosInvalid(t *testing.T) {
	long := strings.Repeat("x", 201)
	var many []string
	for i := 0; i < 21; i++ {
		many = append(many, `{"content":"a","status":"pending"}`)
	}
	cases := map[string]string{
		"too many":   `{"todos":[` + strings.Join(many, ",") + `]}`,
		"no content": `{"todos":[{"content":"  ","status":"pending"}]}`,
		"long":       `{"todos":[{"content":"` + long + `","status":"pending"}]}`,
		"status":     `{"todos":[{"content":"a","status":"done"}]}`,
		"two active": `{"todos":[{"content":"a","status":"in_progress"},{"content":"b","status":"in_progress"}]}`,
		"junk":       `{"todos":"x"}`,
	}
	called := false
	env := &Env{Todos: func([]Todo) { called = true }}
	for name, js := range cases {
		if _, err := runTodos(t, env, js); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if called {
		t.Error("Todos called for invalid input")
	}
}
