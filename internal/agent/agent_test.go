package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/anthropics/anthropic-sdk-go"

	"labmill-client/internal/agent/agenttest"
	"labmill-client/internal/broker"
	"labmill-client/internal/skill"
)

// scriptedModel is a fake broker (agenttest) that streams canned assistant
// messages (JSON) in order and records the requests.
type scriptedModel struct {
	b *agenttest.Broker
	*broker.Client
}

func newScripted(t *testing.T, replies ...string) *scriptedModel {
	b := agenttest.New(t, replies...)
	return &scriptedModel{b: b, Client: b.Client(t)}
}

func textReply(s string) string {
	b, _ := json.Marshal(s)
	return `{"id":"m","type":"message","role":"assistant","model":"x","stop_reason":"end_turn","content":[{"type":"text","text":` + string(b) + `}],"usage":{"input_tokens":1,"output_tokens":1}}`
}

func toolReply(calls ...string) string { // each call: `"id","name",{input}`
	var blocks []string
	for _, c := range calls {
		parts := strings.SplitN(c, ",", 3)
		blocks = append(blocks, `{"type":"tool_use","id":`+parts[0]+`,"name":`+parts[1]+`,"input":`+parts[2]+`}`)
	}
	return `{"id":"m","type":"message","role":"assistant","model":"x","stop_reason":"tool_use","content":[` + strings.Join(blocks, ",") + `],"usage":{"input_tokens":1,"output_tokens":1}}`
}

type fakeApprover struct {
	decision Decision
	err      error
	seen     []Proposal
}

func (f *fakeApprover) Approve(_ context.Context, p Proposal) (Decision, error) {
	f.seen = append(f.seen, p)
	return f.decision, f.err
}

func schema(props map[string]any, required ...string) anthropic.ToolInputSchemaParam {
	return anthropic.ToolInputSchemaParam{Properties: props, Required: required}
}

