package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"labmill-client/internal/agent"
	"labmill-client/internal/store"
)

// job_worked: the user's one-tap answer to "did it work?" after a job that
// ran new code. ask_rse: a request for help to the user's research computing
// team (POST /api/help), with the failure facts the user approves.

type jobWorkedInput struct {
	JobID  int64 `json:"job_id"`
	Worked *bool `json:"worked"`
}

func (t *toolset) jobWorked() agent.Tool {
	return agent.Tool{
		Name: "job_worked",
		Description: "Record the user's answer to \"Did it work?\" for an ended job that ran new code (code you wrote or changed for this job). " +
			"After such a job ends, ask once with ask_user (options Yes and No, allow_other false), then call this with the answer. Never guess the answer, and never ask for a job that only re-ran unchanged code. " +
			"It sends only the word yes or no, with the job's reference, to the user's Labmill account (nothing when no account is linked). Do not use it for problems.",
		Schema: objSchema(map[string]any{
			"job_id": intProp("the job's id"),
			"worked": map[string]any{"type": "boolean", "description": "true when the user said it worked"},
		}, "job_id", "worked"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in jobWorkedInput
			if err := decode(input, &in); err != nil {
				return "", err
			}
			if in.Worked == nil {
				return "", errors.New("worked must be true or false: ask the user first.")
			}
			j, err := t.job(ctx, in.JobID)
			if err != nil {
				return "", err
			}
			switch j.State {
			case store.StateCompleted, store.StateFetched, store.StateFailed, store.StateCanceled:
			default:
				return "", fmt.Errorf("Job %d has not ended, so it is too early to ask.", j.ID)
			}
			if t.env.Worked == nil {
				return "Noted. (No Labmill account is connected, so nothing was sent.)", nil
			}
			if err := t.env.Worked(ctx, j.ID, *in.Worked); err != nil {
				return "", errors.New(sanitize(err.Error()))
			}
			return "Noted. Thank the user in one short sentence.", nil
		},
	}
}

// RSEHelp is what Env.RSEPrepare returns: the team that gets the request and
// the report text the user approves.
type RSEHelp struct {
	Tenant string // the team's id, sent as is
	Team   string // its name, shown
	Report string // JSON, "" when no job is attached
}

type askRSEInput struct {
	JobID   int64  `json:"job_id"`
	Team    string `json:"team"`
	Body    string `json:"body"`
	Details bool   `json:"details"`
}

const maxHelpBody = 2000

func (t *toolset) rseHelp(ctx context.Context, input json.RawMessage) (askRSEInput, RSEHelp, error) {
	var in askRSEInput
	if err := decode(input, &in); err != nil {
		return in, RSEHelp{}, err
	}
	in.Body = strings.TrimSpace(in.Body)
	if in.Body == "" || len(in.Body) > maxHelpBody {
		return in, RSEHelp{}, fmt.Errorf("body must say in plain words what the user needs (up to %d bytes); ask the user", maxHelpBody)
	}
	if t.env.RSEPrepare == nil || t.env.RSESend == nil {
		return in, RSEHelp{}, errors.New("Asking the research computing team isn't available: no Labmill account is connected. Tell the user to run labmill login, or to contact their team directly.")
	}
	if in.JobID != 0 {
		if _, err := t.job(ctx, in.JobID); err != nil {
			return in, RSEHelp{}, err
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	h, err := t.env.RSEPrepare(cctx, in.JobID, in.Team, in.Details)
	if err != nil {
		return in, h, errors.New(sanitize(err.Error()))
	}
	return in, h, nil
}

func askRSEProposal(in askRSEInput, h RSEHelp) agent.Proposal {
	var b strings.Builder
	fmt.Fprintf(&b, "To: the research computing team %s (team id %s, through your Labmill account).\n\nMessage:\n%s\n", sanitize(h.Team), sanitize(h.Tenant), sanitizeLines(in.Body))
	if h.Report != "" {
		fmt.Fprintf(&b, "\nReport attached (the team reads it, you can say no to the whole request):\n%s\n", sanitizeLines(h.Report))
	} else {
		b.WriteString("\nNo job report is attached.\n")
	}
	b.WriteString("Nothing else is sent. The team answers in your Labmill account.")
	return agent.Proposal{
		Tool:    "ask_rse",
		Summary: fmt.Sprintf("Ask the research computing team %s for help", sanitize(h.Team)),
		Details: b.String(),
		// Text leaves the computer: always asks, also under "Approve everything automatically".
		Secret: true,
	}
}

func (t *toolset) askRSE() agent.Tool {
	return agent.Tool{
		Name: "ask_rse",
		Description: "Ask the user's research computing team (the RSEs of their institution, through the Labmill account) for help, in one tap. Use it when the user asks for a person, or when a job keeps failing and you cannot fix it. " +
			"body: what the user needs, in plain words, up to 2000 bytes; no secrets. job_id: the failed job, to attach its scheduler facts (even a private job; the team reads the facts, not the job). " +
			"details: true also attaches the redacted script and last output lines (not for a private job). The user sees the whole message and report and approves. The reply arrives in the Labmill account.",
		Schema: objSchema(map[string]any{
			"body":    strProp("what the user needs, plain words"),
			"job_id":  intProp("the job to attach facts of; 0 or leave out for none"),
			"details": map[string]any{"type": "boolean", "description": "also attach the redacted script and output lines (default false)"},
			"team":    strProp("the team's name; leave out when the account belongs to one team"),
		}, "body"),
		NeedsApproval: true,
		Propose: func(input json.RawMessage) (agent.Proposal, error) {
			in, h, err := t.rseHelp(context.Background(), input)
			if err != nil {
				return agent.Proposal{}, err
			}
			return askRSEProposal(in, h), nil
		},
		Run: func(ctx context.Context, input json.RawMessage, dec agent.Decision) (string, error) {
			in, h, err := t.rseHelp(ctx, input)
			if err != nil {
				return "", err
			}
			if err := sameAsApproved(askRSEProposal(in, h), dec.Proposal); err != nil {
				return "", err
			}
			id, err := t.env.RSESend(ctx, h, in.Body)
			if err != nil {
				return "", errors.New(sanitize(err.Error()))
			}
			return fmt.Sprintf("Sent to %s (request %s). Tell the user the team answers in their Labmill account.", sanitize(h.Team), sanitize(id)), nil
		},
	}
}
