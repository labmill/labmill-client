package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"labmill-client/internal/agent/agenttest"
	"labmill-client/internal/broker"
)

// usageReply sets a reply's usage to input prompt tokens.
func usageReply(reply string, input int) string {
	return strings.Replace(reply, `"usage":{"input_tokens":1`, fmt.Sprintf(`"usage":{"input_tokens":%d`, input), 1)
}

const compactionReply = `{"id":"c","type":"message","role":"assistant","model":"x","stop_reason":"compaction","content":[{"type":"compaction","content":"The user asked about jobs; none are running.","signature":"c2ln"}],"usage":{"input_tokens":0,"output_tokens":0}}`

// The planning request asks for adaptive thinking with the larger token
// cap; every request carries the compaction beta, the top-level cache
// breakpoint and breakpoints on the fixed system block and the last tool.
func TestSendRequestShape(t *testing.T) {
	var ran []string
	m := newScripted(t, toolReply(`"t1","list_jobs",{}`), textReply("Done."))
	a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: &fakeApprover{}, System: "sys", SystemExtra: func() string { return "\nextra" }}
	if _, err := a.Send(context.Background(), nil, "hi", Typed); err != nil {
		t.Fatal(err)
	}
	reqs := m.b.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	for i, r := range reqs {
		if fmt.Sprint(r.Betas) != "[compact-2026-09-04]" {
			t.Errorf("req %d betas = %v", i, r.Betas)
		}
		if cc, _ := r.Body["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
			t.Errorf("req %d: no top-level cache_control: %v", i, r.Body["cache_control"])
		}
		sys, _ := r.Body["system"].([]any)
		if len(sys) != 2 {
			t.Fatalf("req %d system = %v", i, r.Body["system"])
		}
		if s0 := sys[0].(map[string]any); s0["text"] != "sys" || s0["cache_control"] == nil {
			t.Errorf("req %d: system[0] = %v", i, s0)
		}
		if s1 := sys[1].(map[string]any); s1["text"] != "\nextra" || s1["cache_control"] != nil {
			t.Errorf("req %d: system[1] = %v", i, s1)
		}
		tools := r.Body["tools"].([]any)
		for j, tl := range tools {
			_, has := tl.(map[string]any)["cache_control"]
			if has != (j == len(tools)-1) {
				t.Errorf("req %d tool %d cache_control = %v", i, j, has)
			}
		}
		if r.Auth != "Bearer "+agenttest.Token {
			t.Errorf("req %d auth = %q", i, r.Auth)
		}
	}
	if th, _ := reqs[0].Body["thinking"].(map[string]any); th["type"] != "adaptive" || th["display"] != "omitted" {
		t.Errorf("plan thinking = %v", reqs[0].Body["thinking"])
	}
	if reqs[0].Body["max_tokens"] != float64(PlanMaxTokens) || reqs[1].Body["max_tokens"] != float64(RoutineMaxTokens) {
		t.Errorf("max_tokens = %v, %v", reqs[0].Body["max_tokens"], reqs[1].Body["max_tokens"])
	}
	if _, ok := reqs[1].Body["thinking"]; ok {
		t.Errorf("routine request asks for thinking: %v", reqs[1].Body["thinking"])
	}
	if reqs[0].Hint != broker.HintPlan || reqs[1].Hint != broker.HintRoutine {
		t.Errorf("hints = %s, %s", reqs[0].Hint, reqs[1].Hint)
	}

	m2 := newScripted(t, textReply("ok"))
	if _, err := (&Agent{Model: m2, Tools: NewRegistry(), NoThinking: true}).Send(context.Background(), nil, "hi", Typed); err != nil {
		t.Fatal(err)
	}
	if r := m2.b.Requests()[0]; r.Body["thinking"] != nil || r.Body["max_tokens"] != float64(RoutineMaxTokens) {
		t.Errorf("NoThinking request: thinking %v, max_tokens %v", r.Body["thinking"], r.Body["max_tokens"])
	}
}

