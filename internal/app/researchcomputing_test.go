package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/agent/agenttest"
	"labmill-client/internal/skill"
	"labmill-client/skills"
)

// The embedded research-computing skill shows up in the system prompt's skill
// list, load_skill returns its body, and read_reference serves a reference.
func TestResearchComputingSkillLoads(t *testing.T) {
	set, err := skill.Load(skills.FS, "embedded")
	if err != nil {
		t.Fatal(err)
	}
	if sp := set.SystemPrompt(); !strings.Contains(sp, "- research-computing:") {
		t.Errorf("system prompt skill list lacks research-computing:\n%s", sp)
	}
	tools := map[string]agent.Tool{}
	for _, tl := range agent.SkillTools(set) {
		tools[tl.Name] = tl
	}
	out, err := tools["load_skill"].Run(context.Background(), json.RawMessage(`{"name":"research-computing"}`), agent.Decision{})
	if err != nil || !strings.Contains(out, "filesystems-and-storage.md") {
		t.Fatalf("load_skill research-computing = %q, %v", out, err)
	}
	ref, err := tools["read_reference"].Run(context.Background(), json.RawMessage(`{"skill":"research-computing","file":"references/filesystems-and-storage.md"}`), agent.Decision{})
	if err != nil || !strings.Contains(ref, "File systems and storage") {
		t.Errorf("read_reference = %.60q, %v", ref, err)
	}

	// One scripted-model turn: the model calls load_skill and the tool result
	// carries the skill body.
	b := agenttest.New(t,
		agenttest.ToolReply(agenttest.ToolUse{ID: "t1", Name: "load_skill", Input: `{"name":"research-computing"}`}),
		agenttest.TextReply("Loaded."))
	reg := agent.NewRegistry()
	for _, tl := range tools {
		if err := reg.Register(tl); err != nil {
			t.Fatal(err)
		}
	}
	a := &agent.Agent{Model: b.Client(t), Tools: reg}
	h, err := a.Send(context.Background(), nil, "set up my python environment", agent.Typed)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(h)
	if !strings.Contains(string(raw), "filesystems-and-storage.md") {
		t.Errorf("history lacks the research-computing body: %.300s", raw)
	}
	if !strings.Contains(skillToolMap, "research-computing steps") {
		t.Error("skillToolMap lacks the research-computing mapping")
	}
}
