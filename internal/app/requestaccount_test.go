package app

import (
	"context"
	"strings"
	"testing"

	"labmill-client/internal/config"
)

func TestRequestAccountOpensMailAfterApproval(t *testing.T) {
	h := newHarness(t)
	var opened string
	h.env.AccountRequest = func(_ context.Context, c config.Cluster, reason string) (AccountRequestDraft, error) {
		return AccountRequestDraft{To: "help@example.edu", Subject: "Account", Body: "Reason: " + reason, OpenURL: "mailto:help@example.edu?subject=Account", Approver: "your PI", WaitTime: "2 days"}, nil
	}
	h.env.OpenLink = func(u string) error { opened = u; return nil }
	tool := h.tools["request_account"]
	p, err := tool.Propose([]byte(`{"cluster":"hpcone","reason":"RNA-seq"}`))
	if err != nil || !strings.Contains(p.Details, "To: help@example.edu") || !strings.Contains(p.Details, "Reason: RNA-seq") || opened != "" {
		t.Fatalf("proposal %+v %v (opened %q)", p, err, opened)
	}
	out, err := h.run(t, "request_account", map[string]any{"cluster": "hpcone", "reason": "RNA-seq"})
	if err != nil || opened == "" || !strings.Contains(out, "Wait (from the site, data): 2 days") {
		t.Fatalf("run: %q %v (opened %q)", out, err, opened)
	}
}

func TestRequestAccountNoPathOrReason(t *testing.T) {
	h := newHarness(t)
	h.env.AccountRequest = func(context.Context, config.Cluster, string) (AccountRequestDraft, error) {
		return AccountRequestDraft{URL: "https://example.edu/accounts", Steps: "ask your PI"}, nil
	}
	h.env.OpenLink = func(string) error { t.Error("opened without a path"); return nil }
	_, err := h.run(t, "request_account", map[string]any{"cluster": "hpcone", "reason": "x"})
	if err == nil || !strings.Contains(err.Error(), "https://example.edu/accounts") {
		t.Errorf("no path: %v", err)
	}
	if _, err := h.run(t, "request_account", map[string]any{"cluster": "hpcone", "reason": " "}); err == nil {
		t.Error("empty reason accepted")
	}
}
