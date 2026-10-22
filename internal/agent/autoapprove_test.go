package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// M43.2: the decision table of "Approve everything automatically".
func TestAutoApproverDecisionTable(t *testing.T) {
	plain := Proposal{Tool: "upload_data", Summary: "Upload 2 files"}
	editable := Proposal{Tool: "submit_job", Summary: "Submit", Details: "script", Editable: true}
	secret := Proposal{Tool: "remote_command", Summary: "Run: cat ~/.ssh/id_ed25519", Secret: true}
	for _, tc := range []struct {
		name     string
		on       func() bool
		p        Proposal
		wantAuto bool
	}{
		{"on, plain", func() bool { return true }, plain, true},
		{"on, editable", func() bool { return true }, editable, true},
		{"on, secret read", func() bool { return true }, secret, false},
		{"off, plain", func() bool { return false }, plain, false},
		{"off, editable", func() bool { return false }, editable, false},
		{"off, secret read", func() bool { return false }, secret, false},
		{"no setting hook, plain", nil, plain, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := &fakeApprover{decision: Decision{Approved: false, Reason: "asked"}}
			var noted []Proposal
			a := &AutoApprover{Next: next, On: tc.on, Note: func(p Proposal) { noted = append(noted, p) }}
			d, err := a.Approve(context.Background(), tc.p)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantAuto {
				if !d.Approved || d.Edited != "" || len(next.seen) != 0 || len(noted) != 1 || noted[0].Summary != tc.p.Summary {
					t.Errorf("decision %+v, asked %d, noted %v", d, len(next.seen), noted)
				}
				return
			}
			if d.Approved || d.Reason != "asked" || len(next.seen) != 1 || len(noted) != 0 {
				t.Errorf("decision %+v, asked %d, noted %v", d, len(next.seen), noted)
			}
		})
	}
}

func TestAutoApproverReadsTheSettingEveryTime(t *testing.T) {
	on := false
	next := &fakeApprover{decision: Decision{Approved: true}}
	a := &AutoApprover{Next: next, On: func() bool { return on }}
	for i, want := range []bool{false, true, false} {
		on = want
		if _, err := a.Approve(context.Background(), Proposal{Tool: "x"}); err != nil {
			t.Fatal(err)
		}
		if got := len(next.seen); got != []int{1, 1, 2}[i] {
			t.Fatalf("step %d: Next asked %d times", i, got)
		}
	}
}

func TestAutoApproverWithoutNextOrWithCanceledContext(t *testing.T) {
	a := &AutoApprover{On: func() bool { return false }}
	if _, err := a.Approve(context.Background(), Proposal{}); !errors.Is(err, ErrNoApprover) {
		t.Errorf("err = %v, want ErrNoApprover", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := &AutoApprover{On: func() bool { return true }}
	if _, err := b.Approve(ctx, Proposal{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// Through the loop: a gated tool runs unedited without the user's Approver
// being asked, and the usual EventApproved follows.
func TestSendWithAutoApprover(t *testing.T) {
	var ran []string
	var events []Event
	next := &fakeApprover{decision: Decision{Approved: false, Reason: "should not be asked"}}
	ap := &AutoApprover{Next: next, On: func() bool { return true }}
	m := newScripted(t, toolReply(`"s1","submit_job",{"script":"python train.py"}`), textReply("Submitted."))
	a := &Agent{Model: m, Tools: newRegistry(t, &ran), Approver: ap, OnEvent: func(e Event) { events = append(events, e) }}
	if _, err := a.Send(context.Background(), nil, "run it", Typed); err != nil {
		t.Fatal(err)
	}
	if len(next.seen) != 0 || len(ran) != 1 || strings.Contains(ran[0], "should not") {
		t.Errorf("asked %d, ran %v", len(next.seen), ran)
	}
	noted := false
	for _, e := range events {
		noted = noted || (e.Kind == EventToolResult && strings.HasPrefix(e.Text, AutoApprovedNote))
	}
	if !noted {
		t.Error("the result carries no note that nobody reviewed the step")
	}
	approved := false
	for _, e := range events {
		approved = approved || (e.Kind == EventApproved && e.Tool == "submit_job")
	}
	if !approved {
		t.Error("no EventApproved for the automatic approval")
	}
}
