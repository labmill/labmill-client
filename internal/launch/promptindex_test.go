package launch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/knowledge"
)

func TestPromptTrimmedNeedsAllConditions(t *testing.T) {
	st := fixtureSite(t)
	ver := strings.Repeat("a", 64)
	ok := knowledge.Manifest{Site: st.Domain, Version: ver, Index: true}
	base := skillBase{Version: ver}
	tests := []struct {
		name string
		m    knowledge.Manifest
		have bool
		base skillBase
		want bool
	}{
		{"all hold", ok, true, base, true},
		{"no manifest yet", ok, false, base, false},
		{"old broker without index", knowledge.Manifest{Site: st.Domain, Version: ver}, true, base, false},
		{"versions differ", ok, true, skillBase{Version: strings.Repeat("b", 64)}, false},
		{"client version unknown", ok, true, skillBase{}, false},
		{"override folder in use", ok, true, skillBase{Version: ver, Overridden: true}, false},
		{"manifest for another site", knowledge.Manifest{Site: "other.edu", Version: ver, Index: true}, true, base, false},
	}
	for _, tc := range tests {
		if got := promptTrimmed(tc.m, tc.have, st, tc.base); got != tc.want {
			t.Errorf("%s: promptTrimmed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// sysOf returns the system field of a request body, for byte-stability.
func sysOf(t *testing.T, body string) string {
	t.Helper()
	var r struct {
		System json.RawMessage `json:"system"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	return string(r.System)
}

func TestChoosePromptWithoutBrokerIsFull(t *testing.T) {
	st := fixtureSite(t)
	env := &app.Env{Config: &config.Config{}}
	ag := &agent.Agent{}
	choosePrompt(ag, env, nil, st, skillBase{Version: "v"})
	if ag.System != app.SystemPrompt(env) {
		t.Error("no broker must give the full prompt")
	}
}

func TestWithPromptChoiceRunsPrevFirstAndSetsSystem(t *testing.T) {
	st := fixtureSite(t)
	env := &app.Env{Config: &config.Config{}}
	ag := &agent.Agent{}
	var order []string
	prev := func(context.Context) {
		if ag.System != "" {
			t.Error("the prompt was chosen before prev ran")
		}
		order = append(order, "prev")
	}
	withPromptChoice(prev, ag, env, nil, st, skillBase{})(context.Background())
	if len(order) != 1 || ag.System != app.SystemPrompt(env) {
		t.Errorf("order %v, System full = %v", order, ag.System == app.SystemPrompt(env))
	}
	ag2 := &agent.Agent{}
	withPromptChoice(nil, ag2, env, nil, st, skillBase{})(context.Background())
	if ag2.System == "" {
		t.Error("nil prev: System not set")
	}
}
