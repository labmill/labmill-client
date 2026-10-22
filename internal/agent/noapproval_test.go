package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// noApprovalRegistry has one gated tool, "remote", whose Propose returns
// ErrNoApproval for {"ro":true} and a Proposal otherwise. Run records the
// raw input and the Decision it got.
func noApprovalRegistry(t *testing.T, inputs *[]string, decisions *[]Decision) *Registry {
	t.Helper()
	reg := NewRegistry()
	err := reg.Register(Tool{
		Name: "remote", Description: "Run a command.", Schema: schema(map[string]any{}), NeedsApproval: true,
		Propose: func(input json.RawMessage) (Proposal, error) {
			var in struct {
				RO bool `json:"ro"`
			}
			json.Unmarshal(input, &in)
			if in.RO {
				return Proposal{}, ErrNoApproval
			}
			return Proposal{Tool: "remote", Summary: "change things"}, nil
		},
		Run: func(_ context.Context, input json.RawMessage, d Decision) (string, error) {
			*inputs = append(*inputs, string(input))
			*decisions = append(*decisions, d)
			return "ran " + string(input), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// A gated tool whose Propose returns ErrNoApproval runs at once with the
// zero Decision and nobody is asked (even with no Approver); other calls of
// the same tool still ask; calls run one at a time in Claude's order.
func TestProposeErrNoApproval(t *testing.T) {
	var inputs []string
	var decisions []Decision
	reg := noApprovalRegistry(t, &inputs, &decisions)

	// No approver: a read-only call runs, raw bytes intact.
	m := newScripted(t, toolReply(`"t1","remote",{"ro":true,"n":12345678901234567890}`), textReply("done"))
	a := &Agent{Model: m, Tools: reg}
	hist, err := a.Send(context.Background(), nil, "x", Typed)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Approved || decisions[0].Proposal.Summary != "" {
		t.Errorf("decisions = %+v", decisions)
	}
	if len(inputs) != 1 || !strings.Contains(inputs[0], "12345678901234567890") {
		t.Errorf("inputs = %q", inputs)
	}
	assertValid(t, hist)

	// With an approver: only the call that needs approval asks, in order.
	inputs, decisions = nil, nil
	ap := &fakeApprover{decision: Decision{Approved: true}}
	var events []string
	m = newScripted(t, toolReply(`"t1","remote",{"ro":true,"i":1}`, `"t2","remote",{"ro":false,"i":2}`, `"t3","remote",{"ro":true,"i":3}`), textReply("done"))
	a = &Agent{Model: m, Tools: reg, Approver: ap, OnEvent: func(e Event) {
		switch e.Kind {
		case EventToolCall:
			events = append(events, "call")
		case EventApproved:
			events = append(events, "approved")
		case EventToolResult:
			events = append(events, "result")
		}
	}}
	hist, err = a.Send(context.Background(), nil, "x", Typed)
	if err != nil {
		t.Fatal(err)
	}
	if len(ap.seen) != 1 || ap.seen[0].Summary != "change things" {
		t.Errorf("approver saw %+v", ap.seen)
	}
	if len(decisions) != 3 || decisions[0].Approved || !decisions[1].Approved || decisions[1].Proposal.Summary != "change things" || decisions[2].Approved {
		t.Errorf("decisions = %+v", decisions)
	}
	if len(inputs) != 3 || !strings.Contains(inputs[0], `"i":1`) || !strings.Contains(inputs[1], `"i":2`) || !strings.Contains(inputs[2], `"i":3`) {
		t.Errorf("not in Claude's order: %q", inputs)
	}
	if got := strings.Join(events, " "); got != "call result call approved result call result" {
		t.Errorf("events = %s", got)
	}
	// The tool results went back to Claude in the next request.
	if len(hist) < 3 {
		t.Fatalf("history len = %d", len(hist))
	}
	res := lastToolResults(t, hist[:len(hist)-1])
	if len(res) != 3 || res["t1"][1] != "" || res["t2"][1] != "" || res["t3"][1] != "" || !strings.HasPrefix(res["t3"][0], "ran ") {
		t.Errorf("results = %v", res)
	}
	assertValid(t, hist)

	// No approver and a call that needs approval: Send fails fast, the
	// read-only call before it keeps its result.
	inputs, decisions = nil, nil
	m = newScripted(t, toolReply(`"t1","remote",{"ro":true}`, `"t2","remote",{"ro":false}`))
	a = &Agent{Model: m, Tools: reg}
	hist, err = a.Send(context.Background(), nil, "x", Typed)
	if !errors.Is(err, ErrNoApprover) {
		t.Fatalf("err = %v, want ErrNoApprover", err)
	}
	res = lastToolResults(t, hist)
	if len(inputs) != 1 || len(res) != 2 || !strings.HasPrefix(res["t1"][0], "ran ") || res["t2"][1] != "error" {
		t.Errorf("inputs = %q, results = %v", inputs, res)
	}
	assertValid(t, hist)
}
