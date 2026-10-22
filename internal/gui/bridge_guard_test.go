package gui

import (
	"context"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/agent"
)

func TestApproveOnlyOldestPending(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	got := make(chan agent.Decision, 2)
	for i := 0; i < 2; i++ {
		go func() {
			d, _ := b.Approve(context.Background(), agent.Proposal{Summary: "s", Details: "x"})
			got <- d
		}()
		// Wait for the i-th approval event so the order is deterministic.
		deadline := time.Now().Add(2 * time.Second)
		for {
			rec.mu.Lock()
			n := 0
			for _, e := range rec.events {
				if e.name == EvApproval {
					n++
				}
			}
			rec.mu.Unlock()
			if n == i+1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("approval not emitted")
			}
			time.Sleep(time.Millisecond)
		}
	}
	rec.mu.Lock()
	var ids []string
	for _, e := range rec.events {
		if e.name == EvApproval {
			ids = append(ids, e.data.(ApprovalEvent).ID)
		}
	}
	rec.mu.Unlock()
	if err := b.Decide(ids[1], true, "", ""); err == nil {
		t.Fatal("approving the newer approval was accepted")
	}
	// The out-of-order approval was answered as declined, not left waiting.
	if d := <-got; d.Approved || d.Reason != "approved out of order" {
		t.Errorf("out-of-order decision = %+v", d)
	}
	if err := b.Decide(ids[1], false, "", ""); err == nil {
		t.Error("the declined approval can be answered again")
	}
	if err := b.Decide(ids[0], true, "", ""); err != nil {
		t.Fatalf("approve oldest: %v", err)
	}
	if d := <-got; !d.Approved {
		t.Errorf("oldest decision = %+v", d)
	}
}

func TestTextDeltasSplitEscape(t *testing.T) {
	rec := newRecorder()
	b := NewBridge(rec.emit)
	for _, d := range []string{"hi \x1b", "[31", "mred\x1b[0m done"} {
		b.Event(agent.Event{Kind: agent.EventText, Text: d})
	}
	b.Done(nil)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var sb strings.Builder
	for _, e := range rec.events {
		if e.name == EvText {
			sb.WriteString(e.data.(TextEvent).Text)
		}
	}
	if sb.String() != "hi red done" {
		t.Errorf("text = %q", sb.String())
	}
}