func newRegistry(t *testing.T, ran *[]string) *Registry {
	t.Helper()
	r := NewRegistry()
	must := func(tool Tool) {
		if err := r.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	must(Tool{
		Name: "list_jobs", Description: "List jobs.", Schema: schema(map[string]any{}),
		Run: func(_ context.Context, _ json.RawMessage, _ Decision) (string, error) {
			*ran = append(*ran, "list_jobs")
			return "no jobs", nil
		},
	})
	must(Tool{
		Name: "submit_job", Description: "Submit a job.", NeedsApproval: true,
		Schema: schema(map[string]any{"script": map[string]any{"type": "string"}}, "script"),
		Propose: func(in json.RawMessage) (Proposal, error) {
			var v struct{ Script string }
			_ = json.Unmarshal(in, &v)
			return Proposal{Tool: "submit_job", Summary: "Submit to hpcone", Details: v.Script, Editable: true}, nil
		},
		Run: func(_ context.Context, in json.RawMessage, d Decision) (string, error) {
			script := d.Edited
			if script == "" {
				var v struct{ Script string }
				_ = json.Unmarshal(in, &v)
				script = v.Script
			}
			*ran = append(*ran, "submit_job:"+script)
			return "job 42 submitted", nil
		},
	})
	must(Tool{
		Name: "flaky", Description: "Always fails.", Schema: schema(map[string]any{}),
		Run: func(context.Context, json.RawMessage, Decision) (string, error) {
			return "", errors.New("cluster unreachable")
		},
	})
	return r
}

// toolResults extracts the tool_result blocks of the last user message as id -> (text, isError).
func lastToolResults(t *testing.T, history []anthropic.BetaMessageParam) map[string][2]string {
	t.Helper()
	last := history[len(history)-1]
	b, _ := json.Marshal(last)
	var m struct {
		Role    string
		Content []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			IsError   bool   `json:"is_error"`
			Content   []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	out := map[string][2]string{}
	for _, c := range m.Content {
		if c.Type == "tool_result" {
			text := ""
			for _, x := range c.Content {
				text += x.Text
			}
			e := ""
			if c.IsError {
				e = "error"
			}
			out[c.ToolUseID] = [2]string{text, e}
		}
	}
	return out
}

func TestRegistry(t *testing.T) {
	var ran []string
	r := newRegistry(t, &ran)
	if len(r.Params()) != 3 {
		t.Fatalf("Params = %d", len(r.Params()))
	}
	b, _ := json.Marshal(r.Params()[0])
	if !strings.Contains(string(b), `"name":"list_jobs"`) {
		t.Errorf("first param = %s", b)
	}
	bad := []Tool{
		{Name: "", Description: "d", Run: func(context.Context, json.RawMessage, Decision) (string, error) { return "", nil }},
		{Name: "list_jobs", Description: "dup", Run: func(context.Context, json.RawMessage, Decision) (string, error) { return "", nil }},
		{Name: "norun", Description: "d"},
		{Name: "nodesc", Run: func(context.Context, json.RawMessage, Decision) (string, error) { return "", nil }},
		{Name: "gated", Description: "d", NeedsApproval: true, Run: func(context.Context, json.RawMessage, Decision) (string, error) { return "", nil }},
	}
	for _, tool := range bad {
		if err := r.Register(tool); err == nil {
			t.Errorf("Register(%q) succeeded", tool.Name)
		}
	}
	if _, ok := r.Get("submit_job"); !ok {
		t.Error("Get(submit_job) failed")
	}
}

func TestSendPlainText(t *testing.T) {
	var ran []string
	m := newScripted(t, textReply("Hello!"))
	var events []Event
	a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: &fakeApprover{}, System: "sys", OnEvent: func(e Event) { events = append(events, e) }}
	h, err := a.Send(context.Background(), nil, "hi", Typed)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2", len(h))
	}
	if len(m.b.Requests()) != 1 || m.b.Requests()[0].Hint != broker.HintPlan || m.b.Requests()[0].System != "sys" || m.b.Requests()[0].Tools != 3 {
		t.Errorf("request = %+v", m.b.Requests()[0])
	}
	// The step comes before the model call, so the front end shows it
	// before the first token.
	if len(events) != 2 || events[0].Kind != EventStep || events[0].Text != StepWaitingForClaude ||
		events[1].Kind != EventText || events[1].Text != "Hello!" {
		t.Errorf("events = %+v", events)
	}
}

func TestSendToolLoop(t *testing.T) {
	var ran []string
	m := newScripted(t,
		toolReply(`"t1","list_jobs",{}`, `"t2","flaky",{}`, `"t3","nope",{}`),
		textReply("Done."),
	)
	a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: &fakeApprover{}}
	h, err := a.Send(context.Background(), nil, "status?", Typed)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.b.Requests()) != 2 || m.b.Requests()[1].Hint != broker.HintRoutine {
		t.Fatalf("reqs = %d, hint2 = %v", len(m.b.Requests()), m.b.Requests()[len(m.b.Requests())-1].Hint)
	}
	// user, assistant(tool_use), user(tool_results), assistant(text)
	if len(h) != 4 {
		t.Fatalf("history len = %d", len(h))
	}
	res := lastToolResults(t, h[:3])
	if res["t1"] != [2]string{"no jobs", ""} {
		t.Errorf("t1 = %v", res["t1"])
	}
	if res["t2"][1] != "error" || !strings.Contains(res["t2"][0], "cluster unreachable") {
		t.Errorf("t2 = %v", res["t2"])
	}
	// The tool runner answers unknown tools itself ("Error: Tool 'nope' not found").
	if res["t3"][1] != "error" || !strings.Contains(res["t3"][0], "not found") {
		t.Errorf("t3 = %v", res["t3"])
	}
	if strings.Join(ran, ",") != "list_jobs" {
		t.Errorf("ran = %v", ran)
	}
}

