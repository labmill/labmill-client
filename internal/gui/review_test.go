package gui

import (
	"context"
	"sync"
	"testing"
	"time"

	"labmill-client/internal/agent"
	"labmill-client/internal/sshx"
)

func TestAbandonedApproveEmitsDismiss(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := b.Approve(ctx, agent.Proposal{Summary: "s"}); done <- err }()
	ev := rec.waitFor(t, EvApproval).(ApprovalEvent)
	cancel()
	if err := <-done; err == nil {
		t.Error("Approve returned nil after cancel")
	}
	d := rec.waitFor(t, EvDismiss).(DismissEvent)
	if len(d.IDs) != 1 || d.IDs[0] != ev.ID {
		t.Errorf("dismiss = %+v, want [%s]", d, ev.ID)
	}
	if err := b.Decide(ev.ID, true, "", ""); err == nil {
		t.Error("Decide accepted an abandoned request")
	}
}

func TestAbandonedAskEmitsDismiss(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := b.Ask(ctx, sshx.Question{Kind: sshx.AskPassword}); done <- err }()
	ev := rec.waitFor(t, EvPrompt).(PromptEvent)
	cancel()
	if err := <-done; err == nil {
		t.Error("Ask returned nil after cancel")
	}
	d := rec.waitFor(t, EvDismiss).(DismissEvent)
	if len(d.IDs) != 1 || d.IDs[0] != ev.ID {
		t.Errorf("dismiss = %+v, want [%s]", d, ev.ID)
	}
}

func TestAlteredFlag(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	go b.Approve(context.Background(), agent.Proposal{Summary: "s", Details: "rm‮ -rf"})
	ev := rec.waitFor(t, EvApproval).(ApprovalEvent)
	if !ev.Altered || ev.Details != "rm -rf" {
		t.Errorf("event = %+v", ev)
	}
	b.Close()

	rec2 := newRecorder()
	b2 := NewBridge(rec2.emit)
	go b2.Approve(context.Background(), agent.Proposal{Summary: "s", Details: "plain"})
	if ev := rec2.waitFor(t, EvApproval).(ApprovalEvent); ev.Altered {
		t.Errorf("clean details flagged altered: %+v", ev)
	}
	b2.Close()
}

func decideEditable(t *testing.T, edited string) agent.Decision {
	t.Helper()
	rec := newRecorder()
	b := NewBridge(rec.emit)
	got := make(chan agent.Decision, 1)
	go func() {
		d, _ := b.Approve(context.Background(), agent.Proposal{Summary: "s", Details: "#!/bin/sh\necho hi", Editable: true})
		got <- d
	}()
	ev := rec.waitFor(t, EvApproval).(ApprovalEvent)
	if err := b.Decide(ev.ID, true, edited, ""); err != nil {
		t.Fatal(err)
	}
	return <-got
}

func TestEditableChangedSetsEdited(t *testing.T) {
	if d := decideEditable(t, "#!/bin/sh\necho bye"); d.Edited != "#!/bin/sh\necho bye" || !d.Approved {
		t.Errorf("decision = %+v", d)
	}
}

func TestEditableUnchangedLeavesEditedEmpty(t *testing.T) {
	for _, e := range []string{"#!/bin/sh\necho hi", ""} {
		if d := decideEditable(t, e); d.Edited != "" || !d.Approved {
			t.Errorf("edited %q: decision = %+v", e, d)
		}
	}
}

func TestDecideRacingDoneOneReply(t *testing.T) {
	for i := 0; i < 200; i++ {
		rec := newRecorder()
		b := NewBridge(rec.emit)
		got := make(chan agent.Decision, 2)
		go func() { d, _ := b.Approve(context.Background(), agent.Proposal{Summary: "s"}); got <- d }()
		ev := rec.waitFor(t, EvApproval).(ApprovalEvent)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); b.Decide(ev.ID, true, "", "") }()
		go func() { defer wg.Done(); b.Done(nil) }()
		wg.Wait()
		<-got
		select {
		case <-got:
			t.Fatal("two replies for one request")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestSendAfterShutdownFails(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	a := NewApp(b, &fakeSession{b: b}, nil, "")
	a.Shutdown()
	if err := a.Send("hello"); err == nil {
		t.Error("Send after Shutdown succeeded")
	}
}
