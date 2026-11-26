package app

import (
	"strings"
	"testing"
)

// The prompt tells the model to chat in the user's language but to leave
// commands, paths and quoted errors untouched.
func TestPromptUserLanguage(t *testing.T) {
	h := newHarness(t)
	p := SystemPromptFor(h.env, false)
	for _, w := range []string{"Answer in the language the user writes in", "switch when they switch", "job scripts", "quoted error text exactly as they are", "ask_user questions and options", "Pass existing paths and names to tools unchanged"} {
		if !strings.Contains(p, w) {
			t.Errorf("prompt lacks %q", w)
		}
	}
}