func TestSendApproval(t *testing.T) {
	t.Run("approved with edit", func(t *testing.T) {
		var ran []string
		ap := &fakeApprover{decision: Decision{Approved: true, Edited: "#SBATCH --gres=gpu:2\npython train.py"}}
		m := newScripted(t, toolReply(`"s1","submit_job",{"script":"python train.py"}`), textReply("Submitted."))
		a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: ap}
		if _, err := a.Send(context.Background(), nil, "run it", Typed); err != nil {
			t.Fatal(err)
		}
		if len(ap.seen) != 1 || ap.seen[0].Details != "python train.py" || !ap.seen[0].Editable {
			t.Errorf("proposal = %+v", ap.seen)
		}
		if len(ran) != 1 || !strings.Contains(ran[0], "--gres=gpu:2") {
			t.Errorf("ran = %v", ran)
		}
	})
	t.Run("declined", func(t *testing.T) {
		var ran []string
		var events []Event
		ap := &fakeApprover{decision: Decision{Approved: false, Reason: "wrong partition"}}
		m := newScripted(t, toolReply(`"s1","submit_job",{"script":"x"}`), textReply("OK, not submitted."))
		a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: ap, OnEvent: func(e Event) { events = append(events, e) }}
		h, err := a.Send(context.Background(), nil, "run it", Typed)
		if err != nil {
			t.Fatal(err)
		}
		if len(ran) != 0 {
			t.Errorf("declined tool ran: %v", ran)
		}
		res := lastToolResults(t, h[:3])
		// A decline is a normal tool result worded by Labmill, not is_error.
		if res["s1"][1] != "" || !strings.Contains(res["s1"][0], "declined") || !strings.Contains(res["s1"][0], "wrong partition") {
			t.Errorf("s1 = %v", res["s1"])
		}
		found := false
		for _, e := range events {
			if e.Kind == EventDeclined && e.Tool == "submit_job" {
				found = true
			}
		}
		if !found {
			t.Errorf("no EventDeclined in %+v", events)
		}
	})
	t.Run("approver error aborts", func(t *testing.T) {
		var ran []string
		boom := errors.New("ui closed")
		m := newScripted(t, toolReply(`"s1","submit_job",{"script":"x"}`))
		a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: &fakeApprover{err: boom}}
		h, err := a.Send(context.Background(), nil, "run it", Typed)
		if !errors.Is(err, boom) || len(ran) != 0 || len(h) < 2 {
			t.Errorf("err = %v, ran = %v, history = %d", err, ran, len(h))
		}
	})
}

func TestSendVoiceNote(t *testing.T) {
	var ran []string
	m := newScripted(t, textReply("You want sbatch on hpcone, right?"))
	a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: &fakeApprover{}}
	h, err := a.Send(context.Background(), nil, "run S batch on wild wood", Voice)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(h[0])
	if !strings.Contains(string(b), "dictated") || !strings.Contains(string(b), "run S batch on wild wood") {
		t.Errorf("voice message = %s", b)
	}
	h2, _ := (&Agent{Model: newScripted(t, textReply("ok")), Tools: newRegistry(t, &ran), Approver: &fakeApprover{}}).Send(context.Background(), nil, "typed", Typed)
	b2, _ := json.Marshal(h2[0])
	if strings.Contains(string(b2), "dictated") {
		t.Error("typed message got the voice note")
	}
}