// Past CompactAt the runner compacts before the next turn: a compaction
// request (routine hint), then the turn goes on from the compaction block.
func TestSendCompactsMidTurn(t *testing.T) {
	var ran []string
	var events []Event
	m := newScripted(t, usageReply(toolReply(`"t1","list_jobs",{}`), 120), compactionReply, textReply("Done."))
	var stops []string
	a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: &fakeApprover{}, CompactAt: 100,
		OnEvent: func(e Event) { events = append(events, e) },
		OnReply: func(m *anthropic.BetaMessage) { stops = append(stops, string(m.StopReason)) }}
	h, err := a.Send(context.Background(), nil, "status?", Typed)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(stops) != "[tool_use compaction end_turn]" {
		t.Errorf("OnReply stop reasons = %v", stops)
	}
	reqs := m.b.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests", len(reqs))
	}
	if _, ok := reqs[1].Body["compaction"]; !ok || reqs[1].Hint != broker.HintRoutine {
		t.Errorf("request 2 is not a routine compaction request: hint %s, body keys %v", reqs[1].Hint, keys(reqs[1].Body))
	}
	if _, ok := reqs[2].Body["compaction"]; ok {
		t.Error("request 3 compacts again")
	}
	b, _ := json.Marshal(reqs[2].Body["messages"])
	if !strings.HasPrefix(string(b), `[{"content":[{"content":"The user asked about jobs`) {
		t.Errorf("request 3 messages = %s", b)
	}
	if len(ran) != 1 {
		t.Errorf("ran = %v", ran)
	}
	var steps, notes []string
	for _, e := range events {
		switch e.Kind {
		case EventStep:
			steps = append(steps, e.Text)
		case EventNote:
			notes = append(notes, e.Text)
		}
	}
	if fmt.Sprint(steps) != fmt.Sprint([]string{StepWaitingForClaude, StepSummarizing, StepWaitingForClaude}) {
		t.Errorf("steps = %q", steps)
	}
	if fmt.Sprint(notes) != fmt.Sprint([]string{NoteCompacted}) {
		t.Errorf("notes = %q", notes)
	}
	// compaction message, final reply
	if len(h) != 2 || h[0].Role != anthropic.BetaMessageParamRoleAssistant || h[0].Content[0].OfCompaction == nil {
		hb, _ := json.Marshal(h)
		t.Errorf("history = %s", hb)
	}
}

// A final reply past CompactAt is followed by the compaction, so the next
// Send starts from the summary.
func TestSendCompactsAfterLastReply(t *testing.T) {
	m := newScripted(t, usageReply(textReply("Done."), 500), compactionReply, textReply("Again."))
	a := &Agent{Model: m, Tools: NewRegistry(), CompactAt: 100}
	h, err := a.Send(context.Background(), nil, "hi", Typed)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.b.Requests()) != 2 || len(h) != 1 || h[0].Content[0].OfCompaction == nil {
		hb, _ := json.Marshal(h)
		t.Fatalf("requests %d, history %s", len(m.b.Requests()), hb)
	}
	h, err = a.Send(context.Background(), h, "more", Typed)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 3 {
		t.Errorf("history after the next Send = %d messages, want 3", len(h))
	}
	// The next Send's first request is a planning request again.
	if r := m.b.Requests()[2]; r.Hint != broker.HintPlan {
		t.Errorf("hint = %s", r.Hint)
	}
}

// A negative CompactAt never compacts.
func TestSendCompactionOff(t *testing.T) {
	m := newScripted(t, usageReply(textReply("Done."), 10_000_000))
	if _, err := (&Agent{Model: m, Tools: NewRegistry(), CompactAt: -1}).Send(context.Background(), nil, "hi", Typed); err != nil {
		t.Fatal(err)
	}
	if n := len(m.b.Requests()); n != 1 {
		t.Errorf("%d requests", n)
	}
}

// A thinking block shows as a step; its text never reaches the chat or the
// text events.
func TestSendThinkingIndicator(t *testing.T) {
	reply := `{"id":"m","type":"message","role":"assistant","model":"x","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"secret plan","signature":"c2ln"},{"type":"text","text":"Answer."}],"usage":{"input_tokens":1,"output_tokens":1}}`
	var events []Event
	m := newScripted(t, reply)
	h, err := (&Agent{Model: m, Tools: NewRegistry(), OnEvent: func(e Event) { events = append(events, e) }}).Send(context.Background(), nil, "hi", Typed)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, fmt.Sprintf("%d:%s", e.Kind, e.Text))
		if strings.Contains(e.Text, "secret") {
			t.Errorf("thinking text leaked into %+v", e)
		}
	}
	want := []string{fmt.Sprintf("%d:%s", EventStep, StepWaitingForClaude), fmt.Sprintf("%d:%s", EventStep, StepThinking), fmt.Sprintf("%d:Answer.", EventText)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	// The signed thinking block stays in the history (the API needs it back).
	if len(h) != 2 || h[1].Content[0].OfThinking == nil {
		hb, _ := json.Marshal(h)
		t.Errorf("history = %s", hb)
	}
}

// A stream that ends without message_stop is an error and its partial
// reply is dropped.
func TestSendStreamCutOff(t *testing.T) {
	full, _ := agenttest.SSE(textReply("partial"))
	cut := full[:strings.Index(full, "event: message_delta")]
	m := newScripted(t, agenttest.Raw+cut)
	h, err := (&Agent{Model: m, Tools: NewRegistry()}).Send(context.Background(), nil, "hi", Typed)
	if err == nil || !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("err = %v", err)
	}
	if len(h) != 1 {
		t.Errorf("history = %d messages, want only the user message", len(h))
	}
}

