package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/store"
)

func TestJobWorked(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	done, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "a", Cluster: "hpcone", Backend: "slurm", RemoteID: "5", State: store.StateCompleted})
	run, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "b", Cluster: "hpcone", Backend: "slurm", RemoteID: "6", State: store.StateRunning})
	out, err := h.run(t, "job_worked", map[string]any{"job_id": done, "worked": true})
	if err != nil || !strings.Contains(out, "nothing was sent") {
		t.Errorf("no account: %q %v", out, err)
	}
	var gotID int64
	var gotYes bool
	h.env.Worked = func(_ context.Context, id int64, yes bool) error { gotID, gotYes = id, yes; return nil }
	if _, err := h.run(t, "job_worked", map[string]any{"job_id": done, "worked": false}); err != nil || gotID != done || gotYes {
		t.Errorf("worked: %d %v %v", gotID, gotYes, err)
	}
	if _, err := h.run(t, "job_worked", map[string]any{"job_id": run, "worked": true}); err == nil {
		t.Error("a running job was accepted")
	}
	if _, err := h.run(t, "job_worked", map[string]any{"job_id": done}); err == nil {
		t.Error("a missing answer was accepted")
	}
}

func TestAskRSE(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "a", Cluster: "hpcone", Backend: "slurm", RemoteID: "5", State: store.StateFailed})
	in := map[string]any{"body": "my job fails with an out of memory error", "job_id": id}
	if _, err := h.run(t, "ask_rse", in); err == nil || !strings.Contains(err.Error(), "labmill login") {
		t.Errorf("no account: %v", err)
	}
	report := `{"state":"failed"}`
	var sent string
	h.env.RSEPrepare = func(context.Context, int64, string, bool) (RSEHelp, error) {
		return RSEHelp{Team: "Example RCI", Report: report}, nil
	}
	h.env.RSESend = func(_ context.Context, _ int64, team, body string, _ bool) (string, error) {
		sent = body
		return "h1", nil
	}
	tool := h.tools["ask_rse"]
	b, _ := json.Marshal(in)
	p, err := tool.Propose(b)
	if err != nil || !p.Secret || !strings.Contains(p.Details, "Example RCI") || !strings.Contains(p.Details, `"state"`) {
		t.Fatalf("proposal: %+v %v", p, err)
	}
	out, err := h.run(t, "ask_rse", in)
	if err != nil || sent != in["body"] || !strings.Contains(out, "h1") {
		t.Errorf("send: %q %q %v", out, sent, err)
	}
	// The report changed after approval: refused, nothing sent.
	sent = ""
	report = `{"state":"failed","script":"x"}`
	if _, err := tool.Run(ctx, b, agent.Decision{Approved: true, Proposal: p}); err == nil || sent != "" {
		t.Errorf("a changed report was sent: %v", err)
	}
	if _, err := h.run(t, "ask_rse", map[string]any{"body": " "}); err == nil {
		t.Error("an empty body was accepted")
	}
}