func TestSendMaxTurns(t *testing.T) {
	var ran []string
	var replies []string
	for i := 0; i < 5; i++ {
		replies = append(replies, toolReply(`"t","list_jobs",{}`))
	}
	m := newScripted(t, replies...)
	a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: &fakeApprover{}, MaxTurns: 3}
	var texts []string
	a.OnEvent = func(e Event) {
		if e.Kind == EventText {
			texts = append(texts, e.Text)
		}
	}
	h, err := a.Send(context.Background(), nil, "loop", Typed)
	if err != nil {
		t.Fatalf("err = %v, want none", err)
	}
	if len(m.b.Requests()) != 3 {
		t.Errorf("model calls = %d", len(m.b.Requests()))
	}
	// The history ends with not-run results and the stop message, so it is
	// valid for the next request.
	if n := len(h); n < 3 || h[n-1].Role != anthropic.BetaMessageParamRoleAssistant || h[n-2].Role != anthropic.BetaMessageParamRoleUser {
		t.Fatalf("history roles end wrong: %d messages", len(h))
	}
	raw, _ := json.Marshal(h[len(h)-2:])
	if !strings.Contains(string(raw), "not run: too many tool-use turns") || !strings.Contains(string(raw), MaxTurnsMessage(3)) {
		t.Errorf("tail = %s", raw)
	}
	if len(texts) == 0 || !strings.Contains(texts[len(texts)-1], "I stopped after 3 steps so you can check in. Say 'continue'") {
		t.Errorf("events = %q", texts)
	}
	// The next message continues normally.
	m2 := newScripted(t, textReply("done"))
	a2 := &Agent{Model: m2, Tools: newRegistry(t, &ran), Approver: &fakeApprover{}, MaxTurns: 3}
	h2, err := a2.Send(context.Background(), h, "continue", Typed)
	if err != nil || len(h2) != len(h)+2 {
		t.Fatalf("continue: err = %v, history %d -> %d", err, len(h), len(h2))
	}
	reqs := m2.b.Requests()
	body, _ := json.Marshal(reqs[0].Body)
	if len(reqs) != 1 || !strings.Contains(string(body), "I stopped after 3 steps") {
		t.Errorf("continue request lacks the history: %d", len(reqs))
	}
}

