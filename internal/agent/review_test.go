package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/anthropics/anthropic-sdk-go"

	"labmill-client/internal/skill"
)

func replyWith(stop, content string) string {
	return `{"id":"m","type":"message","role":"assistant","model":"x","stop_reason":"` + stop + `","content":[` + content + `],"usage":{"input_tokens":1,"output_tokens":1}}`
}

// assertValid checks every tool_use in an assistant message is answered by a
// tool_result in the next message, and that no assistant message is empty.
func assertValid(t *testing.T, history []anthropic.BetaMessageParam) {
	t.Helper()
	for i, m := range history {
		b, _ := json.Marshal(m)
		var v struct {
			Role    string
			Content []struct {
				Type      string
				ID        string
				ToolUseID string `json:"tool_use_id"`
			}
		}
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		if v.Role == "assistant" && len(v.Content) == 0 {
			t.Errorf("history[%d]: empty assistant message", i)
		}
		var ids []string
		for _, c := range v.Content {
			if c.Type == "tool_use" {
				ids = append(ids, c.ID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		if i+1 >= len(history) {
			t.Errorf("history[%d]: tool_use without following message", i)
			continue
		}
		nb, _ := json.Marshal(history[i+1])
		for _, id := range ids {
			if !strings.Contains(string(nb), `"tool_use_id":"`+id+`"`) {
				t.Errorf("history[%d]: tool_use %s has no result", i, id)
			}
		}
	}
}

func TestReviewNonEditableIgnoresEditedAndSetsProposal(t *testing.T) {
	var gotD Decision
	r := NewRegistry()
	_ = r.Register(Tool{
		Name: "go", Description: "d", Schema: schema(map[string]any{}), NeedsApproval: true,
		Propose: func(json.RawMessage) (Proposal, error) {
			return Proposal{Tool: "go", Summary: "s", Details: "shown", Editable: false}, nil
		},
		Run: func(_ context.Context, _ json.RawMessage, d Decision) (string, error) {
			gotD = d
			return "ok", nil
		},
	})
	m := newScripted(t, toolReply(`"t1","go",{}`), textReply("done"))
	a := &Agent{Model: m, Tools: r, Approver: &fakeApprover{decision: Decision{Approved: true, Edited: "evil"}}}
	if _, err := a.Send(context.Background(), nil, "x", Typed); err != nil {
		t.Fatal(err)
	}
	if gotD.Edited != "" {
		t.Errorf("Edited = %q, want cleared", gotD.Edited)
	}
	if gotD.Proposal.Details != "shown" || gotD.Proposal.Tool != "go" {
		t.Errorf("Decision.Proposal = %+v", gotD.Proposal)
	}
}

func TestReviewEditableKeepsEdited(t *testing.T) {
	var ran []string
	m := newScripted(t, toolReply(`"t1","submit_job",{"script":"a"}`), textReply("done"))
	a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: &fakeApprover{decision: Decision{Approved: true, Edited: "b"}}}
	if _, err := a.Send(context.Background(), nil, "x", Typed); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0] != "submit_job:b" {
		t.Errorf("ran = %v", ran)
	}
}

func TestReviewCancelBetweenTools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ran []string
	r := NewRegistry()
	_ = r.Register(Tool{
		Name: "a", Description: "d", Schema: schema(map[string]any{}),
		Run: func(context.Context, json.RawMessage, Decision) (string, error) {
			ran = append(ran, "a")
			cancel()
			return "A done", nil
		},
	})
	_ = r.Register(Tool{
		Name: "b", Description: "d", Schema: schema(map[string]any{}), NeedsApproval: true,
		Propose: func(json.RawMessage) (Proposal, error) { return Proposal{Tool: "b"}, nil },
		Run: func(context.Context, json.RawMessage, Decision) (string, error) {
			ran = append(ran, "b")
			return "B", nil
		},
	})
	ap := &fakeApprover{decision: Decision{Approved: true}}
	m := newScripted(t, toolReply(`"t1","a",{}`, `"t2","b",{}`))
	a := &Agent{Model: m, Tools: r, Approver: ap}
	hist, err := a.Send(ctx, nil, "x", Typed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(ran) != 1 || len(ap.seen) != 0 {
		t.Errorf("ran = %v, approvals = %d", ran, len(ap.seen))
	}
	res := lastToolResults(t, hist)
	if res["t1"][0] != "A done" || res["t2"] != [2]string{"canceled", "error"} {
		t.Errorf("results = %v", res)
	}
	assertValid(t, hist)
}

func TestReviewCancelDuringApproval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ran []string
	ap := cancelApprover{cancel: cancel}
	m := newScripted(t, toolReply(`"t1","submit_job",{"script":"a"}`))
	a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: ap}
	hist, err := a.Send(ctx, nil, "x", Typed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(ran) != 0 {
		t.Errorf("tool ran after cancel: %v", ran)
	}
	if res := lastToolResults(t, hist); res["t1"] != [2]string{"canceled", "error"} {
		t.Errorf("results = %v", res)
	}
	assertValid(t, hist)
}

