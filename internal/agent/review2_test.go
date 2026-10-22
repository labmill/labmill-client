package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"labmill-client/internal/agent/agenttest"
)

// Regression (review M28): a cancel during the first request of a later
// Send keeps the earlier turn's real tool results and the new message.
func TestSendCancelFirstRequestKeepsEarlierResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := agenttest.NewFunc(t, func(int, agenttest.Request) (string, error) {
		cancel()
		time.Sleep(50 * time.Millisecond)
		return textReply("too late"), nil
	})
	h := []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("go")),
		{Role: anthropic.BetaMessageParamRoleAssistant, Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaToolUseBlock("t1", map[string]any{}, "submit_job")}},
		anthropic.NewBetaUserMessage(anthropic.NewBetaToolResultBlock("t1", "submitted job 7", false)),
	}
	var ran []string
	got, err := (&Agent{Model: b.Client(t), Tools: newRegistry(t, &ran), Approver: &fakeApprover{}}).Send(ctx, h, "and now?", Typed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("history = %d messages, want 4", len(got))
	}
	if res := lastToolResults(t, got[:3]); res["t1"] != [2]string{"submitted job 7", ""} {
		t.Errorf("t1 = %v", res["t1"])
	}
	if blk := got[3].Content[0]; blk.OfText == nil || blk.OfText.Text != "and now?" {
		hb, _ := json.Marshal(got[3])
		t.Errorf("new message = %s", hb)
	}
	assertValid(t, got)
}

// Unknown-tool calls keep their "not found" results when the turn is
// aborted, and all tool events come in Claude's order.
func TestSendAbortKeepsUnknownToolResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewRegistry()
	_ = r.Register(Tool{Name: "a", Description: "d", Schema: schema(map[string]any{}),
		Run: func(context.Context, json.RawMessage, Decision) (string, error) { cancel(); return "A ok", nil }})
	_ = r.Register(Tool{Name: "b", Description: "d", Schema: schema(map[string]any{}),
		Run: func(context.Context, json.RawMessage, Decision) (string, error) { return "B ok", nil }})
	var order []string
	m := newScripted(t, toolReply(`"t1","nope",{}`, `"t2","a",{}`, `"t3","zip",{}`, `"t4","b",{}`))
	a := &Agent{Model: m, Tools: r, OnEvent: func(e Event) {
		if e.Kind == EventToolCall || e.Kind == EventToolResult {
			order = append(order, fmt.Sprintf("%d:%s", e.Kind, e.Tool))
		}
	}}
	h, err := a.Send(ctx, nil, "go", Typed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	res := lastToolResults(t, h)
	want := map[string][2]string{
		"t1": {"Error: Tool 'nope' not found", "error"},
		"t2": {"A ok", ""},
		"t3": {"Error: Tool 'zip' not found", "error"},
		"t4": {"canceled", "error"},
	}
	for id, w := range want {
		if res[id] != w {
			t.Errorf("%s = %v, want %v", id, res[id], w)
		}
	}
	wantOrder := []string{"1:nope", "2:nope", "1:a", "2:a", "1:zip", "2:zip", "2:b"}
	if fmt.Sprint(order) != fmt.Sprint(wantOrder) {
		t.Errorf("events = %v\nwant %v", order, wantOrder)
	}
	assertValid(t, h)
}

// Unknown tools around known ones are reported in Claude's order.
func TestSendUnknownToolEventOrder(t *testing.T) {
	var ran []string
	var order []string
	m := newScripted(t, toolReply(`"t1","nope",{}`, `"t2","list_jobs",{}`, `"t3","zip",{}`), textReply("ok"))
	a := &Agent{Model: m, Tools: newRegistry(t, &ran), OnEvent: func(e Event) {
		if e.Kind == EventToolCall || e.Kind == EventToolResult {
			order = append(order, fmt.Sprintf("%d:%s", e.Kind, e.Tool))
		}
	}}
	if _, err := a.Send(context.Background(), nil, "go", Typed); err != nil {
		t.Fatal(err)
	}
	want := []string{"1:nope", "2:nope", "1:list_jobs", "2:list_jobs", "1:zip", "2:zip"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Errorf("events = %v, want %v", order, want)
	}
}