func TestSendCanceled(t *testing.T) {
	var ran []string
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &Agent{Model: newScripted(t, textReply("x")), Tools: newRegistry(t, &ran), Approver: &fakeApprover{}}
	if _, err := a.Send(ctx, nil, "hi", Typed); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestSkillTools(t *testing.T) {
	set, err := skill.Load(fstest.MapFS{
		"alpha/SKILL.md":        {Data: []byte("---\nname: alpha\ndescription: Alpha.\n---\nAlpha body.\n")},
		"alpha/references/a.md": {Data: []byte("ref A")},
	}, "embedded")
	if err != nil {
		t.Fatal(err)
	}
	tools := SkillTools(set)
	if len(tools) != 2 || tools[0].Name != "load_skill" || tools[1].Name != "read_reference" {
		t.Fatalf("tools = %+v", tools)
	}
	for _, tool := range tools {
		if tool.NeedsApproval {
			t.Errorf("%s needs approval", tool.Name)
		}
	}
	b, _ := json.Marshal(tools[0].Schema)
	if !strings.Contains(string(b), `"alpha"`) {
		t.Errorf("load_skill schema lacks enum of names: %s", b)
	}
	out, err := tools[0].Run(context.Background(), json.RawMessage(`{"name":"alpha"}`), Decision{})
	if err != nil || !strings.Contains(out, "Alpha body.") {
		t.Errorf("load_skill = %q, %v", out, err)
	}
	out, err = tools[1].Run(context.Background(), json.RawMessage(`{"skill":"alpha","file":"a.md"}`), Decision{})
	if err != nil || out != "ref A" {
		t.Errorf("read_reference = %q, %v", out, err)
	}
	if _, err := tools[1].Run(context.Background(), json.RawMessage(`{"skill":"alpha","file":"../x"}`), Decision{}); err == nil {
		t.Error("read_reference traversal succeeded")
	}
	if _, err := tools[0].Run(context.Background(), json.RawMessage(`{"name":"nope"}`), Decision{}); err == nil {
		t.Error("load_skill unknown succeeded")
	}
}

// fakeChooser answers every Choice with a fixed answer and records it.
type fakeChooser struct {
	answer ChoiceAnswer
	seen   []Choice
}

func (f *fakeChooser) Choose(_ context.Context, q Choice) (ChoiceAnswer, error) {
	f.seen = append(f.seen, q)
	return f.answer, nil
}

// A question tool (like app's ask_user) is not gated: it reaches the user
// through a Chooser, never through the Approver.
func TestChoiceToolNeedsNoApprover(t *testing.T) {
	ch := &fakeChooser{answer: ChoiceAnswer{Index: 1}}
	r := NewRegistry()
	if err := r.Register(Tool{
		Name: "ask_user", Description: "Ask the user.", Schema: schema(map[string]any{}),
		Run: func(ctx context.Context, _ json.RawMessage, d Decision) (string, error) {
			if d.Approved {
				t.Error("an ungated tool got an approved Decision")
			}
			a, err := ch.Choose(ctx, Choice{Question: "Which?", Options: []ChoiceOption{{Label: "a"}, {Label: "b"}}, AllowOther: true})
			if err != nil {
				return "", err
			}
			return "The user chose: " + []string{"a", "b"}[a.Index], nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	m := newScripted(t, toolReply(`"t1","ask_user",{}`), textReply("ok"))
	ag := &Agent{Model: m, Tools: r} // no Approver at all
	h, err := ag.Send(context.Background(), nil, "help", Typed)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.seen) != 1 || ch.seen[0].Question != "Which?" {
		t.Errorf("chooser saw %+v", ch.seen)
	}
	if res := lastToolResults(t, h[:len(h)-1]); res["t1"][0] != "The user chose: b" || res["t1"][1] != "" {
		t.Errorf("tool result = %v", res)
	}
}

// SystemExtra is read once per Send and appended to System for every model
// call of that Send; a later Send sees the new value.
func TestSendSystemExtra(t *testing.T) {
	var ran []string
	extra := "\nfolders: A"
	calls := 0
	m := newScripted(t, toolReply(`"t1","list_jobs",{}`), textReply("done"), textReply("again"))
	a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: &fakeApprover{}, System: "sys",
		SystemExtra: func() string { calls++; return extra }}
	h, err := a.Send(context.Background(), nil, "hi", Typed)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.b.Requests()) != 2 || m.b.Requests()[0].System != "sys\nfolders: A" || m.b.Requests()[1].System != "sys\nfolders: A" {
		t.Fatalf("systems = %q", []string{m.b.Requests()[0].System, m.b.Requests()[len(m.b.Requests())-1].System})
	}
	extra = ""
	if _, err := a.Send(context.Background(), h, "more", Typed); err != nil {
		t.Fatal(err)
	}
	if m.b.Requests()[2].System != "sys" || calls != 2 {
		t.Errorf("second Send: system %q, SystemExtra calls %d (want \"sys\", 2)", m.b.Requests()[2].System, calls)
	}
}

// Every model call is announced with a step (the busy indicator), after the
// tool results of the previous round.
func TestSendSteps(t *testing.T) {
	var ran []string
	m := newScripted(t, toolReply(`"t1","list_jobs",{}`), textReply("Done."))
	var kinds []EventKind
	a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: &fakeApprover{}, OnEvent: func(e Event) {
		kinds = append(kinds, e.Kind)
		if e.Kind == EventStep && e.Text != StepWaitingForClaude {
			t.Errorf("step text = %q", e.Text)
		}
	}}
	if _, err := a.Send(context.Background(), nil, "status?", Typed); err != nil {
		t.Fatal(err)
	}
	want := []EventKind{EventStep, EventToolCall, EventToolResult, EventStep, EventText}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Errorf("events = %v, want %v", kinds, want)
	}
	if StepRunning("cluster_info") != "Running cluster_info…" {
		t.Errorf("StepRunning = %q", StepRunning("cluster_info"))
	}
}