// A broker error is returned with the history so far.
func TestSendModelError(t *testing.T) {
	var ran []string
	m := newScripted(t, toolReply(`"t1","list_jobs",{}`)) // the second request gets HTTP 500
	h, err := (&Agent{Model: m, Tools: newRegistry(t, &ran)}).Send(context.Background(), nil, "hi", Typed)
	if err == nil || !strings.Contains(err.Error(), "agent: model") {
		t.Fatalf("err = %v", err)
	}
	assertValid(t, h)
	if len(h) != 3 {
		t.Errorf("history = %d messages, want user, tool call, results", len(h))
	}
}

type failingModel struct{}

func (failingModel) BetaMessages() (*anthropic.BetaMessageService, []option.RequestOption, error) {
	return nil, nil, errors.New("no broker configured")
}

func TestSendNoModel(t *testing.T) {
	h, err := (&Agent{Model: failingModel{}, Tools: NewRegistry()}).Send(context.Background(), nil, "hi", Typed)
	if err == nil || !strings.Contains(err.Error(), "no broker configured") || len(h) != 1 {
		t.Errorf("err = %v, history %d", err, len(h))
	}
}

// orderedRegistry has three tools that record start and end; "gated" needs
// approval.
func orderedRegistry(t *testing.T, mu *sync.Mutex, log *[]string) *Registry {
	r := NewRegistry()
	for _, name := range []string{"a", "gated", "c"} {
		tool := Tool{Name: name, Description: "d", Schema: schema(map[string]any{"n": map[string]any{"type": "integer"}}),
			Run: func(_ context.Context, in json.RawMessage, _ Decision) (string, error) {
				mu.Lock()
				*log = append(*log, "start "+name+string(in))
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				mu.Lock()
				*log = append(*log, "end "+name)
				mu.Unlock()
				return name + " ok", nil
			}}
		if name == "gated" {
			tool.NeedsApproval = true
			tool.Propose = func(json.RawMessage) (Proposal, error) { return Proposal{Tool: name, Summary: "s"}, nil }
		}
		if err := r.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// Tools run one at a time, in Claude's order, although the runner starts
// them together; the events say started, approved, finished per tool.
func TestSendToolsInOrder(t *testing.T) {
	var mu sync.Mutex
	var log []string
	var kinds []string
	m := newScripted(t, toolReply(`"t1","c",{"n":1}`, `"t2","gated",{}`, `"t3","a",{}`, `"t4","c",{"n":2}`), textReply("done"))
	a := &Agent{Model: m, Tools: orderedRegistry(t, &mu, &log), Approver: &fakeApprover{decision: Decision{Approved: true}},
		OnEvent: func(e Event) {
			if e.Kind == EventToolCall || e.Kind == EventToolResult || e.Kind == EventApproved {
				kinds = append(kinds, fmt.Sprintf("%d:%s", e.Kind, e.Tool))
			}
		}}
	h, err := a.Send(context.Background(), nil, "go", Typed)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`start c{"n":1}`, "end c", "start gated{}", "end gated", "start a{}", "end a", `start c{"n":2}`, "end c"}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Errorf("run log = %q\nwant %q", log, want)
	}
	wantKinds := []string{"1:c", "2:c", "1:gated", "5:gated", "2:gated", "1:a", "2:a", "1:c", "2:c"}
	if fmt.Sprint(kinds) != fmt.Sprint(wantKinds) {
		t.Errorf("events = %v\nwant %v", kinds, wantKinds)
	}
	res := lastToolResults(t, h[:3])
	for id, want := range map[string]string{"t1": "c ok", "t2": "gated ok", "t3": "a ok", "t4": "c ok"} {
		if res[id][0] != want || res[id][1] != "" {
			t.Errorf("%s = %v", id, res[id])
		}
	}
}

// blockingApprover never answers until its context ends.
type blockingApprover struct{ asked chan struct{} }

func (b blockingApprover) Approve(ctx context.Context, _ Proposal) (Decision, error) {
	close(b.asked)
	<-ctx.Done()
	time.Sleep(10 * time.Millisecond) // a dialog that is slow to close
	return Decision{}, ctx.Err()
}

// Canceling while a dialog is open ends Send promptly with valid history.
func TestSendCancelDuringOpenDialog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var log []string
	asked := make(chan struct{})
	m := newScripted(t, toolReply(`"t1","a",{}`, `"t2","gated",{}`, `"t3","c",{}`))
	a := &Agent{Model: m, Tools: orderedRegistry(t, &mu, &log), Approver: blockingApprover{asked: asked}}
	go func() {
		<-asked
		cancel()
	}()
	done := make(chan struct{})
	var h []anthropic.BetaMessageParam
	var err error
	go func() {
		h, err = a.Send(ctx, nil, "go", Typed)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Send hangs on an open dialog after cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	res := lastToolResults(t, h)
	if res["t1"][0] != "a ok" || res["t2"] != [2]string{"canceled", "error"} || res["t3"] != [2]string{"canceled", "error"} {
		t.Errorf("results = %v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(log) != fmt.Sprint([]string{"start a{}", "end a"}) {
		t.Errorf("ran = %v", log)
	}
	assertValid(t, h)
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
