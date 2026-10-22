package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"labmill-client/internal/agent"
	"labmill-client/internal/textsafe"
)

// Limits of the ask_user tool's input and of a typed answer.
const (
	maxQuestionRunes    = 300
	maxLabelRunes       = 60
	maxDescriptionRunes = 300
	minChoiceOptions    = 2
	maxChoiceOptions    = 8
	maxOtherRunes       = 500
)

// singleLine cleans untrusted text for display and collapses all whitespace
// (newlines included) to single spaces.
func singleLine(s string) string {
	return strings.Join(strings.Fields(textsafe.Clean(s)), " ")
}

// runeCount is the length of s in runes.
func runeCount(s string) int { return len([]rune(s)) }

// parseChoice validates ask_user's input and turns it into an agent.Choice:
// a question of 1 to 300 runes; 2 to 8 options, each with a label of 1 to 60
// runes (unique, case-insensitive) and an optional description of at most
// 300 runes; allow_other defaulting to true. Text is cleaned with
// textsafe.Clean; labels and descriptions become single lines. Errors are
// sentences Claude can act on.
func parseChoice(input json.RawMessage) (agent.Choice, error) {
	var in struct {
		Question string `json:"question"`
		Options  []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
		AllowOther *bool `json:"allow_other"`
	}
	if err := decode(input, &in); err != nil {
		return agent.Choice{}, err
	}
	q := agent.Choice{Question: strings.TrimSpace(textsafe.Clean(in.Question)), AllowOther: true}
	if in.AllowOther != nil {
		q.AllowOther = *in.AllowOther
	}
	switch n := runeCount(q.Question); {
	case n == 0:
		return agent.Choice{}, errors.New("the question is empty")
	case n > maxQuestionRunes:
		return agent.Choice{}, fmt.Errorf("the question is too long (%d characters, at most %d)", n, maxQuestionRunes)
	}
	if n := len(in.Options); n < minChoiceOptions || n > maxChoiceOptions {
		return agent.Choice{}, fmt.Errorf("give %d to %d options (got %d); for a yes/no question or an open question, just ask in your reply", minChoiceOptions, maxChoiceOptions, n)
	}
	seen := map[string]bool{}
	for i, o := range in.Options {
		label, desc := singleLine(o.Label), singleLine(o.Description)
		switch n := runeCount(label); {
		case n == 0:
			return agent.Choice{}, fmt.Errorf("option %d has no label", i+1)
		case n > maxLabelRunes:
			return agent.Choice{}, fmt.Errorf("the label of option %d is too long (%d characters, at most %d); put details in its description", i+1, n, maxLabelRunes)
		}
		if n := runeCount(desc); n > maxDescriptionRunes {
			return agent.Choice{}, fmt.Errorf("the description of option %d is too long (%d characters, at most %d)", i+1, n, maxDescriptionRunes)
		}
		key := strings.ToLower(label)
		if seen[key] {
			return agent.Choice{}, fmt.Errorf("two options have the label %q; labels must differ", label)
		}
		seen[key] = true
		q.Options = append(q.Options, agent.ChoiceOption{Label: label, Description: desc})
	}
	return q, nil
}

// choiceResult turns the user's answer into the tool result for Claude.
func choiceResult(q agent.Choice, a agent.ChoiceAnswer) (string, error) {
	switch {
	case a.Skipped:
		return "The user skipped the question.", nil
	case a.Other != "":
		other := singleLine(a.Other)
		if r := []rune(other); len(r) > maxOtherRunes {
			other = strings.TrimSpace(string(r[:maxOtherRunes]))
		}
		if !q.AllowOther {
			return "", errors.New("the dialog returned a typed answer although typing one was not allowed")
		}
		if other == "" {
			return "The user skipped the question.", nil
		}
		return "The user answered: " + other, nil
	case a.Index < 0 || a.Index >= len(q.Options):
		return "", fmt.Errorf("the dialog returned option %d, which doesn't exist", a.Index+1)
	}
	return "The user chose: " + q.Options[a.Index].Label, nil
}

// askUser is the ask_user tool: a question with 2 to 8 answers that the user
// picks in a selection dialog (Env.Choose). It changes nothing, so it needs
// no approval, and it is never a substitute for one.
func (t *toolset) askUser() agent.Tool {
	return agent.Tool{
		Name: "ask_user",
		Description: "Ask the user to pick one of 2 to 8 options in a selection dialog (arrow keys, number keys or a click), " +
			"instead of writing a numbered list and asking them to type a number. Use it whenever you offer the user a choice; put the recommended option first. " +
			"Never use it to ask permission for an upload, job submission, cancellation or download: those tools ask for approval themselves. " +
			"The result says which option the user chose, what they typed instead (when allow_other is true), or that they skipped the question.",
		Schema: objSchema(map[string]any{
			"question": strProp("the question, short and in plain language (at most 300 characters)"),
			"options": map[string]any{
				"type":        "array",
				"description": "2 to 8 answers to choose from, the recommended one first",
				"minItems":    minChoiceOptions,
				"maxItems":    maxChoiceOptions,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"label":       strProp("a short label (at most 60 characters), unique among the options"),
						"description": strProp("optional one-line explanation (at most 300 characters)"),
					},
					"required": []string{"label"},
				},
			},
			"allow_other": map[string]any{"type": "boolean", "description": "let the user type their own answer instead (default true)"},
		}, "question", "options"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			q, err := parseChoice(input)
			if err != nil {
				return "", err
			}
			if t.env.Choose == nil {
				return "", errors.New("this Labmill window can't show a selection dialog; ask the question in your reply instead")
			}
			a, err := t.env.Choose(ctx, q)
			if err != nil {
				return "", err
			}
			return choiceResult(q, a)
		},
	}
}
