package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"labmill-client/internal/agent"
)

func opts(labels ...string) []map[string]any {
	var out []map[string]any
	for _, l := range labels {
		out = append(out, map[string]any{"label": l, "description": "about " + l})
	}
	return out
}

func TestAskUserResults(t *testing.T) {
	cases := []struct {
		name   string
		answer agent.ChoiceAnswer
		want   string
	}{
		{"first option", agent.ChoiceAnswer{Index: 0}, "The user chose: Use a module"},
		{"second option", agent.ChoiceAnswer{Index: 1}, "The user chose: Make my own env"},
		{"typed", agent.ChoiceAnswer{Index: -1, Other: "  use\nconda \x1b[31m please "}, "The user answered: use conda please"},
		{"skipped", agent.ChoiceAnswer{Index: 1, Skipped: true}, "The user skipped the question."},
		{"typed blank", agent.ChoiceAnswer{Index: -1, Other: " \x1b[0m "}, "The user skipped the question."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var got agent.Choice
			h.env.Choose = func(_ context.Context, q agent.Choice) (agent.ChoiceAnswer, error) {
				got = q
				return tc.answer, nil
			}
			out, err := h.run(t, "ask_user", map[string]any{
				"question": "How should I set up Python?",
				"options":  opts("Use a module", "Make my own env"),
			})
			if err != nil {
				t.Fatal(err)
			}
			if out != tc.want {
				t.Errorf("result = %q, want %q", out, tc.want)
			}
			if !got.AllowOther || len(got.Options) != 2 || got.Options[1].Description != "about Make my own env" {
				t.Errorf("choice = %+v", got)
			}
		})
	}
}

func TestAskUserTypedCapped(t *testing.T) {
	h := newHarness(t)
	h.env.Choose = func(context.Context, agent.Choice) (agent.ChoiceAnswer, error) {
		return agent.ChoiceAnswer{Index: -1, Other: strings.Repeat("x", 900)}, nil
	}
	out, err := h.run(t, "ask_user", map[string]any{"question": "q?", "options": opts("a", "b")})
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(strings.TrimPrefix(out, "The user answered: "))); n != maxOtherRunes {
		t.Errorf("typed answer has %d runes, want %d", n, maxOtherRunes)
	}
}

func TestAskUserValidation(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	cases := []struct {
		name  string
		input map[string]any
		want  string // substring of the error; "" means valid
	}{
		{"ok", map[string]any{"question": "q?", "options": opts("a", "b", "c", "d")}, ""},
		{"empty question", map[string]any{"question": "  ", "options": opts("a", "b")}, "empty"},
		{"long question", map[string]any{"question": long(301), "options": opts("a", "b")}, "too long"},
		{"max question", map[string]any{"question": long(300), "options": opts("a", "b")}, ""},
		{"one option", map[string]any{"question": "q?", "options": opts("a")}, "2 to 8"},
		{"five options", map[string]any{"question": "q?", "options": opts("a", "b", "c", "d", "e")}, ""},
		{"eight options", map[string]any{"question": "q?", "options": opts("a", "b", "c", "d", "e", "f", "g", "h")}, ""},
		{"nine options", map[string]any{"question": "q?", "options": opts("a", "b", "c", "d", "e", "f", "g", "h", "i")}, "2 to 8"},
		{"no label", map[string]any{"question": "q?", "options": []map[string]any{{"label": " "}, {"label": "b"}}}, "no label"},
		{"long label", map[string]any{"question": "q?", "options": opts(long(61), "b")}, "too long"},
		{"max label", map[string]any{"question": "q?", "options": opts(long(60), "b")}, ""},
		{"long description", map[string]any{"question": "q?", "options": []map[string]any{{"label": "a", "description": long(301)}, {"label": "b"}}}, "too long"},
		{"duplicate labels", map[string]any{"question": "q?", "options": opts("GPU", "gpu")}, "must differ"},
		{"bad json", map[string]any{"question": 3, "options": opts("a", "b")}, "understand"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			called := false
			h.env.Choose = func(context.Context, agent.Choice) (agent.ChoiceAnswer, error) {
				called = true
				return agent.ChoiceAnswer{Index: 0}, nil
			}
			_, err := h.run(t, "ask_user", tc.input)
			if tc.want == "" {
				if err != nil || !called {
					t.Fatalf("err = %v, called = %v", err, called)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if called {
				t.Error("the dialog was shown for invalid input")
			}
		})
	}
}

func TestAskUserAllowOther(t *testing.T) {
	h := newHarness(t)
	var got agent.Choice
	h.env.Choose = func(_ context.Context, q agent.Choice) (agent.ChoiceAnswer, error) {
		got = q
		return agent.ChoiceAnswer{Index: -1, Other: "something else"}, nil
	}
	_, err := h.run(t, "ask_user", map[string]any{"question": "q?", "options": opts("a", "b"), "allow_other": false})
	if got.AllowOther {
		t.Error("allow_other false was not passed on")
	}
	if err == nil {
		t.Error("a typed answer was accepted although allow_other is false")
	}
}

func TestAskUserErrors(t *testing.T) {
	h := newHarness(t)
	in := map[string]any{"question": "q?", "options": opts("a", "b")}
	if _, err := h.run(t, "ask_user", in); err == nil || !strings.Contains(err.Error(), "in your reply") {
		t.Errorf("no Choose: err = %v", err)
	}
	h.env.Choose = func(context.Context, agent.Choice) (agent.ChoiceAnswer, error) {
		return agent.ChoiceAnswer{Index: 2}, nil
	}
	if _, err := h.run(t, "ask_user", in); err == nil {
		t.Error("out-of-range index accepted")
	}
	h.env.Choose = func(context.Context, agent.Choice) (agent.ChoiceAnswer, error) {
		return agent.ChoiceAnswer{Skipped: true}, errors.New("Labmill's window was closed")
	}
	if _, err := h.run(t, "ask_user", in); err == nil {
		t.Error("a Choose error was swallowed")
	}
}

// Eight options reach the dialog intact and in order, and the eighth can be
// picked.
func TestAskUserEightOptions(t *testing.T) {
	h := newHarness(t)
	labels := []string{"one", "two", "three", "four", "five", "six", "seven", "eight"}
	var got agent.Choice
	h.env.Choose = func(_ context.Context, q agent.Choice) (agent.ChoiceAnswer, error) {
		got = q
		return agent.ChoiceAnswer{Index: 7}, nil
	}
	out, err := h.run(t, "ask_user", map[string]any{"question": "Which image?", "options": opts(labels...)})
	if err != nil {
		t.Fatal(err)
	}
	if out != "The user chose: eight" {
		t.Errorf("result = %q", out)
	}
	if len(got.Options) != 8 {
		t.Fatalf("dialog got %d options", len(got.Options))
	}
	for i, l := range labels {
		if got.Options[i].Label != l {
			t.Errorf("option %d = %q, want %q", i+1, got.Options[i].Label, l)
		}
	}
	tool := h.tools["ask_user"]
	opt := tool.Schema.Properties.(map[string]any)["options"].(map[string]any)
	if opt["maxItems"] != 8 {
		t.Errorf("schema maxItems = %v, want 8", opt["maxItems"])
	}
}
