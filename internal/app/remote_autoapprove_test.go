package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
)

// The M37.3 safeguards apply to an automatically approved command too: the
// output is redacted and the result starts with the auto-run note.
func TestRemoteAutoApprovedOutputIsRedacted(t *testing.T) {
	fr := &fakeRunner{allowStdin: true, fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: "user=bob\ntoken=abc123secret\n"}, nil
	}}
	h := remoteHarness(t, fr)
	tool := h.tools["remote_command"]
	in := json.RawMessage(`{"cluster":"hpcone","argv":["bash","-c","cat cfg"]}`)
	p, err := tool.Propose(in)
	if err != nil {
		t.Fatal(err)
	}
	auto := &agent.AutoApprover{Next: approverFunc(func(agent.Proposal) agent.Decision { return agent.Decision{} }), On: func() bool { return true }}
	d, err := auto.Approve(context.Background(), p)
	if err != nil || !d.Auto {
		t.Fatalf("decision %+v, %v", d, err)
	}
	d.Proposal = p
	out, err := tool.Run(context.Background(), in, d)
	if err != nil || !strings.HasPrefix(out, remoteAutoNote+"\n") || strings.Contains(out, "abc123secret") || !strings.Contains(out, "user=bob") {
		t.Errorf("out = %q, %v", out, err)
	}
}

// M43.2: the proposal of a remote_command that names a sign-in secret file is
// marked Secret, so "Approve everything automatically" leaves it to the
// user; an ordinary gated command is not, and goes through the automatic
// approver unedited and still runs.
func TestRemoteProposalSecretFlagAndAutoApprover(t *testing.T) {
	fr := &fakeRunner{allowStdin: true}
	h := remoteHarness(t, fr)
	tool := h.tools["remote_command"]
	plain := json.RawMessage(`{"cluster":"hpcone","argv":["mkdir","-p","out"]}`)
	sec := json.RawMessage(`{"cluster":"hpcone","argv":["cat",".ssh/id_rsa"]}`)
	pp, err := tool.Propose(plain)
	if err != nil || pp.Secret {
		t.Fatalf("plain: %+v, %v", pp, err)
	}
	ps, err := tool.Propose(sec)
	if err != nil || !ps.Secret {
		t.Fatalf("secret read: %+v, %v", ps, err)
	}
	asked := 0
	next := approverFunc(func(agent.Proposal) agent.Decision { asked++; return agent.Decision{Approved: false, Reason: "user"} })
	auto := &agent.AutoApprover{Next: next, On: func() bool { return true }}
	d, err := auto.Approve(context.Background(), pp)
	if err != nil || !d.Approved || asked != 0 {
		t.Fatalf("plain via auto: %+v %v asked=%d", d, err, asked)
	}
	d.Proposal = pp
	if _, err := tool.Run(context.Background(), plain, d); err != nil {
		t.Errorf("auto-approved command did not run: %v", err)
	}
	d, err = auto.Approve(context.Background(), ps)
	if err != nil || d.Approved || asked != 1 {
		t.Fatalf("secret via auto: %+v %v asked=%d", d, err, asked)
	}
}

// An auto-approved submit gets the auto-run note first and the submit hint
// after it, so the runner (which checks the prefix) adds no second note.
func TestRemoteAutoApprovedSubmitSingleNote(t *testing.T) {
	fr := &fakeRunner{allowStdin: true, fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: "Submitted batch job 42\n"}, nil
	}}
	h := remoteHarness(t, fr)
	tool := h.tools["remote_command"]
	in := json.RawMessage(`{"cluster":"hpcone","argv":["sbatch","job.sh"]}`)
	p, err := tool.Propose(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Run(context.Background(), in, agent.Decision{Approved: true, Auto: true, Proposal: p})
	if err != nil || !strings.HasPrefix(out, agent.AutoRanPrefix) || !strings.Contains(out, remoteSubmitHint) {
		t.Errorf("out = %q, %v (want the auto-run note first, then the submit hint)", out, err)
	}
}

type approverFunc func(agent.Proposal) agent.Decision

func (f approverFunc) Approve(_ context.Context, p agent.Proposal) (agent.Decision, error) {
	return f(p), nil
}