// Run gets Claude's input bytes, so large integers stay exact.
func TestSendRawToolInput(t *testing.T) {
	var got json.RawMessage
	r := NewRegistry()
	_ = r.Register(Tool{Name: "job", Description: "d", Schema: schema(map[string]any{"id": map[string]any{"type": "integer"}}),
		Run: func(_ context.Context, in json.RawMessage, _ Decision) (string, error) { got = in; return "ok", nil }})
	m := newScripted(t, toolReply(`"t1","job",{"id":12345678901234567890123}`), textReply("done"))
	if _, err := (&Agent{Model: m, Tools: r}).Send(context.Background(), nil, "go", Typed); err != nil {
		t.Fatal(err)
	}
	var v struct {
		ID json.Number `json:"id"`
	}
	if err := json.Unmarshal(got, &v); err != nil || v.ID.String() != "12345678901234567890123" {
		t.Errorf("Run got %s (id %v, %v)", got, v.ID, err)
	}
}

// A compaction stream cut off before message_stop leaves the history as
// the compaction request sent it.
func TestSendCompactionCutOff(t *testing.T) {
	full, _ := agenttest.SSE(compactionReply)
	cut := full[:strings.Index(full, "event: message_delta")]
	m := newScripted(t, usageReply(textReply("Done."), 500), agenttest.Raw+cut)
	h, err := (&Agent{Model: m, Tools: NewRegistry(), CompactAt: 100}).Send(context.Background(), nil, "hi", Typed)
	if err == nil || !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("err = %v", err)
	}
	if len(h) != 2 || h[1].Content[0].OfText == nil || h[1].Content[0].OfText.Text != "Done." {
		hb, _ := json.Marshal(h)
		t.Errorf("history = %s", hb)
	}
}

// A reply cut off with tool calls is never compacted (the runner can't).
func TestSendNoCompactionAfterCutOffToolCalls(t *testing.T) {
	var ran []string
	reply := usageReply(replyWith("max_tokens", `{"type":"tool_use","id":"t1","name":"list_jobs","input":{}}`), 500)
	m := newScripted(t, reply)
	h, err := (&Agent{Model: m, Tools: newRegistry(t, &ran), CompactAt: 100}).Send(context.Background(), nil, "hi", Typed)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v", err)
	}
	if n := len(m.b.Requests()); n != 1 {
		t.Errorf("%d requests, want 1 (no compaction)", n)
	}
	assertValid(t, h)
}

// The fake broker streams like the real one: tool input in
// input_json_delta chunks, thinking with a signature_delta.
func TestSSEShape(t *testing.T) {
	msg := `{"id":"m","type":"message","role":"assistant","model":"x","stop_reason":"tool_use","content":[` +
		`{"type":"thinking","thinking":"hm","signature":"c2ln"},` +
		`{"type":"tool_use","id":"t1","name":"job","input":{"id":12345678901234567890123,"name":"ä"}}],"usage":{"input_tokens":1,"output_tokens":1}}`
	s, err := agenttest.SSE(msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"signature":"c2ln","type":"signature_delta"`, `"type":"thinking_delta"`, `"content_block":{"id":"t1","input":{},"name":"job","type":"tool_use"}`, `"type":"input_json_delta"`} {
		if !strings.Contains(s, want) {
			t.Errorf("stream lacks %s:\n%s", want, s)
		}
	}
	var m anthropic.BetaMessage
	for _, line := range strings.Split(s, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev anthropic.BetaRawMessageStreamEventUnion
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatal(err)
		}
		if err := m.Accumulate(ev); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.Content) != 2 || m.Content[0].Signature != "c2ln" || string(m.Content[1].Input) != `{"id":12345678901234567890123,"name":"ä"}` {
		t.Errorf("accumulated = %+v / input %s", m.Content, m.Content[1].Input)
	}
}