type cancelApprover struct{ cancel func() }

func (c cancelApprover) Approve(context.Context, Proposal) (Decision, error) {
	c.cancel()
	return Decision{Approved: true}, nil
}

func TestReviewMaxTokensWithToolUse(t *testing.T) {
	var ran []string
	ap := &fakeApprover{decision: Decision{Approved: true}}
	m := newScripted(t, replyWith("max_tokens",
		`{"type":"text","text":"cut"},{"type":"tool_use","id":"t1","name":"list_jobs","input":{}},{"type":"tool_use","id":"t2","name":"submit_job","input":{}}`))
	a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: ap}
	hist, err := a.Send(context.Background(), nil, "x", Typed)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v", err)
	}
	if len(ran) != 0 || len(ap.seen) != 0 {
		t.Errorf("ran %v, approvals %d", ran, len(ap.seen))
	}
	res := lastToolResults(t, hist)
	for _, id := range []string{"t1", "t2"} {
		if res[id][1] != "error" || !strings.Contains(res[id][0], "not run: response ended with max_tokens") {
			t.Errorf("%s = %v", id, res[id])
		}
	}
	assertValid(t, hist)
}

func TestReviewRefusalEmptyContent(t *testing.T) {
	var ran []string
	m := newScripted(t, replyWith("refusal", ""))
	a := &Agent{Model: m, Tools: newRegistry(t, &ran)}
	hist, err := a.Send(context.Background(), nil, "x", Typed)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v", err)
	}
	if len(hist) != 1 {
		t.Errorf("history len = %d, want only the user message", len(hist))
	}
	assertValid(t, hist)
}

func TestReviewMaxTokensText(t *testing.T) {
	m := newScripted(t, strings.Replace(textReply("hi"), "end_turn", "max_tokens", 1))
	a := &Agent{Model: m, Tools: NewRegistry()}
	hist, err := a.Send(context.Background(), nil, "x", Typed)
	if !errors.Is(err, ErrTruncated) || len(hist) != 2 {
		t.Errorf("err = %v, history len = %d", err, len(hist))
	}
}

func TestReviewNilApprover(t *testing.T) {
	var ran []string
	m := newScripted(t, toolReply(`"t1","list_jobs",{}`, `"t2","submit_job",{"script":"s"}`, `"t3","list_jobs",{}`))
	a := &Agent{Model: m, Tools: newRegistry(t, &ran)}
	hist, err := a.Send(context.Background(), nil, "x", Typed)
	if !errors.Is(err, ErrNoApprover) {
		t.Fatalf("err = %v", err)
	}
	if len(ran) != 1 || ran[0] != "list_jobs" {
		t.Errorf("ran = %v", ran)
	}
	res := lastToolResults(t, hist)
	if len(res) != 3 || res["t1"][0] != "no jobs" || res["t2"][1] != "error" || res["t3"][1] != "error" {
		t.Errorf("results = %v", res)
	}
	assertValid(t, hist)
}

func TestReviewEmptySkillSetNoEnum(t *testing.T) {
	set, err := skill.Load(fstest.MapFS{}, "embedded")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range SkillTools(set) {
		b, _ := json.Marshal(tool.Schema)
		if strings.Contains(string(b), "enum") {
			t.Errorf("%s schema has enum: %s", tool.Name, b)
		}
	}
}

func TestEmptyTextBlockNotInHistory(t *testing.T) {
	var ran []string
	reply := `{"id":"m","type":"message","role":"assistant","model":"x","stop_reason":"max_tokens","content":[{"type":"text","text":""}],"usage":{"input_tokens":1,"output_tokens":1}}`
	a := &Agent{Model: newScripted(t, reply), Tools: newRegistry(t, &ran), Approver: &fakeApprover{}}
	h, err := a.Send(context.Background(), nil, "hi", Typed)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v", err)
	}
	if len(h) != 1 {
		t.Fatalf("history len = %d, want 1 (only the user message)", len(h))
	}
}

func TestAbortEmitsToolResultEvents(t *testing.T) {
	var ran []string
	var events []Event
	m := newScripted(t, toolReply(`"s1","submit_job",{"script":"x"}`, `"t2","list_jobs",{}`))
	a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: &fakeApprover{err: errors.New("ui closed")}, OnEvent: func(e Event) { events = append(events, e) }}
	_, _ = a.Send(context.Background(), nil, "go", Typed)
	results := 0
	for _, e := range events {
		if e.Kind == EventToolResult && e.IsError {
			results++
		}
	}
	if results != 2 {
		t.Errorf("got %d error EventToolResult, want 2: %+v", results, events)
	}
}
